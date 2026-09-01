package main

import (
	"context"
	"encoding/json"
	"fmt"
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
	tags := []glTag{{Name: "unrelated-1.0.0"}, {Name: "metaphor-v0.2.0-rc.4"}, {Name: "metaphor-v0.2.0-rc.3"}, {Name: "metaphor-v0.2.0-rc.2"}}
	if got := newestTag(tags, "metaphor-v"); got != "metaphor-v0.2.0-rc.4" {
		t.Fatalf("newestTag = %q, want metaphor-v0.2.0-rc.4", got)
	}
	sha := []glTag{{Name: "konstruct-v0.7.8-rc.57f3903b"}, {Name: "konstruct-v0.6.5-rc.44372335"}}
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
	// the bundle tree: declared order, exact pins, read at the published tag,
	// each enriched with its provenance line (counter-rc pins ride main)
	wantBundle := []depJSON{
		{Name: "metaphor", Version: "0.11.0-rc.13", Source: "main"},
		{Name: "metaphor-dashboard-manager", Version: "0.12.0-rc.15", Source: "main"},
		{Name: "metaphor-micro-frontend", Version: "0.1.0-rc.7", Source: "main"},
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
		{Name: "metaphor-v0.2.0-epic-20-pink.9"},
		{Name: "metaphor-v0.2.0-rc.19"},
		{Name: "metaphor-v0.2.0-rc.4"},
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

// TestFeaturesGrouping pins the by-epic feature view: distinct branches are
// distinct features, every service is listed with its derivation state, and
// stale epics stay out.
func TestFeaturesGrouping(t *testing.T) {
	gl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.EscapedPath()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(p, "/repository/tags"):
			w.Write([]byte(`[{"name":"metaphor-v0.2.0-epic-101-aurora.2"},{"name":"metaphor-v0.2.0-rc.4"}]`))
		case strings.Contains(p, "metaphor-macro%2FChart.yaml") && strings.Contains(r.URL.RawQuery, "epic-101-aurora"):
			w.Header().Set("Content-Type", "text/plain")
			w.Write([]byte("version: 0.2.0\ndependencies:\n  - name: metaphor\n    version: \"0.11.0-epic-101-aurora.1\"\n  - name: metaphor-dashboard-manager\n    version: \"0.12.0-rc.15\"\n"))
		case strings.HasSuffix(p, "/merge_requests") && r.URL.Query().Get("source_branch") == "epic-101-aurora":
			w.Write([]byte(`[{"iid":12,"title":"Draft: epic-101-aurora","state":"opened","web_url":"http://gl/x/-/merge_requests/12"}]`))
		case strings.HasSuffix(p, "/merge_requests"):
			w.Write([]byte(`[]`))
		case strings.HasSuffix(p, "/repository/branches") && strings.Contains(p, "micro-frontend"):
			// micro-frontend never joined the feature
			w.Write([]byte(`[{"name":"main","web_url":"http://gl/t/m","commit":{"short_id":"aa","title":"x","author_name":"jd","committed_date":"2026-08-26T10:00:00Z"}}]`))
		case strings.HasSuffix(p, "/repository/branches"):
			w.Write([]byte(`[
				{"name":"main","web_url":"http://gl/t/m","commit":{"short_id":"aa","title":"x","author_name":"jd","committed_date":"2026-08-26T10:00:00Z"}},
				{"name":"epic-101-aurora","web_url":"http://gl/t/e","commit":{"short_id":"bb","title":"y","author_name":"jd","committed_date":"2026-08-26T09:00:00Z"}},
				{"name":"epic-7-legacy","web_url":"http://gl/t/l","commit":{"short_id":"cc","title":"z","author_name":"jd","committed_date":"2026-06-01T00:00:00Z"}},
				{"name":"epic-showrishi","web_url":"http://gl/t/s","commit":{"short_id":"dd","title":"w","author_name":"jd","committed_date":"2026-08-26T08:00:00Z"}}]`))
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
		Features []featureJSON `json:"features"`
	}
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	byBranch := map[string]featureJSON{}
	for _, f := range out.Features {
		byBranch[f.Branch] = f
	}
	if _, ok := byBranch["epic-7-legacy"]; ok {
		t.Fatal("stale epic leaked into features")
	}
	a, ok := byBranch["epic-101-aurora"]
	if !ok {
		t.Fatalf("aurora missing: %+v", out.Features)
	}
	sh, ok := byBranch["epic-showrishi"]
	if !ok || sh.EpicIID != 0 {
		t.Fatalf("showrishi must be its own feature with no epic: %+v", sh)
	}
	if a.EpicIID != 101 || !a.Charts || a.MacroVer != "0.2.0-epic-101-aurora.2" {
		t.Fatalf("aurora = %+v", a)
	}
	st := map[string]featSvcJSON{}
	for _, sv := range a.Services {
		st[sv.Name] = sv
	}
	if st["metaphor"].State != "updated" {
		t.Fatalf("metaphor state = %q (pin is a feature version)", st["metaphor"].State)
	}
	if st["metaphor-dashboard-manager"].State != "joined" {
		t.Fatalf("dashboard state = %q (branch exists, pin stable)", st["metaphor-dashboard-manager"].State)
	}
	if st["metaphor-micro-frontend"].State != "main" {
		t.Fatalf("micro-frontend state = %q (never joined)", st["metaphor-micro-frontend"].State)
	}
	if st["metaphor"].MRIID != 12 {
		t.Fatalf("metaphor mr = %+v", st["metaphor"])
	}
}

// TestMergedBranchDeleted: merging deletes the source branch — the feature
// must still read merged (or the epic never closes as Done).
func TestMergedBranchDeleted(t *testing.T) {
	gl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.EscapedPath()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(p, "/repository/tags"):
			w.Write([]byte(`[]`))
		case strings.HasSuffix(p, "/merge_requests") && r.URL.Query().Get("source_branch") == "epic-9-gone":
			w.Write([]byte(`[{"iid":21,"state":"merged","merged_at":"2026-08-27T08:00:00Z","source_branch":"epic-9-gone","web_url":"http://gl/x/-/merge_requests/21"}]`))
		case strings.HasSuffix(p, "/merge_requests"):
			w.Write([]byte(`[]`))
		case strings.HasSuffix(p, "/repository/branches") && strings.Contains(p, "%2Fcharts"):
			// only the charts twin survives the merge
			w.Write([]byte(`[{"name":"main","web_url":"u","commit":{"short_id":"aa","title":"x","author_name":"jd","committed_date":"2026-08-27T07:00:00Z"}},{"name":"epic-9-gone","web_url":"u","commit":{"short_id":"bb","title":"y","author_name":"jd","committed_date":"2026-08-27T07:30:00Z"}}]`))
		case strings.HasSuffix(p, "/repository/branches"):
			w.Write([]byte(`[{"name":"main","web_url":"u","commit":{"short_id":"aa","title":"x","author_name":"jd","committed_date":"2026-08-27T07:00:00Z"}}]`))
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
		Features []featureJSON `json:"features"`
	}
	json.NewDecoder(res.Body).Decode(&out)
	var f *featureJSON
	for i := range out.Features {
		if out.Features[i].Branch == "epic-9-gone" {
			f = &out.Features[i]
		}
	}
	if f == nil {
		t.Fatalf("feature missing: %+v", out.Features)
	}
	if !f.Merged || f.MergedAt == nil {
		t.Fatalf("merged rollup = %+v", f)
	}
	found := false
	for _, sv := range f.Services {
		if sv.State == "merged" && sv.MRIID == 21 {
			found = true
		}
	}
	if !found {
		t.Fatalf("no merged service row: %+v", f.Services)
	}
}

