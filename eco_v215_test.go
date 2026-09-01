package main

// Phase C of v2.15.0: payload-level coverage — charts-only feature visibility,
// the org-wide hotfix rollup, and bundle-pin provenance through /api/bundle.
// Fake timestamps are now-relative: fixed dates rot out of freshness windows.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func agoRFC(d time.Duration) string { return time.Now().UTC().Add(-d).Format(time.RFC3339) }

// TestChartsOnlyFeatureVisibility: a feature started from the macro card
// exists only on the charts repo — it must still surface as a feature row with
// charts=true and every service honestly "main" (not joined yet).
func TestChartsOnlyFeatureVisibility(t *testing.T) {
	fresh := agoRFC(2 * time.Hour)
	gl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.EscapedPath()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(p, "/repository/tags"):
			w.Write([]byte(`[{"name":"metaphor-v0.2.0-rc.4"}]`))
		case strings.Contains(p, "metaphor-macro%2FChart.yaml") && strings.Contains(r.URL.RawQuery, "epic-77-solo"):
			w.Header().Set("Content-Type", "text/plain")
			w.Write([]byte("version: 0.2.0\ndependencies:\n  - name: metaphor\n    version: \"0.11.0-rc.13\"\n"))
		case strings.HasSuffix(p, "/merge_requests"):
			w.Write([]byte(`[]`))
		case strings.HasSuffix(p, "/repository/branches") && strings.Contains(p, "%2Fcharts"):
			w.Write([]byte(`[
				{"name":"main","web_url":"http://gl/c/m","commit":{"short_id":"aa","title":"x","author_name":"jd","committed_date":"` + fresh + `"}},
				{"name":"epic-77-solo","web_url":"http://gl/c/e77","commit":{"short_id":"bb","title":"start","author_name":"jd","committed_date":"` + fresh + `"}}]`))
		case strings.HasSuffix(p, "/repository/branches"):
			w.Write([]byte(`[{"name":"main","web_url":"http://gl/s/m","commit":{"short_id":"cc","title":"y","author_name":"jd","committed_date":"` + fresh + `"}}]`))
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
	var solo *featureJSON
	for i := range out.Features {
		if out.Features[i].Branch == "epic-77-solo" {
			solo = &out.Features[i]
		}
	}
	if solo == nil {
		t.Fatalf("charts-only feature missing from features: %+v", out.Features)
	}
	if !solo.Charts {
		t.Fatal("charts-only feature must report charts=true")
	}
	for _, sv := range solo.Services {
		if sv.State != "main" {
			t.Fatalf("service %s state = %q, want main (nobody joined)", sv.Name, sv.State)
		}
	}
}

