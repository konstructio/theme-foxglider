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
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// themeVersion is the human-visible build marker. Bump it with every change
// worth seeing land — the header badge surfaces it so you can tell at a glance
// which build of the theme is actually serving.
const themeVersion = "2.11.0"

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

type depJSON struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// orderedDeps is macroDeps preserving file order — the bundle tree renders
// subcharts in the order the umbrella declares them.
func orderedDeps(raw string) []depJSON {
	var out []depJSON
	var name string
	for _, ln := range strings.Split(stripComments(raw), "\n") {
		t := strings.TrimSpace(ln)
		switch {
		case strings.HasPrefix(t, "- name:"):
			name = unquote(strings.TrimSpace(strings.TrimPrefix(t, "- name:")))
		case strings.HasPrefix(t, "version:") && name != "":
			out = append(out, depJSON{Name: name, Version: unquote(strings.TrimSpace(strings.TrimPrefix(t, "version:")))})
			name = ""
		}
	}
	return out
}

// --- version comparison (X.Y.Z with optional -rc.N; release > its rc's) ---

type ver struct {
	maj, min, pat, rc int
	hasRC             bool
	ok                bool
}

// End-anchored on purpose: a feature version like 0.2.0-epic-20-pink.3 must
// NOT parse as release 0.2.0 (it would outrank every rc as "newest"). Epic
// versions are deliberately un-orderable here — they never become "latest".
var reSemRC = regexp.MustCompile(`^v?(\d+)\.(\d+)\.(\d+)(?:-rc\.(\d+))?$`)

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

// reRCTag matches an rc tag suffix of either style: numeric counters
// (metaphor: -rc.19) or sha-suffixed (konstruct: -rc.443f3828).
var reRCTag = regexp.MustCompile(`-rc\.[0-9a-f]+$`)

// newestTag returns the newest rc tag carrying prefix. The tags list arrives
// ordered by update time (newest first), which is the only ordering that
// works for BOTH rc styles — semver-comparing sha suffixes is a trap (an
// all-digit sha parses as a huge counter and outranks everything).
func newestTag(tags []glTag, prefix string) string {
	for _, t := range tags {
		if strings.HasPrefix(t.Name, prefix) && reRCTag.MatchString(t.Name) {
			return t.Name
		}
	}
	return ""
}

