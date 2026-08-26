package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestChartVersionAndTargetRevision(t *testing.T) {
	chart := `apiVersion: v2
name: metaphor-macro
# version: 9.9.9  (a commented decoy must be ignored)
version: 0.2.0
appVersion: "0.1.0"`
	if got := chartVersion(chart); got != "0.2.0" {
		t.Fatalf("chartVersion = %q, want 0.2.0", got)
	}
	app := `spec:
  # bumps ONLY spec.source.targetRevision in this file
  source:
    targetRevision: 0.2.0-rc.2 # bumped by CI`
	if got := targetRevision(app); got != "0.2.0-rc.2" {
		t.Fatalf("targetRevision = %q, want 0.2.0-rc.2", got)
	}
}

func TestMacroDeps(t *testing.T) {
	raw := `version: 0.2.0
dependencies:
  - name: metaphor
    version: "0.11.0-rc.13"
    repository: "oci://x"
  - name: metaphor-dashboard-manager
    version: "0.12.0-rc.15"
  - name: metaphor-micro-frontend
    version: "0.1.0-rc.7"`
	d := macroDeps(raw)
	for name, want := range map[string]string{
		"metaphor":                   "0.11.0-rc.13",
		"metaphor-dashboard-manager": "0.12.0-rc.15",
		"metaphor-micro-frontend":    "0.1.0-rc.7",
	} {
		if d[name] != want {
			t.Fatalf("dep %s = %q, want %q", name, d[name], want)
		}
	}
	if len(d) != 3 {
		t.Fatalf("deps = %+v, want 3 (top-level version must not leak in)", d)
	}
}

