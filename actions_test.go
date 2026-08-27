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

// feature: creates the branch on the micro AND the macro repo; an existing
// branch means joining, not failing.
func TestFeatureAction(t *testing.T) {
	created := map[string]int{}
	gl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.EscapedPath()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(p, "/repository/branches") && r.Method == "POST":
			// charts already has the branch (join case); micro creates fresh
			if strings.Contains(p, "%2Fcharts") {
				created["charts"]++
				w.WriteHeader(400)
				w.Write([]byte(`{"message":"Branch already exists"}`))
				return
			}
			created["micro"]++
			w.Write([]byte(`{"name":"epic-20-pink","web_url":"http://gl/x/-/tree/epic-20-pink"}`))
		case strings.HasSuffix(p, "/merge_requests") && r.Method == "GET":
			w.Write([]byte(`[]`)) // no MR yet — the action drafts one
		case strings.HasSuffix(p, "/merge_requests") && r.Method == "POST":
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			created["mr"]++
			if body["source_branch"] != "epic-20-pink" || body["target_branch"] != "main" || !strings.HasPrefix(body["title"].(string), "Draft:") {
				t.Errorf("draft MR body = %+v", body)
			}
			if !strings.Contains(body["description"].(string), "&20") {
				t.Errorf("draft MR description misses the epic ref: %v", body["description"])
			}
			w.Write([]byte(`{"iid":88,"title":"Draft: epic-20-pink","state":"opened","web_url":"http://gl/x/-/merge_requests/88"}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer gl.Close()
	t.Setenv("GITLAB_ACTION_TOKEN", "act-tok")
	srv := httptest.NewServer(newAPI(newGLClient(gl.URL, "tok"), nil))
	defer srv.Close()

	res, err := http.Post(srv.URL+"/api/actions/run", "application/json",
		strings.NewReader(`{"project":"civo/metaphor/metaphor","action":"feature","branch":"epic-20-pink","epic_iid":20}`))
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		OK            bool   `json:"ok"`
		MicroCreated  bool   `json:"micro_created"`
		ChartsCreated bool   `json:"charts_created"`
		Checkout      string `json:"checkout"`
		MR            *struct {
			IID    int    `json:"iid"`
			WebURL string `json:"web_url"`
		} `json:"mr"`
	}
	json.NewDecoder(res.Body).Decode(&out)
	if res.StatusCode != 200 || !out.OK || !out.MicroCreated || out.ChartsCreated {
		t.Fatalf("feature = %d %+v (want micro created, charts joined)", res.StatusCode, out)
	}
	if created["micro"] != 1 || created["charts"] != 1 {
		t.Fatalf("branch calls = %+v", created)
	}
	if created["mr"] != 1 || out.MR == nil || out.MR.IID != 88 {
		t.Fatalf("draft MR = calls %d, resp %+v", created["mr"], out.MR)
	}
	if out.Checkout != "git fetch origin epic-20-pink && git checkout epic-20-pink" {
		t.Fatalf("checkout = %q", out.Checkout)
	}
	// bad names refused
	res, _ = http.Post(srv.URL+"/api/actions/run", "application/json",
		strings.NewReader(`{"project":"civo/metaphor/metaphor","action":"feature","branch":"Bad Name!!"}`))
	if res.StatusCode != 400 {
		t.Fatalf("bad branch name = %d, want 400", res.StatusCode)
	}
	// macro refused
	res, _ = http.Post(srv.URL+"/api/actions/run", "application/json",
		strings.NewReader(`{"project":"civo/metaphor/charts","action":"feature","branch":"epic-20-pink"}`))
	if res.StatusCode != 400 {
		t.Fatalf("feature on macro = %d, want 400", res.StatusCode)
	}
}

// branch-scoped trigger runs the branch's CI; branch-scoped release plays
// the release job from the branch's newest pipeline.
func TestBranchScopedActions(t *testing.T) {
	var createdRef string
	var playedJob int
	gl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.EscapedPath()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(p, "/pipeline"): // trigger-on-ref
			var v map[string]any
			json.NewDecoder(r.Body).Decode(&v)
			createdRef, _ = v["ref"].(string)
			w.Write([]byte(`{"id":800,"status":"created","ref":"` + createdRef + `","sha":"e","web_url":"http://gl/x/-/pipelines/800","created_at":"2026-08-26T00:00:00Z","updated_at":"2026-08-26T00:00:00Z"}`))
		case strings.HasSuffix(p, "/pipelines") && r.URL.Query().Get("ref") == "hotfix/0.2":
			w.Write([]byte(`[{"id":801,"status":"success","ref":"hotfix/0.2","web_url":"http://gl/x/-/pipelines/801","updated_at":"2026-08-26T00:00:00Z"}]`))
		case strings.HasSuffix(p, "/pipelines/801"):
			w.Write([]byte(`{"id":801,"status":"success","ref":"hotfix/0.2","sha":"h","web_url":"http://gl/x/-/pipelines/801","created_at":"2026-08-26T00:00:00Z","updated_at":"2026-08-26T00:00:00Z"}`))
		case strings.HasSuffix(p, "/pipelines/801/jobs"):
			w.Write([]byte(`[{"id":9,"name":"release","status":"manual"}]`))
		case strings.HasSuffix(p, "/jobs/9/play"):
			playedJob = 9
			w.Write([]byte(`{"id":9,"name":"release","status":"pending","web_url":"http://gl/x/-/jobs/9","pipeline":{"id":801,"web_url":"http://gl/x/-/pipelines/801"}}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer gl.Close()
	t.Setenv("GITLAB_ACTION_TOKEN", "act-tok")
	srv := httptest.NewServer(newAPI(newGLClient(gl.URL, "tok"), nil))
	defer srv.Close()

	res, _ := http.Post(srv.URL+"/api/actions/run", "application/json",
		strings.NewReader(`{"project":"civo/metaphor/metaphor","action":"trigger","ref":"epic-101-aurora"}`))
	if res.StatusCode != 200 || createdRef != "epic-101-aurora" {
		t.Fatalf("trigger-on-ref = %d ref=%q", res.StatusCode, createdRef)
	}
	res, _ = http.Post(srv.URL+"/api/actions/run", "application/json",
		strings.NewReader(`{"project":"civo/metaphor/metaphor","action":"release","ref":"hotfix/0.2","confirm":"metaphor"}`))
	if res.StatusCode != 200 || playedJob != 9 {
		t.Fatalf("release-on-ref = %d played=%d", res.StatusCode, playedJob)
	}
}

// TestDeleteBranch covers the housekeeping guards: merged hotfixes delete
// freely, unmerged ones demand the branch-name confirm (server re-checks
// live), and nothing outside hotfix* is deletable.
func TestDeleteBranch(t *testing.T) {
	deleted := map[string]bool{}
	gl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.EscapedPath()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(p, "/repository/compare"):
			if strings.Contains(r.URL.RawQuery, "hotfix-done") {
				w.Write([]byte(`{"commits":[]}`))
				return
			}
			w.Write([]byte(`{"commits":[{"id":"a","author_name":"Jared","author_email":"j@civo.com"},{"id":"b","author_name":"John","author_email":"jd@civo.com"}]}`))
		case strings.Contains(p, "/repository/branches/") && r.Method == "DELETE":
			deleted[p[strings.Index(p, "branches/")+len("branches/"):]] = true
			w.WriteHeader(204)
		case strings.HasSuffix(p, "/repository/branches"):
			// deliberately STALE: still lists hotfix-done after its delete —
			// GitLab's branch list lags DELETEs by a few seconds.
			w.Write([]byte(`[{"name":"main","web_url":"http://gl/t/m","commit":{"short_id":"aa","title":"x","author_name":"jd","committed_date":"2026-08-26T10:00:00Z"}},{"name":"hotfix-done","web_url":"http://gl/t/hd","commit":{"short_id":"bb","title":"y","author_name":"jd","committed_date":"2026-08-26T09:00:00Z"}}]`))
		case strings.HasSuffix(p, "/repository/tags"):
			w.Write([]byte(`[]`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer gl.Close()
	t.Setenv("GITLAB_ACTION_TOKEN", "act-tok")
	srv := httptest.NewServer(newAPI(newGLClient(gl.URL, "tok"), nil))
	defer srv.Close()

	post := func(body string) (*http.Response, map[string]any) {
		res, err := http.Post(srv.URL+"/api/actions/run", "application/json", bytes.NewBufferString(body))
		if err != nil {
			t.Fatal(err)
		}
		var out map[string]any
		json.NewDecoder(res.Body).Decode(&out)
		return res, out
	}

	// merged branch: deletes with no confirm at all
	res, out := post(`{"action":"delete","project":"civo/metaphor/metaphor","ref":"hotfix-done"}`)
	if res.StatusCode != 200 || out["ok"] != true {
		t.Fatalf("merged delete = %d %v", res.StatusCode, out)
	}
	if !deleted["hotfix-done"] {
		t.Fatal("merged branch was not deleted upstream")
	}

	// upstream still lists it (stale) — the view must not resurrect it
	bres, err := http.Get(srv.URL + "/api/branches")
	if err != nil {
		t.Fatal(err)
	}
	var bout struct {
		Repos []repoBranches `json:"repos"`
	}
	json.NewDecoder(bres.Body).Decode(&bout)
	for _, rb := range bout.Repos {
		if rb.Project != "civo/metaphor/metaphor" {
			continue
		}
		for _, b := range rb.Hotfix {
			if b.Name == "hotfix-done" {
				t.Fatal("deleted branch resurrected by stale upstream list")
			}
		}
	}

	// unmerged branch without confirm: 409 with the live ahead count
	res, out = post(`{"action":"delete","project":"civo/metaphor/metaphor","ref":"hotfix/0.2"}`)
	if res.StatusCode != 409 || out["ahead"] != float64(2) {
		t.Fatalf("unmerged delete without confirm = %d %v (want 409 ahead=2)", res.StatusCode, out)
	}
	if deleted["hotfix%2F0.2"] {
		t.Fatal("unmerged branch deleted without confirm")
	}

	// unmerged branch with the branch-name confirm: goes through
	res, out = post(`{"action":"delete","project":"civo/metaphor/metaphor","ref":"hotfix/0.2","confirm":"hotfix/0.2"}`)
	if res.StatusCode != 200 || out["ahead"] != float64(2) {
		t.Fatalf("confirmed delete = %d %v", res.StatusCode, out)
	}
	if !deleted["hotfix%2F0.2"] {
		t.Fatal("confirmed delete never reached GitLab")
	}

	// main and epic branches are never deletable from here
	for _, ref := range []string{"main", "epic-101-aurora"} {
		res, _ = post(`{"action":"delete","project":"civo/metaphor/metaphor","ref":"` + ref + `"}`)
		if res.StatusCode != 400 {
			t.Fatalf("delete %s = %d (want 400)", ref, res.StatusCode)
		}
	}

	// off-topology project refused before anything else
	res, _ = post(`{"action":"delete","project":"civo/other/thing","ref":"hotfix-x"}`)
	if res.StatusCode != 400 {
		t.Fatalf("off-topology delete = %d (want 400)", res.StatusCode)
	}
}

// TestMergeMRAction: only open epic-* MRs merge, with the repo-name confirm.
func TestMergeMRAction(t *testing.T) {
	merged := map[string]bool{}
	gl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.EscapedPath()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(p, "/merge_requests/9") && r.Method == "GET":
			w.Write([]byte(`{"iid":9,"state":"opened","source_branch":"epic-70-pink","web_url":"http://gl/x/-/merge_requests/9"}`))
		case strings.HasSuffix(p, "/merge_requests/10") && r.Method == "GET":
			w.Write([]byte(`{"iid":10,"state":"opened","source_branch":"fix/oops","web_url":"http://gl/x/-/merge_requests/10"}`))
		case strings.HasSuffix(p, "/merge_requests/11") && r.Method == "GET":
			w.Write([]byte(`{"iid":11,"state":"merged","source_branch":"epic-70-pink","web_url":"http://gl/x/-/merge_requests/11"}`))
		case strings.HasSuffix(p, "/approve"):
			w.Write([]byte(`{}`))
		case strings.HasSuffix(p, "/merge"):
			merged[p] = true
			w.Write([]byte(`{"iid":9,"state":"merged","web_url":"http://gl/x/-/merge_requests/9"}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer gl.Close()
	t.Setenv("GITLAB_ACTION_TOKEN", "act-tok")
	srv := httptest.NewServer(newAPI(newGLClient(gl.URL, "tok"), nil))
	defer srv.Close()

	post := func(body string) (*http.Response, map[string]any) {
		res, err := http.Post(srv.URL+"/api/actions/run", "application/json", bytes.NewBufferString(body))
		if err != nil {
			t.Fatal(err)
		}
		var out map[string]any
		json.NewDecoder(res.Body).Decode(&out)
		return res, out
	}

	// no confirm → refused before any lookup
	res, _ := post(`{"action":"merge-mr","project":"civo/metaphor/metaphor","mr_iid":9}`)
	if res.StatusCode != 400 {
		t.Fatalf("no confirm = %d", res.StatusCode)
	}
	// non-epic source branch → refused
	res, _ = post(`{"action":"merge-mr","project":"civo/metaphor/metaphor","mr_iid":10,"confirm":"metaphor"}`)
	if res.StatusCode != 400 {
		t.Fatalf("non-epic = %d", res.StatusCode)
	}
	// already merged → honest 409
	res, _ = post(`{"action":"merge-mr","project":"civo/metaphor/metaphor","mr_iid":11,"confirm":"metaphor"}`)
	if res.StatusCode != 409 {
		t.Fatalf("already merged = %d", res.StatusCode)
	}
	// the good path
	res, out := post(`{"action":"merge-mr","project":"civo/metaphor/metaphor","mr_iid":9,"confirm":"metaphor"}`)
	if res.StatusCode != 200 || out["state"] != "merged" {
		t.Fatalf("merge = %d %+v", res.StatusCode, out)
	}
	if len(merged) != 1 {
		t.Fatalf("merge calls = %+v", merged)
	}
}
