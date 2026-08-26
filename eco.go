package main

// eco.go — the delivery-ecosystem view: the metaphor supply chain rendered from
// GitLab data. Each microservice publishes a microchart RC; the metaphor-macro
// umbrella exact-pins those RCs (its deps ARE the bundled versions); the newest
// `metaphor-v*` tag is the macro's published RC; metaphor-gitops' Application
// `targetRevision` is what's actually delivered. Drift = delivered vs published.
// Everything is read-only GitLab; parsing is stdlib-only to keep the zero-dep
// ethos, over CI-generated files whose shape is stable.

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// themeVersion is the human-visible build marker. Bump it with every change
// worth seeing land — the header badge surfaces it so you can tell at a glance
// which build of the theme is actually serving.
const themeVersion = "1.9.0"

const ttlEco = 45 * time.Second

type svcSpec struct {
	Name    string
	Project string // GitLab path, e.g. civo/metaphor/metaphor
	Chart   string // path to that repo's Chart.yaml (base version lives here)
}

type deliverySpec struct {
	Env     string
	Cluster string
	Project string // gitops repo path
	App     string // path to the ArgoCD Application yaml
}

type topology struct {
	Services  []svcSpec
	MacroName string
	MacroProj string
	MacroFile string
	MacroTag  string // tag prefix, e.g. "metaphor-v"
	Delivery  []deliverySpec
}

// defaultTopology is the metaphor supply chain. This theme is org-pinned to
// civo/metaphor and IS the metaphor delivery-view, so the topology is concrete.
func defaultTopology() topology {
	return topology{
		Services: []svcSpec{
			{"metaphor", "civo/metaphor/metaphor", "charts/metaphor/Chart.yaml"},
			{"metaphor-dashboard-manager", "civo/metaphor/metaphor-dashboard-manager", "charts/metaphor-dashboard-manager/Chart.yaml"},
			{"metaphor-micro-frontend", "civo/metaphor/metaphor-micro-frontend", "charts/metaphor-micro-frontend/Chart.yaml"},
		},
		MacroName: "metaphor-macro",
		MacroProj: "civo/metaphor/charts",
		MacroFile: "charts/metaphor-macro/Chart.yaml",
		MacroTag:  "metaphor-v",
		Delivery: []deliverySpec{
			{"development-33", "dev-33", "civo/metaphor/metaphor-gitops", "registry/environments/development-33/dev-33/metaphor-macro.yaml"},
		},
	}
}

// --- parsing (line/regex over CI-generated YAML; no yaml dependency) ---

var reVersion = regexp.MustCompile(`(?m)^\s*version:\s*"?([^"\s#]+)"?`)
var reTargetRev = regexp.MustCompile(`targetRevision:\s*"?([^"\s#]+)"?`)

// stripComments drops whole-line and trailing `#` comments so a commented
// mention of a field never false-matches the real one.
func stripComments(raw string) string {
	var b strings.Builder
	for _, ln := range strings.Split(raw, "\n") {
		if i := strings.IndexByte(ln, '#'); i >= 0 {
			ln = ln[:i]
		}
		b.WriteString(ln)
		b.WriteByte('\n')
	}
	return b.String()
}

// chartVersion returns the top-level `version:` of a Chart.yaml.
func chartVersion(raw string) string {
	m := reVersion.FindStringSubmatch(stripComments(raw))
	if len(m) == 2 {
		return m[1]
	}
	return ""
}

// targetRevision returns the first `targetRevision:` value in an Application.
func targetRevision(raw string) string {
	m := reTargetRev.FindStringSubmatch(stripComments(raw))
	if len(m) == 2 {
		return m[1]
	}
	return ""
}

// macroDeps maps each umbrella dependency name to its exact-pinned version.
func macroDeps(raw string) map[string]string {
	deps := map[string]string{}
	var name string
	for _, ln := range strings.Split(stripComments(raw), "\n") {
		t := strings.TrimSpace(ln)
		switch {
		case strings.HasPrefix(t, "- name:"):
			name = unquote(strings.TrimSpace(strings.TrimPrefix(t, "- name:")))
		case strings.HasPrefix(t, "version:") && name != "":
			deps[name] = unquote(strings.TrimSpace(strings.TrimPrefix(t, "version:")))
			name = ""
		}
	}
	return deps
}

