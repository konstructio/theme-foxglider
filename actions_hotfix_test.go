package main

// Phase C of v2.15.0: handler-level coverage for the hotfix lane — the
// start-only regression, hotfix-join, hotfix-preview, and the full
// hotfix-from-version cut (branch every pinned micro at its exact ref).

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// TestFeaturesRouteGone: v2.15 made the feature modal start-only — the old
// join-list endpoint must be gone, not lingering half-supported.
func TestFeaturesRouteGone(t *testing.T) {
	t.Setenv("GITLAB_ACTION_TOKEN", "act-tok")
	srv := httptest.NewServer(newAPI(newGLClient("http://unused", "tok"), nil))
	defer srv.Close()
	res, err := http.Get(srv.URL + "/api/actions/features")
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != 404 {
		t.Fatalf("/api/actions/features = %d, want 404 (route removed)", res.StatusCode)
	}
}

// TestHotfixJoin: the per-tile join creates the branch from main, joins
// idempotently, and refuses non-hotfix names and foreign projects.
func TestHotfixJoin(t *testing.T) {
	var mu sync.Mutex
	made := map[string]string{} // project short → ref branched from
	gl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.EscapedPath()
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(p, "/repository/branches") && r.Method == "POST" {
			proj := p[:strings.LastIndex(p, "/repository/branches")]
			proj = proj[strings.LastIndex(proj, "%2F")+3:]
			mu.Lock()
			_, dup := made[proj]
			if !dup {
				made[proj] = r.URL.Query().Get("ref")
			}
			mu.Unlock()
			if dup {
				w.WriteHeader(400)
				w.Write([]byte(`{"message":"Branch already exists"}`))
				return
			}
			w.Write([]byte(`{"name":"` + r.URL.Query().Get("branch") + `","web_url":"http://gl/x/-/tree/hf"}`))
			return
		}
		w.WriteHeader(404)
	}))
	defer gl.Close()
	t.Setenv("GITLAB_ACTION_TOKEN", "act-tok")
	srv := httptest.NewServer(newAPI(newGLClient(gl.URL, "tok"), nil))
	defer srv.Close()

	post := func(body string) (*http.Response, map[string]any) {
		res, err := http.Post(srv.URL+"/api/actions/run", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		var out map[string]any
		json.NewDecoder(res.Body).Decode(&out)
		return res, out
	}

	// create from main
	res, out := post(`{"action":"hotfix-join","project":"civo/metaphor/metaphor","branch":"hotfix-cache-stampede"}`)
	if res.StatusCode != 200 || out["ok"] != true || out["joined"] != false {
		t.Fatalf("hotfix-join create = %d %+v", res.StatusCode, out)
	}
	mu.Lock()
	ref := made["metaphor"]
	mu.Unlock()
	if ref != "main" {
		t.Fatalf("hotfix-join branched from %q, want main", ref)
	}
	if !strings.Contains(out["checkout"].(string), "hotfix-cache-stampede") {
		t.Fatalf("checkout = %v", out["checkout"])
	}
	// idempotent join
	if res, out = post(`{"action":"hotfix-join","project":"civo/metaphor/metaphor","branch":"hotfix-cache-stampede"}`); res.StatusCode != 200 || out["joined"] != true {
		t.Fatalf("hotfix-join rerun = %d %+v (want joined)", res.StatusCode, out)
	}
	// charts repo is a legal join target (the twin lane)
	if res, _ = post(`{"action":"hotfix-join","project":"civo/metaphor/charts","branch":"hotfix-cache-stampede"}`); res.StatusCode != 200 {
		t.Fatalf("hotfix-join on charts = %d", res.StatusCode)
	}
	// non-hotfix names refused
	if res, _ = post(`{"action":"hotfix-join","project":"civo/metaphor/metaphor","branch":"epic-9-nope"}`); res.StatusCode != 400 {
		t.Fatalf("epic name via hotfix-join = %d, want 400", res.StatusCode)
	}
	// foreign project refused
	if res, _ = post(`{"action":"hotfix-join","project":"civo/other/thing","branch":"hotfix-x"}`); res.StatusCode != 400 {
		t.Fatalf("foreign project = %d, want 400", res.StatusCode)
	}
}

