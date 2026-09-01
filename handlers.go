package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
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
	topo   topology
	act    *actions
	// glDelivery, when set, reads ONLY the delivery app files (cross-group).
	glDelivery *glClient

	// hot marks projects with a just-fired action: their pipeline reads take a
	// 5s cache lane (instead of 45s) so the tile reflects the new commit/SHA
	// as soon as the trigger job pushes it.
	hotMu sync.Mutex
	hot   map[string]time.Time
	// lastSvc remembers each service's latest pipeline state so a completion
	// (running → success) can hot-lane the macro: the finished run's dep-bump
	// job is already pushing the next macro RC tag.
	lastSvc map[string]string
	// recentDel tombstones just-deleted branches: GitLab's branch list can
	// serve a stale copy for a few seconds after a DELETE, and re-caching
	// that would resurrect the branch on screen for a minute. We KNOW it's
	// gone (the DELETE returned 2xx) — renders skip it while upstream
	// catches up.
	recentDel map[string]time.Time
	// epicClosed: one-shot memory for the merge observer — an epic is closed
	// as Done exactly once per process (verified against live state first).
	epicClosed map[int]bool
	// chartsTwinned: one-shot memory for the charts-twin observer.
	chartsTwinned map[string]bool
	// clients: one GitLab client per (host, token_env) delivery target —
	// control planes live on different hosts with different credentials.
	clientMu sync.Mutex
	clients  map[string]*glClient
}