// TestOrgHotfixes: the ecosystem payload rolls fresh hotfix branches up
// org-wide — one row per branch, a cell per repo (services + charts) with has
// flags; stale branches stay out.
func TestOrgHotfixes(t *testing.T) {
	fresh := agoRFC(3 * time.Hour)
	stale := agoRFC(60 * 24 * time.Hour)
	branchRows := func(rows string) string { return "[" + rows + "]" }
	mainRow := `{"name":"main","web_url":"http://gl/m","commit":{"short_id":"aa","title":"x","author_name":"jd","committed_date":"` + fresh + `"}}`
	hfRow := `{"name":"hotfix-alpha","web_url":"http://gl/hf","commit":{"short_id":"bb","title":"fix","author_name":"jd","committed_date":"` + fresh + `"}}`
	oldRow := `{"name":"hotfix-ancient","web_url":"http://gl/old","commit":{"short_id":"cc","title":"z","author_name":"jd","committed_date":"` + stale + `"}}`
	gl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.EscapedPath()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(p, "metaphor-macro%2FChart.yaml"):
			w.Header().Set("Content-Type", "text/plain")
			w.Write([]byte("version: 0.2.0\ndependencies:\n  - name: metaphor\n    version: \"0.11.0-rc.13\"\n"))
		case strings.Contains(p, "charts%2Fmetaphor%2FChart.yaml"), strings.Contains(p, "dashboard-manager%2FChart.yaml"), strings.Contains(p, "micro-frontend%2FChart.yaml"):
			w.Write([]byte("version: 0.11.0\n"))
		case strings.Contains(p, "metaphor-macro.yaml"):
			w.Write([]byte("spec:\n  source:\n    targetRevision: 0.2.0-rc.2\n"))
		case strings.HasSuffix(p, "/repository/tags"):
			w.Write([]byte(`[{"name":"metaphor-v0.2.0-rc.4"}]`))
		case strings.Contains(p, "/repository/compare"):
			w.Write([]byte(`{"commits":[{"id":"a","author_name":"jd","author_email":"jd@civo.com"}]}`))
		case strings.HasSuffix(p, "/merge_requests"):
			w.Write([]byte(`[]`))
		case strings.HasSuffix(p, "/repository/branches") && strings.Contains(p, "%2Fmetaphor/repository"):
			w.Write([]byte(branchRows(mainRow + "," + hfRow)))
		case strings.HasSuffix(p, "/repository/branches") && strings.Contains(p, "micro-frontend"):
			w.Write([]byte(branchRows(mainRow + "," + hfRow)))
		case strings.HasSuffix(p, "/repository/branches") && strings.Contains(p, "%2Fcharts"):
			w.Write([]byte(branchRows(mainRow + "," + hfRow + "," + oldRow)))
		case strings.HasSuffix(p, "/repository/branches"):
			w.Write([]byte(branchRows(mainRow)))
		default:
			w.WriteHeader(404)
		}
	}))
	defer gl.Close()
	srv := httptest.NewServer(newAPI(newGLClient(gl.URL, "tok"), nil))
	defer srv.Close()

	res, err := http.Get(srv.URL + "/api/ecosystem")
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		OrgHotfixes []orgHotfixJSON `json:"org_hotfixes"`
	}
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.OrgHotfixes) != 1 {
		t.Fatalf("org_hotfixes = %+v (want exactly hotfix-alpha; ancient is stale)", out.OrgHotfixes)
	}
	row := out.OrgHotfixes[0]
	if row.Branch != "hotfix-alpha" {
		t.Fatalf("row = %+v", row)
	}
	if len(row.Repos) != 4 {
		t.Fatalf("repo cells = %d, want 4 (3 services + charts): %+v", len(row.Repos), row.Repos)
	}
	has := map[string]bool{}
	for _, c := range row.Repos {
		has[c.Name] = c.Has
	}
	if !has["metaphor"] || has["metaphor-dashboard-manager"] || !has["metaphor-micro-frontend"] || !has["charts"] {
		t.Fatalf("has flags = %+v", has)
	}
}