func unquote(s string) string { return strings.Trim(s, `"'`) }

// --- version comparison (X.Y.Z with optional -rc.N; release > its rc's) ---

type ver struct {
	maj, min, pat, rc int
	hasRC             bool
	ok                bool
}

var reSemRC = regexp.MustCompile(`^v?(\d+)\.(\d+)\.(\d+)(?:-rc\.(\d+))?`)

func parseVer(s string) ver {
	m := reSemRC.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return ver{}
	}
	v := ver{ok: true}
	v.maj, _ = strconv.Atoi(m[1])
	v.min, _ = strconv.Atoi(m[2])
	v.pat, _ = strconv.Atoi(m[3])
	if m[4] != "" {
		v.rc, _ = strconv.Atoi(m[4])
		v.hasRC = true
	}
	return v
}

// cmpVer returns -1, 0, 1. Same base: a release (no -rc) sorts above its rc's.
func cmpVer(a, b ver) int {
	for _, d := range [][2]int{{a.maj, b.maj}, {a.min, b.min}, {a.pat, b.pat}} {
		if d[0] != d[1] {
			if d[0] < d[1] {
				return -1
			}
			return 1
		}
	}
	if a.hasRC != b.hasRC {
		if a.hasRC { // a is an rc, b is the release
			return -1
		}
		return 1
	}
	if a.rc != b.rc {
		if a.rc < b.rc {
			return -1
		}
		return 1
	}
	return 0
}

// newestTag returns the highest-versioned tag carrying prefix (full tag name).
func newestTag(tags []glTag, prefix string) string {
	best, bestV := "", ver{}
	for _, t := range tags {
		if !strings.HasPrefix(t.Name, prefix) {
			continue
		}
		v := parseVer(strings.TrimPrefix(t.Name, prefix))
		if !v.ok {
			continue
		}
		if best == "" || cmpVer(v, bestV) > 0 {
			best, bestV = t.Name, v
		}
	}
	return best
}

// drift classifies a delivered version against the published one.
func drift(delivered, published string) (state string, behind int) {
	d, p := parseVer(delivered), parseVer(published)
	if delivered == "" || published == "" || !d.ok || !p.ok {
		return "unknown", 0
	}
	switch c := cmpVer(d, p); {
	case c == 0:
		return "current", 0
	case c > 0:
		return "ahead", 0
	default:
		if d.maj == p.maj && d.min == p.min && d.pat == p.pat && d.hasRC && p.hasRC {
			return "behind", p.rc - d.rc
		}
		return "behind", 0
	}
}

// --- response shapes ---

type svcNode struct {
	Name     string        `json:"name"`
	Project  string        `json:"project"`
	WebURL   string        `json:"web_url"`
	BaseVer  string        `json:"base_version"`
	Bundled  string        `json:"bundled_version"`
	Pipeline *pipelineJSON `json:"pipeline,omitempty"`
	Commit   *commitJSON   `json:"commit,omitempty"`
	SHAPipes []shaPipeJSON `json:"sha_pipelines,omitempty"`
}

type macroNode struct {
	Name         string        `json:"name"`
	Project      string        `json:"project"`
	WebURL       string        `json:"web_url"`
	BaseVer      string        `json:"base_version"`
	PublishedRC  string        `json:"published_rc"`
	PublishedTag string        `json:"published_tag"`
	Pipeline     *pipelineJSON `json:"pipeline,omitempty"`
	Commit       *commitJSON   `json:"commit,omitempty"`
	SHAPipes     []shaPipeJSON `json:"sha_pipelines,omitempty"`
}

// commitJSON is the headline of the commit behind the latest pipeline — the
// human story ("what changed") next to the version story.
type commitJSON struct {
	SHA        string    `json:"sha"`
	ShortSHA   string    `json:"short_sha"`
	Title      string    `json:"title"`
	WebURL     string    `json:"web_url"`
	AuthorName string    `json:"author_name"`
	AuthoredAt time.Time `json:"authored_at"`
}