func TestVersionCompareAndNewestTag(t *testing.T) {
	// release sorts above its rc's; higher rc above lower.
	cases := []struct {
		a, b string
		want int
	}{
		{"0.2.0-rc.2", "0.2.0-rc.4", -1},
		{"0.2.0-rc.4", "0.2.0-rc.2", 1},
		{"0.2.0", "0.2.0-rc.9", 1},
		{"0.2.0-rc.9", "0.2.0", -1},
		{"0.3.0-rc.1", "0.2.0-rc.99", 1},
		{"0.2.0-rc.2", "0.2.0-rc.2", 0},
	}
	for _, c := range cases {
		if got := cmpVer(parseVer(c.a), parseVer(c.b)); got != c.want {
			t.Fatalf("cmpVer(%s,%s) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
	// tags arrive ordered by update time (newest first) — positional pick
	// works for numeric counters AND sha-suffixed rc styles (konstruct)
	tags := []glTag{{"unrelated-1.0.0"}, {"metaphor-v0.2.0-rc.4"}, {"metaphor-v0.2.0-rc.3"}, {"metaphor-v0.2.0-rc.2"}}
	if got := newestTag(tags, "metaphor-v"); got != "metaphor-v0.2.0-rc.4" {
		t.Fatalf("newestTag = %q, want metaphor-v0.2.0-rc.4", got)
	}
	sha := []glTag{{"konstruct-v0.7.8-rc.57f3903b"}, {"konstruct-v0.6.5-rc.44372335"}}
	if got := newestTag(sha, "konstruct-v"); got != "konstruct-v0.7.8-rc.57f3903b" {
		t.Fatalf("sha-rc newestTag = %q — all-digit shas must not outrank by fake semver", got)
	}
}

func TestDrift(t *testing.T) {
	if s, n := drift("0.2.0-rc.2", "0.2.0-rc.4"); s != "behind" || n != 2 {
		t.Fatalf("drift behind = %s,%d want behind,2", s, n)
	}
	if s, n := drift("0.2.0-rc.4", "0.2.0-rc.4"); s != "current" || n != 0 {
		t.Fatalf("drift current = %s,%d", s, n)
	}
	if s, _ := drift("0.3.0-rc.1", "0.2.0-rc.4"); s != "ahead" {
		t.Fatalf("drift ahead = %s", s)
	}
	if s, _ := drift("", "0.2.0-rc.4"); s != "unknown" {
		t.Fatalf("drift unknown = %s", s)
	}
}

// fakeEcoGitLab serves the metaphor topology, routing on the URL-encoded
// project/file paths the client sends (%2F is preserved in EscapedPath).
func fakeEcoGitLab(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("PRIVATE-TOKEN") != "tok" {
			w.WriteHeader(401)
			return
		}
		p := r.URL.EscapedPath()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(p, "metaphor-macro%2FChart.yaml"):
			w.Write([]byte("version: 0.2.0\ndependencies:\n  - name: metaphor\n    version: \"0.11.0-rc.13\"\n  - name: metaphor-dashboard-manager\n    version: \"0.12.0-rc.15\"\n  - name: metaphor-micro-frontend\n    version: \"0.1.0-rc.7\"\n"))
		case strings.Contains(p, "charts%2Fmetaphor%2FChart.yaml"):
			w.Write([]byte("version: 0.11.0\n"))
		case strings.Contains(p, "metaphor-dashboard-manager%2FChart.yaml"):
			w.Write([]byte("version: 0.12.0\n"))
		case strings.Contains(p, "metaphor-micro-frontend%2FChart.yaml"):
			w.Write([]byte("version: 0.1.0\n"))
		case strings.Contains(p, "metaphor-macro.yaml"): // delivery Application
			w.Write([]byte("spec:\n  source:\n    targetRevision: 0.2.0-rc.2\n"))
		case strings.HasSuffix(p, "/repository/tags"):
			w.Write([]byte(`[{"name":"metaphor-v0.2.0-rc.4"},{"name":"metaphor-v0.2.0-rc.3"}]`))
		case strings.HasSuffix(p, "/pipelines/latest") && strings.Contains(p, "dashboard-manager"):
			w.Write([]byte(`{"id":150,"status":"skipped","ref":"main","sha":"deadbeefcafe","web_url":"http://gl/x/-/pipelines/150","created_at":"2026-08-25T00:03:00Z","updated_at":"2026-08-25T00:03:00Z"}`))
		case strings.HasSuffix(p, "/pipelines/98"):
			w.Write([]byte(`{"id":98,"status":"success","ref":"main","sha":"deadbeefcafe","web_url":"http://gl/x/-/pipelines/98","created_at":"2026-08-25T00:00:00Z","updated_at":"2026-08-25T00:01:30Z","user":{"name":"Jared Edwards","username":"jared","avatar_url":"http://gl/avatar/jared.png"}}`))
		case strings.HasSuffix(p, "/pipelines") && r.URL.Query().Get("sha") == "" && strings.Contains(p, "dashboard-manager"):
			w.Write([]byte(`[{"id":150,"status":"skipped","ref":"main","web_url":"http://gl/x/-/pipelines/150","updated_at":"2026-08-25T00:03:00Z"},{"id":98,"status":"success","ref":"main","web_url":"http://gl/x/-/pipelines/98","updated_at":"2026-08-25T00:01:30Z"}]`))
		case strings.HasSuffix(p, "/pipelines/latest"):
			w.Write([]byte(`{"id":99,"status":"success","ref":"main","sha":"deadbeefcafe","web_url":"http://gl/x/-/pipelines/99","created_at":"2026-08-25T00:00:00Z","updated_at":"2026-08-25T00:02:00Z","user":{"name":"John Dietz","username":"jd","avatar_url":"http://gl/avatar/jd.png"}}`))
		case strings.Contains(p, "/repository/commits/deadbeefcafe"):
			w.Write([]byte(`{"id":"deadbeefcafe0123","short_id":"deadbeef","title":"fix: keep the healthz contract honest","web_url":"http://gl/x/-/commit/deadbeefcafe","author_name":"John Dietz","authored_date":"2026-08-25T00:00:00Z"}`))
		case strings.HasSuffix(p, "/pipelines") && r.URL.Query().Get("sha") != "":
			// the SHA's full pipeline story: the branch run + its RC tag run
			w.Write([]byte(`[{"id":99,"status":"success","ref":"main","source":"push","web_url":"http://gl/x/-/pipelines/99","updated_at":"2026-08-25T00:02:00Z"},{"id":100,"status":"failed","ref":"metaphor-v0.2.0-rc.4","source":"push","web_url":"http://gl/x/-/pipelines/100","updated_at":"2026-08-25T00:05:00Z"}]`))
		default:
			w.WriteHeader(404)
		}
	}))
}