// TestMRStateLabel pins the chip vocabulary: merged/closed outrank draft,
// draft outranks feedback, comments flip ready→feedback.
func TestMRStateLabel(t *testing.T) {
	cases := []struct {
		mr   glMR
		want string
	}{
		{glMR{State: "merged", Draft: true}, "merged"},
		{glMR{State: "closed"}, "closed"},
		{glMR{State: "opened", Draft: true, UserNotesCount: 3}, "draft"},
		{glMR{State: "opened", Title: "Draft: epic-x"}, "draft"},
		{glMR{State: "opened", UserNotesCount: 2}, "feedback"},
		{glMR{State: "opened"}, "ready"},
	}
	for _, c := range cases {
		if got := mrStateLabel(c.mr); got != c.want {
			t.Fatalf("%+v → %q (want %q)", c.mr, got, c.want)
		}
	}
	// bestMR: an open MR beats a newer merged one; merged beats closed
	open := glMR{IID: 2, State: "opened"}
	merged := glMR{IID: 1, State: "merged"}
	if m := bestMR([]glMR{merged, open}); m.IID != 2 {
		t.Fatalf("bestMR = %+v", m)
	}
	if m := bestMR([]glMR{{IID: 3, State: "closed"}, merged}); m.IID != 1 {
		t.Fatalf("bestMR merged-over-closed = %+v", m)
	}
}