// shaPipeJSON is one run in the SHA's complete pipeline story.
type shaPipeJSON struct {
	ID        int       `json:"id"`
	Status    string    `json:"status"`
	Ref       string    `json:"ref"`
	Source    string    `json:"source"`
	WebURL    string    `json:"web_url"`
	UpdatedAt time.Time `json:"updated_at"`
}

type deliveryNode struct {
	Env       string `json:"environment"`
	Cluster   string `json:"cluster"`
	Delivered string `json:"delivered_version"`
	WebURL    string `json:"web_url"`
	State     string `json:"state"`
	Behind    int    `json:"behind"`
}

// --- cached fetch helpers ---

func (a *api) rawFile(ctx context.Context, proj, file string) string {
	ttl := ttlEco
	if a.isHot(proj) {
		ttl = 5 * time.Second
	}
	v, err := a.c.do("file:"+proj+":"+file, ttl, func() (any, error) {
		return a.gl.fileRaw(ctx, proj, file, "main")
	})
	if err != nil {
		return ""
	}
	return v.(string)
}

func (a *api) cachedTags(ctx context.Context, proj string) []glTag {
	ttl := ttlEco
	if a.isHot(proj) {
		ttl = 5 * time.Second
	}
	v, err := a.c.do("tags:"+proj, ttl, func() (any, error) {
		return a.gl.tags(ctx, proj, 100)
	})
	if err != nil {
		return nil
	}
	return v.([]glTag)
}

func (a *api) latestPipe(ctx context.Context, proj string) *pipelineJSON {
	ttl := ttlEco
	if a.isHot(proj) {
		ttl = 5 * time.Second
	}
	v, err := a.c.do("lp:"+proj, ttl, func() (any, error) {
		pl, err := a.gl.latestPipeline(ctx, proj)
		if err != nil {
			return pl, err
		}
		// [skip ci] version-set commits leave "skipped" as the newest pipeline —
		// noise, not news. Surface the newest REAL run instead.
		if pl.Status == "skipped" {
			if recent, err := a.gl.recentPipelines(ctx, proj, pl.Ref, "", 10); err == nil {
				for _, r := range recent {
					if r.Status == "skipped" {
						continue
					}
					if full, err := a.gl.pipelineByPath(ctx, proj, r.ID); err == nil && full.ID != 0 {
						return full, nil
					}
					break
				}
			}
		}
		return pl, nil
	})
	if err != nil {
		return nil
	}
	pl := v.(glLatestPipeline)
	if pl.ID == 0 {
		return nil
	}
	short := pl.SHA
	if len(short) > 8 {
		short = short[:8]
	}
	pj := pipelineJSON{
		ID: pl.ID, Status: pl.Status, Ref: pl.Ref, SHA: pl.SHA, ShortSHA: short,
		WebURL: pl.WebURL, CreatedAt: pl.CreatedAt, UpdatedAt: pl.UpdatedAt,
		FinishedAt: pl.FinishedAt,
		DurationS:  pl.UpdatedAt.Sub(pl.CreatedAt).Seconds(),
	}
	if pl.User != nil {
		pj.AuthorName = pl.User.Name
		pj.AuthorAvatar = pl.User.AvatarURL
	}
	return &pj
}

func (a *api) cachedCommit(ctx context.Context, proj, sha string) *commitJSON {
	if sha == "" {
		return nil
	}
	v, err := a.c.do("ci:"+proj+":"+sha, ttlEco, func() (any, error) {
		return a.gl.commitInfo(ctx, proj, sha)
	})
	if err != nil {
		return nil
	}
	c := v.(glCommit)
	if c.ID == "" {
		return nil
	}
	return &commitJSON{SHA: c.ID, ShortSHA: c.ShortID, Title: c.Title,
		WebURL: c.WebURL, AuthorName: c.AuthorName, AuthoredAt: c.AuthoredDate}
}

