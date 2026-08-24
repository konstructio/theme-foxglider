// fakegitlab is a dev-only canned GitLab API for local frontend work.
// Run: go run ./cmd/fakegitlab   then: GITLAB_HOST=http://localhost:9911 GITLAB_TOKEN=dev go run .
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
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

func main() {
	now := time.Now().UTC()
	projects := []map[string]any{
		proj(1, "konduit", "platform"), proj(2, "foxglider", "platform"),
		proj(3, "metropolis", "themes"), proj(4, "kontrol-room", "themes"),
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
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v4/projects/"), "/")
		id, _ := strconv.Atoi(parts[0])
		w.Header().Set("Content-Type", "application/json")
		switch {
		case len(parts) == 2 && parts[1] == "pipelines":
			json.NewEncoder(w).Encode(pipelines[id])
		case len(parts) == 2 && parts[1] == "events":
			json.NewEncoder(w).Encode(events(id, now))
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
	log.Println("fakegitlab on :9911")
	log.Fatal(http.ListenAndServe(":9911", mux))
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
		"web_url":             "https://gitlab.example.com/" + group + "/" + name,
		"default_branch":      "main", "namespace": map[string]any{"full_path": group}}
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