// TestOrgLogo pins the proxy: /api/org offers the proxied path only when the
// group has an avatar, and /api/org-logo streams it with the content type.
func TestOrgLogo(t *testing.T) {
	gl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.EscapedPath(), "/avatar") && strings.Contains(r.URL.EscapedPath(), "/groups/"):
			if r.Header.Get("PRIVATE-TOKEN") != "tok" {
				w.WriteHeader(401)
				return
			}
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Write([]byte("\x89PNG\r\n\x1a\nrest"))
		case strings.HasPrefix(r.URL.EscapedPath(), "/api/v4/groups/"):
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"name":"Metaphor","full_path":"civo/metaphor","avatar_url":"%s/logo.png","web_url":"http://gl/groups/civo/metaphor"}`, "http://"+r.Host)
		case r.URL.Path == "/logo.png":
			// the web-session route: token headers are ignored → 401 always,
			// exactly like real GitLab /uploads paths
			w.WriteHeader(401)
		default:
			w.WriteHeader(404)
		}
	}))
	defer gl.Close()
	srv := httptest.NewServer(newAPI(newGLClient(gl.URL, "tok"), []string{"civo/metaphor"}))
	defer srv.Close()

	res, _ := http.Get(srv.URL + "/api/org")
	var org struct {
		Logo string `json:"logo"`
		Name string `json:"name"`
	}
	json.NewDecoder(res.Body).Decode(&org)
	if org.Logo != "/api/org-logo" || org.Name != "Metaphor" {
		t.Fatalf("org = %+v", org)
	}
	res, _ = http.Get(srv.URL + "/api/org-logo")
	b := make([]byte, 8)
	res.Body.Read(b)
	if res.StatusCode != 200 || res.Header.Get("Content-Type") != "image/png" {
		t.Fatalf("logo = %d %s (want sniffed image/png from octet-stream)", res.StatusCode, res.Header.Get("Content-Type"))
	}
}

// TestPromotionRows pins the in-env rules: direct beats everything, merged
// features ride rcs cut AFTER the merge, everything else is missing.
func TestPromotionRows(t *testing.T) {
	mergedAt := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	afterMerge := mergedAt.Add(30 * time.Minute)
	beforeMerge := mergedAt.Add(-30 * time.Minute)
	tags := []glTag{
		{Name: "metaphor-v0.2.0-rc.31", Commit: struct {
			CreatedAt *time.Time `json:"created_at"`
		}{&afterMerge}},
		{Name: "metaphor-v0.2.0-rc.30", Commit: struct {
			CreatedAt *time.Time `json:"created_at"`
		}{&beforeMerge}},
	}
	feats := []featureJSON{
		{Branch: "epic-7-live", Merged: false},
		{Branch: "epic-8-done", Merged: true, MergedAt: &mergedAt},
	}
	envs := func(delivered string) []deliveryNode {
		return []deliveryNode{{Env: "dev-33", Delivered: delivered}}
	}

	// unmerged + env runs its own umbrella → direct
	out := promotionRows([]featureJSON{feats[0]}, envs("0.2.0-epic-7-live.3"), tags, "metaphor-v")
	if out[0].Envs[0].State != "direct" {
		t.Fatalf("direct = %+v", out[0].Envs[0])
	}
	// unmerged + env on an rc → missing
	out = promotionRows([]featureJSON{feats[0]}, envs("0.2.0-rc.31"), tags, "metaphor-v")
	if out[0].Envs[0].State != "missing" {
		t.Fatalf("unmerged rc = %+v", out[0].Envs[0])
	}
	// merged + rc cut AFTER the merge → via-rc (the rc carries it)
	out = promotionRows([]featureJSON{feats[1]}, envs("0.2.0-rc.31"), tags, "metaphor-v")
	if out[0].Envs[0].State != "via-rc" {
		t.Fatalf("via-rc = %+v", out[0].Envs[0])
	}
	// merged + rc cut BEFORE the merge → missing (that rc predates the work)
	out = promotionRows([]featureJSON{feats[1]}, envs("0.2.0-rc.30"), tags, "metaphor-v")
	if out[0].Envs[0].State != "missing" {
		t.Fatalf("old rc = %+v", out[0].Envs[0])
	}
}

// TestBuiltFrom pins the built-from derivation for the umbrella picker.
func TestBuiltFrom(t *testing.T) {
	cases := map[string]string{
		"metaphor-v0.2.0-rc.31":                 "main",
		"metaphor-v0.2.0-epic-106-background.2": "epic-106-background",
		"konstruct-v0.7.8-rc.57f3903b":          "main",
		"":                                      "",
	}
	for in, want := range cases {
		if got := builtFrom(in); got != want {
			t.Fatalf("builtFrom(%q) = %q want %q", in, got, want)
		}
	}
}

// TestSingleAppMode: no macro — apps deliver directly, pending targets render
// honestly, ready ones drift by their own version.
func TestSingleAppMode(t *testing.T) {
	gl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.EscapedPath()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(p, "solo%2FChart.yaml"):
			w.Header().Set("Content-Type", "text/plain")
			w.Write([]byte("version: 1.4.0\n"))
		case strings.Contains(p, "solo-app.yaml"):
			w.Header().Set("Content-Type", "text/plain")
			w.Write([]byte("spec:\n  source:\n    targetRevision: 1.4.0\n"))
		case strings.HasSuffix(p, "/pipelines/latest"):
			w.Write([]byte(`{"id":9,"status":"success","ref":"main","sha":"abcd1234","web_url":"http://gl/x/-/pipelines/9","created_at":"2026-08-27T00:00:00Z","updated_at":"2026-08-27T00:01:00Z"}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer gl.Close()
	t.Setenv("TOPOLOGY", `{"services":[{"name":"solo","project":"civo/x/solo","chart":"charts/solo/Chart.yaml","delivery":[{"env":"prod-a","cluster":"a","project":"civo/x/gitops","app":"solo-app.yaml","write":"mr"},{"env":"kubefunk-b","cluster":"b","host":"https://gitlab.kubefunk.net","project":"y/gitops","app":"solo-app.yaml","token_env":"KFUNK_TOKEN","write":"mr"}]}]}`)
	srv := httptest.NewServer(newAPI(newGLClient(gl.URL, "tok"), nil))
	defer srv.Close()

	res, err := http.Get(srv.URL + "/api/ecosystem")
	if err != nil {
		t.Fatal(err)
	}
	var eco struct {
		Macro    macroNode      `json:"macro"`
		Delivery []deliveryNode `json:"delivery"`
	}
	json.NewDecoder(res.Body).Decode(&eco)
	if eco.Macro.Project != "" {
		t.Fatalf("single-app mode grew a macro: %+v", eco.Macro)
	}
	if len(eco.Delivery) != 2 {
		t.Fatalf("delivery = %+v", eco.Delivery)
	}
	byEnv := map[string]deliveryNode{}
	for _, d := range eco.Delivery {
		byEnv[d.Env] = d
	}
	a := byEnv["prod-a"]
	if !a.Ready || a.For != "solo" || a.Delivered != "1.4.0" || a.State != "current" {
		t.Fatalf("prod-a = %+v", a)
	}
	b := byEnv["kubefunk-b"]
	if b.Ready || b.State != "pending" {
		t.Fatalf("kubefunk-b must be pending without its token: %+v", b)
	}
}