func TestEcosystem(t *testing.T) {
	gl := fakeEcoGitLab(t)
	defer gl.Close()
	srv := httptest.NewServer(newAPI(newGLClient(gl.URL, "tok"), nil))
	defer srv.Close()

	res, err := http.Get(srv.URL + "/api/ecosystem")
	if err != nil {
		t.Fatal(err)
	}
	var eco struct {
		Macro    macroNode      `json:"macro"`
		Services []svcNode      `json:"services"`
		Delivery []deliveryNode `json:"delivery"`
	}
	if err := json.NewDecoder(res.Body).Decode(&eco); err != nil {
		t.Fatal(err)
	}

	if eco.Macro.BaseVer != "0.2.0" || eco.Macro.PublishedRC != "0.2.0-rc.4" || eco.Macro.PublishedTag != "metaphor-v0.2.0-rc.4" {
		t.Fatalf("macro = %+v", eco.Macro)
	}
	if eco.Macro.Pipeline == nil || eco.Macro.Pipeline.Status != "success" {
		t.Fatalf("macro pipeline = %+v", eco.Macro.Pipeline)
	}
	// the bundle tree: declared order, exact pins, read at the published tag
	wantBundle := []depJSON{
		{"metaphor", "0.11.0-rc.13"},
		{"metaphor-dashboard-manager", "0.12.0-rc.15"},
		{"metaphor-micro-frontend", "0.1.0-rc.7"},
	}
	if len(eco.Macro.Bundle) != 3 {
		t.Fatalf("bundle = %+v", eco.Macro.Bundle)
	}
	for i, w := range wantBundle {
		if eco.Macro.Bundle[i] != w {
			t.Fatalf("bundle[%d] = %+v (want %+v)", i, eco.Macro.Bundle[i], w)
		}
	}
	if eco.Macro.BundleRef != "metaphor-v0.2.0-rc.4" {
		t.Fatalf("bundle_ref = %q", eco.Macro.BundleRef)
	}
	if eco.Macro.Pipeline.AuthorName != "John Dietz" || eco.Macro.Pipeline.AuthorAvatar == "" {
		t.Fatalf("macro pipeline author = %+v", eco.Macro.Pipeline)
	}
	if len(eco.Services) != 3 {
		t.Fatalf("services = %+v", eco.Services)
	}
	byName := map[string]svcNode{}
	for _, s := range eco.Services {
		byName[s.Name] = s
	}
	if s := byName["metaphor"]; s.BaseVer != "0.11.0" || s.Bundled != "0.11.0-rc.13" {
		t.Fatalf("metaphor svc = %+v", s)
	}
	if s := byName["metaphor"]; s.Commit == nil || s.Commit.ShortSHA != "deadbeef" ||
		s.Commit.Title != "fix: keep the healthz contract honest" {
		t.Fatalf("metaphor commit = %+v", s.Commit)
	}
	if s := byName["metaphor"]; len(s.SHAPipes) != 2 || s.SHAPipes[1].Ref != "metaphor-v0.2.0-rc.4" {
		t.Fatalf("metaphor sha pipelines = %+v", s.SHAPipes)
	}
	if s := byName["metaphor-dashboard-manager"]; s.Bundled != "0.12.0-rc.15" {
		t.Fatalf("dashboard svc = %+v", s)
	}
	if s := byName["metaphor-dashboard-manager"]; s.Pipeline == nil || s.Pipeline.Status != "success" || s.Pipeline.ID != 98 {
		t.Fatalf("skipped latest must fall back to the newest real run, got %+v", s.Pipeline)
	}
	for _, sp := range byName["metaphor"].SHAPipes {
		if sp.Status == "skipped" {
			t.Fatalf("skipped runs must not appear in sha chips: %+v", sp)
		}
	}
	if len(eco.Delivery) != 1 {
		t.Fatalf("delivery = %+v", eco.Delivery)
	}
	if d := eco.Delivery[0]; d.Delivered != "0.2.0-rc.2" || d.State != "behind" || d.Behind != 2 {
		t.Fatalf("delivery = %+v", d)
	}
}

