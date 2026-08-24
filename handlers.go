package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	ttlProjects  = 5 * time.Minute
	ttlPipelines = 30 * time.Second
	ttlEvents    = 60 * time.Second
	fanout       = 8 // concurrent GitLab calls
	histLimit    = 20
)

type api struct {
	gl     *glClient
	groups []string // namespace prefixes; empty = all
	c      *cache
}

func newAPI(gl *glClient, groups []string) http.Handler {
	a := &api{gl: gl, groups: groups, c: newCache()}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/overview", a.guard(a.overview))
	mux.HandleFunc("GET /api/projects/{id}/pipelines", a.guard(a.projectPipelines))
	mux.HandleFunc("GET /api/pipelines/{pid}/{plid}", a.guard(a.pipelineDetail))
	mux.HandleFunc("GET /api/activity", a.guard(a.activity))
	return mux
}

// guard rejects everything with an honest 503 when no token is configured.
func (a *api) guard(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a.gl.token == "" {
			writeErr(w, 503, "GITLAB_TOKEN not configured — not connected to GitLab")
			return
		}
		h(w, r)
	}
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func (a *api) inScope(nsPath string) bool {
	if len(a.groups) == 0 {
		return true
	}
	for _, g := range a.groups {
		if nsPath == g || strings.HasPrefix(nsPath, g+"/") {
			return true
		}
	}
	return false
}

func (a *api) cachedProjects(ctx context.Context) ([]glProject, error) {
	v, err := a.c.do("projects", ttlProjects, func() (any, error) {
		ps, err := a.gl.projects(ctx)
		if err != nil {
			return nil, err
		}
		var in []glProject
		for _, p := range ps {
			if a.inScope(p.Namespace.FullPath) {
				in = append(in, p)
			}
		}
		return in, nil
	})
	if err != nil {
		return nil, err
	}
	return v.([]glProject), nil
}

func (a *api) cachedPipelines(ctx context.Context, projectID int) ([]glPipeline, error) {
	v, err := a.c.do(fmt.Sprintf("pl:%d", projectID), ttlPipelines, func() (any, error) {
		return a.gl.pipelines(ctx, projectID, histLimit)
	})
	if err != nil {
		return nil, err
	}
	return v.([]glPipeline), nil
}

type pipelineJSON struct {
	ID        int       `json:"id"`
	Status    string    `json:"status"`
	Ref       string    `json:"ref"`
	SHA       string    `json:"sha"`
	ShortSHA  string    `json:"short_sha"`
	WebURL    string    `json:"web_url"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	DurationS float64   `json:"duration_s"`
}

func toPipelineJSON(p glPipeline) pipelineJSON {
	short := p.SHA
	if len(short) > 8 {
		short = short[:8]
	}
	return pipelineJSON{
		ID: p.ID, Status: p.Status, Ref: p.Ref, SHA: p.SHA, ShortSHA: short,
		WebURL: p.WebURL, CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt,
		DurationS: p.UpdatedAt.Sub(p.CreatedAt).Seconds(),
	}
}

type projectJSON struct {
	ID            int            `json:"id"`
	Name          string         `json:"name"`
	Path          string         `json:"path"`
	WebURL        string         `json:"web_url"`
	DefaultBranch string         `json:"default_branch"`
	Pipelines     []pipelineJSON `json:"pipelines"`
}

type groupJSON struct {
	Path     string        `json:"path"`
	Projects []projectJSON `json:"projects"`
}

func (a *api) overview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	projects, err := a.cachedProjects(ctx)
	if err != nil {
		writeErr(w, 503, "GitLab unreachable: "+err.Error())
		return
	}

	sem := make(chan struct{}, fanout)
	var mu sync.Mutex
	var wg sync.WaitGroup
	byGroup := map[string][]projectJSON{}
	for _, p := range projects {
		wg.Add(1)
		go func(p glProject) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			pls, err := a.cachedPipelines(ctx, p.ID)
			if err != nil {
				pls = nil // project row still renders; empty timeline is honest
			}
			pj := projectJSON{ID: p.ID, Name: p.Name, Path: p.PathWithNamespace,
				WebURL: p.WebURL, DefaultBranch: p.DefaultBranch, Pipelines: []pipelineJSON{}}
			for _, pl := range pls {
				pj.Pipelines = append(pj.Pipelines, toPipelineJSON(pl))
			}
			mu.Lock()
			byGroup[p.Namespace.FullPath] = append(byGroup[p.Namespace.FullPath], pj)
			mu.Unlock()
		}(p)
	}
	wg.Wait()

	var groups []groupJSON
	for path, ps := range byGroup {
		sort.Slice(ps, func(i, j int) bool { return ps[i].Path < ps[j].Path })
		groups = append(groups, groupJSON{Path: path, Projects: ps})
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i].Path < groups[j].Path })
	writeJSON(w, map[string]any{"generated_at": time.Now().UTC(), "groups": groups})
}

func (a *api) projectPipelines(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		writeErr(w, 400, "bad project id")
		return
	}
	pls, err := a.cachedPipelines(r.Context(), id)
	if err != nil {
		writeErr(w, 503, "GitLab unreachable: "+err.Error())
		return
	}
	out := make([]pipelineJSON, 0, len(pls))
	for _, p := range pls {
		out = append(out, toPipelineJSON(p))
	}
	writeJSON(w, map[string]any{"pipelines": out})
}

type jobJSON struct {
	Name       string     `json:"name"`
	Stage      string     `json:"stage"`
	Status     string     `json:"status"`
	StartedAt  *time.Time `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at"`
	DurationS  float64    `json:"duration_s"`
	WebURL     string     `json:"web_url"`
}