// TestBundleSources: /api/bundle classifies every pin's provenance — feature
// suffix from the version string, counter-rc as main, sha-rc through the
// commit-refs lookup.
func TestBundleSources(t *testing.T) {
	gl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.EscapedPath()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(p, "metaphor-macro%2FChart.yaml") && strings.Contains(r.URL.RawQuery, "metaphor-v0.9.0-rc.77"):
			w.Header().Set("Content-Type", "text/plain")
			w.Write([]byte("version: 0.9.0-rc.77\ndependencies:\n  - name: metaphor\n    version: \"0.11.0-epic-9-x.2\"\n  - name: metaphor-dashboard-manager\n    version: \"0.12.0-rc.13\"\n  - name: metaphor-micro-frontend\n    version: \"0.5.0-rc.abcdef12\"\n"))
		case strings.Contains(p, "micro-frontend") && strings.Contains(p, "/repository/commits/abcdef12/refs"):
			w.Write([]byte(`[{"type":"branch","name":"hotfix-z"}]`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer gl.Close()
	srv := httptest.NewServer(newAPI(newGLClient(gl.URL, "tok"), nil))
	defer srv.Close()

	res, err := http.Get(srv.URL + "/api/bundle?tag=metaphor-v0.9.0-rc.77")
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Deps []depJSON `json:"deps"`
	}
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	src := map[string]string{}
	for _, d := range out.Deps {
		src[d.Name] = d.Source
	}
	if src["metaphor"] != "epic-9-x" {
		t.Fatalf("feature pin source = %q, want epic-9-x", src["metaphor"])
	}
	if src["metaphor-dashboard-manager"] != "main" {
		t.Fatalf("counter-rc pin source = %q, want main", src["metaphor-dashboard-manager"])
	}
	if src["metaphor-micro-frontend"] != "hotfix-z" {
		t.Fatalf("sha-rc pin source = %q, want hotfix-z via commit refs", src["metaphor-micro-frontend"])
	}
}

// TestCommitRefsAndExists pins the two new client calls: refs decode, and the
// 404-vs-outage split commitExists callers depend on.
func TestCommitRefsAndExists(t *testing.T) {
	gl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.EscapedPath()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(p, "/repository/commits/feedf00d/refs"):
			w.Write([]byte(`[{"type":"branch","name":"hotfix-a"},{"type":"branch","name":"main"}]`))
		case strings.HasSuffix(p, "/repository/commits/feedf00d"):
			w.Write([]byte(`{"id":"feedf00d1234"}`))
		case strings.HasSuffix(p, "/repository/commits/00000000"):
			w.WriteHeader(404)
		case strings.HasSuffix(p, "/repository/commits/deadbad0"):
			w.WriteHeader(500)
		default:
			w.WriteHeader(404)
		}
	}))
	defer gl.Close()
	c := newGLClient(gl.URL, "tok")
	ctx := t.Context()

	refs, err := c.commitRefs(ctx, "g/p", "feedf00d")
	if err != nil || len(refs) != 2 || refs[0].Name != "hotfix-a" {
		t.Fatalf("commitRefs = %+v, %v", refs, err)
	}
	if ok, err := c.commitExists(ctx, "g/p", "feedf00d"); err != nil || !ok {
		t.Fatalf("existing commit = %v, %v", ok, err)
	}
	if ok, err := c.commitExists(ctx, "g/p", "00000000"); err != nil || ok {
		t.Fatalf("404 must be (false, nil), got %v, %v", ok, err)
	}
	if _, err := c.commitExists(ctx, "g/p", "deadbad0"); err == nil {
		t.Fatal("a 500 must surface as an error, never as absence")
	}
}

// TestFriendlyAuthor pins the bot un-masking: GitLab returns "****" as the
// display name for access-token bot users.
func TestFriendlyAuthor(t *testing.T) {
	cases := []struct{ name, username, want string }{
		{"John Dietz", "john.dietz", "John Dietz"},
		{"****", "group_1642_bot_34005c8398", "token bot (group_1642)"},
		{"****", "project_9_bot_ff", "token bot (project_9)"},
		{"", "kbot", "kbot"},
		{"****", "", "someone"},
		{"", "", "someone"},
	}
	for _, c := range cases {
		if got := friendlyAuthor(c.name, c.username); got != c.want {
			t.Fatalf("friendlyAuthor(%q,%q) = %q, want %q", c.name, c.username, got, c.want)
		}
	}
}