func TestMetaEndpointUnguarded(t *testing.T) {
	// meta must answer even without a token, so the version badge always renders.
	srv := httptest.NewServer(newAPI(newGLClient("http://unused", ""), nil))
	defer srv.Close()
	res, _ := http.Get(srv.URL + "/api/meta")
	if res.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	var m struct {
		Theme, Version string
	}
	json.NewDecoder(res.Body).Decode(&m)
	if m.Theme != "foxglider" || m.Version != themeVersion {
		t.Fatalf("meta = %+v", m)
	}
}

func TestRollupStagesProgressBaseline(t *testing.T) {
	start := time.Now().Add(-40 * time.Second)
	jobs := []glJob{
		{Name: "lint", Stage: "validate", Status: "success", Duration: 9},
		{Name: "build", Stage: "build", Status: "running", StartedAt: &start},
		{Name: "publish", Stage: "publish", Status: "created"},
	}
	hist := map[string]float64{"build": 95, "lint": 8}
	st := rollupStages(jobs, hist)
	if len(st) != 3 {
		t.Fatalf("stages = %+v", st)
	}
	if st[0].Status != "success" || st[0].Jobs[0].DurationS != 9 || st[0].Jobs[0].ExpectedS != 8 {
		t.Fatalf("validate stage = %+v", st[0])
	}
	if st[1].Running == nil || st[1].Running.Name != "build" || st[1].Running.ExpectedS != 95 || st[1].Running.StartedAt == nil {
		t.Fatalf("build stage running baseline = %+v", st[1].Running)
	}
	if st[2].Status != "created" || st[2].Running != nil {
		t.Fatalf("publish stage = %+v", st[2])
	}
}

func TestNoteServicePipelineTransitions(t *testing.T) {
	a := &api{hot: map[string]time.Time{}, lastSvc: map[string]string{}}
	p := "civo/metaphor/metaphor"
	if a.noteServicePipeline(p, "running") {
		t.Fatal("first observation must not fire")
	}
	if !a.noteServicePipeline(p, "success") {
		t.Fatal("running -> success must fire (dep-bump in flight)")
	}
	if a.noteServicePipeline(p, "success") {
		t.Fatal("steady success must not re-fire")
	}
	a.noteServicePipeline(p, "failed")
	if a.noteServicePipeline(p, "success") {
		t.Fatal("failed -> success (new run seen only at terminal) must not fire")
	}
	// and the hot lane actually engages via markHot
	a.markHot("civo/metaphor/charts")
	if !a.isHot("civo/metaphor/charts") {
		t.Fatal("markHot/isHot broken")
	}
}

func TestEpicVersionsNeverOutrankRCs(t *testing.T) {
	// a feature tag must not parse (end anchor) — otherwise it would read as
	// a clean release and outrank every rc as "newest"
	if v := parseVer("0.2.0-epic-20-pink.3"); v.ok {
		t.Fatalf("epic version parsed as ordered semver: %+v", v)
	}
	tags := []glTag{
		{"metaphor-v0.2.0-epic-20-pink.9"},
		{"metaphor-v0.2.0-rc.19"},
		{"metaphor-v0.2.0-rc.4"},
	}
	if got := newestTag(tags, "metaphor-v"); got != "metaphor-v0.2.0-rc.19" {
		t.Fatalf("newestTag = %q — epic tags must never win even when newer", got)
	}
	// clean releases and rcs still parse
	if !parseVer("0.2.0").ok || !parseVer("0.2.0-rc.19").ok || !parseVer("v1.2.3").ok {
		t.Fatal("normal versions must still parse")
	}
}