func newAPI(gl *glClient, groups []string) http.Handler {
	a := &api{gl: gl, groups: groups, c: newCache(), topo: loadTopology(), hot: map[string]time.Time{}, lastSvc: map[string]string{}, recentDel: map[string]time.Time{}, epicClosed: map[int]bool{}, chartsTwinned: map[string]bool{}, clients: map[string]*glClient{}}
	// Cross-group delivery files (e.g. konstruct's internal targetRevision in
	// civo/platform/civo-gitops) may need their own read credential.
	if tok := os.Getenv("GITLAB_TOKEN_DELIVERY"); tok != "" {
		a.glDelivery = newGLClient(gl.base, tok)
	}
	a.act = newActions(gl, a.topo, groups)
	a.act.markHot = a.markHot
	a.act.dropBranches = func(project string) { a.c.drop("br:" + project) }
	a.act.noteDeleted = a.noteBranchDeleted
	a.act.dropMRs = func(project, branch string) { a.c.drop("mrs:" + project + "@" + branch) }
	a.act.clientFor = a.clientFor
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/ecosystem", a.guard(a.ecosystem))
	mux.HandleFunc("GET /api/pipeline-progress", a.guard(a.pipelineProgress))
	mux.HandleFunc("GET /api/meta", a.meta)
	mux.HandleFunc("GET /api/bundle", a.guard(a.bundleAt))
	mux.HandleFunc("GET /api/org", a.guard(a.orgInfo))
	mux.HandleFunc("GET /api/org-logo", a.guard(a.orgLogo))
	mux.HandleFunc("GET /api/pipelines/{pid}/{plid}", a.guard(a.pipelineDetail))
	mux.HandleFunc("GET /api/branches", a.guard(a.branchesView))
	// Actions guard themselves on the separate write token, not the read token.
	mux.HandleFunc("GET /api/actions/status", a.act.status)
	mux.HandleFunc("GET /api/actions/epics", a.act.epicsList)
	mux.HandleFunc("GET /api/actions/upgrade-preview", a.act.upgradePreview)
	mux.HandleFunc("GET /api/actions/hotfix-preview", a.act.hotfixPreview)
	mux.HandleFunc("POST /api/actions/run", a.act.run)
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
	ID           int        `json:"id"`
	Status       string     `json:"status"`
	Ref          string     `json:"ref"`
	SHA          string     `json:"sha"`
	ShortSHA     string     `json:"short_sha"`
	WebURL       string     `json:"web_url"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
	DurationS    float64    `json:"duration_s"`
	FinishedAt   *time.Time `json:"finished_at,omitempty"`
	AuthorName   string     `json:"author_name,omitempty"`
	AuthorAvatar string     `json:"author_avatar,omitempty"`
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

type releaseJSON struct {
	Tag        string    `json:"tag"`
	Name       string    `json:"name,omitempty"`
	ReleasedAt time.Time `json:"released_at"`
	WebURL     string    `json:"web_url,omitempty"`
	DaysAgo    int       `json:"days_ago"`
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
		ctx := context.Background()
		d, err := a.gl.pipeline(ctx, pid, plid)
		if err != nil {
			return nil, err
		}
		jobs, err := a.gl.jobs(ctx, pid, plid)
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

// orgGroup resolves which group represents this org: the first configured
// scope, else the macro project's parent group.
func (a *api) orgGroup() string {
	if len(a.groups) > 0 && a.groups[0] != "" {
		return a.groups[0]
	}
	p := a.topo.MacroProj
	if i := strings.LastIndex(p, "/"); i > 0 {
		return p[:i]
	}
	return p
}

// orgInfo surfaces the group's identity; the avatar is offered as a proxied
// path (never GitLab's raw URL — private groups 401 the browser).
func (a *api) orgInfo(w http.ResponseWriter, r *http.Request) {
	v, err := a.c.do("org:"+a.orgGroup(), 10*time.Minute, func() (any, error) {
		g, err := a.gl.group(context.Background(), a.orgGroup())
		if err != nil {
			return nil, err
		}
		return g, nil
	})
	if err != nil {
		writeJSON(w, map[string]any{"group": a.orgGroup()})
		return
	}
	g := v.(glGroup)
	out := map[string]any{"group": a.orgGroup(), "name": g.Name, "web_url": g.WebURL}
	if g.AvatarURL != "" {
		out["logo"] = "/api/org-logo"
	}
	writeJSON(w, out)
}

// orgLogo streams the group avatar through the server's credential.
func (a *api) orgLogo(w http.ResponseWriter, r *http.Request) {
	type img struct {
		b  []byte
		ct string
	}
	v, err := a.c.do("orglogo:"+a.orgGroup(), 10*time.Minute, func() (any, error) {
		ctx := context.Background()
		g, err := a.gl.group(ctx, a.orgGroup())
		if err != nil {
			return nil, err
		}
		if g.AvatarURL == "" {
			return nil, fmt.Errorf("group has no avatar")
		}
		// NOT g.AvatarURL: /uploads/... is a web-session route that 401s
		// token-header requests. The API's avatar endpoint honors the token,
		// but answers octet-stream — sniff the real type from the bytes.
		b, ct, err := a.gl.fetchBytes(ctx, a.gl.base+"/api/v4/groups/"+url.QueryEscape(a.orgGroup())+"/avatar")
		if err != nil {
			return nil, err
		}
		if ct == "" || ct == "application/octet-stream" {
			ct = http.DetectContentType(b)
		}
		return img{b, ct}, nil
	})
	if err != nil {
		w.WriteHeader(404)
		return
	}
	im := v.(img)
	if im.ct != "" {
		w.Header().Set("Content-Type", im.ct)
	}
	w.Header().Set("Cache-Control", "max-age=600")
	w.Write(im.b)
}

// clientFor resolves the client for a delivery target: its own host and
// credential when named, the primary client otherwise. nil = the target is
// not reachable from here (unsupported kind, or its token isn't configured) —
// callers render "credentials pending" instead of guessing.
func (a *api) clientFor(d deliverySpec) *glClient {
	if d.Kind != "" && d.Kind != "gitlab" {
		return nil // github driver pending — the target renders as such
	}
	host := d.Host
	if host == "" {
		host = a.gl.base
	}
	tok := a.gl.token
	if d.TokenEnv != "" {
		tok = os.Getenv(d.TokenEnv)
		if tok == "" {
			return nil
		}
	} else if host != a.gl.base {
		return nil // a foreign host always needs its own credential
	}
	key := host + "\x00" + d.TokenEnv
	a.clientMu.Lock()
	defer a.clientMu.Unlock()
	if c, ok := a.clients[key]; ok {
		return c
	}
	c := newGLClient(host, tok)
	a.clients[key] = c
	return c
}

// noteBranchDeleted / branchDeleted implement the delete tombstone (2 min —
// far past any upstream list staleness).
func (a *api) noteBranchDeleted(project, branch string) {
	a.hotMu.Lock()
	a.recentDel[project+"\x00"+branch] = time.Now().Add(2 * time.Minute)
	a.hotMu.Unlock()
}

func (a *api) branchDeleted(project, branch string) bool {
	a.hotMu.Lock()
	defer a.hotMu.Unlock()
	until, ok := a.recentDel[project+"\x00"+branch]
	if !ok {
		return false
	}
	if time.Now().After(until) {
		delete(a.recentDel, project+"\x00"+branch)
		return false
	}
	return true
}

// markHot puts a project on the fast cache lane for two minutes after an
// action fires — long enough to cover the trigger job's push plus the new
// pipeline's first stages.
func (a *api) markHot(project string) {
	a.hotMu.Lock()
	a.hot[project] = time.Now().Add(2 * time.Minute)
	a.hotMu.Unlock()
}

func (a *api) isHot(project string) bool {
	a.hotMu.Lock()
	defer a.hotMu.Unlock()
	until, ok := a.hot[project]
	if !ok {
		return false
	}
	if time.Now().After(until) {
		delete(a.hot, project)
		return false
	}
	return true
}

// noteServicePipeline records a service's latest pipeline status and reports
// whether it just COMPLETED successfully — the moment its macro dep-bump is
// in flight. Callers hot-lane the macro on true.
func (a *api) noteServicePipeline(project, status string) bool {
	if status == "" {
		return false
	}
	a.hotMu.Lock()
	prev := a.lastSvc[project]
	a.lastSvc[project] = status
	a.hotMu.Unlock()
	wasLive := prev == "running" || prev == "pending" || prev == "created"
	return wasLive && status == "success"
}
