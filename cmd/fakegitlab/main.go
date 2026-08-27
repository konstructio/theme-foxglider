// fakegitlab is a dev-only canned GitLab API for local frontend work.
// Run: go run ./cmd/fakegitlab   then: GITLAB_HOST=http://localhost:9911 GITLAB_TOKEN=dev go run .
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

type pl struct {
	ID        int    `json:"id"`
	Status    string `json:"status"`
	Ref       string `json:"ref"`
	SHA       string `json:"sha"`
	WebURL    string `json:"web_url"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

var statuses = []string{"success", "success", "failed", "success", "canceled", "success", "running"}

// bootStarted anchors the demo's running job: a fixed instant ~45s before the
// fake booted, so client-side elapsed/progress advances in real time.
var bootStarted = time.Now().UTC().Add(-45 * time.Second).Format(time.RFC3339)

// deletedBranches tracks branch deletes so the list stays honest mid-demo.
var delMu sync.Mutex
var deletedBranches = map[string]bool{}

func main() {
	now := time.Now().UTC()
	projects := []map[string]any{
		proj(1, "metaphor", "civo/metaphor"), proj(2, "metaphor-dashboard-manager", "civo/metaphor"),
		proj(3, "metaphor-micro-frontend", "civo/metaphor"), proj(4, "charts", "civo/metaphor"),
		proj(5, "metaphor-gitops", "civo/metaphor"), proj(6, "metaphor-operator", "civo/metaphor"),
	}
	pipelines := map[int][]pl{}
	for _, p := range projects {
		id := p["id"].(int)
		var list []pl
		for i := 0; i < 12; i++ {
			st := statuses[(id+i)%len(statuses)]
			if i > 0 && st == "running" {
				st = "pending"
			} // only newest can still run
			if id == 1 && i == 0 {
				st = "running" // keep one live pipeline so the pulse/running paths always render
			}
			start := now.Add(-time.Duration(i*2) * time.Hour)
			dur := time.Duration(90+30*((id+i)%5)) * time.Second
			list = append(list, pl{ID: id*100 + i, Status: st, Ref: "main",
				SHA: fmt.Sprintf("%08x%032x", id*100+i, i), WebURL: fmt.Sprintf("%s/-/pipelines/%d", p["web_url"], id*100+i),
				CreatedAt: start.Format(time.RFC3339), UpdatedAt: start.Add(dur).Format(time.RFC3339)})
		}
		pipelines[id] = list
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/projects", jsonH(func(r *http.Request) any { return projects }))
	mux.HandleFunc("/api/v4/projects/", func(w http.ResponseWriter, r *http.Request) {
		// Path-addressed projects (civo%2Fmetaphor%2F…) are the delivery
		// ecosystem's calls; %2F survives only in EscapedPath.
		if strings.Contains(r.URL.EscapedPath(), "%2F") {
			ecoFake(w, r)
			return
		}
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v4/projects/"), "/")
		id, _ := strconv.Atoi(parts[0])
		w.Header().Set("Content-Type", "application/json")
		switch {
		case len(parts) == 2 && parts[1] == "pipelines":
			json.NewEncoder(w).Encode(pipelines[id])
		case len(parts) == 2 && parts[1] == "events":
			json.NewEncoder(w).Encode(events(id, now))
		case len(parts) == 2 && parts[1] == "releases":
			if id == 4 { // charts: never released
				fmt.Fprint(w, `[]`)
				return
			}
			days := []int{0, 3, 11, 47, 0, 92, 8}[id%7]
			fmt.Fprintf(w, `[{"tag_name":"v0.1%d.0","name":"v0.1%d.0","released_at":%q,"_links":{"self":"https://git.civo.com/x/-/releases/v0.1%d.0"}}]`,
				id, id, now.Add(-time.Duration(days*24)*time.Hour).Format(time.RFC3339), id)
		case len(parts) == 3 && parts[1] == "pipelines":
			plid, _ := strconv.Atoi(parts[2])
			json.NewEncoder(w).Encode(detail(pipelines[id], plid))
		case len(parts) == 4 && parts[1] == "pipelines" && parts[3] == "jobs":
			plid, _ := strconv.Atoi(parts[2])
			json.NewEncoder(w).Encode(jobs(detail(pipelines[id], plid)))
		default:
			w.WriteHeader(404)
		}
	})
	// group-scoped calls (the acting-as roster) route through ecoFake too
	mux.HandleFunc("/api/v4/groups/", ecoFake)
	mux.HandleFunc("/api/v4/groups/civo%2Fmetaphor", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"name":"Metaphor","full_path":"civo/metaphor","avatar_url":"http://localhost:9911/grouplogo.png","web_url":"https://git.civo.com/groups/civo/metaphor"}`)
	})
	mux.HandleFunc("/api/v4/groups/civo%2Fmetaphor/avatar", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		fmt.Fprint(w, "\x89PNG\r\n\x1a\n") // sniffable magic — the proxy detects image/png
	})
	mux.HandleFunc("/grouplogo.png", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/svg+xml")
		fmt.Fprint(w, `<svg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 16 16'><rect width='16' height='16' rx='3' fill='#f97316'/><text x='8' y='12' font-size='10' text-anchor='middle' fill='#fff'>M</text></svg>`)
	})
	mux.HandleFunc("/api/v4/avatar", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.RawQuery, "jared") {
			fmt.Fprint(w, `{"avatar_url":""}`) // exercises the initials fallback
			return
		}
		fmt.Fprint(w, `{"avatar_url":"data:image/svg+xml,%3Csvg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 16 16'%3E%3Ccircle cx='8' cy='8' r='8' fill='%2334d399'/%3E%3C/svg%3E"}`)
	})
	log.Println("fakegitlab on :9911")
	log.Fatal(http.ListenAndServe(":9911", mux))
}