// drift classifies a delivered version against the published one.
func drift(delivered, published string) (state string, behind int) {
	d, p := parseVer(delivered), parseVer(published)
	if delivered == "" || published == "" {
		return "unknown", 0
	}
	if !d.ok || !p.ok {
		// sha-suffixed rc lines aren't ordered — equality is still knowable
		if delivered == published {
			return "current", 0
		}
		return "differs", 0
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
	// Features: epic-* branches on this repo touched in the last 2 days —
	// rendered on the tile as first-class secondary targets (the tile's main
	// block stays main; these carry their own trigger/release).
	Features []branchJSON `json:"features,omitempty"`
	// LatestRelease feeds the tile's "main release" section.
	LatestRelease *releaseJSON `json:"latest_release,omitempty"`
}

type macroNode struct {
	Name         string `json:"name"`
	Project      string `json:"project"`
	WebURL       string `json:"web_url"`
	BaseVer      string `json:"base_version"`
	PublishedRC  string `json:"published_rc"`
	PublishedTag string `json:"published_tag"`
	// Bundle: the subchart pins inside the PUBLISHED umbrella (deps at the
	// tag ref; falls back to main's tip when the tag read misses — BundleRef
	// says which one you're looking at).
	Bundle        []depJSON     `json:"bundle,omitempty"`
	BundleRef     string        `json:"bundle_ref,omitempty"`
	LatestRelease *releaseJSON  `json:"latest_release,omitempty"`
	Pipeline      *pipelineJSON `json:"pipeline,omitempty"`
	Commit        *commitJSON   `json:"commit,omitempty"`
	SHAPipes      []shaPipeJSON `json:"sha_pipelines,omitempty"`
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
		// noise, not news. Surface the newest REAL run instead, across ALL refs:
		// the macro's true latest is usually its RC tag pipeline, and main can
		// be an unbroken streak of skips.
		if pl.Status == "skipped" {
			if recent, err := a.gl.recentPipelines(ctx, proj, "", "", 20); err == nil {
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
	svcFeats := make([][]branchJSON, len(t.Services))
	svcRels := make([]*releaseJSON, len(t.Services))
	var macroRel *releaseJSON
	total := 4 + len(t.Services)*4 + len(t.Delivery)
	ch := make(chan slot, total)
	go func() { ch <- slot{"macroRaw", 0, a.rawFile(ctx, t.MacroProj, t.MacroFile)} }()
	go func() { ch <- slot{"tag", 0, newestTag(a.cachedTags(ctx, t.MacroProj), t.MacroTag)} }()
	go func() { ch <- slot{"macroPipe", 0, a.pipeBundleFor(ctx, t.MacroProj)} }()
	go func() { ch <- slot{"macroRel", 0, a.cachedRelease(ctx, t.MacroProj)} }()
	for i, s := range t.Services {
		i, s := i, s
		go func() { ch <- slot{"svcRaw", i, a.rawFile(ctx, s.Project, s.Chart)} }()
		go func() { ch <- slot{"svcPipe", i, a.pipeBundleFor(ctx, s.Project)} }()
		go func() { ch <- slot{"svcFeat", i, a.activeFeatures(ctx, s.Project)} }()
		go func() { ch <- slot{"svcRel", i, a.cachedRelease(ctx, s.Project)} }()
	}
	for i, d := range t.Delivery {
		i, d := i, d
		go func() { ch <- slot{"delRaw", i, a.deliveryFile(ctx, d.Project, d.App)} }()
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
			case "svcFeat":
				svcFeats[s.i], _ = s.v.([]branchJSON)
			case "svcRel":
				svcRels[s.i], _ = s.v.(*releaseJSON)
			case "macroRel":
				macroRel, _ = s.v.(*releaseJSON)
			}
		case <-deadline:
			break collect
		}
	}

	deps := macroDeps(macroRaw)
	publishedRC := strings.TrimPrefix(publishedTag, t.MacroTag)
	// The bundle tree shows what's inside the PUBLISHED umbrella — deps read
	// at the tag ref (immutable, so cached long). A miss falls back to main's
	// tip, which in steady state is the same pins.
	bundle, bundleRef := orderedDeps(macroRaw), "main"
	if publishedTag != "" {
		if v, err := a.c.do("bundle:"+t.MacroProj+"@"+publishedTag, time.Hour, func() (any, error) {
			bctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			raw, err := a.gl.fileRaw(bctx, t.MacroProj, t.MacroFile, publishedTag)
			if err != nil {
				return nil, err
			}
			return orderedDeps(raw), nil
		}); err == nil {
			bundle, bundleRef = v.([]depJSON), publishedTag
		}
	}
	macro := macroNode{
		Name: t.MacroName, Project: t.MacroProj,
		WebURL:  a.gl.base + "/" + t.MacroProj,
		BaseVer: chartVersion(macroRaw), PublishedRC: publishedRC, PublishedTag: publishedTag,
		Bundle: bundle, BundleRef: bundleRef, LatestRelease: macroRel,
		Pipeline: macroPB.Pipe, Commit: macroPB.Commit, SHAPipes: macroPB.Pipes,
	}

	services := make([]svcNode, len(t.Services))
	for i, s := range t.Services {
		services[i] = svcNode{
			Name: s.Name, Project: s.Project,
			WebURL:  a.gl.base + "/" + s.Project,
			BaseVer: chartVersion(svcRaw[i]), Bundled: deps[s.Name],
			Pipeline: svcPB[i].Pipe, Commit: svcPB[i].Commit, SHAPipes: svcPB[i].Pipes,
			Features: svcFeats[i], LatestRelease: svcRels[i],
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
		"summary":      ecoSummary(t, macro, delivery, a.cachedTags(ctx, t.MacroProj)),
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

// --- branches swimlanes (main / hotfix-* / epic-*) ---

const ttlBranches = 60 * time.Second

type committerJSON struct {
	Name   string `json:"name"`
	Avatar string `json:"avatar,omitempty"`
}

type branchJSON struct {
	Name    string `json:"name"`
	WebURL  string `json:"web_url"`
	Short   string `json:"short_sha,omitempty"`
	Title   string `json:"title,omitempty"`
	Author  string `json:"author,omitempty"`
	When    string `json:"when,omitempty"`
	EpicIID int    `json:"epic_iid,omitempty"`
	// Stale: no commits in 30 days — surfaced separately so live lanes stay
	// scannable. Ahead: commits not yet merged back to main (hotfix lanes);
	// a pointer because nil means "not checked" (beyond the compare bound),
	// which must never render as "merged".
	Stale bool `json:"stale,omitempty"`
	Ahead *int `json:"ahead,omitempty"`
	// Committers: who wrote the unmerged commits (hotfix lanes, newest first).
	Committers []committerJSON `json:"committers,omitempty"`
	// CompareURL: GitLab's main...branch compare page — the click-through for
	// the ↑N badge.
	CompareURL string `json:"compare_url,omitempty"`
	// MacroVer/MacroURL: the end-result umbrella version this branch's line
	// publishes (newest macro tag whose prerelease id matches the branch;
	// main → the newest rc), with a link to that tag.
	MacroVer string `json:"macro_ver,omitempty"`
	MacroURL string `json:"macro_url,omitempty"`
}

type repoBranches struct {
	Name    string       `json:"name"`
	Project string       `json:"project"`
	Main    []branchJSON `json:"main"`
	Hotfix  []branchJSON `json:"hotfix"`
	Epic    []branchJSON `json:"epic"`
}

var reEpicBranch = regexp.MustCompile(`^epic-(\d+)`)

func toBranchJSON(b glBranch) branchJSON {
	out := branchJSON{Name: b.Name, WebURL: b.WebURL}
	if b.Commit != nil {
		out.Short = b.Commit.ShortID
		out.Title = b.Commit.Title
		out.Author = b.Commit.AuthorName
		out.When = b.Commit.CommittedDate.UTC().Format(time.RFC3339)
	}
	if m := reEpicBranch.FindStringSubmatch(b.Name); m != nil {
		if n, err := strconv.Atoi(m[1]); err == nil {
			out.EpicIID = n
		}
	}
	if b.Commit != nil && time.Since(b.Commit.CommittedDate) > 30*24*time.Hour {
		out.Stale = true
	}
	return out
}

// cachedBranches lists a repo's branches (60s cache) enriched with the hotfix
// divergence data — shared by the Branches tab and the delivery tiles.
func (a *api) cachedBranches(ctx context.Context, project string) ([]branchJSON, error) {
	v, err := a.c.do("br:"+project, ttlBranches, func() (any, error) {
		brs, err := a.gl.branches(ctx, project, "")
		if err != nil {
			return nil, err
		}
		out := make([]branchJSON, 0, len(brs))
		for _, b := range brs {
			out = append(out, toBranchJSON(b))
		}
		// hotfix divergence: commits not yet merged back to main.
		// Bounded to the 10 most recent — repos like konstruct-ui carry
		// dozens of old hotfix branches and each check is an API call.
		hix := []int{}
		for i, bj := range out {
			if strings.HasPrefix(bj.Name, "hotfix") {
				hix = append(hix, i)
			}
		}
		sort.Slice(hix, func(x, y int) bool { return out[hix[x]].When > out[hix[y]].When })
		if len(hix) > 10 {
			hix = hix[:10]
		}
		for _, i := range hix {
			n, people, err := a.gl.compareBranch(ctx, project, "main", out[i].Name)
			if err != nil {
				continue
			}
			ah := n
			out[i].Ahead = &ah
			out[i].CompareURL = a.gl.base + "/" + project + "/-/compare/main..." + url.PathEscape(out[i].Name)
			for _, p := range people {
				out[i].Committers = append(out[i].Committers,
					committerJSON{Name: p.AuthorName, Avatar: a.avatarFor(ctx, p.AuthorEmail)})
			}
		}
		return out, nil
	})
	if err != nil {
		return nil, err
	}
	return v.([]branchJSON), nil
}

// activeFeatures filters a repo's epic-* branches to those touched within the
// last two days — the tile's "this repo is part of live feature work" signal.
func (a *api) activeFeatures(ctx context.Context, project string) []branchJSON {
	brs, err := a.cachedBranches(ctx, project)
	if err != nil {
		return nil
	}
	cutoff := time.Now().Add(-48 * time.Hour).UTC().Format(time.RFC3339)
	var out []branchJSON
	for _, b := range brs {
		if strings.HasPrefix(b.Name, "epic-") && b.When >= cutoff && !a.branchDeleted(project, b.Name) {
			out = append(out, b)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].When > out[j].When })
	if len(out) > 4 {
		out = out[:4]
	}
	return out
}

type featSvcJSON struct {
	Name    string `json:"name"`
	Project string `json:"project"`
	// State: "updated" — the feature's charts branch pins this service at a
	// feature version; "joined" — the branch exists but the pin is still
	// stable; "main" — no branch, the service rides main inside this feature.
	State   string `json:"state"`
	When    string `json:"when,omitempty"`
	WebURL  string `json:"web_url,omitempty"`
	MRIID   int    `json:"mr_iid,omitempty"`
	MRURL   string `json:"mr_url,omitempty"`
	MRState string `json:"mr_state,omitempty"`
}

type featureJSON struct {
	Branch   string        `json:"branch"`
	EpicIID  int           `json:"epic_iid,omitempty"`
	EpicURL  string        `json:"epic_url,omitempty"`
	Charts   bool          `json:"charts"`
	MacroVer string        `json:"macro_ver,omitempty"`
	MacroURL string        `json:"macro_url,omitempty"`
	When     string        `json:"when,omitempty"`
	Services []featSvcJSON `json:"services"`
}

// mrStateLabel maps an MR to the human read on the chip: merged/closed win,
// then draft, then "feedback" once anyone has commented, else ready.
func mrStateLabel(m glMR) string {
	switch {
	case m.State == "merged":
		return "merged"
	case m.State == "closed":
		return "closed"
	case m.Draft || strings.HasPrefix(m.Title, "Draft:"):
		return "draft"
	case m.UserNotesCount > 0:
		return "feedback"
	default:
		return "ready"
	}
}

// bestMR picks the MR that best represents a branch: an open one beats a
// merged one beats anything else; ties go to the newest (list order).
func bestMR(list []glMR) *glMR {
	for _, st := range []string{"opened", "merged"} {
		for i := range list {
			if list[i].State == st {
				return &list[i]
			}
		}
	}
	if len(list) > 0 {
		return &list[0]
	}
	return nil
}

// assembleFeatures groups epic-* branches into features: one entry per branch
// name, every topology service listed with its derivation state (from the
// charts feature-branch pins), plus the carrying MR per joined service.
func (a *api) assembleFeatures(ctx context.Context, t topology, repos []repoBranches, allTags []glTag) []featureJSON {
	type presence struct {
		when, webURL string
	}
	found := map[string]map[string]presence{} // branch → project → presence
	chartsHas := map[string]string{}          // branch → when (macro repo)
	newest := map[string]string{}
	for _, rb := range repos {
		for _, b := range rb.Epic {
			if found[b.Name] == nil {
				found[b.Name] = map[string]presence{}
			}
			found[b.Name][rb.Project] = presence{b.When, b.WebURL}
			if b.When > newest[b.Name] {
				newest[b.Name] = b.When
			}
			if rb.Project == t.MacroProj {
				chartsHas[b.Name] = b.When
			}
		}
	}
	names := make([]string, 0, len(found))
	for n := range found {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool { return newest[names[i]] > newest[names[j]] })
	// stale features (30d+ quiet everywhere) live in the stale section, not here
	staleCut := time.Now().Add(-30 * 24 * time.Hour).UTC().Format(time.RFC3339)
	fresh := names[:0]
	for _, n := range names {
		if newest[n] >= staleCut {
			fresh = append(fresh, n)
		}
	}
	names = fresh
	if len(names) > 6 {
		names = names[:6]
	}
	group := t.MacroProj
	if i := strings.LastIndex(group, "/"); i > 0 {
		group = group[:i]
	}
	out := make([]featureJSON, 0, len(names))
	for _, name := range names {
		f := featureJSON{Branch: name, When: newest[name]}
		if m := reEpicBranch.FindStringSubmatch(name); m != nil {
			if iid, err := strconv.Atoi(m[1]); err == nil && iid > 0 {
				f.EpicIID = iid
				f.EpicURL = a.gl.base + "/groups/" + group + "/-/epics/" + m[1]
			}
		}
		_, f.Charts = chartsHas[name]
		if tag := macroTagFor(allTags, t.MacroTag, name); tag != "" {
			f.MacroVer = strings.TrimPrefix(tag, t.MacroTag)
			f.MacroURL = a.gl.base + "/" + t.MacroProj + "/-/tags/" + url.PathEscape(tag)
		}
		// which services the feature ACTUALLY moves: the charts branch pins
		var pins map[string]string
		if f.Charts {
			if v, err := a.c.do("fdeps:"+name, ttlBranches, func() (any, error) {
				bctx, cancel := context.WithTimeout(ctx, 5*time.Second)
				defer cancel()
				raw, err := a.gl.fileRaw(bctx, t.MacroProj, t.MacroFile, name)
				if err != nil {
					return nil, err
				}
				return macroDeps(raw), nil
			}); err == nil {
				pins, _ = v.(map[string]string)
			}
		}
		needle := "-" + reBranchID.ReplaceAllString(strings.ToLower(name), "-") + "."
		for _, svc := range t.Services {
			fs := featSvcJSON{Name: svc.Name, Project: svc.Project, State: "main"}
			if p, ok := found[name][svc.Project]; ok {
				fs.State, fs.When, fs.WebURL = "joined", p.when, p.webURL
				if strings.Contains(pins[svc.Name], needle) {
					fs.State = "updated"
				}
				// the MR carrying this work (draft or open)
				if v, err := a.c.do("mrs:"+svc.Project+"@"+name, ttlBranches, func() (any, error) {
					bctx, cancel := context.WithTimeout(ctx, 5*time.Second)
					defer cancel()
					return a.gl.mrsBySource(bctx, svc.Project, name)
				}); err == nil {
					if list, _ := v.([]glMR); len(list) > 0 {
						if m := bestMR(list); m != nil {
							fs.MRIID, fs.MRURL, fs.MRState = m.IID, m.WebURL, mrStateLabel(*m)
						}
					}
				}
			}
			f.Services = append(f.Services, fs)
		}
		out = append(out, f)
	}
	return out
}

// branchesView lists every topology repo's branches grouped into lanes, plus
// the macro repo's newest tags — the release/feature timeline in one payload.
func (a *api) branchesView(w http.ResponseWriter, r *http.Request) {
	ctx := context.Background()
	t := a.topo
	repos := append([]svcSpec{}, t.Services...)
	repos = append(repos, svcSpec{Name: t.MacroName, Project: t.MacroProj})
	out := make([]repoBranches, len(repos))
	done := make(chan int, len(repos))
	for i, s := range repos {
		i, s := i, s
		go func() {
			rb := repoBranches{Name: s.Name, Project: s.Project,
				Main: []branchJSON{}, Hotfix: []branchJSON{}, Epic: []branchJSON{}}
			v, err := a.cachedBranches(ctx, s.Project)
			if err == nil {
				for _, bj := range v {
					if a.branchDeleted(s.Project, bj.Name) {
						continue
					}
					switch {
					case bj.Name == "main":
						rb.Main = append(rb.Main, bj)
					case strings.HasPrefix(bj.Name, "hotfix"):
						rb.Hotfix = append(rb.Hotfix, bj)
					case strings.HasPrefix(bj.Name, "epic-"):
						rb.Epic = append(rb.Epic, bj)
					}
				}
			}
			out[i] = rb
			done <- i
		}()
	}
	for range repos {
		<-done
	}
	allTags := a.cachedTags(ctx, t.MacroProj)
	tags := []string{}
	for _, tg := range allTags {
		tags = append(tags, tg.Name)
		if len(tags) >= 12 {
			break
		}
	}
	// every branch gets its end-result umbrella version: the newest macro tag
	// whose prerelease id matches the branch (main → the newest rc).
	for ri := range out {
		for _, lane := range [][]branchJSON{out[ri].Main, out[ri].Hotfix, out[ri].Epic} {
			for bi := range lane {
				if tag := macroTagFor(allTags, t.MacroTag, lane[bi].Name); tag != "" {
					lane[bi].MacroVer = strings.TrimPrefix(tag, t.MacroTag)
					lane[bi].MacroURL = a.gl.base + "/" + t.MacroProj + "/-/tags/" + url.PathEscape(tag)
				}
			}
		}
	}
	writeJSON(w, map[string]any{"repos": out, "macro_tags": tags,
		"features": a.assembleFeatures(ctx, t, out, allTags),
		"group":    strings.TrimSuffix(t.MacroProj, "/"+t.MacroProj[strings.LastIndex(t.MacroProj, "/")+1:])})
}

// cachedRelease fetches a repo's newest release (5-min cache; nil when the
// repo has never cut one).
func (a *api) cachedRelease(ctx context.Context, project string) *releaseJSON {
	v, err := a.c.do("relp:"+project, 5*time.Minute, func() (any, error) {
		r, err := a.gl.latestReleaseByPath(ctx, project)
		if err != nil {
			return nil, err
		}
		return r, nil
	})
	if err != nil {
		return nil
	}
	r, _ := v.(*glRelease)
	if r == nil {
		return nil
	}
	return &releaseJSON{Tag: r.TagName, Name: r.Name, ReleasedAt: r.ReleasedAt,
		WebURL: r.Links.Self, DaysAgo: int(time.Since(r.ReleasedAt).Hours() / 24)}
}

// avatarFor resolves (and long-caches) the avatar for a commit email; "" when
// unknown so the frontend falls back to an initials circle.
func (a *api) avatarFor(ctx context.Context, email string) string {
	if email == "" {
		return ""
	}
	v, err := a.c.do("av:"+strings.ToLower(email), 6*time.Hour, func() (any, error) {
		return a.gl.avatar(ctx, email)
	})
	if err != nil {
		return ""
	}
	s, _ := v.(string)
	return s
}

// reBranchID collapses a branch name to the prerelease id CI publishes under
// (helm-publish's PRE_ID sanitize): lowercase, everything else becomes '-'.
var reBranchID = regexp.MustCompile(`[^a-z0-9-]`)

// macroTagFor finds the newest (update-ordered) macro tag a branch's line
// publishes: main → the newest rc tag; any other branch → the newest tag
// whose prerelease id is the sanitized branch name.
func macroTagFor(tags []glTag, prefix, branch string) string {
	if branch == "main" {
		for _, t := range tags {
			if strings.HasPrefix(t.Name, prefix) && reRCTag.MatchString(t.Name) {
				return t.Name
			}
		}
		return ""
	}
	needle := "-" + reBranchID.ReplaceAllString(strings.ToLower(branch), "-") + "."
	for _, t := range tags {
		if strings.HasPrefix(t.Name, prefix) && strings.Contains(t.Name[len(prefix):], needle) {
			return t.Name
		}
	}
	return ""
}

// ecoSummary composes the newcomer's narrative from the same data the cards
// render — deterministic, no drift.
func ecoSummary(t topology, m macroNode, del []deliveryNode, tags []glTag) string {
	names := make([]string, len(t.Services))
	for i, s := range t.Services {
		names[i] = s.Name
	}
	features := 0
	seen := map[string]bool{}
	for _, tg := range tags {
		if i := strings.Index(tg.Name, "-epic-"); i > 0 {
			key := tg.Name[i:]
			if j := strings.LastIndex(key, "."); j > 0 {
				key = key[:j]
			}
			if !seen[key] {
				seen[key] = true
				features++
			}
		}
	}
	b := &strings.Builder{}
	fmt.Fprintf(b, "%s bundles %d microservice charts (%s). ", m.Name, len(t.Services), strings.Join(names, ", "))
	b.WriteString("Each service publishes release candidates from main; every publish bumps the umbrella, ")
	if m.PublishedRC != "" {
		fmt.Fprintf(b, "which currently stands at %s. ", m.PublishedRC)
	} else {
		b.WriteString("which has no published RC yet. ")
	}
	for _, d := range del {
		switch d.State {
		case "current":
			fmt.Fprintf(b, "%s (%s) runs the current version. ", d.Env, d.Cluster)
		case "behind":
			fmt.Fprintf(b, "%s (%s) asks for %s — %d candidate(s) behind; the upgrade button delivers the newest (or any published version you type). ", d.Env, d.Cluster, d.Delivered, d.Behind)
		default:
			fmt.Fprintf(b, "%s (%s) asks for %s. ", d.Env, d.Cluster, d.Delivered)
		}
	}
	if features > 0 {
		fmt.Fprintf(b, "Feature branches (epic-*) publish their own umbrella versions — %d feature line(s) exist — and are delivered deliberately, never automatically.", features)
	} else {
		b.WriteString("Feature branches (epic-*) publish their own umbrella versions and are delivered deliberately, never automatically.")
	}
	return b.String()
}

// --- topology as config (multi-org: metaphor default, konstruct via env) ---

// topologyJSON is the TOPOLOGY env shape — lowercase mirrors of the structs.
type topologyJSON struct {
	Services []struct {
		Name    string `json:"name"`
		Project string `json:"project"`
		Chart   string `json:"chart"`
	} `json:"services"`
	Macro struct {
		Name      string `json:"name"`
		Project   string `json:"project"`
		File      string `json:"file"`
		TagPrefix string `json:"tagPrefix"`
	} `json:"macro"`
	Delivery []struct {
		Env     string `json:"env"`
		Cluster string `json:"cluster"`
		Project string `json:"project"`
		App     string `json:"app"`
	} `json:"delivery"`
}

// loadTopology returns the TOPOLOGY env override when present and valid,
// else the metaphor default. A broken override falls back loudly in logs
// rather than serving a half-empty view.
func loadTopology() topology {
	raw := os.Getenv("TOPOLOGY")
	if raw == "" {
		return defaultTopology()
	}
	var tj topologyJSON
	if err := json.Unmarshal([]byte(raw), &tj); err != nil || tj.Macro.Project == "" || len(tj.Services) == 0 {
		log.Printf("TOPOLOGY env invalid (%v) — using the metaphor default", err)
		return defaultTopology()
	}
	t := topology{
		MacroName: tj.Macro.Name, MacroProj: tj.Macro.Project,
		MacroFile: tj.Macro.File, MacroTag: tj.Macro.TagPrefix,
	}
	for _, s := range tj.Services {
		t.Services = append(t.Services, svcSpec{Name: s.Name, Project: s.Project, Chart: s.Chart})
	}
	for _, d := range tj.Delivery {
		t.Delivery = append(t.Delivery, deliverySpec{Env: d.Env, Cluster: d.Cluster, Project: d.Project, App: d.App})
	}
	log.Printf("TOPOLOGY: %d services, macro %s, %d delivery targets", len(t.Services), t.MacroProj, len(t.Delivery))
	return t
}

// deliveryFile reads a delivery app file — with the dedicated cross-group
// credential when configured, else the primary read token.
func (a *api) deliveryFile(ctx context.Context, proj, file string) string {
	if a.glDelivery == nil {
		return a.rawFile(ctx, proj, file)
	}
	ttl := ttlEco
	if a.isHot(proj) {
		ttl = 5 * time.Second
	}
	v, err := a.c.do("dfile:"+proj+":"+file, ttl, func() (any, error) {
		return a.glDelivery.fileRaw(ctx, proj, file, "main")
	})
	if err != nil {
		return ""
	}
	return v.(string)
}