func (a *api) cachedSHAPipes(ctx context.Context, proj, sha string) []shaPipeJSON {
	if sha == "" {
		return nil
	}
	ttl := ttlEco
	if a.isHot(proj) {
		ttl = 5 * time.Second
	}
	v, err := a.c.do("shapl:"+proj+":"+sha, ttl, func() (any, error) {
		return a.gl.pipelinesForSHA(ctx, proj, sha, 8)
	})
	if err != nil {
		return nil
	}
	pls := v.([]glSHAPipeline)
	out := make([]shaPipeJSON, 0, len(pls))
	for _, p := range pls {
		if p.Status == "skipped" {
			continue // [skip ci] noise
		}
		out = append(out, shaPipeJSON{ID: p.ID, Status: p.Status, Ref: p.Ref,
			Source: p.Source, WebURL: p.WebURL, UpdatedAt: p.UpdatedAt})
	}
	return out
}

// pipeBundle is a project's latest pipeline plus that SHA's commit headline and
// full pipeline story. The commit/story fetches depend on the pipeline's SHA,
// so they chain after it — but run against the cache, so steady-state is free.
type pipeBundle struct {
	Pipe   *pipelineJSON
	Commit *commitJSON
	Pipes  []shaPipeJSON
}

func (a *api) pipeBundleFor(ctx context.Context, proj string) pipeBundle {
	pb := pipeBundle{Pipe: a.latestPipe(ctx, proj)}
	if pb.Pipe != nil && pb.Pipe.SHA != "" {
		done := make(chan struct{}, 2)
		go func() { pb.Commit = a.cachedCommit(ctx, proj, pb.Pipe.SHA); done <- struct{}{} }()
		go func() { pb.Pipes = a.cachedSHAPipes(ctx, proj, pb.Pipe.SHA); done <- struct{}{} }()
		<-done
		<-done
	}
	return pb
}