// TestBuiltFromHotfixAndShaImmunity pins the R4 regex change: hotfix lines
// resolve like epic lines, while sha-suffixed rcs stay on "main".
func TestBuiltFromHotfixAndShaImmunity(t *testing.T) {
	cases := map[string]string{
		"metaphor-v0.2.0-hotfix-0-9.3":          "hotfix-0-9",
		"konstruct-v0.11.0-hotfix-2-4.1":        "hotfix-2-4",
		"metaphor-v0.2.0-epic-106-background.2": "epic-106-background",
		"konstruct-v0.7.8-rc.57f3903b":          "main", // sha-rc must NOT match
		"0.7.8-rc.57f3903b":                     "main",
		"metaphor-v0.2.0-rc.13":                 "main", // counter rc
	}
	for in, want := range cases {
		if got := builtFrom(in); got != want {
			t.Fatalf("builtFrom(%q) = %q want %q", in, got, want)
		}
	}
}

// TestRcSHA pins the sha-vs-counter split: only hex-with-a-letter is a sha.
func TestRcSHA(t *testing.T) {
	cases := map[string]string{
		"0.11.0-rc.57f3903b": "57f3903b", // real sha
		"0.11.0-rc.13":       "",         // short counter
		"0.11.0-rc.1234567":  "",         // 7-digit all-digit counter, not a sha
		"0.11.0-rc.4":        "",
		"0.11.0":             "",
		"0.11.0-epic-x.1":    "",
	}
	for in, want := range cases {
		if got := rcSHA(in); got != want {
			t.Fatalf("rcSHA(%q) = %q want %q", in, got, want)
		}
	}
}

