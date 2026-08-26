package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