// ecoProjFromPath decodes the "civo/metaphor/<repo>" project from an
// encoded /api/v4/projects/<enc>/... path.
func ecoProjFromPath(p string) string {
	proj := "civo/metaphor/charts"
	if i := strings.Index(p, "/projects/"); i >= 0 {
		seg := p[i+len("/projects/"):]
		if j := strings.Index(seg, "/"); j >= 0 {
			seg = seg[:j]
		}
		if d := strings.ReplaceAll(seg, "%2F", "/"); d != "" {
			proj = d
		}
	}
	return proj
}

// fakeCommitter maps a project to a canned committer + a ui-avatars image so the
// Delivery cards show distinct faces in dev.
func fakeCommitter(proj string) (name, avatar string) {
	// dev-only placeholders; production uses the real GitLab user per pipeline.
	people := map[string][2]string{
		"civo/metaphor/metaphor":                   {"John Dietz", "05df72"},
		"civo/metaphor/metaphor-dashboard-manager": {"Jared Edwards", "fca326"},
		"civo/metaphor/metaphor-micro-frontend":    {"kbot", "8b5cf6"},
		"civo/metaphor/charts":                     {"metaphor ci", "00bcd4"},
	}
	pr, ok := people[proj]
	if !ok {
		pr = [2]string{"John Dietz", "05df72"}
	}
	name = pr[0]
	avatar = "https://ui-avatars.com/api/?background=" + pr[1] + "&color=0b1220&bold=true&size=48&name=" + strings.ReplaceAll(name, " ", "+")
	return
}