// ecosystem renders the metaphor supply chain: services → umbrella → delivered.
// Every GitLab call fires concurrently — the handler's wall-clock is one slow
// call, not the sum — so a sluggish GitLab can't push it past the ingress
// timeout. Each fetch degrades to empty on error, so partial data still renders.
func (a *api) ecosystem(w http.ResponseWriter, r *http.Request) {
	// Detached from the request: a slow GitLab shouldn't get cancelled the moment
	// the browser/ingress times out — let it finish and warm the cache so the
	// next 20s poll returns instantly.
	ctx := context.Background()
	t := a.topo

	var (
		macroRaw, publishedTag string
		macroPB                pipeBundle
		svcRaw                 = make([]string, len(t.Services))
		svcPB                  = make([]pipeBundle, len(t.Services))
		delRaw                 = make([]string, len(t.Delivery))
	)

	// Every fetch reports on a buffered channel (buffered so a late fetch never
	// blocks/leaks). We collect until everything's in OR a short deadline hits —
	// so a crawling GitLab yields a fast partial 200 instead of a 504. Fetches
	// that miss the deadline keep running on the detached ctx and warm the cache
	// for the next poll, so the view fills in rather than showing "not connected".
	type slot struct {
		kind string
		i    int
		v    any
	}
	total := 3 + len(t.Services)*2 + len(t.Delivery)
	ch := make(chan slot, total)
	go func() { ch <- slot{"macroRaw", 0, a.rawFile(ctx, t.MacroProj, t.MacroFile)} }()
	go func() { ch <- slot{"tag", 0, newestTag(a.cachedTags(ctx, t.MacroProj), t.MacroTag)} }()
	go func() { ch <- slot{"macroPipe", 0, a.pipeBundleFor(ctx, t.MacroProj)} }()
	for i, s := range t.Services {
		i, s := i, s
		go func() { ch <- slot{"svcRaw", i, a.rawFile(ctx, s.Project, s.Chart)} }()
		go func() { ch <- slot{"svcPipe", i, a.pipeBundleFor(ctx, s.Project)} }()
	}
	for i, d := range t.Delivery {
		i, d := i, d
		go func() { ch <- slot{"delRaw", i, a.rawFile(ctx, d.Project, d.App)} }()
	}

	deadline := time.After(9 * time.Second)
collect:
	for got := 0; got < total; got++ {
		select {
		case s := <-ch:
			switch s.kind {
			case "macroRaw":
				macroRaw = s.v.(string)
			case "tag":
				publishedTag = s.v.(string)
			case "macroPipe":
				macroPB = s.v.(pipeBundle)
			case "svcRaw":
				svcRaw[s.i] = s.v.(string)
			case "svcPipe":
				svcPB[s.i] = s.v.(pipeBundle)
			case "delRaw":
				delRaw[s.i] = s.v.(string)
			}
		case <-deadline:
			break collect
		}
	}

	deps := macroDeps(macroRaw)
	publishedRC := strings.TrimPrefix(publishedTag, t.MacroTag)
	macro := macroNode{
		Name: t.MacroName, Project: t.MacroProj,
		WebURL:  a.gl.base + "/" + t.MacroProj,
		BaseVer: chartVersion(macroRaw), PublishedRC: publishedRC, PublishedTag: publishedTag,
		Pipeline: macroPB.Pipe, Commit: macroPB.Commit, SHAPipes: macroPB.Pipes,
	}

	services := make([]svcNode, len(t.Services))
	for i, s := range t.Services {
		services[i] = svcNode{
			Name: s.Name, Project: s.Project,
			WebURL:  a.gl.base + "/" + s.Project,
			BaseVer: chartVersion(svcRaw[i]), Bundled: deps[s.Name],
			Pipeline: svcPB[i].Pipe, Commit: svcPB[i].Commit, SHAPipes: svcPB[i].Pipes,
		}
		// A service run just finished: its dep-bump is pushing the next macro
		// RC — put the macro on the fast lane so the handoff shows promptly.
		if svcPB[i].Pipe != nil && a.noteServicePipeline(s.Project, svcPB[i].Pipe.Status) {
			a.markHot(t.MacroProj)
		}
	}

	delivery := make([]deliveryNode, len(t.Delivery))
	for i, d := range t.Delivery {
		delivered := targetRevision(delRaw[i])
		state, behind := drift(delivered, publishedRC)
		delivery[i] = deliveryNode{
			Env: d.Env, Cluster: d.Cluster, Delivered: delivered,
			WebURL: a.gl.base + "/" + d.Project + "/-/blob/main/" + d.App,
			State:  state, Behind: behind,
		}
	}

	writeJSON(w, map[string]any{
		"generated_at": time.Now().UTC(),
		"macro":        macro,
		"services":     services,
		"delivery":     delivery,
	})
}

// meta exposes the theme build marker and connection scope. Unguarded so the
// header version badge renders even before a token is wired.
func (a *api) meta(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{
		"theme":       "foxglider",
		"version":     themeVersion,
		"gitlab_host": a.gl.base,
		"groups":      a.groups,
	})
}

// ttlProgress keeps live stage-progress fresh without hammering GitLab: the
// frontend only polls this for running pipelines, and hits this short cache.
// ttlHistory caches the previous successful run's per-job durations — the
// baseline that turns the stage bar into a real progress bar.
const (
	ttlProgress = 5 * time.Second
	ttlHistory  = 10 * time.Minute
)

type jobProgress struct {
	Name      string  `json:"name"`
	Status    string  `json:"status"`
	DurationS float64 `json:"duration_s,omitempty"` // actual, for finished jobs
	ExpectedS float64 `json:"expected_s,omitempty"` // from the previous successful run
}

type stageProgress struct {
	Name   string        `json:"name"`
	Status string        `json:"status"`
	Jobs   []jobProgress `json:"jobs"`
	// Running carries what the frontend needs to animate the fill locally
	// (elapsed vs expected) — smooth progress with no extra polling.
	Running *struct {
		Name      string     `json:"name"`
		StartedAt *time.Time `json:"started_at"`
		ExpectedS float64    `json:"expected_s"`
	} `json:"running,omitempty"`
}