// TestChartVerFor pins the hotfix ChartVer lookup: newest matching dep-bump,
// matched by branch needle OR tip sha, always for the right service.
func TestChartVerFor(t *testing.T) {
	commits := []glCommit{
		{Title: "ci: update konstruct-api dependency to 0.11.0-rc.a1b2c3d4"},
		{Title: "ci: bump metaphor dependency to 0.11.0-hotfix-0-9.2"},
		{Title: "chore: something unrelated"},
		{Title: "ci: update konstruct-api dependency to 0.10.0-rc.deadbeef"},
	}
	cases := []struct {
		description, svc, needle, tip, want string
	}{
		{"matches by branch needle", "metaphor", "-hotfix-0-9.", "", "0.11.0-hotfix-0-9.2"},
		{"matches by tip sha, newest first", "konstruct-api", "", "a1b2c3d4", "0.11.0-rc.a1b2c3d4"},
		{"wrong service never matches", "metaphor", "", "a1b2c3d4", ""},
		{"no needle or tip match returns empty", "konstruct-api", "-nope.", "zzzz", ""},
	}
	for _, c := range cases {
		t.Run(c.description, func(t *testing.T) {
			if got := chartVerFor(commits, c.svc, c.needle, c.tip); got != c.want {
				t.Fatalf("chartVerFor = %q want %q", got, c.want)
			}
		})
	}
}

// TestMacroTagForTip pins the tip-aware mapping: branch-name needle first,
// then the konstruct-hotfix -rc.<shortSHA> fallback, else empties.
func TestMacroTagForTip(t *testing.T) {
	tags := []glTag{
		{Name: "metaphor-v0.2.0-epic-101-aurora.2"},
		{Name: "metaphor-v0.2.0-rc.4"},
		{Name: "metaphor-v0.3.0-rc.a1b2c3d4"}, // a hotfix tip's sha-suffixed umbrella
	}
	cases := []struct {
		description, branch, short, wantVer, wantTag string
	}{
		{"main → newest rc", "main", "zz", "0.2.0-rc.4", "metaphor-v0.2.0-rc.4"},
		{"epic → its feature tag", "epic-101-aurora", "zz", "0.2.0-epic-101-aurora.2", "metaphor-v0.2.0-epic-101-aurora.2"},
		{"hotfix tip → tag ending -rc.<sha>", "hotfix-0.3", "a1b2c3d4", "0.3.0-rc.a1b2c3d4", "metaphor-v0.3.0-rc.a1b2c3d4"},
		{"no needle, no sha match → empties", "hotfix-9.9", "ffffffff", "", ""},
	}
	for _, c := range cases {
		t.Run(c.description, func(t *testing.T) {
			ver, tag := macroTagForTip(tags, "metaphor-v", c.branch, c.short)
			if ver != c.wantVer || tag != c.wantTag {
				t.Fatalf("macroTagForTip = (%q,%q) want (%q,%q)", ver, tag, c.wantVer, c.wantTag)
			}
		})
	}
}