// ecoFake serves canned metaphor supply-chain data (Chart.yaml versions, macro
// deps, tags, delivery targetRevision, per-repo pipelines) so the Delivery tab
// renders in local dev. Routes on the URL-encoded project/file path.
func ecoFake(w http.ResponseWriter, r *http.Request) {
	p := r.URL.EscapedPath()
	switch {
	case strings.Contains(p, "metaphor-macro%2FChart.yaml") && strings.Contains(r.URL.RawQuery, "epic-101-aurora"):
		fmt.Fprint(w, "version: 0.2.0\ndependencies:\n  - name: metaphor\n    version: \"0.11.0-rc.13\"\n  - name: metaphor-dashboard-manager\n    version: \"0.12.0-epic-101-aurora.2\"\n  - name: metaphor-micro-frontend\n    version: \"0.1.0-rc.7\"\n")
	case strings.Contains(p, "metaphor-macro%2FChart.yaml") && strings.Contains(r.URL.RawQuery, "epic-showrishi"):
		fmt.Fprint(w, "version: 0.2.0\ndependencies:\n  - name: metaphor\n    version: \"0.11.0-rc.13\"\n  - name: metaphor-dashboard-manager\n    version: \"0.12.0-rc.15\"\n  - name: metaphor-micro-frontend\n    version: \"0.1.0-epic-showrishi.1\"\n")
	case strings.Contains(p, "metaphor-macro%2FChart.yaml"):
		if strings.Contains(r.URL.RawQuery, "rc.2") {
			fmt.Fprint(w, "version: 0.2.0\ndependencies:\n  - name: metaphor\n    version: \"0.11.0-rc.10\"\n  - name: metaphor-dashboard-manager\n    version: \"0.12.0-rc.15\"\n  - name: metaphor-micro-frontend\n    version: \"0.1.0-rc.7\"\n")
			return
		}
		fmt.Fprint(w, "apiVersion: v2\nname: metaphor-macro\nversion: 0.2.0\nappVersion: \"0.1.0\"\ndependencies:\n  - name: metaphor\n    version: \"0.11.0-rc.13\"\n  - name: metaphor-dashboard-manager\n    version: \"0.12.0-rc.15\"\n  - name: metaphor-micro-frontend\n    version: \"0.1.0-rc.7\"\n")
	case strings.Contains(p, "charts%2Fmetaphor%2FChart.yaml"):
		fmt.Fprint(w, "name: metaphor\nversion: 0.11.0\n")
	case strings.Contains(p, "metaphor-dashboard-manager%2FChart.yaml"):
		fmt.Fprint(w, "name: metaphor-dashboard-manager\nversion: 0.12.0\n")
	case strings.Contains(p, "metaphor-micro-frontend%2FChart.yaml"):
		fmt.Fprint(w, "name: metaphor-micro-frontend\nversion: 0.1.0\n")
	case strings.Contains(p, "metaphor-macro.yaml"): // delivery Application
		fmt.Fprint(w, "spec:\n  source:\n    targetRevision: 0.2.0-rc.2\n")
	case strings.HasSuffix(p, "/repository/tags"):
		w.Header().Set("Content-Type", "application/json")
		now3 := time.Now().UTC()
		fmt.Fprintf(w, `[{"name":"metaphor-v0.2.0-epic-101-aurora.2","commit":{"created_at":%q}},{"name":"metaphor-v0.2.0-rc.4","commit":{"created_at":%q}},{"name":"metaphor-v0.2.0-rc.3","commit":{"created_at":%q}},{"name":"metaphor-v0.2.0-rc.2","commit":{"created_at":%q}}]`,
			now3.Add(-2*time.Hour).Format(time.RFC3339), now3.Add(-10*time.Minute).Format(time.RFC3339), now3.Add(-20*time.Minute).Format(time.RFC3339), now3.Add(-30*time.Minute).Format(time.RFC3339))
	case (strings.Contains(p, "%2Fepics%2F") || strings.Contains(p, "/epics/")) && r.Method == "PUT":
		fmt.Fprint(w, `{"iid":101,"state":"closed"}`)
	case strings.Contains(p, "%2Fepics%2F"), strings.Contains(p, "/epics/"):
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"iid":101,"title":"Redesign opening screen with aurora green background","state":"opened","web_url":"https://git.civo.com/groups/civo/metaphor/-/epics/101"}`)
	case strings.HasSuffix(p, "/epics"):
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `[{"iid":101,"title":"Redesign opening screen with aurora green background","state":"opened","web_url":"https://git.civo.com/groups/civo/metaphor/-/epics/101"},{"iid":20,"title":"Turn metaphor pink","state":"opened","web_url":"https://git.civo.com/groups/civo/metaphor/-/epics/20"}]`)
	case strings.Contains(p, "/repository/branches/") && r.Method == "DELETE":
		name := p[strings.Index(p, "/repository/branches/")+len("/repository/branches/"):]
		if u, err := url.PathUnescape(name); err == nil {
			name = u
		}
		delMu.Lock()
		deletedBranches[name] = true
		delMu.Unlock()
		w.WriteHeader(204)
	case strings.Contains(p, "/repository/compare"):
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.RawQuery, "hotfix-done") {
			fmt.Fprint(w, `{"commits":[]}`) // fully merged back
			return
		}
		fmt.Fprint(w, `{"commits":[{"id":"a","author_name":"Jared Edwards","author_email":"jared@civo.com"},{"id":"b","author_name":"John Dietz","author_email":"john.dietz@civo.com"},{"id":"c","author_name":"John Dietz","author_email":"john.dietz@civo.com"}]}`)
	case strings.HasSuffix(p, "/repository/branches") && r.Method == "POST":
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"name":"epic-20-pink","web_url":"https://git.civo.com/x/-/tree/epic-20-pink"}`)
	case strings.HasSuffix(p, "/repository/branches"):
		w.Header().Set("Content-Type", "application/json")
		now2 := time.Now().UTC()
		type fb struct {
			name, sha, title, author string
			when                     time.Time
		}
		all := []fb{
			{"main", "feedface", "feat: latest", "John Dietz", now2.Add(-30 * time.Minute)},
			{"epic-101-aurora", "abc12345", "ci: bump dep", "kbot", now2.Add(-2 * time.Hour)},
			{"hotfix/0.2", "def67890", "fix: patch", "Jared Edwards", now2.Add(-26 * time.Hour)},
			{"hotfix-done", "beefcafe", "fix: already merged", "John Dietz", now2.Add(-3 * time.Hour)},
			{"epic-7-legacy", "old00001", "wip: abandoned spike", "John Dietz", now2.Add(-45 * 24 * time.Hour)},
		}
		// epic-showrishi joins only the micro-frontend + charts — the feature
		// view's "one service updated, others from main" demo
		if strings.Contains(p, "micro-frontend") || strings.Contains(p, "%2Fcharts") {
			all = append(all, fb{"epic-showrishi", "aa11bb22", "wip: show rishi", "John Dietz", now2.Add(-40 * time.Minute)})
		}
		delMu.Lock()
		items := []string{}
		for _, b := range all {
			if deletedBranches[b.name] {
				continue
			}
			items = append(items, fmt.Sprintf(`{"name":%q,"web_url":"https://git.civo.com/x/-/tree/%s","commit":{"short_id":%q,"title":%q,"author_name":%q,"committed_date":%q}}`,
				b.name, strings.ReplaceAll(b.name, "/", "-"), b.sha, b.title, b.author, b.when.Format(time.RFC3339)))
		}
		delMu.Unlock()
		fmt.Fprint(w, "["+strings.Join(items, ",")+"]")
	case strings.HasSuffix(p, "/members/all"):
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `[{"username":"john.dietz","name":"John Dietz","access_level":50},{"username":"jared","name":"Jared Edwards","access_level":50},{"username":"group_1642_bot_x","name":"token bot","access_level":40}]`)
	case strings.Contains(p, "/merge_requests") && strings.Contains(p, "/repository/commits/"):
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `[{"iid":7,"title":"feat: aurora background","source_branch":"epic-101-aurora","web_url":"https://git.civo.com/x/-/merge_requests/7"}]`)
	case strings.HasSuffix(p, "/repository/commits"):
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `[{"id":"c1","short_id":"c1short","title":"feat: aurora background (epic-101)","author_name":"John Dietz","web_url":"https://git.civo.com/x/-/commit/c1","authored_date":%q}]`, time.Now().UTC().Add(-40*time.Minute).Format(time.RFC3339))
	case strings.Contains(p, "%2Frepository%2Fcommits%2F"), strings.Contains(p, "/repository/commits/"):
		w.Header().Set("Content-Type", "application/json")
		proj := ecoProjFromPath(p)
		fmt.Fprintf(w, `{"id":"feedfacecafe0000deadbeef","short_id":"feedface","title":"feat: sharpen the delivery story on the tiles","web_url":"https://git.civo.com/%s/-/commit/feedfacecafe0000","author_name":"John Dietz","authored_date":%q}`,
			proj, time.Now().UTC().Add(-25*time.Minute).Format(time.RFC3339))
	case strings.HasSuffix(p, "/pipeline"): // deliver: create pipeline on a tag
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":701,"status":"created","ref":"metaphor-v0.2.0-rc.4","sha":"feedfacecafe0000","web_url":"https://git.civo.com/civo/metaphor/charts/-/pipelines/701","created_at":"2026-08-26T00:00:00Z","updated_at":"2026-08-26T00:00:00Z"}`)
	case strings.HasSuffix(p, "/releases") && r.Method == "GET":
		if strings.Contains(p, "%2Fcharts") {
			fmt.Fprintf(w, `[{"tag_name":"metaphor-v0.1.0","name":"metaphor-v0.1.0","released_at":%q,"_links":{"self":"https://git.civo.com/civo/metaphor/charts/-/releases/metaphor-v0.1.0"}}]`, time.Now().UTC().Add(-24*time.Hour).Format(time.RFC3339))
			return
		}
		fmt.Fprintf(w, `[{"tag_name":"v0.11.0","name":"v0.11.0","released_at":%q,"_links":{"self":"https://git.civo.com/x/-/releases/v0.11.0"}}]`, time.Now().UTC().Add(-3*24*time.Hour).Format(time.RFC3339))
	case strings.HasSuffix(p, "/merge_requests") && r.Method == "POST":
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		fmt.Fprintf(w, `{"iid":88,"title":%q,"state":"opened","web_url":"https://git.civo.com/x/-/merge_requests/88"}`, body["title"])
	case strings.HasSuffix(p, "/merge_requests") && r.Method == "GET" && r.URL.Query().Get("source_branch") != "":
		if r.URL.Query().Get("source_branch") == "epic-101-aurora" {
			fmt.Fprintf(w, `[{"iid":12,"title":"feat: aurora","state":"merged","merged_at":%q,"source_branch":"epic-101-aurora","web_url":"https://git.civo.com/x/-/merge_requests/12"}]`,
				time.Now().UTC().Add(-1*time.Hour).Format(time.RFC3339))
			return
		}
		if r.URL.Query().Get("source_branch") == "epic-showrishi" {
			fmt.Fprint(w, `[{"iid":9,"title":"feat: show rishi","state":"opened","source_branch":"epic-showrishi","web_url":"https://git.civo.com/x/-/merge_requests/9"}]`)
			return
		}
		fmt.Fprint(w, `[]`)
	case strings.HasSuffix(p, "/merge_requests") && r.Method == "GET":
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `[{"iid":55,"title":"chore: bump metaphor-macro to 0.2.0-rc.4 (release_preview)","state":"opened","web_url":"https://git.civo.com/civo/metaphor/metaphor-gitops/-/merge_requests/55"}]`)
	case strings.Contains(p, "/merge_requests/9") && r.Method == "GET":
		fmt.Fprint(w, `{"iid":9,"title":"feat: show rishi","state":"opened","source_branch":"epic-showrishi","web_url":"https://git.civo.com/x/-/merge_requests/9"}`)
	case strings.HasSuffix(p, "/approve"), strings.HasSuffix(p, "/merge"):
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"iid":55,"state":"merged"}`)
	case strings.Contains(p, "/play"):
		// answers like GitLab so the action modal flow completes in dev
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":7,"name":"trigger:manual","status":"pending","web_url":"https://git.civo.com/civo/metaphor/charts/-/jobs/7","pipeline":{"id":900,"web_url":"https://git.civo.com/civo/metaphor/charts/-/pipelines/900"}}`)
	case strings.Contains(p, "/pipelines/899/jobs"):
		// the previous successful run — the progress baseline (all durations)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `[{"id":11,"name":"lint","stage":"validate","status":"success","duration":9.1},{"id":12,"name":"test","stage":"validate","status":"success","duration":26.0},{"id":13,"name":"build","stage":"build","status":"success","duration":95.0},{"id":14,"name":"publish-chart","stage":"publish","status":"success","duration":31.0},{"id":15,"name":"publish-image","stage":"publish","status":"success","duration":44.0}]`)
	case strings.HasSuffix(p, "/jobs"):
		// canned running pipeline: earlier stages done, build running, publish
		// queued — plus the two playable manual jobs the action layer targets.
		// started is FIXED at process boot so elapsed grows like real GitLab.
		w.Header().Set("Content-Type", "application/json")
		started := bootStarted
		fmt.Fprintf(w, `[{"id":1,"name":"lint","stage":"validate","status":"success","duration":9.4},{"id":2,"name":"test","stage":"validate","status":"success","duration":24.8},{"id":3,"name":"build","stage":"build","status":"running","started_at":%q},{"id":4,"name":"publish-chart","stage":"publish","status":"created"},{"id":5,"name":"publish-image","stage":"publish","status":"created"},{"id":6,"name":"release","stage":"release","status":"manual"},{"id":7,"name":"trigger:manual","stage":"toolbelt","status":"manual"}]`, started)
	case strings.HasSuffix(p, "/pipelines/latest"):
		w.Header().Set("Content-Type", "application/json")
		proj := ecoProjFromPath(p)
		st := "success"
		if strings.Contains(p, "micro-frontend") {
			st = "running"
		}
		name, avatar := fakeCommitter(proj)
		now := time.Now().UTC()
		web := "https://git.civo.com/" + proj + "/-/pipelines"
		fmt.Fprintf(w, `{"id":900,"status":%q,"ref":"main","sha":"feedfacecafe0000","web_url":%q,"created_at":%q,"updated_at":%q,"user":{"name":%q,"username":"dev","avatar_url":%q,"web_url":"https://git.civo.com/dev"}}`,
			st, web, now.Add(-20*time.Minute).Format(time.RFC3339), now.Add(-18*time.Minute).Format(time.RFC3339), name, avatar)
	case strings.HasSuffix(p, "/pipelines"):
		w.Header().Set("Content-Type", "application/json")
		proj := ecoProjFromPath(p)
		st := "success"
		if strings.Contains(p, "micro-frontend") {
			st = "running"
		}
		now := time.Now().UTC()
		web := "https://git.civo.com/" + proj + "/-/pipelines"
		if rf := r.URL.Query().Get("ref"); rf != "" && rf != "main" {
			// branch pipelines: epic branches run, hotfix/0.2 has an old red one
			st2, when := "running", time.Now().UTC().Add(-3*time.Minute)
			if strings.HasPrefix(rf, "hotfix") {
				st2, when = "failed", time.Now().UTC().Add(-2*time.Hour)
			}
			fmt.Fprintf(w, `[{"id":950,"status":%q,"ref":%q,"web_url":%q,"created_at":%q,"updated_at":%q}]`,
				st2, rf, web+"/950", when.Format(time.RFC3339), when.Add(90*time.Second).Format(time.RFC3339))
			return
		}
		if r.URL.Query().Get("status") == "success" {
			fmt.Fprintf(w, `[{"id":899,"status":"success","ref":"main","sha":"0ldfacecafe","web_url":%q,"created_at":%q,"updated_at":%q}]`,
				web, now.Add(-3*time.Hour).Format(time.RFC3339), now.Add(-3*time.Hour+2*time.Minute).Format(time.RFC3339))
			return
		}
		if r.URL.Query().Get("sha") != "" {
			// the SHA's full story: branch run + its RC tag run
			fmt.Fprintf(w, `[{"id":900,"status":%q,"ref":"main","source":"push","sha":"feedfacecafe0000","web_url":%q,"created_at":%q,"updated_at":%q},{"id":901,"status":"success","ref":"metaphor-v0.2.0-rc.4","source":"push","sha":"feedfacecafe0000","web_url":%q,"created_at":%q,"updated_at":%q}]`,
				st, web, now.Add(-20*time.Minute).Format(time.RFC3339), now.Add(-18*time.Minute).Format(time.RFC3339),
				web, now.Add(-16*time.Minute).Format(time.RFC3339), now.Add(-14*time.Minute).Format(time.RFC3339))
			return
		}
		fmt.Fprintf(w, `[{"id":900,"status":%q,"ref":"main","sha":"feedfacecafe0000","web_url":%q,"created_at":%q,"updated_at":%q}]`,
			st, web, now.Add(-20*time.Minute).Format(time.RFC3339), now.Add(-18*time.Minute).Format(time.RFC3339))
	default:
		w.WriteHeader(404)
	}
}