func (a *api) pipelineDetail(w http.ResponseWriter, r *http.Request) {
	pid, err1 := strconv.Atoi(r.PathValue("pid"))
	plid, err2 := strconv.Atoi(r.PathValue("plid"))
	if err1 != nil || err2 != nil {
		writeErr(w, 400, "bad pipeline path")
		return
	}
	key := fmt.Sprintf("detail:%d:%d", pid, plid)
	v, err := a.c.do(key, ttlPipelines, func() (any, error) {
		d, err := a.gl.pipeline(r.Context(), pid, plid)
		if err != nil {
			return nil, err
		}
		jobs, err := a.gl.jobs(r.Context(), pid, plid)
		if err != nil {
			return nil, err
		}
		jj := make([]jobJSON, 0, len(jobs))
		for _, j := range jobs {
			jj = append(jj, jobJSON{Name: j.Name, Stage: j.Stage, Status: j.Status,
				StartedAt: j.StartedAt, FinishedAt: j.FinishedAt, DurationS: j.Duration, WebURL: j.WebURL})
		}
		return map[string]any{
			"id": d.ID, "status": d.Status, "ref": d.Ref, "sha": d.SHA,
			"web_url": d.WebURL, "created_at": d.CreatedAt,
			"started_at": d.StartedAt, "finished_at": d.FinishedAt,
			"duration_s": d.Duration, "jobs": jj,
		}, nil
	})
	if err != nil {
		writeErr(w, 503, "GitLab unreachable: "+err.Error())
		return
	}
	writeJSON(w, v)
}

type activityItem struct {
	Type    string    `json:"type"`
	Project string    `json:"project"`
	Title   string    `json:"title"`
	Status  string    `json:"status,omitempty"`
	Author  string    `json:"author,omitempty"`
	WebURL  string    `json:"web_url"`
	At      time.Time `json:"at"`
}

func eventItem(p glProject, e glEvent) activityItem {
	it := activityItem{Project: p.PathWithNamespace, Author: e.Author.Username, At: e.CreatedAt}
	switch {
	case e.PushData != nil:
		it.Type = "push"
		it.Title = fmt.Sprintf("%s (%d commit(s) to %s)", e.PushData.CommitTitle, e.PushData.CommitCount, e.PushData.Ref)
		it.WebURL = p.WebURL + "/-/commits/" + e.PushData.Ref
	case e.TargetType == "MergeRequest":
		it.Type = "merge_request"
		it.Title = e.ActionName + ": " + e.TargetTitle
		it.WebURL = fmt.Sprintf("%s/-/merge_requests/%d", p.WebURL, e.TargetIID)
	case e.TargetType == "Issue":
		it.Type = "issue"
		it.Title = e.ActionName + ": " + e.TargetTitle
		it.WebURL = fmt.Sprintf("%s/-/issues/%d", p.WebURL, e.TargetIID)
	case e.Note != nil:
		it.Type = "comment"
		it.Title = "commented: " + e.TargetTitle
		switch e.Note.NoteableType {
		case "MergeRequest":
			it.WebURL = fmt.Sprintf("%s/-/merge_requests/%d", p.WebURL, e.Note.NoteableIID)
		case "Issue":
			it.WebURL = fmt.Sprintf("%s/-/issues/%d", p.WebURL, e.Note.NoteableIID)
		default:
			it.WebURL = p.WebURL + "/activity"
		}
	default:
		it.Type = "comment" // note-like fallback
		it.Title = e.ActionName + ": " + e.TargetTitle
		it.WebURL = p.WebURL + "/activity"
	}
	return it
}

func (a *api) activity(w http.ResponseWriter, r *http.Request) {
	hours, _ := strconv.Atoi(r.URL.Query().Get("hours"))
	if hours < 1 {
		hours = 24
	}
	if hours > 168 {
		hours = 168
	}
	cutoff := time.Now().Add(-time.Duration(hours) * time.Hour)

	ctx := r.Context()
	projects, err := a.cachedProjects(ctx)
	if err != nil {
		writeErr(w, 503, "GitLab unreachable: "+err.Error())
		return
	}

	sem := make(chan struct{}, fanout)
	var mu sync.Mutex
	var wg sync.WaitGroup
	var items []activityItem
	for _, p := range projects {
		wg.Add(1)
		go func(p glProject) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			var local []activityItem
			evs, err := a.c.do(fmt.Sprintf("ev:%d", p.ID), ttlEvents, func() (any, error) {
				return a.gl.events(ctx, p.ID, 50)
			})
			if err == nil {
				for _, e := range evs.([]glEvent) {
					if e.CreatedAt.After(cutoff) {
						local = append(local, eventItem(p, e))
					}
				}
			}
			if pls, err := a.cachedPipelines(ctx, p.ID); err == nil {
				for _, pl := range pls {
					if pl.CreatedAt.After(cutoff) {
						short := pl.SHA
						if len(short) > 8 {
							short = short[:8]
						}
						local = append(local, activityItem{Type: "pipeline",
							Project: p.PathWithNamespace, Title: pl.Ref + " · " + short,
							Status: pl.Status, WebURL: pl.WebURL, At: pl.CreatedAt})
					}
				}
			}
			mu.Lock()
			items = append(items, local...)
			mu.Unlock()
		}(p)
	}
	wg.Wait()
	sort.Slice(items, func(i, j int) bool { return items[i].At.After(items[j].At) })
	if items == nil {
		items = []activityItem{}
	}
	writeJSON(w, map[string]any{"items": items})
}