// TestAssembleOrgHotfixes pins the org rollup: live hotfix branches grouped by
// name, a cell per service + macro repo, stale branches dropped, newest-first.
func TestAssembleOrgHotfixes(t *testing.T) {
	topo := topology{
		Services: []svcSpec{
			{Name: "metaphor", Project: "civo/metaphor/metaphor"},
			{Name: "mdm", Project: "civo/metaphor/metaphor-dashboard-manager"},
		},
		MacroName: "metaphor-macro", MacroProj: "civo/metaphor/charts",
	}
	repos := []repoBranches{
		{Name: "metaphor", Project: "civo/metaphor/metaphor", Hotfix: []branchJSON{
			{Name: "hotfix-0.9", When: "2026-08-27T10:00:00Z", WebURL: "u1",
				ChartVer: "0.11.0-rc.aaa", MacroVer: "0.2.0-rc.9", MacroURL: "m1"},
			{Name: "hotfix-old", When: "2026-01-01T00:00:00Z", Stale: true}, // dropped
		}},
		{Name: "mdm", Project: "civo/metaphor/metaphor-dashboard-manager", Hotfix: []branchJSON{
			{Name: "hotfix-0.9", When: "2026-08-27T09:00:00Z", WebURL: "u2"},
		}},
		{Name: "metaphor-macro", Project: "civo/metaphor/charts", Hotfix: []branchJSON{
			{Name: "hotfix-charts", When: "2026-08-26T00:00:00Z", WebURL: "u3"},
		}},
	}
	out := assembleOrgHotfixes(topo, repos)
	if len(out) != 2 {
		t.Fatalf("rows = %+v", out)
	}
	if out[0].Branch != "hotfix-0.9" || out[1].Branch != "hotfix-charts" {
		t.Fatalf("order/branch = %+v", out)
	}
	row := out[0]
	if len(row.Repos) != 3 { // two services + the macro repo
		t.Fatalf("cols = %+v", row.Repos)
	}
	byProj := map[string]orgHotfixRepo{}
	for _, c := range row.Repos {
		byProj[c.Project] = c
	}
	m := byProj["civo/metaphor/metaphor"]
	if !m.Has || m.ChartVer != "0.11.0-rc.aaa" || m.MacroVer != "0.2.0-rc.9" || m.WebURL != "u1" {
		t.Fatalf("metaphor cell = %+v", m)
	}
	if c := byProj["civo/metaphor/charts"]; c.Has {
		t.Fatalf("charts must not carry hotfix-0.9: %+v", c)
	}
	if c := byProj["civo/metaphor/charts"]; c.Name != "charts" {
		t.Fatalf("macro column display name = %q, want short 'charts'", c.Name)
	}
	for _, r := range out {
		if r.Branch == "hotfix-old" {
			t.Fatal("stale hotfix leaked into the rollup")
		}
	}
}

// TestDepSource pins the provenance classifier: branch-suffix → the branch id;
// counter/plain → main; sha-rc → the commit's branch (main preferred, then a
// hotfix line); unknown service → "" (omit); commit on no branch → "unknown".
func TestDepSource(t *testing.T) {
	gl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.EscapedPath()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(p, "/commits/aaaaaaa1/refs"):
			w.Write([]byte(`[{"type":"branch","name":"main"},{"type":"branch","name":"hotfix-0.9"}]`))
		case strings.HasSuffix(p, "/commits/bbbbbbb2/refs"):
			w.Write([]byte(`[{"type":"branch","name":"hotfix-0.9"}]`))
		case strings.HasSuffix(p, "/commits/ccccccc3/refs"):
			w.Write([]byte(`[]`)) // on no branch we can name
		default:
			w.WriteHeader(404)
		}
	}))
	defer gl.Close()
	a := &api{gl: newGLClient(gl.URL, "tok"), c: newCache()}
	topo := topology{Services: []svcSpec{{Name: "metaphor", Project: "civo/metaphor/metaphor"}}}
	ctx := context.Background()
	cases := []struct {
		description, name, version, want string
	}{
		{"epic branch-suffix → the branch id", "metaphor", "0.11.0-epic-101-aurora.1", "epic-101-aurora"},
		{"hotfix branch-suffix → the branch id", "metaphor", "0.11.0-hotfix-0-9.2", "hotfix-0-9"},
		{"counter rc → main", "metaphor", "0.11.0-rc.13", "main"},
		{"plain release → main", "metaphor", "0.11.0", "main"},
		{"unknown service name → empty so the field omits", "ghost", "0.11.0-rc.aaaaaaa1", ""},
		{"sha-rc contained in main → main", "metaphor", "0.11.0-rc.aaaaaaa1", "main"},
		{"sha-rc only on a hotfix → that hotfix", "metaphor", "0.11.0-rc.bbbbbbb2", "hotfix-0.9"},
		{"sha-rc on no named branch → unknown", "metaphor", "0.11.0-rc.ccccccc3", "unknown"},
	}
	for _, c := range cases {
		t.Run(c.description, func(t *testing.T) {
			if got := a.depSource(ctx, topo, c.name, c.version); got != c.want {
				t.Fatalf("depSource(%q,%q) = %q want %q", c.name, c.version, got, c.want)
			}
		})
	}
}