// konstruct-shaped fake for the hotfix-from-version pair: four pinned
// services — alpha resolves to its pinned commit, bravo's commit is gone but a
// tag carries the pin, charlie's counter pin resolves via its publish
// pipeline's stamped name, echo has none of the three and must skip.
func fakeHotfixGitLab(t *testing.T, made map[string]string, mu *sync.Mutex, alphaCommitStatus int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.EscapedPath()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(p, "charts%2Fkonstruct%2FChart.yaml") && strings.Contains(r.URL.RawQuery, "konstruct-v9.9.0-rc.aabbccdd"):
			w.Header().Set("Content-Type", "text/plain")
			w.Write([]byte("version: 9.9.0-rc.aabbccdd\ndependencies:\n  - name: alpha\n    version: \"1.1.0-rc.aabbccdd\"\n  - name: bravo\n    version: \"2.2.2-rc.beefbee1\"\n  - name: charlie\n    version: \"3.3.0-rc.7\"\n  - name: echo\n    version: \"4.0.0-rc.9\"\n"))
		case strings.Contains(p, "alpha%2Frepository%2Fcommits%2Faabbccdd") || (strings.Contains(p, "alpha") && strings.Contains(p, "/repository/commits/aabbccdd")):
			if alphaCommitStatus != 200 {
				w.WriteHeader(alphaCommitStatus)
				return
			}
			w.Write([]byte(`{"id":"aabbccdd1234","short_id":"aabbccdd"}`))
		case strings.Contains(p, "bravo") && strings.Contains(p, "/repository/commits/beefbee1"):
			w.WriteHeader(404)
		case strings.Contains(p, "bravo") && strings.HasSuffix(p, "/repository/tags"):
			w.Write([]byte(`[{"name":"v2.2.2-rc.beefbee1"}]`))
		case strings.Contains(p, "charlie") && strings.HasSuffix(p, "/repository/tags"):
			w.Write([]byte(`[{"name":"v1.0.0"}]`))
		case strings.Contains(p, "charlie") && strings.HasSuffix(p, "/pipelines"):
			// the publish pipeline's stamped name carries the rc.N version;
			// a non-matching neighbor proves the prefix match is exact
			w.Write([]byte(`[{"id":41,"name":"[3.3.0-rc.70 | main] decoy","sha":"ffff0000ffff0000","status":"success"},` +
				`{"id":42,"name":"[3.3.0-rc.7 | main] merge thing","sha":"cafe1234feedface","status":"success"}]`))
		case strings.Contains(p, "echo") && strings.HasSuffix(p, "/pipelines"):
			w.Write([]byte(`[]`))
		case strings.HasSuffix(p, "/repository/branches") && r.Method == "POST":
			proj := p[:strings.LastIndex(p, "/repository/branches")]
			proj = proj[strings.LastIndex(proj, "%2F")+3:]
			mu.Lock()
			_, dup := made[proj]
			if !dup {
				made[proj] = r.URL.Query().Get("ref")
			}
			mu.Unlock()
			if dup {
				w.WriteHeader(400)
				w.Write([]byte(`{"message":"Branch already exists"}`))
				return
			}
			w.Write([]byte(`{"name":"` + r.URL.Query().Get("branch") + `","web_url":"http://gl/` + proj + `/-/tree/hf"}`))
		default:
			w.WriteHeader(404)
		}
	}))
}

// delta is in the topology but NOT pinned by the umbrella — it must surface as
// an explicit skip, never silently vanish from the plan.
const hotfixTopo = `{"branch_policy":"hotfix-only","services":[` +
	`{"name":"alpha","project":"civo/konstruct/alpha","chart":"charts/alpha/Chart.yaml"},` +
	`{"name":"bravo","project":"civo/konstruct/bravo","chart":"charts/bravo/Chart.yaml"},` +
	`{"name":"charlie","project":"civo/konstruct/charlie","chart":"charts/charlie/Chart.yaml"},` +
	`{"name":"delta","project":"civo/konstruct/delta","chart":"charts/delta/Chart.yaml"},` +
	`{"name":"echo","project":"civo/konstruct/echo","chart":"charts/echo/Chart.yaml"}],` +
	`"macro":{"name":"konstruct","project":"civo/konstruct/charts","file":"charts/konstruct/Chart.yaml","tagPrefix":"konstruct-v"}}`

