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
	tags := []glTag{{"metaphor-v0.2.0-rc.2"}, {"metaphor-v0.2.0-rc.4"}, {"metaphor-v0.2.0-rc.3"}, {"unrelated-1.0.0"}}
	if got := newestTag(tags, "metaphor-v"); got != "metaphor-v0.2.0-rc.4" {
		t.Fatalf("newestTag = %q, want metaphor-v0.2.0-rc.4", got)
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