func TestLoadTopologyOverride(t *testing.T) {
	t.Setenv("TOPOLOGY", `{"services":[{"name":"konstruct-api","project":"civo/konstruct/konstruct-api","chart":"charts/civo/konstruct/konstruct-api/Chart.yaml"}],"macro":{"name":"konstruct","project":"civo/konstruct/charts","file":"charts/konstruct/Chart.yaml","tagPrefix":"konstruct-v"},"delivery":[{"env":"internal","cluster":"konstruct-civo-internal","project":"civo/platform/civo-gitops","app":"registry/clusters/konstruct-civo-internal/components/konstruct-system/konstruct.yaml"}]}`)
	tp := loadTopology()
	if tp.MacroProj != "civo/konstruct/charts" || tp.MacroTag != "konstruct-v" ||
		len(tp.Services) != 1 || tp.Services[0].Name != "konstruct-api" ||
		len(tp.Delivery) != 1 || tp.Delivery[0].Cluster != "konstruct-civo-internal" {
		t.Fatalf("topology = %+v", tp)
	}
	t.Setenv("TOPOLOGY", "{broken")
	if tp := loadTopology(); tp.MacroProj != "civo/metaphor/charts" {
		t.Fatalf("broken TOPOLOGY must fall back to metaphor default, got %s", tp.MacroProj)
	}
}

// TestBranchesView pins the housekeeping payload: pointer-ahead semantics
// (nil = unchecked, never "merged"), committer stacks from the compare, and
// every branch's end-result macro version.
func TestBranchesView(t *testing.T) {
	gl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.EscapedPath()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(p, "/repository/tags"):
			w.Write([]byte(`[{"name":"metaphor-v0.2.0-epic-101-aurora.2"},{"name":"metaphor-v0.2.0-rc.4"},{"name":"metaphor-v0.2.0-rc.3"}]`))
		case strings.Contains(p, "/repository/compare"):
			if strings.Contains(r.URL.RawQuery, "hotfix-done") {
				w.Write([]byte(`{"commits":[]}`))
				return
			}
			w.Write([]byte(`{"commits":[{"id":"a","author_name":"Jared Edwards","author_email":"jared@civo.com"},{"id":"b","author_name":"John Dietz","author_email":"jd@civo.com"},{"id":"c","author_name":"John Dietz","author_email":"jd@civo.com"}]}`))
		case strings.HasSuffix(p, "/avatar"):
			w.Write([]byte(`{"avatar_url":"http://gl/av.png"}`))
		case strings.HasSuffix(p, "/repository/branches"):
			w.Write([]byte(`[
				{"name":"main","web_url":"http://gl/t/main","commit":{"short_id":"aa","title":"feat: x","author_name":"John Dietz","committed_date":"2026-08-26T10:00:00Z"}},
				{"name":"hotfix/0.2","web_url":"http://gl/t/h02","commit":{"short_id":"bb","title":"fix: y","author_name":"Jared Edwards","committed_date":"2026-08-25T10:00:00Z"}},
				{"name":"hotfix-done","web_url":"http://gl/t/hd","commit":{"short_id":"cc","title":"fix: z","author_name":"John Dietz","committed_date":"2026-08-26T09:00:00Z"}},
				{"name":"epic-101-aurora","web_url":"http://gl/t/e101","commit":{"short_id":"dd","title":"ci: bump","author_name":"kbot","committed_date":"2026-08-26T08:00:00Z"}}]`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer gl.Close()
	srv := httptest.NewServer(newAPI(newGLClient(gl.URL, "tok"), nil))
	defer srv.Close()

	res, err := http.Get(srv.URL + "/api/branches")
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Repos []repoBranches `json:"repos"`
	}
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Repos) == 0 {
		t.Fatal("no repos")
	}
	r0 := out.Repos[0]

	byName := map[string]branchJSON{}
	for _, b := range append(append([]branchJSON{}, r0.Main...), append(r0.Hotfix, r0.Epic...)...) {
		byName[b.Name] = b
	}

	// hotfix/0.2: ahead=3, two distinct committers newest-first, avatars resolved
	h := byName["hotfix/0.2"]
	if h.Ahead == nil || *h.Ahead != 3 {
		t.Fatalf("hotfix/0.2 ahead = %v", h.Ahead)
	}
	if len(h.Committers) != 2 || h.Committers[0].Name != "John Dietz" || h.Committers[1].Name != "Jared Edwards" {
		t.Fatalf("hotfix/0.2 committers = %+v", h.Committers)
	}
	if h.Committers[0].Avatar != "http://gl/av.png" {
		t.Fatalf("avatar not resolved: %+v", h.Committers[0])
	}

	// hotfix-done: ahead=0 (genuinely merged), no committers
	d := byName["hotfix-done"]
	if d.Ahead == nil || *d.Ahead != 0 || len(d.Committers) != 0 {
		t.Fatalf("hotfix-done = ahead %v committers %+v", d.Ahead, d.Committers)
	}

	// macro mapping: main → newest rc; epic branch → its feature tag
	if byName["main"].MacroVer != "0.2.0-rc.4" {
		t.Fatalf("main macro_ver = %q", byName["main"].MacroVer)
	}
	e := byName["epic-101-aurora"]
	if e.MacroVer != "0.2.0-epic-101-aurora.2" || !strings.Contains(e.MacroURL, "/-/tags/metaphor-v0.2.0-epic-101-aurora.2") {
		t.Fatalf("epic macro = %q %q", e.MacroVer, e.MacroURL)
	}
	// hotfix lines don't publish umbrellas in this org — no false mapping
	if h.MacroVer != "" {
		t.Fatalf("hotfix macro_ver = %q (want empty)", h.MacroVer)
	}
	// the ↑N badge's click-through: GitLab's main...branch compare
	if !strings.Contains(h.CompareURL, "/-/compare/main...hotfix%2F0.2") {
		t.Fatalf("compare_url = %q", h.CompareURL)
	}
}

