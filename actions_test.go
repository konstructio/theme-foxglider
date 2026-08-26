package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeActionsGitLab serves the minimum action surface: a latest pipeline, its
// jobs (release + trigger:manual in playable state), and a play endpoint that
// records the variables it received.
func fakeActionsGitLab(t *testing.T, played map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.EscapedPath()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(p, "/pipelines/latest"):
			w.Write([]byte(`{"id":500,"status":"success","ref":"main","sha":"cafe","web_url":"http://gl/x/-/pipelines/500","created_at":"2026-08-25T00:00:00Z","updated_at":"2026-08-25T00:01:00Z"}`))
		case strings.HasSuffix(p, "/pipelines/500/jobs"):
			w.Write([]byte(`[{"id":1,"name":"publish:chart:rc","status":"success"},{"id":2,"name":"release","status":"manual"},{"id":3,"name":"trigger:manual","status":"manual"}]`))
		case strings.HasSuffix(p, "/jobs/2/play"), strings.HasSuffix(p, "/jobs/3/play"):
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			played[p] = body
			w.Write([]byte(`{"id":2,"name":"x","status":"pending","web_url":"http://gl/x/-/jobs/2","pipeline":{"id":500,"web_url":"http://gl/x/-/pipelines/500"}}`))
		default:
			w.WriteHeader(404)
		}
	}))
}