func jsonH(f func(*http.Request) any) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(f(r))
	}
}

func proj(id int, name, group string) map[string]any {
	return map[string]any{"id": id, "name": name,
		"path_with_namespace": group + "/" + name,
		"web_url":             "https://git.civo.com/" + group + "/" + name,
		"default_branch":      "main", "namespace": map[string]any{"full_path": group},
		"last_activity_at": time.Now().UTC().Add(-time.Duration(id*7) * time.Hour).Format(time.RFC3339)}
}

func detail(list []pl, plid int) map[string]any {
	for _, p := range list {
		if p.ID == plid {
			c, _ := time.Parse(time.RFC3339, p.CreatedAt)
			u, _ := time.Parse(time.RFC3339, p.UpdatedAt)
			return map[string]any{"id": p.ID, "status": p.Status, "ref": p.Ref, "sha": p.SHA,
				"web_url": p.WebURL, "created_at": p.CreatedAt, "updated_at": p.UpdatedAt,
				"started_at": p.CreatedAt, "finished_at": p.UpdatedAt, "duration": u.Sub(c).Seconds()}
		}
	}
	return map[string]any{}
}

func jobs(d map[string]any) []map[string]any {
	started, _ := time.Parse(time.RFC3339, d["started_at"].(string))
	dur := d["duration"].(float64)
	web := strings.Replace(d["web_url"].(string), "pipelines", "jobs", 1)
	mk := func(name, stage, status string, off, frac float64) map[string]any {
		s := started.Add(time.Duration(off*dur) * time.Second)
		f := s.Add(time.Duration(frac*dur) * time.Second)
		return map[string]any{"name": name, "stage": stage, "status": status,
			"started_at": s.Format(time.RFC3339), "finished_at": f.Format(time.RFC3339),
			"duration": frac * dur, "web_url": web}
	}
	st := d["status"].(string)
	last := "success"
	if st == "failed" {
		last = "failed"
	}
	return []map[string]any{
		mk("lint", "check", "success", 0, .2), mk("test", "check", "success", 0, .45),
		mk("build", "build", "success", .45, .35), mk("deploy", "ship", last, .8, .2),
	}
}

func events(id int, now time.Time) []map[string]any {
	mk := func(minsAgo int, m map[string]any) map[string]any {
		m["created_at"] = now.Add(-time.Duration(minsAgo) * time.Minute).Format(time.RFC3339)
		m["author"] = map[string]any{"username": "john"}
		return m
	}
	return []map[string]any{
		mk(30*id, map[string]any{"action_name": "pushed to",
			"push_data": map[string]any{"ref": "main", "commit_title": "feat: sample change", "commit_count": 2}}),
		mk(60*id, map[string]any{"action_name": "opened", "target_type": "MergeRequest",
			"target_iid": 7, "target_title": "Add timeline polish"}),
		mk(90*id, map[string]any{"action_name": "commented on", "target_type": "Note",
			"target_title": "Add timeline polish",
			"note":         map[string]any{"noteable_type": "MergeRequest", "noteable_iid": 7}}),
	}
}