// TestOverviewActivity pins the fleet's activity-view payload: per-project
// last_activity_at, the latest human-readable event, and release staleness.
func TestOverviewActivity(t *testing.T) {
	gl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		switch {
		case p == "/api/v4/projects":
			w.Write([]byte(`[{"id":9,"name":"zippy","path_with_namespace":"civo/metaphor/zippy","web_url":"http://gl/z","default_branch":"main","last_activity_at":"2026-08-26T09:00:00Z","namespace":{"full_path":"civo/metaphor"}}]`))
		case strings.HasSuffix(p, "/projects/9/pipelines"):
			w.Write([]byte(`[]`))
		case strings.HasSuffix(p, "/projects/9/events"):
			w.Write([]byte(`[{"action_name":"pushed to","created_at":"2026-08-26T09:00:00Z","author":{"username":"jd"},"push_data":{"ref":"main","commit_title":"feat: sharpen","commit_count":2}}]`))
		case strings.HasSuffix(p, "/projects/9/releases"):
			w.Write([]byte(`[{"tag_name":"v0.11.0","name":"v0.11.0","released_at":"2026-08-20T00:00:00Z","_links":{"self":"http://gl/z/-/releases/v0.11.0"}}]`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer gl.Close()
	srv := httptest.NewServer(newAPI(newGLClient(gl.URL, "tok"), nil))
	defer srv.Close()

	res, err := http.Get(srv.URL + "/api/overview")
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Groups []struct {
			Projects []projectJSON `json:"projects"`
		} `json:"groups"`
	}
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Groups) != 1 || len(out.Groups[0].Projects) != 1 {
		t.Fatalf("groups = %+v", out.Groups)
	}
	p := out.Groups[0].Projects[0]
	if p.LastActivityAt.IsZero() {
		t.Fatal("last_activity_at missing")
	}
	if p.LatestEvent == nil || p.LatestEvent.Type != "push" || !strings.Contains(p.LatestEvent.Title, "feat: sharpen") {
		t.Fatalf("latest_event = %+v", p.LatestEvent)
	}
	if p.LatestRelease == nil || p.LatestRelease.Tag != "v0.11.0" || p.LatestRelease.DaysAgo < 5 {
		t.Fatalf("latest_release = %+v", p.LatestRelease)
	}
}