func TestActions(t *testing.T) {
	played := map[string]any{}
	gl := fakeActionsGitLab(t, played)
	defer gl.Close()
	t.Setenv("GITLAB_ACTION_TOKEN", "act-tok")
	srv := httptest.NewServer(newAPI(newGLClient(gl.URL, "tok"), nil))
	defer srv.Close()

	// status reports the server-set actor — no roster, nothing to pick.
	res, err := http.Get(srv.URL + "/api/actions/status")
	if err != nil {
		t.Fatal(err)
	}
	var st struct {
		Enabled bool   `json:"enabled"`
		Actor   string `json:"actor"`
	}
	json.NewDecoder(res.Body).Decode(&st)
	if !st.Enabled || st.Actor != "konstruct" {
		t.Fatalf("status = %+v (want enabled, neutral fallback actor)", st)
	}

	post := func(body string) *http.Response {
		req, _ := http.NewRequest("POST", srv.URL+"/api/actions/run", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		// the konstruct shell communicated this session's identity
		req.Header.Set("X-Konstruct-Actor", "kbot")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	if res := post(`{"project":"civo/metaphor/charts","action":"trigger"}`); res.StatusCode != 200 {
		t.Fatalf("trigger = %d", res.StatusCode)
	}
	if res := post(`{"project":"civo/metaphor/charts","action":"release"}`); res.StatusCode != 400 {
		t.Fatalf("release without typed confirm = %d, want 400", res.StatusCode)
	}
	if res := post(`{"project":"civo/metaphor/charts","action":"release","confirm":"charts"}`); res.StatusCode != 200 {
		t.Fatalf("release = %d", res.StatusCode)
	}
	if res := post(`{"project":"civo/other/thing","action":"trigger"}`); res.StatusCode != 400 {
		t.Fatalf("off-topology project = %d, want 400", res.StatusCode)
	}

	// a malformed communicated identity must fall back, not pass through
	badreq, _ := http.NewRequest("POST", srv.URL+"/api/actions/run",
		bytes.NewBufferString(`{"project":"civo/metaphor/charts","action":"trigger"}`))
	badreq.Header.Set("Content-Type", "application/json")
	badreq.Header.Set("X-Konstruct-Actor", "evil user!! not@a@username")
	resBad, err := http.DefaultClient.Do(badreq)
	if err != nil {
		t.Fatal(err)
	}
	if resBad.StatusCode != 200 {
		t.Fatalf("trigger with malformed actor header = %d", resBad.StatusCode)
	}
	for path, b := range played {
		bb, _ := json.Marshal(b)
		if !strings.Contains(string(bb), "INITIATED_BY") {
			t.Fatalf("play %s missing INITIATED_BY: %s", path, bb)
		}
		if strings.Contains(string(bb), "evil") {
			t.Fatalf("malformed identity leaked into %s: %s", path, bb)
		}
	}
	if len(played) != 2 {
		t.Fatalf("played = %d distinct jobs, want 2 (trigger + release)", len(played))
	}
}

func TestActorEnvOverride(t *testing.T) {
	t.Setenv("GITLAB_ACTION_TOKEN", "act-tok")
	t.Setenv("ACTION_ACTOR", "konstruct-sso-user")
	srv := httptest.NewServer(newAPI(newGLClient("http://unused", "tok"), nil))
	defer srv.Close()
	res, _ := http.Get(srv.URL + "/api/actions/status")
	var st struct {
		Actor string `json:"actor"`
	}
	json.NewDecoder(res.Body).Decode(&st)
	if st.Actor != "konstruct-sso-user" {
		t.Fatalf("actor = %q, want env override", st.Actor)
	}
}

// When the latest pipeline has no playable trigger job (e.g. a zero-job
// config-error pipeline), trigger falls back to creating a fresh pipeline.
func TestTriggerFallsBackToFreshPipeline(t *testing.T) {
	var created map[string]any
	gl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.EscapedPath()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(p, "/pipelines/latest"):
			w.Write([]byte(`{"id":600,"status":"failed","ref":"main","sha":"dead","web_url":"http://gl/x/-/pipelines/600","created_at":"2026-08-25T00:00:00Z","updated_at":"2026-08-25T00:00:01Z"}`))
		case strings.HasSuffix(p, "/pipelines/600/jobs"):
			w.Write([]byte(`[]`)) // zero jobs — nothing playable
		case strings.HasSuffix(p, "/pipeline"): // create-pipeline fallback
			json.NewDecoder(r.Body).Decode(&created)
			w.Write([]byte(`{"id":601,"status":"created","ref":"main","sha":"dead","web_url":"http://gl/x/-/pipelines/601","created_at":"2026-08-25T00:01:00Z","updated_at":"2026-08-25T00:01:00Z"}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer gl.Close()
	t.Setenv("GITLAB_ACTION_TOKEN", "act-tok")
	srv := httptest.NewServer(newAPI(newGLClient(gl.URL, "tok"), nil))
	defer srv.Close()

	res, err := http.Post(srv.URL+"/api/actions/run", "application/json",
		strings.NewReader(`{"project":"civo/metaphor/charts","action":"trigger"}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != 200 {
		t.Fatalf("fallback trigger = %d, want 200", res.StatusCode)
	}
	bb, _ := json.Marshal(created)
	if !strings.Contains(string(bb), "INITIATED_BY") || !strings.Contains(string(bb), "konstruct") {
		t.Fatalf("fresh pipeline missing fallback INITIATED_BY: %s", bb)
	}
	// release must NOT silently fall back — it still refuses honestly
	res2, _ := http.Post(srv.URL+"/api/actions/run", "application/json",
		strings.NewReader(`{"project":"civo/metaphor/charts","action":"release","confirm":"charts"}`))
	if res2.StatusCode != 409 {
		t.Fatalf("release on jobless pipeline = %d, want 409", res2.StatusCode)
	}
}

func TestActionsDisabledWithoutToken(t *testing.T) {
	srv := httptest.NewServer(newAPI(newGLClient("http://unused", "tok"), nil))
	defer srv.Close()
	res, _ := http.Get(srv.URL + "/api/actions/status")
	var st struct {
		Enabled bool `json:"enabled"`
	}
	json.NewDecoder(res.Body).Decode(&st)
	if st.Enabled {
		t.Fatal("actions must be disabled without GITLAB_ACTION_TOKEN")
	}
	res2, _ := http.Post(srv.URL+"/api/actions/run", "application/json", strings.NewReader(`{}`))
	if res2.StatusCode != 503 {
		t.Fatalf("run without token = %d, want 503", res2.StatusCode)
	}
}

// deliver: runs the tag pipeline for the newest RC and auto-merges the dev
// bump MR the deploy bridge produces — and only that MR.
func TestDeliver(t *testing.T) {
	var createdRef string
	var createdVars []byte
	merged := map[int]bool{}
	gl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.EscapedPath()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(p, "/repository/tags"):
			w.Write([]byte(`[{"name":"metaphor-v0.2.0-rc.9"},{"name":"metaphor-v0.2.0-rc.8"}]`))
		case strings.HasSuffix(p, "/pipeline"): // create on tag
			var body struct {
				Ref       string `json:"ref"`
				Variables []struct{ Key, Value string }
			}
			raw, _ := json.Marshal(func() any { var v map[string]any; json.NewDecoder(r.Body).Decode(&v); return v }())
			createdVars = raw
			var v map[string]any
			json.Unmarshal(raw, &v)
			body.Ref, _ = v["ref"].(string)
			createdRef = body.Ref
			w.Write([]byte(`{"id":700,"status":"created","ref":"` + body.Ref + `","sha":"t","web_url":"http://gl/charts/-/pipelines/700","created_at":"2026-08-26T00:00:00Z","updated_at":"2026-08-26T00:00:00Z"}`))
		case strings.HasSuffix(p, "/merge_requests") && r.Method == "GET":
			w.Write([]byte(`[{"iid":55,"title":"chore: bump metaphor-macro to 0.2.0-rc.9 (release_preview)","state":"opened","web_url":"http://gl/gitops/-/merge_requests/55"},{"iid":56,"title":"chore: bump metaphor-macro to 0.9.9-rc.1 (release_preview)","state":"opened","web_url":"http://gl/gitops/-/merge_requests/56"}]`))
		case strings.HasSuffix(p, "/approve"):
			w.Write([]byte(`{}`))
		case strings.HasSuffix(p, "/merge"):
			for _, seg := range strings.Split(p, "/") {
				if seg == "55" {
					merged[55] = true
				}
				if seg == "56" {
					merged[56] = true
				}
			}
			w.Write([]byte(`{"iid":55,"state":"merged"}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer gl.Close()
	t.Setenv("GITLAB_ACTION_TOKEN", "act-tok")
	srv := httptest.NewServer(newAPI(newGLClient(gl.URL, "tok"), nil))
	defer srv.Close()

	// wrong project refused
	res, _ := http.Post(srv.URL+"/api/actions/run", "application/json",
		strings.NewReader(`{"project":"civo/metaphor/metaphor","action":"deliver","confirm":"metaphor"}`))
	if res.StatusCode != 400 {
		t.Fatalf("deliver on service = %d, want 400", res.StatusCode)
	}
	// no confirm refused
	res, _ = http.Post(srv.URL+"/api/actions/run", "application/json",
		strings.NewReader(`{"project":"civo/metaphor/charts","action":"deliver"}`))
	if res.StatusCode != 400 {
		t.Fatalf("deliver without confirm = %d, want 400", res.StatusCode)
	}
	// the real thing
	res, err := http.Post(srv.URL+"/api/actions/run", "application/json",
		strings.NewReader(`{"project":"civo/metaphor/charts","action":"deliver","confirm":"charts"}`))
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		OK      bool   `json:"ok"`
		Version string `json:"version"`
		Tag     string `json:"tag"`
	}
	json.NewDecoder(res.Body).Decode(&out)
	if res.StatusCode != 200 || !out.OK || out.Tag != "metaphor-v0.2.0-rc.9" || out.Version != "0.2.0-rc.9" {
		t.Fatalf("deliver = %d %+v", res.StatusCode, out)
	}
	if createdRef != "metaphor-v0.2.0-rc.9" {
		t.Fatalf("pipeline ref = %q, want the newest RC tag", createdRef)
	}
	if !strings.Contains(string(createdVars), "INITIATED_BY") {
		t.Fatalf("tag pipeline missing INITIATED_BY: %s", createdVars)
	}
	// the watcher merges the matching bump MR — and never the other one
	deadline := time.Now().Add(3 * time.Second)
	for !merged[55] && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if !merged[55] {
		t.Fatal("dev bump MR !55 was not merged")
	}
	if merged[56] {
		t.Fatal("unrelated MR !56 must not be touched")
	}
}