// jobHistory maps job name → duration from the newest successful pipeline on
// the same ref — the progress baseline. Empty map when there's no history.
func (a *api) jobHistory(ctx context.Context, proj, ref string, excludeID int) map[string]float64 {
	v, err := a.c.do("hist:"+proj+":"+ref, ttlHistory, func() (any, error) {
		hist := map[string]float64{}
		pls, err := a.gl.recentPipelines(ctx, proj, ref, "success", 3)
		if err != nil {
			return hist, nil // no baseline is fine — bars degrade to pulses
		}
		for _, pl := range pls {
			if pl.ID == excludeID {
				continue
			}
			jobs, err := a.gl.jobsByPath(ctx, proj, pl.ID)
			if err != nil {
				break
			}
			for _, j := range jobs {
				if j.Duration > 0 {
					hist[j.Name] = j.Duration
				}
			}
			break // newest prior success only
		}
		return hist, nil
	})
	if err != nil {
		return map[string]float64{}
	}
	return v.(map[string]float64)
}

// pipelineProgress rolls a running pipeline's jobs up into ordered per-stage
// statuses, enriched with per-job timings and a baseline from the previous
// successful run so the frontend can draw honest progress bars.
func (a *api) pipelineProgress(w http.ResponseWriter, r *http.Request) {
	proj := r.URL.Query().Get("project")
	id, _ := strconv.Atoi(r.URL.Query().Get("id"))
	if proj == "" || id == 0 {
		writeErr(w, 400, "project and id required")
		return
	}
	ctx := context.Background()
	v, err := a.c.do(fmt.Sprintf("prog:%s:%d", proj, id), ttlProgress, func() (any, error) {
		return a.gl.jobsByPath(ctx, proj, id)
	})
	if err != nil {
		writeErr(w, 503, "GitLab unreachable: "+err.Error())
		return
	}
	hist := a.jobHistory(ctx, proj, "main", id)
	stages := rollupStages(v.([]glJob), hist)
	writeJSON(w, map[string]any{"stages": stages, "status": overallStatus(stages)})
}

// rollupStages collapses jobs into ordered per-stage progress (first-seen
// order), attaching timings and the running job's animation baseline.
func rollupStages(jobs []glJob, hist map[string]float64) []stageProgress {
	var order []string
	byStage := map[string][]glJob{}
	for _, j := range jobs {
		if _, ok := byStage[j.Stage]; !ok {
			order = append(order, j.Stage)
		}
		byStage[j.Stage] = append(byStage[j.Stage], j)
	}
	out := make([]stageProgress, 0, len(order))
	for _, s := range order {
		sp := stageProgress{Name: s}
		var statuses []string
		for _, j := range byStage[s] {
			statuses = append(statuses, j.Status)
			jp := jobProgress{Name: j.Name, Status: j.Status, ExpectedS: hist[j.Name]}
			if j.Duration > 0 {
				jp.DurationS = j.Duration
			}
			sp.Jobs = append(sp.Jobs, jp)
			if j.Status == "running" && sp.Running == nil {
				sp.Running = &struct {
					Name      string     `json:"name"`
					StartedAt *time.Time `json:"started_at"`
					ExpectedS float64    `json:"expected_s"`
				}{Name: j.Name, StartedAt: j.StartedAt, ExpectedS: hist[j.Name]}
			}
		}
		sp.Status = stageStatus(statuses)
		out = append(out, sp)
	}
	return out
}

// stageStatus picks the most salient status for a stage: failed beats running
// beats not-started beats success.
func stageStatus(ss []string) string {
	rank := map[string]int{"failed": 5, "running": 4, "pending": 3, "created": 3, "manual": 2, "success": 1}
	best, bestRank := "success", 0
	for _, s := range ss {
		if r := rank[s]; r > bestRank {
			bestRank, best = r, s
		}
	}
	return best
}

func overallStatus(stages []stageProgress) string {
	for _, s := range stages {
		if s.Status == "failed" {
			return "failed"
		}
	}
	for _, s := range stages {
		if s.Status == "running" || s.Status == "pending" || s.Status == "created" {
			return "running"
		}
	}
	return "success"
}