// TestHotfixPreview: the plan is honest per repo — commit, tag, or an explicit
// skip — and a wire error refuses to guess.
func TestHotfixPreview(t *testing.T) {
	var mu sync.Mutex
	gl := fakeHotfixGitLab(t, map[string]string{}, &mu, 200)
	defer gl.Close()
	t.Setenv("GITLAB_ACTION_TOKEN", "act-tok")
	t.Setenv("TOPOLOGY", hotfixTopo)
	srv := httptest.NewServer(newAPI(newGLClient(gl.URL, "tok"), nil))
	defer srv.Close()

	res, err := http.Get(srv.URL + "/api/actions/hotfix-preview?tag=konstruct-v9.9.0-rc.aabbccdd")
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Tag     string `json:"tag"`
		Version string `json:"version"`
		Repos   []struct {
			Name, Project, Pin, Ref, Kind, Reason string
		} `json:"repos"`
	}
	json.NewDecoder(res.Body).Decode(&out)
	if res.StatusCode != 200 || out.Version != "9.9.0-rc.aabbccdd" || len(out.Repos) != 6 {
		t.Fatalf("preview = %d %+v", res.StatusCode, out)
	}
	byName := map[string]struct{ Name, Project, Pin, Ref, Kind, Reason string }{}
	for _, r := range out.Repos {
		byName[r.Name] = r
	}
	if a := byName["alpha"]; a.Kind != "commit" || a.Ref != "aabbccdd" {
		t.Fatalf("alpha = %+v (want the pinned commit)", a)
	}
	if b := byName["bravo"]; b.Kind != "tag" || b.Ref != "v2.2.2-rc.beefbee1" {
		t.Fatalf("bravo = %+v (want the pin-carrying tag)", b)
	}
	if c := byName["charlie"]; c.Kind != "commit" || c.Ref != "cafe1234" {
		t.Fatalf("charlie = %+v (want the publish pipeline's commit for the rc.N pin)", c)
	}
	if e := byName["echo"]; e.Kind != "skip" || !strings.Contains(e.Reason, "4.0.0-rc.9") {
		t.Fatalf("echo = %+v (want an explicit skip naming the pin)", e)
	}
	if d := byName["delta"]; d.Kind != "skip" || !strings.Contains(d.Reason, "not declared") {
		t.Fatalf("delta = %+v (unpinned topology service must skip visibly, not vanish)", d)
	}
	if m := byName["charts"]; m.Kind != "tag" || m.Ref != "konstruct-v9.9.0-rc.aabbccdd" {
		t.Fatalf("charts = %+v (want the macro tag itself)", m)
	}
	// wrong prefix refused
	if res, _ := http.Get(srv.URL + "/api/actions/hotfix-preview?tag=metaphor-v1.0.0"); res.StatusCode != 400 {
		t.Fatalf("foreign tag prefix = %d, want 400", res.StatusCode)
	}

	// a GitLab outage on the commit check must fail the preview loudly, never
	// mis-report "skipped".
	gl2 := fakeHotfixGitLab(t, map[string]string{}, &mu, 500)
	defer gl2.Close()
	srv2 := httptest.NewServer(newAPI(newGLClient(gl2.URL, "tok"), nil))
	defer srv2.Close()
	if res, _ := http.Get(srv2.URL + "/api/actions/hotfix-preview?tag=konstruct-v9.9.0-rc.aabbccdd"); res.StatusCode != 502 {
		t.Fatalf("preview during outage = %d, want 502", res.StatusCode)
	}
}