// TestBranchDivergenceAndAvatars: branch rows carry ahead AND behind (both
// compare directions, tip-sha cached) plus the last committer's avatar; the
// org hotfix cells and feature cells inherit them.
func TestBranchDivergenceAndAvatars(t *testing.T) {
	fresh := agoRFC(2 * time.Hour)
	gl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.EscapedPath()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(p, "metaphor-macro%2FChart.yaml"):
			w.Header().Set("Content-Type", "text/plain")
			w.Write([]byte("version: 0.2.0\ndependencies:\n  - name: metaphor\n    version: \"0.11.0-rc.13\"\n"))
		case strings.HasSuffix(p, "/repository/tags"):
			w.Write([]byte(`[{"name":"metaphor-v0.2.0-rc.4"}]`))
		case strings.Contains(p, "/repository/compare"):
			if r.URL.Query().Get("from") != "main" { // branch...main = behind
				w.Write([]byte(`{"commits":[{"id":"m1","author_name":"jd","author_email":"jd@civo.com"}]}`))
				return
			}
			w.Write([]byte(`{"commits":[{"id":"a","author_name":"jd","author_email":"jd@civo.com"},{"id":"b","author_name":"jd","author_email":"jd@civo.com"}]}`))
		case strings.HasSuffix(p, "/avatar"):
			w.Write([]byte(`{"avatar_url":"http://gl/av-jd.png"}`))
		case strings.HasSuffix(p, "/merge_requests"):
			w.Write([]byte(`[]`))
		case strings.HasSuffix(p, "/repository/branches"):
			w.Write([]byte(`[
				{"name":"main","web_url":"http://gl/m","commit":{"short_id":"aa","title":"x","author_name":"jd","author_email":"jd@civo.com","committed_date":"` + fresh + `"}},
				{"name":"hotfix-hot","web_url":"http://gl/h","commit":{"short_id":"bb","title":"f","author_name":"jd","author_email":"jd@civo.com","committed_date":"` + fresh + `"}},
				{"name":"epic-9-x","web_url":"http://gl/e","commit":{"short_id":"cc","title":"e","author_name":"****","author_email":"group_1642_bot_abc@noreply.git.civo.com","committed_date":"` + fresh + `"}}]`))
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
		t.Fatal("no repos in branches payload")
	}
	var hot, epic *branchJSON
	for i := range out.Repos {
		for j := range out.Repos[i].Hotfix {
			if out.Repos[i].Hotfix[j].Name == "hotfix-hot" {
				hot = &out.Repos[i].Hotfix[j]
			}
		}
		for j := range out.Repos[i].Epic {
			if out.Repos[i].Epic[j].Name == "epic-9-x" {
				epic = &out.Repos[i].Epic[j]
			}
		}
	}
	if hot == nil || hot.Ahead == nil || *hot.Ahead != 2 || hot.Behind == nil || *hot.Behind != 1 {
		t.Fatalf("hotfix divergence = %+v (want ahead 2, behind 1)", hot)
	}
	if hot.AuthorAvatar != "http://gl/av-jd.png" {
		t.Fatalf("hotfix author avatar = %q", hot.AuthorAvatar)
	}
	if epic == nil || epic.Ahead == nil || *epic.Ahead != 2 || epic.Behind == nil || *epic.Behind != 1 {
		t.Fatalf("epic divergence = %+v (epic lanes must be checked too)", epic)
	}
	if epic.Author != "token bot (group_1642)" {
		t.Fatalf("bot-authored branch = %q (masked name must un-mask via the noreply email)", epic.Author)
	}

	// the eco payload's org hotfix cells and feature cells inherit the fields
	res, err = http.Get(srv.URL + "/api/ecosystem")
	if err != nil {
		t.Fatal(err)
	}
	var eco struct {
		OrgHotfixes []orgHotfixJSON `json:"org_hotfixes"`
		Promotions  []featureJSON   `json:"promotions"`
	}
	if err := json.NewDecoder(res.Body).Decode(&eco); err != nil {
		t.Fatal(err)
	}
	foundCell := false
	for _, row := range eco.OrgHotfixes {
		for _, c := range row.Repos {
			if c.Has && c.Ahead != nil && *c.Ahead == 2 && c.AuthorAvatar != "" {
				foundCell = true
			}
		}
	}
	if !foundCell {
		t.Fatalf("no org hotfix cell carries divergence+avatar: %+v", eco.OrgHotfixes)
	}
	foundFeat := false
	for _, f := range eco.Promotions {
		for _, sv := range f.Services {
			if sv.State != "main" && sv.Ahead != nil && sv.AuthorAvatar != "" {
				foundFeat = true
			}
		}
	}
	if !foundFeat {
		t.Fatalf("no feature cell carries divergence+avatar: %+v", eco.Promotions)
	}
}