// TestHotfixFromVersion: the cut branches every resolvable repo at its exact
// pinned ref, skips the unresolvable, honors the confirm handshake, and joins
// idempotently on rerun — all under the hotfix-only policy.
func TestHotfixFromVersion(t *testing.T) {
	var mu sync.Mutex
	made := map[string]string{}
	gl := fakeHotfixGitLab(t, made, &mu, 200)
	defer gl.Close()
	t.Setenv("GITLAB_ACTION_TOKEN", "act-tok")
	t.Setenv("TOPOLOGY", hotfixTopo)
	srv := httptest.NewServer(newAPI(newGLClient(gl.URL, "tok"), nil))
	defer srv.Close()

	post := func(body string) (*http.Response, map[string]any) {
		res, err := http.Post(srv.URL+"/api/actions/run", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		var out map[string]any
		json.NewDecoder(res.Body).Decode(&out)
		return res, out
	}

	// confirm handshake first: mismatch creates nothing
	if res, _ := post(`{"action":"hotfix","tag":"konstruct-v9.9.0-rc.aabbccdd","branch":"hotfix-cve","confirm":"nope"}`); res.StatusCode != 400 {
		t.Fatalf("confirm mismatch = %d, want 400", res.StatusCode)
	}
	mu.Lock()
	n := len(made)
	mu.Unlock()
	if n != 0 {
		t.Fatalf("confirm mismatch still created branches: %+v", made)
	}
	// name shape enforced
	if res, _ := post(`{"action":"hotfix","tag":"konstruct-v9.9.0-rc.aabbccdd","branch":"fix-cve","confirm":"charts"}`); res.StatusCode != 400 {
		t.Fatalf("non-hotfix name = %d, want 400", res.StatusCode)
	}

	// the real cut
	res, out := post(`{"action":"hotfix","tag":"konstruct-v9.9.0-rc.aabbccdd","branch":"hotfix-cve","confirm":"charts"}`)
	if res.StatusCode != 200 || out["ok"] != true {
		t.Fatalf("hotfix = %d %+v", res.StatusCode, out)
	}
	mu.Lock()
	if made["alpha"] != "aabbccdd" || made["bravo"] != "v2.2.2-rc.beefbee1" || made["charts"] != "konstruct-v9.9.0-rc.aabbccdd" {
		mu.Unlock()
		t.Fatalf("branch refs = %+v (want pinned commit / pin tag / macro tag)", made)
	}
	if made["charlie"] != "cafe1234" {
		mu.Unlock()
		t.Fatalf("charlie ref = %q, want the publish pipeline's commit for its rc.N pin", made["charlie"])
	}
	if _, ok := made["echo"]; ok {
		mu.Unlock()
		t.Fatal("echo was branched despite an unresolvable pin")
	}
	if _, ok := made["delta"]; ok {
		mu.Unlock()
		t.Fatal("delta was branched despite not being pinned by the umbrella")
	}
	mu.Unlock()
	raw, _ := json.Marshal(out["results"])
	var results []struct{ Project, Status, Ref, Reason string }
	json.Unmarshal(raw, &results)
	byProj := map[string]struct{ Project, Status, Ref, Reason string }{}
	for _, r := range results {
		byProj[r.Project] = r
	}
	if byProj["civo/konstruct/alpha"].Status != "created" || byProj["civo/konstruct/charlie"].Status != "created" || byProj["civo/konstruct/echo"].Status != "skipped" {
		t.Fatalf("results = %+v", results)
	}
	if !strings.Contains(out["checkout"].(string), "hotfix-cve") {
		t.Fatalf("checkout = %v", out["checkout"])
	}
	// rerun: everything resolvable reports joined, nothing errors
	if _, out = post(`{"action":"hotfix","tag":"konstruct-v9.9.0-rc.aabbccdd","branch":"hotfix-cve","confirm":"charts"}`); out["ok"] != true {
		t.Fatalf("hotfix rerun = %+v", out)
	}
	raw, _ = json.Marshal(out["results"])
	json.Unmarshal(raw, &results)
	for _, r := range results {
		if r.Status == "skipped" {
			continue // echo + delta stay skipped on rerun too
		}
		if r.Status != "joined" {
			t.Fatalf("rerun %s = %q, want joined", r.Project, r.Status)
		}
	}
}

// TestRetireFeature: retiring deletes the epic branch on every repo that has
// it + the charts twin, closes the epic, refuses while carrying MRs are open,
// and reruns idempotently (absent everywhere).
func TestRetireFeature(t *testing.T) {
	var mu sync.Mutex
	mrsOpen := true
	building := true // start with a pipeline in flight → retire must refuse
	deleted := map[string]bool{}
	epicClosed := false
	gl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.EscapedPath()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(p, "/pipelines"):
			mu.Lock()
			b := building
			mu.Unlock()
			if b {
				w.Write([]byte(`[{"id":9,"status":"running","ref":"epic-9-x","web_url":"http://gl/9"}]`))
			} else {
				w.Write([]byte(`[]`))
			}
		case strings.Contains(p, "/merge_requests") && r.Method == "GET":
			mu.Lock()
			openNow := mrsOpen
			mu.Unlock()
			if openNow && strings.Contains(p, "dashboard-manager") {
				w.Write([]byte(`[{"iid":31,"state":"opened","source_branch":"epic-9-x","web_url":"http://gl/x/-/merge_requests/31"}]`))
				return
			}
			w.Write([]byte(`[{"iid":30,"state":"merged","source_branch":"epic-9-x","web_url":"http://gl/x/-/merge_requests/30"}]`))
		case strings.Contains(p, "/repository/branches/") && r.Method == "DELETE":
			proj := p[:strings.Index(p, "/repository/branches/")]
			proj = proj[strings.LastIndex(proj, "%2F")+3:]
			mu.Lock()
			dup := deleted[proj]
			deleted[proj] = true
			mu.Unlock()
			// only metaphor + charts carry the branch; others (and reruns) 404
			if dup || (proj != "metaphor" && proj != "charts") {
				w.WriteHeader(404)
				return
			}
			w.WriteHeader(204)
		case strings.Contains(p, "/epics/9") && r.Method == "PUT":
			mu.Lock()
			epicClosed = true
			mu.Unlock()
			w.Write([]byte(`{"iid":9,"state":"closed"}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer gl.Close()
	t.Setenv("GITLAB_ACTION_TOKEN", "act-tok")
	srv := httptest.NewServer(newAPI(newGLClient(gl.URL, "tok"), nil))
	defer srv.Close()

	post := func(body string) (*http.Response, map[string]any) {
		res, err := http.Post(srv.URL+"/api/actions/run", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		var out map[string]any
		json.NewDecoder(res.Body).Decode(&out)
		return res, out
	}

	// non-epic branch refused
	if res, _ := post(`{"action":"retire","project":"civo/metaphor/charts","branch":"hotfix-nope","confirm":"hotfix-nope"}`); res.StatusCode != 400 {
		t.Fatalf("non-epic retire = %d, want 400", res.StatusCode)
	}
	// in-flight pipeline BLOCKS retire — and deletes nothing
	res, out := post(`{"action":"retire","project":"civo/metaphor/charts","branch":"epic-9-x","confirm":"epic-9-x"}`)
	if res.StatusCode != 409 {
		t.Fatalf("retire with pipeline in flight = %d %+v, want 409", res.StatusCode, out)
	}
	if msg, _ := out["error"].(string); !strings.Contains(msg, "still running") {
		t.Fatalf("409 must explain the in-flight pipeline: %+v", out)
	}
	mu.Lock()
	if len(deleted) != 0 {
		mu.Unlock()
		t.Fatalf("in-flight retire still deleted branches: %+v", deleted)
	}
	building = false // pipelines finished — retire may proceed
	mu.Unlock()
	// confirm handshake
	if res, _ := post(`{"action":"retire","project":"civo/metaphor/charts","branch":"epic-9-x","confirm":"nope"}`); res.StatusCode != 400 {
		t.Fatalf("confirm mismatch = %d, want 400", res.StatusCode)
	}
	// open carrying MR blocks the whole retire — and deletes nothing
	res, out = post(`{"action":"retire","project":"civo/metaphor/charts","branch":"epic-9-x","confirm":"epic-9-x"}`)
	if res.StatusCode != 409 {
		t.Fatalf("open-MR retire = %d %+v, want 409", res.StatusCode, out)
	}
	if msg, _ := out["error"].(string); !strings.Contains(msg, "!31") {
		t.Fatalf("409 must name the open MR: %+v", out)
	}
	mu.Lock()
	if len(deleted) != 0 {
		mu.Unlock()
		t.Fatalf("409 path still deleted branches: %+v", deleted)
	}
	mrsOpen = false
	mu.Unlock()

	// the real retire: deletes where present, absent elsewhere, closes the epic
	res, out = post(`{"action":"retire","project":"civo/metaphor/charts","branch":"epic-9-x","confirm":"epic-9-x"}`)
	if res.StatusCode != 200 || out["ok"] != true {
		t.Fatalf("retire = %d %+v", res.StatusCode, out)
	}
	if n, _ := out["deleted"].(float64); n != 2 {
		t.Fatalf("deleted = %v, want 2 (metaphor + charts)", out["deleted"])
	}
	if out["epic_closed"] != true {
		t.Fatalf("epic not closed: %+v", out)
	}
	mu.Lock()
	ec := epicClosed
	mu.Unlock()
	if !ec {
		t.Fatal("epic close PUT never reached GitLab")
	}
	// rerun: everything already gone — absent across the board, still ok
	if _, out = post(`{"action":"retire","project":"civo/metaphor/charts","branch":"epic-9-x","confirm":"epic-9-x"}`); out["ok"] != true {
		t.Fatalf("retire rerun = %+v", out)
	}
	if n, _ := out["deleted"].(float64); n != 0 {
		t.Fatalf("rerun deleted = %v, want 0", out["deleted"])
	}
}
