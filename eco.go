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
const themeVersion = "2.16.0"

const ttlEco = 45 * time.Second

type svcSpec struct {
	Name    string
	Project string // GitLab path, e.g. civo/metaphor/metaphor
	Chart   string // path to that repo's Chart.yaml (base version lives here)
	// Delivery: SINGLE-APP delivery targets — this app's chart pinned in an
	// environment directly, no umbrella in between. Same conventions
	// (epic/main/hotfix, trigger, release, pipelines) apply.
	Delivery []deliverySpec
}

type deliverySpec struct {
	Env     string
	Cluster string
	Project string // gitops repo path
	App     string // path to the ArgoCD Application yaml
	// Multi-target delivery (2026-08-27): control planes live on different
	// hosts with different credentials and write etiquettes.
	Kind     string // "gitlab" (default) | "github" (driver pending)
	Host     string // e.g. https://gitlab.kubefunk.net; "" = the primary host
	TokenEnv string // env var holding this target's token; "" = primary creds
	Write    string // "tag-pipeline" (default, metaphor CI) | "mr" | "commit"
	Branch   string // target branch of the gitops repo; "" = main
}

type topology struct {
	Services  []svcSpec
	MacroName string
	MacroProj string // "" = single-app mode: no umbrella, apps deliver directly
	MacroFile string
	MacroTag  string // tag prefix, e.g. "metaphor-v"
	Delivery  []deliverySpec
	// BranchPolicy "hotfix-only": app repos take changes on hotfix- branches
	// only — feature (epic-*) creation and epic-ref triggers are refused
	// (John 2026-08-27, the konstruct rule).
	BranchPolicy string
}

// hasMacro: umbrella mode vs single-app mode.
func (t topology) hasMacro() bool { return t.MacroProj != "" }

// defaultTopology is the metaphor supply chain. This theme is org-pinned to
// civo/metaphor and IS the metaphor delivery-view, so the topology is concrete.
func defaultTopology() topology {
	return topology{
		Services: []svcSpec{
			{Name: "metaphor", Project: "civo/metaphor/metaphor", Chart: "charts/metaphor/Chart.yaml"},
			{Name: "metaphor-dashboard-manager", Project: "civo/metaphor/metaphor-dashboard-manager", Chart: "charts/metaphor-dashboard-manager/Chart.yaml"},
			{Name: "metaphor-micro-frontend", Project: "civo/metaphor/metaphor-micro-frontend", Chart: "charts/metaphor-micro-frontend/Chart.yaml"},
		},
		MacroName: "metaphor-macro",
		MacroProj: "civo/metaphor/charts",
		MacroFile: "charts/metaphor-macro/Chart.yaml",
		MacroTag:  "metaphor-v",
		Delivery: []deliverySpec{
			{Env: "development-33", Cluster: "dev-33", Project: "civo/metaphor/metaphor-gitops",
				App: "registry/environments/development-33/dev-33/metaphor-macro.yaml", Write: "tag-pipeline"},
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
	// Source: the line this pinned build came from — "main", an epic/hotfix
	// branch id, or "unknown" when a sha-rc's commit can't be placed. Omitted
	// when the service can't be identified at all.
	Source string `json:"source,omitempty"`
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
	// Hotfixes: fresh hotfix branches on this repo — the tile's hotfix section.
	Hotfixes []branchJSON `json:"hotfixes,omitempty"`
	// BranchPipes: latest pipeline per epic/hotfix branch shown on the tile —
	// the "I pushed, where is it" answer, live.
	BranchPipes map[string]*pipelineJSON `json:"branch_pipes,omitempty"`
}

type macroNode struct {
	Name         string `json:"name"`
	Project      string `json:"project"`
	WebURL       string `json:"web_url"`
	BaseVer      string `json:"base_version"`
	PublishedRC  string `json:"published_rc"`
	PublishedTag string `json:"published_tag"`
	// BuiltFrom: the line the displayed umbrella was built from — "main" for
	// rc tags, the epic branch id for feature tags.
	BuiltFrom string `json:"built_from,omitempty"`
	// Tags: the newest published umbrella tags — the version picker's menu.
	Tags []string `json:"tags,omitempty"`
	// Bundle: the subchart pins inside the PUBLISHED umbrella (deps at the
	// tag ref; falls back to main's tip when the tag read misses — BundleRef
	// says which one you're looking at).
	Bundle        []depJSON     `json:"bundle,omitempty"`
	BundleRef     string        `json:"bundle_ref,omitempty"`
	LatestRelease *releaseJSON  `json:"latest_release,omitempty"`
	Pipeline      *pipelineJSON `json:"pipeline,omitempty"`
	Commit        *commitJSON   `json:"commit,omitempty"`
	SHAPipes      []shaPipeJSON `json:"sha_pipelines,omitempty"`
	// Hotfixes: fresh hotfix branches on the macro (charts) repo, and the
	// newest pipeline per epic/hotfix branch — the macro tile's live lanes.
	Hotfixes    []branchJSON             `json:"hotfixes,omitempty"`
	BranchPipes map[string]*pipelineJSON `json:"branch_pipes,omitempty"`
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
	// For: "" = the umbrella; an app name = single-app delivery of that app.
	For string `json:"for,omitempty"`
	// Ready=false: the target's credential/driver isn't configured here —
	// rendered as "credentials pending", never guessed at.
	Ready bool   `json:"ready"`
	Write string `json:"write,omitempty"` // tag-pipeline | mr | commit
	Kind  string `json:"kind,omitempty"`
	Host  string `json:"host,omitempty"`
}

// deliveryTargets flattens the umbrella-level and per-service targets.
func deliveryTargets(t topology) []struct {
	spec deliverySpec
	app  string
} {
	var out []struct {
		spec deliverySpec
		app  string
	}
	for _, d := range t.Delivery {
		out = append(out, struct {
			spec deliverySpec
			app  string
		}{d, ""})
	}
	for _, sv := range t.Services {
		for _, d := range sv.Delivery {
			out = append(out, struct {
				spec deliverySpec
				app  string
			}{d, sv.Name})
		}
	}
	return out
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
		pj.AuthorName = friendlyAuthor(pl.User.Name, pl.User.Username)
		pj.AuthorAvatar = pl.User.AvatarURL
	}
	return &pj
}

// reBotUser matches GitLab access-token bot usernames (group_<id>_bot_<hex>).
var reBotUser = regexp.MustCompile(`^(group|project)_\d+_bot_`)

// friendlyAuthor un-masks GitLab's redacted bot identities: access-token bot
// users surface with display name "****", which reads terribly in a tooltip.
// Real names pass through untouched.
func friendlyAuthor(name, username string) string {
	if strings.Trim(name, "* ") != "" {
		return name
	}
	if m := reBotUser.FindString(username); m != "" {
		return "token bot (" + strings.TrimSuffix(m, "_bot_") + ")"
	}
	if username != "" {
		return username
	}
	return "someone"
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

// depSource classifies where a bundled dependency's pinned build came from:
// a branch-suffixed version → that branch id; a counter rc or plain release →
// "main"; a sha-suffixed rc → the branch its commit lives on (main preferred,
// then a hotfix line), looked up once and cached 6h. "" when the service name
// isn't in the topology (nothing to look up), "unknown" when the lookup fails
// or the commit is on no branch we can name.
func (a *api) depSource(ctx context.Context, t topology, name, version string) string {
	if m := reBuiltFrom.FindStringSubmatch(version); m != nil {
		return m[1] // epic-/hotfix- branch line, straight off the version
	}
	sha := rcSHA(version)
	if sha == "" {
		return "main" // counter rc or plain release rides main
	}
	proj := ""
	for _, s := range t.Services {
		if s.Name == name {
			proj = s.Project
			break
		}
	}
	if proj == "" {
		return "" // not a topology service — omit rather than guess
	}
	v, err := a.c.do("cref:"+proj+":"+sha, 6*time.Hour, func() (any, error) {
		return a.gl.commitRefs(ctx, proj, sha)
	})
	if err != nil {
		return "unknown"
	}
	refs, _ := v.([]glRef)
	for _, r := range refs {
		if r.Name == "main" {
			return "main"
		}
	}
	for _, r := range refs {
		if strings.HasPrefix(r.Name, "hotfix") {
			return r.Name
		}
	}
	if len(refs) > 0 {
		return refs[0].Name
	}
	return "unknown"
}

// depsWithSource enriches bundle deps with their provenance line. Called from
// inside the cached bundle closures so the (possibly networked) lookups run
// once per cached bundle, not once per request.
func (a *api) depsWithSource(ctx context.Context, t topology, deps []depJSON) []depJSON {
	out := make([]depJSON, len(deps))
	for i, d := range deps {
		d.Source = a.depSource(ctx, t, d.Name, d.Version)
		out[i] = d
	}
	return out
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
		allTargets             = deliveryTargets(t)
		delRaw                 = make([]string, len(allTargets))
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
	type tileBranches struct {
		feats, hots []branchJSON
		pipes       map[string]*pipelineJSON
	}
	svcTB := make([]tileBranches, len(t.Services))
	svcRels := make([]*releaseJSON, len(t.Services))
	var macroRel *releaseJSON
	var macroTB tileBranches
	total := 5 + len(t.Services)*4 + len(allTargets)
	ch := make(chan slot, total)
	go func() { ch <- slot{"macroRaw", 0, a.rawFile(ctx, t.MacroProj, t.MacroFile)} }()
	go func() { ch <- slot{"tag", 0, newestTag(a.cachedTags(ctx, t.MacroProj), t.MacroTag)} }()
	go func() { ch <- slot{"macroPipe", 0, a.pipeBundleFor(ctx, t.MacroProj)} }()
	go func() { ch <- slot{"macroRel", 0, a.cachedRelease(ctx, t.MacroProj)} }()
	go func() {
		f, h, p := a.tileBranchData(ctx, t.MacroProj)
		ch <- slot{"macroTB", 0, tileBranches{f, h, p}}
	}()
	for i, s := range t.Services {
		i, s := i, s
		go func() { ch <- slot{"svcRaw", i, a.rawFile(ctx, s.Project, s.Chart)} }()
		go func() { ch <- slot{"svcPipe", i, a.pipeBundleFor(ctx, s.Project)} }()
		go func() {
			f, h, p := a.tileBranchData(ctx, s.Project)
			ch <- slot{"svcFeat", i, tileBranches{f, h, p}}
		}()
		go func() { ch <- slot{"svcRel", i, a.cachedRelease(ctx, s.Project)} }()
	}
	for i, dt := range allTargets {
		i, dt := i, dt
		go func() { ch <- slot{"delRaw", i, a.deliveryFileFor(ctx, dt.spec)} }()
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
				svcTB[s.i], _ = s.v.(tileBranches)
			case "svcRel":
				svcRels[s.i], _ = s.v.(*releaseJSON)
			case "macroRel":
				macroRel, _ = s.v.(*releaseJSON)
			case "macroTB":
				macroTB, _ = s.v.(tileBranches)
			}
		case <-deadline:
			break collect
		}
	}

	deps := macroDeps(macroRaw)
	publishedRC := strings.TrimPrefix(publishedTag, t.MacroTag)
	// The bundle tree shows what's inside the PUBLISHED umbrella — deps read
	// at the tag ref (immutable, so cached long). A miss falls back to main's
	// tip, which in steady state is the same pins. The fallback enrichment is
	// deadline-bound like its siblings: it runs outside the fan-out's collect
	// window, and cold provenance lookups must never hold the whole handler.
	fbctx, fbcancel := context.WithTimeout(ctx, 5*time.Second)
	bundle, bundleRef := a.depsWithSource(fbctx, t, orderedDeps(macroRaw)), "main"
	fbcancel()
	if publishedTag != "" {
		if v, err := a.c.do("bundle:"+t.MacroProj+"@"+publishedTag, time.Hour, func() (any, error) {
			bctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			raw, err := a.gl.fileRaw(bctx, t.MacroProj, t.MacroFile, publishedTag)
			if err != nil {
				return nil, err
			}
			return a.depsWithSource(bctx, t, orderedDeps(raw)), nil
		}); err == nil {
			bundle, bundleRef = v.([]depJSON), publishedTag
		}
	}
	macro := macroNode{
		Name: t.MacroName, Project: t.MacroProj,
		WebURL:  a.gl.base + "/" + t.MacroProj,
		BaseVer: chartVersion(macroRaw), PublishedRC: publishedRC, PublishedTag: publishedTag,
		Bundle: bundle, BundleRef: bundleRef, LatestRelease: macroRel,
		BuiltFrom: builtFrom(publishedTag),
		Pipeline:  macroPB.Pipe, Commit: macroPB.Commit, SHAPipes: macroPB.Pipes,
		Hotfixes: macroTB.hots, BranchPipes: macroTB.pipes,
	}
	for _, tg := range a.cachedTags(ctx, t.MacroProj) {
		if strings.HasPrefix(tg.Name, t.MacroTag) {
			macro.Tags = append(macro.Tags, tg.Name)
		}
		if len(macro.Tags) >= 12 {
			break
		}
	}

	services := make([]svcNode, len(t.Services))
	for i, s := range t.Services {
		services[i] = svcNode{
			Name: s.Name, Project: s.Project,
			WebURL:  a.gl.base + "/" + s.Project,
			BaseVer: chartVersion(svcRaw[i]), Bundled: deps[s.Name],
			Pipeline: svcPB[i].Pipe, Commit: svcPB[i].Commit, SHAPipes: svcPB[i].Pipes,
			Features: svcTB[i].feats, Hotfixes: svcTB[i].hots,
			BranchPipes: svcTB[i].pipes, LatestRelease: svcRels[i],
		}
		// A service run just finished: its dep-bump is pushing the next macro
		// RC — put the macro on the fast lane so the handoff shows promptly.
		if svcPB[i].Pipe != nil && a.noteServicePipeline(s.Project, svcPB[i].Pipe.Status) {
			a.markHot(t.MacroProj)
		}
	}

	delivery := make([]deliveryNode, len(allTargets))
	for i, dt := range allTargets {
		d := dt.spec
		host := d.Host
		if host == "" {
			host = a.gl.base
		}
		br := d.Branch
		if br == "" {
			br = "main"
		}
		node := deliveryNode{
			Env: d.Env, Cluster: d.Cluster, For: dt.app,
			WebURL: host + "/" + d.Project + "/-/blob/" + br + "/" + d.App,
			Ready:  a.clientFor(d) != nil, Write: d.Write, Kind: d.Kind, Host: d.Host,
		}
		if node.Ready {
			node.Delivered = targetRevision(delRaw[i])
			ref := publishedRC
			if dt.app != "" {
				// single-app target: compare against the app's own version
				// (its umbrella pin when there is one, else its base chart)
				for _, sv := range services {
					if sv.Name == dt.app {
						ref = sv.Bundled
						if ref == "" {
							ref = sv.BaseVer
						}
					}
				}
			}
			node.State, node.Behind = drift(node.Delivered, ref)
		} else {
			node.State = "pending"
		}
		delivery[i] = node
	}

	// One org-wide branch pass feeds BOTH the promotion matrix and the org
	// hotfix rollup — the same repoBranches, so the two views can't drift.
	repos := a.repoBranchesAll(ctx, t)
	writeJSON(w, map[string]any{
		"generated_at": time.Now().UTC(),
		"macro":        macro,
		"services":     services,
		"delivery":     delivery,
		"summary":      ecoSummary(t, macro, delivery, a.cachedTags(ctx, t.MacroProj)),
		"promotions":   a.promotions(ctx, t, delivery, repos),
		"org_hotfixes": assembleOrgHotfixes(t, repos),
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
	// Behind: commits on main this branch hasn't taken — drift the other way.
	// Same nil-means-unchecked contract as Ahead.
	Behind *int `json:"behind,omitempty"`
	// AuthorAvatar: the last committer's avatar — who's changing what.
	AuthorAvatar string `json:"author_avatar,omitempty"`
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
	// ChartVer: the umbrella chart version a hotfix lane's dep-bump produced,
	// read off the macro repo's dep-bump commits (hotfix lanes only; epic
	// lanes carry their pin in featSvcJSON.Version instead).
	ChartVer string `json:"chart_ver,omitempty"`
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
		mainTip := ""
		for i, b := range brs {
			out = append(out, toBranchJSON(b))
			if b.Name == "main" && b.Commit != nil {
				mainTip = b.Commit.ShortID
			}
			// last-committer avatar: 6h per-email cache, so every branch gets
			// one. Bot-authored commits carry the masked "****" display name
			// but a group_<id>_bot_* noreply local-part — un-mask like
			// pipeline attribution does.
			if b.Commit != nil {
				out[i].AuthorAvatar = a.avatarFor(ctx, b.Commit.AuthorEmail)
				if lp, _, ok := strings.Cut(b.Commit.AuthorEmail, "@"); ok {
					out[i].Author = friendlyAuthor(out[i].Author, lp)
				}
			}
		}
		// divergence vs main: hotfix lanes (10 newest) + live epic lanes
		// (6 newest; hard cap 16 combined). Results are keyed by BOTH tips —
		// the branch's and main's — so nothing recomputes while neither side
		// moves, and a merge or fresh main commit invalidates immediately.
		dix := []int{}
		for i, bj := range out {
			if strings.HasPrefix(bj.Name, "hotfix") {
				dix = append(dix, i)
			}
		}
		sort.Slice(dix, func(x, y int) bool { return out[dix[x]].When > out[dix[y]].When })
		if len(dix) > 10 {
			dix = dix[:10]
		}
		eix := []int{}
		for i, bj := range out {
			if strings.HasPrefix(bj.Name, "epic-") && !bj.Stale {
				eix = append(eix, i)
			}
		}
		sort.Slice(eix, func(x, y int) bool { return out[eix[x]].When > out[eix[y]].When })
		if len(eix) > 6 {
			eix = eix[:6]
		}
		dix = append(dix, eix...)
		if len(dix) > 16 {
			dix = dix[:16]
		}
		for _, i := range dix {
			d, ok := a.divergence(ctx, project, out[i].Name, out[i].Short, mainTip)
			if !ok {
				continue
			}
			ah, bh := d.Ahead, d.Behind
			out[i].Ahead, out[i].Behind = &ah, &bh
			out[i].CompareURL = a.gl.base + "/" + project + "/-/compare/main..." + url.PathEscape(out[i].Name)
			out[i].Committers = d.Committers
		}
		return out, nil
	})
	if err != nil {
		return nil, err
	}
	return v.([]branchJSON), nil
}

// reBuiltFrom pulls the prerelease line out of an umbrella tag/version:
// "...-epic-106-background.2" → "epic-106-background",
// "...-hotfix-0-9.2" → "hotfix-0-9"; no match → main line. End-anchored on
// purpose: a sha-rc like "0.7.8-rc.57f3903b" must NOT match (→ "main").
var reBuiltFrom = regexp.MustCompile(`-((?:epic|hotfix)-[a-z0-9-]+)\.[0-9]+$`)

// reShaRC matches a sha-suffixed rc tail (konstruct: -rc.57f3903b). The
// capture is hex, but callers still reject all-digit captures — a metaphor
// counter like -rc.13 is NOT a sha (see rcSHA).
var reShaRC = regexp.MustCompile(`-rc\.([0-9a-f]{7,12})$`)

// rcSHA returns the commit sha a sha-suffixed rc version carries, or "" when
// the version is a counter rc (-rc.13), a branch line, or a plain release.
func rcSHA(version string) string {
	m := reShaRC.FindStringSubmatch(strings.TrimSpace(version))
	if m == nil {
		return ""
	}
	for _, r := range m[1] {
		if r < '0' || r > '9' {
			return m[1] // has a hex letter → a real sha, not a counter
		}
	}
	return "" // all digits → a counter, not a sha
}

// shortName is a repo's display name: the last path segment (civo/metaphor/
// charts → charts).
func shortName(project string) string {
	if i := strings.LastIndex(project, "/"); i >= 0 {
		return project[i+1:]
	}
	return project
}

func builtFrom(tagOrVer string) string {
	if m := reBuiltFrom.FindStringSubmatch(tagOrVer); m != nil {
		return m[1]
	}
	if tagOrVer == "" {
		return ""
	}
	return "main"
}

// freshHotfixes: a repo's non-stale hotfix branches, newest first, tile-sized.
func (a *api) freshHotfixes(ctx context.Context, project string) []branchJSON {
	brs, err := a.cachedBranches(ctx, project)
	if err != nil {
		return nil
	}
	var out []branchJSON
	for _, b := range brs {
		if strings.HasPrefix(b.Name, "hotfix") && !b.Stale && !a.branchDeleted(project, b.Name) {
			out = append(out, b)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].When > out[j].When })
	if len(out) > 4 {
		out = out[:4]
	}
	return out
}

// branchPipe: the newest pipeline on a specific branch — 30s cache, 5s on the
// hot lane so a fresh push shows up while you're watching.
func (a *api) branchPipe(ctx context.Context, project, ref string) *pipelineJSON {
	ttl := 30 * time.Second
	if a.isHot(project) {
		ttl = 5 * time.Second
	}
	v, err := a.c.do("bp:"+project+"@"+ref, ttl, func() (any, error) {
		pls, err := a.gl.recentPipelines(ctx, project, ref, "", 1)
		if err != nil {
			return nil, err
		}
		if len(pls) == 0 {
			return (*pipelineJSON)(nil), nil
		}
		p := pls[0]
		pj := pipelineJSON{ID: p.ID, Status: p.Status, Ref: p.Ref,
			WebURL: p.WebURL, CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt,
			DurationS: p.UpdatedAt.Sub(p.CreatedAt).Seconds()}
		return &pj, nil
	})
	if err != nil {
		return nil
	}
	p, _ := v.(*pipelineJSON)
	return p
}

// tileBranchData bundles a repo's tile-facing branch intel: fresh hotfixes,
// active features, and the newest pipeline for each of those branches.
func (a *api) tileBranchData(ctx context.Context, project string) ([]branchJSON, []branchJSON, map[string]*pipelineJSON) {
	feats := a.activeFeatures(ctx, project)
	hots := a.freshHotfixes(ctx, project)
	pipes := map[string]*pipelineJSON{}
	for _, b := range append(append([]branchJSON{}, feats...), hots...) {
		if p := a.branchPipe(ctx, project, b.Name); p != nil {
			pipes[b.Name] = p
		}
	}
	return feats, hots, pipes
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
	State string `json:"state"`
	// Version: the feature-branch pin this service carries in the charts
	// branch (set only when State is "updated") — the umbrella dep line.
	Version    string     `json:"version,omitempty"`
	When       string     `json:"when,omitempty"`
	WebURL     string     `json:"web_url,omitempty"`
	MRIID      int        `json:"mr_iid,omitempty"`
	MRURL      string     `json:"mr_url,omitempty"`
	MRState    string     `json:"mr_state,omitempty"`
	MRMergedAt *time.Time `json:"mr_merged_at,omitempty"`
	// Who last touched this repo's branch, and how far it has drifted from
	// main — same contracts as branchJSON (nil = unchecked).
	Author       string `json:"author,omitempty"`
	AuthorAvatar string `json:"author_avatar,omitempty"`
	Ahead        *int   `json:"ahead,omitempty"`
	Behind       *int   `json:"behind,omitempty"`
	CompareURL   string `json:"compare_url,omitempty"`
}

// featEnvJSON: one feature's presence in one delivery environment.
// State: "direct" (env runs the feature's own -branch. umbrella), "via-rc"
// (feature merged and the env runs an rc cut after the merge), "missing".
type featEnvJSON struct {
	Env     string `json:"env"`
	State   string `json:"state"`
	Version string `json:"version,omitempty"`
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
	// Merged: every service the feature actually updated has its carrying MR
	// merged (and at least one exists). MergedAt is the latest of them — the
	// instant the feature's content started riding main.
	Merged   bool          `json:"merged,omitempty"`
	MergedAt *time.Time    `json:"merged_at,omitempty"`
	Envs     []featEnvJSON `json:"envs,omitempty"`
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
		b            branchJSON
	}
	found := map[string]map[string]presence{} // branch → project → presence
	chartsHas := map[string]string{}          // branch → when (macro repo)
	newest := map[string]string{}
	for _, rb := range repos {
		for _, b := range rb.Epic {
			if found[b.Name] == nil {
				found[b.Name] = map[string]presence{}
			}
			found[b.Name][rb.Project] = presence{b.When, b.WebURL, b}
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
		lookupMR := func(project string) *glMR {
			v, err := a.c.do("mrs:"+project+"@"+name, ttlBranches, func() (any, error) {
				bctx, cancel := context.WithTimeout(ctx, 5*time.Second)
				defer cancel()
				return a.gl.mrsBySource(bctx, project, name)
			})
			if err != nil {
				return nil
			}
			list, _ := v.([]glMR)
			return bestMR(list)
		}
		for _, svc := range t.Services {
			fs := featSvcJSON{Name: svc.Name, Project: svc.Project, State: "main"}
			if p, ok := found[name][svc.Project]; ok {
				fs.State, fs.When, fs.WebURL = "joined", p.when, p.webURL
				fs.Author, fs.AuthorAvatar = p.b.Author, p.b.AuthorAvatar
				fs.Ahead, fs.Behind, fs.CompareURL = p.b.Ahead, p.b.Behind, p.b.CompareURL
				if strings.Contains(pins[svc.Name], needle) {
					fs.State, fs.Version = "updated", pins[svc.Name]
				}
				if m := lookupMR(svc.Project); m != nil {
					fs.MRIID, fs.MRURL, fs.MRState = m.IID, m.WebURL, mrStateLabel(*m)
					if m.State == "merged" {
						fs.MRMergedAt = m.MergedAt
					}
				}
			} else if m := lookupMR(svc.Project); m != nil && m.State == "merged" {
				// merging deletes the source branch — the work still happened
				// here, and it MUST count or the epic never closes as Done
				fs.State = "merged"
				fs.MRIID, fs.MRURL, fs.MRState = m.IID, m.WebURL, "merged"
				fs.MRMergedAt = m.MergedAt
			}
			f.Services = append(f.Services, fs)
		}
		// merged = at least one carrying MR is merged AND no in-flight work
		// remains open. Post-merge branch deletion turns rows into state
		// "merged", which counts — joined-but-untouched rows never block.
		mergedN, openN := 0, 0
		for _, fs := range f.Services {
			switch {
			case fs.MRState == "merged":
				mergedN++
				if fs.MRMergedAt != nil && (f.MergedAt == nil || fs.MRMergedAt.After(*f.MergedAt)) {
					f.MergedAt = fs.MRMergedAt
				}
			case fs.State == "updated":
				openN++
			}
		}
		f.Merged = mergedN > 0 && openN == 0
		out = append(out, f)
	}
	return out
}

// promotionRows answers "which features are not yet in an environment": for
// every feature × delivery env — direct (the env runs the feature's own
// umbrella), via-rc (merged, and the env's rc tag was cut after the merge —
// the rc carries it), or missing.
func promotionRows(features []featureJSON, delivery []deliveryNode, tags []glTag, tagPrefix string) []featureJSON {
	tagTime := map[string]*time.Time{}
	for _, t := range tags {
		tagTime[t.Name] = t.Commit.CreatedAt
	}
	for fi := range features {
		f := &features[fi]
		needle := "-" + reBranchID.ReplaceAllString(strings.ToLower(f.Branch), "-") + "."
		for _, d := range delivery {
			e := featEnvJSON{Env: d.Env, State: "missing", Version: d.Delivered}
			switch {
			case strings.Contains(d.Delivered, needle):
				e.State = "direct"
			case f.Merged && f.MergedAt != nil:
				if ct := tagTime[tagPrefix+d.Delivered]; ct != nil && ct.After(*f.MergedAt) {
					e.State = "via-rc"
				}
			}
			f.Envs = append(f.Envs, e)
		}
	}
	return features
}

// branchesView lists every topology repo's branches grouped into lanes, plus
// the macro repo's newest tags — the release/feature timeline in one payload.
// repoBranchesAll assembles every topology repo's lane-grouped branches —
// shared by the Branches tab and the ecosystem's promotion summary.
func (a *api) repoBranchesAll(ctx context.Context, t topology) []repoBranches {
	repos := append([]svcSpec{}, t.Services...)
	if t.hasMacro() {
		repos = append(repos, svcSpec{Name: t.MacroName, Project: t.MacroProj})
	}
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
	// every branch gets its end-result umbrella version: the newest macro tag
	// whose prerelease id matches the branch (main → the newest rc), or — for
	// a konstruct hotfix tip — the tag ending -rc.<shortSHA>. Hotfix lanes also
	// get the umbrella chart version their dep-bump produced.
	allTags := a.cachedTags(ctx, t.MacroProj)
	chartsCommits := a.cachedChartsCommits(ctx, t)
	for ri := range out {
		for _, lane := range [][]branchJSON{out[ri].Main, out[ri].Hotfix, out[ri].Epic} {
			for bi := range lane {
				if ver, tag := macroTagForTip(allTags, t.MacroTag, lane[bi].Name, lane[bi].Short); ver != "" {
					lane[bi].MacroVer = ver
					lane[bi].MacroURL = a.gl.base + "/" + t.MacroProj + "/-/tags/" + url.PathEscape(tag)
				}
				if strings.HasPrefix(lane[bi].Name, "hotfix") {
					needle := "-" + reBranchID.ReplaceAllString(strings.ToLower(lane[bi].Name), "-") + "."
					if cv := chartVerFor(chartsCommits, out[ri].Name, needle, lane[bi].Short); cv != "" {
						lane[bi].ChartVer = cv
					}
				}
			}
		}
	}
	return out
}

func (a *api) branchesView(w http.ResponseWriter, r *http.Request) {
	ctx := context.Background()
	t := a.topo
	out := a.repoBranchesAll(ctx, t)
	allTags := a.cachedTags(ctx, t.MacroProj)
	tags := []string{}
	for _, tg := range allTags {
		tags = append(tags, tg.Name)
		if len(tags) >= 12 {
			break
		}
	}
	writeJSON(w, map[string]any{"repos": out, "macro_tags": tags,
		"features": a.assembleFeatures(ctx, t, out, allTags),
		"group":    strings.TrimSuffix(t.MacroProj, "/"+t.MacroProj[strings.LastIndex(t.MacroProj, "/")+1:])})
}

// promotions builds the feature→environment matrix and runs the merge
// observer: a feature whose every carrying MR is merged closes its epic as
// Done (John's lifecycle — merge means the content rides main; promotion is
// tracked HERE from then on, not on the epic).
func (a *api) promotions(ctx context.Context, t topology, delivery []deliveryNode, repos []repoBranches) []featureJSON {
	tags := a.cachedTags(ctx, t.MacroProj)
	feats := a.assembleFeatures(ctx, t, repos, tags)
	feats = promotionRows(feats, delivery, tags, t.MacroTag)
	a.observeMerged(ctx, feats)
	a.observeChartsTwins(ctx, t, feats)
	return feats
}

// observeChartsTwins late-creates the charts branch for any active feature
// missing it — a branch pushed straight from a terminal gets its umbrella
// line without anyone clicking anything (John: any time a branch is created
// in one, it should be created in charts as well).
func (a *api) observeChartsTwins(ctx context.Context, t topology, feats []featureJSON) {
	if a.act == nil || !a.act.enabled() || !t.hasMacro() {
		return
	}
	for _, f := range feats {
		if f.Charts {
			continue
		}
		a.hotMu.Lock()
		seen := a.chartsTwinned[f.Branch]
		a.hotMu.Unlock()
		if seen {
			continue
		}
		if _, err := a.act.gl.createBranch(ctx, t.MacroProj, f.Branch, "main"); err == nil ||
			strings.Contains(err.Error(), "already exists") {
			log.Printf("LIFECYCLE charts twin created branch=%s", f.Branch)
			a.c.drop("br:" + t.MacroProj)
		} else {
			log.Printf("LIFECYCLE charts twin failed branch=%s err=%v", f.Branch, err)
		}
		a.hotMu.Lock()
		a.chartsTwinned[f.Branch] = true
		a.hotMu.Unlock()
	}
}

// observeMerged closes fully-merged features' epics (Done) exactly once —
// whether the merge happened through foxglider's button or raw GitLab.
func (a *api) observeMerged(ctx context.Context, feats []featureJSON) {
	if a.act == nil || !a.act.enabled() {
		return
	}
	group := a.orgGroup()
	for _, f := range feats {
		if f.EpicIID == 0 || !f.Merged {
			continue
		}
		a.hotMu.Lock()
		seen := a.epicClosed[f.EpicIID]
		a.hotMu.Unlock()
		if seen {
			continue
		}
		ep, err := a.act.gl.epicByIID(ctx, group, f.EpicIID)
		if err != nil {
			continue
		}
		if ep.State != "opened" {
			a.hotMu.Lock()
			a.epicClosed[f.EpicIID] = true
			a.hotMu.Unlock()
			continue
		}
		if err := a.act.gl.epicUpdate(ctx, group, f.EpicIID,
			"status::Done", "status::In Review,status::In Progress", "close"); err == nil {
			log.Printf("LIFECYCLE epic=%d merged→Done+closed branch=%s", f.EpicIID, f.Branch)
			a.hotMu.Lock()
			a.epicClosed[f.EpicIID] = true
			a.hotMu.Unlock()
		} else {
			log.Printf("LIFECYCLE epic=%d Done+close failed: %v", f.EpicIID, err)
		}
	}
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

// bundleAt answers the umbrella version picker: the subchart pins inside ANY
// published umbrella tag (immutable → cached long), plus the line it was
// built from. ?tag= must be one of the macro repo's published tags.
func (a *api) bundleAt(w http.ResponseWriter, r *http.Request) {
	tag := strings.TrimSpace(r.URL.Query().Get("tag"))
	t := a.topo
	if tag == "" || !strings.HasPrefix(tag, t.MacroTag) || len(tag) > 128 {
		writeErr(w, 400, "tag must be a published umbrella tag")
		return
	}
	ctx := context.Background()
	v, err := a.c.do("bundle:"+t.MacroProj+"@"+tag, time.Hour, func() (any, error) {
		bctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		raw, err := a.gl.fileRaw(bctx, t.MacroProj, t.MacroFile, tag)
		if err != nil {
			return nil, err
		}
		return a.depsWithSource(bctx, t, orderedDeps(raw)), nil
	})
	if err != nil {
		writeErr(w, 404, "that tag's chart could not be read: "+err.Error())
		return
	}
	deps, _ := v.([]depJSON)
	writeJSON(w, map[string]any{
		"tag": tag, "version": strings.TrimPrefix(tag, t.MacroTag),
		"built_from": builtFrom(tag), "deps": deps,
	})
}

// branchDivergence is the sha-keyed compare result for one branch tip.
type branchDivergence struct {
	Ahead, Behind int
	Committers    []committerJSON
}

// divergence computes a branch's drift vs main, cached by BOTH tips — ahead
// and behind are functions of the branch tip AND main's tip, so the key must
// carry each: a merge to main (behind changes, ahead may drop to 0) busts the
// cache immediately instead of lingering for the TTL.
func (a *api) divergence(ctx context.Context, project, branch, tip, mainTip string) (branchDivergence, bool) {
	v, err := a.c.do("div:"+project+":"+branch+"@"+tip+"~"+mainTip, 6*time.Hour, func() (any, error) {
		ahead, people, err := a.gl.compareBranch(ctx, project, "main", branch)
		if err != nil {
			return nil, err
		}
		behind, _, err := a.gl.compareBranch(ctx, project, branch, "main")
		if err != nil {
			return nil, err
		}
		d := branchDivergence{Ahead: ahead, Behind: behind}
		for _, p := range people {
			d.Committers = append(d.Committers,
				committerJSON{Name: p.AuthorName, Avatar: a.avatarFor(ctx, p.AuthorEmail)})
		}
		return d, nil
	})
	if err != nil {
		return branchDivergence{}, false
	}
	d, _ := v.(branchDivergence)
	return d, true
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
	if prefix == "" {
		return "" // single-app mode: no umbrella tags to map
	}
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

// macroTagForTip is macroTagFor with a konstruct-hotfix fallback: when the
// branch-name needle matches no tag, any tag ending -rc.<shortSHA> (a hotfix
// tip's sha-suffixed umbrella) wins. Returns (version, tag) or empties.
func macroTagForTip(tags []glTag, prefix, branch, shortSHA string) (string, string) {
	if tag := macroTagFor(tags, prefix, branch); tag != "" {
		return strings.TrimPrefix(tag, prefix), tag
	}
	if prefix == "" || shortSHA == "" {
		return "", ""
	}
	suffix := "-rc." + shortSHA
	for _, t := range tags {
		if strings.HasPrefix(t.Name, prefix) && strings.HasSuffix(t.Name, suffix) {
			return strings.TrimPrefix(t.Name, prefix), t.Name
		}
	}
	return "", ""
}

// reDepBump matches the macro repo's dependency-bump commits — the trail a
// service's publish leaves in the umbrella ("ci: update <name> dependency to
// <version>").
var reDepBump = regexp.MustCompile(`^ci: (?:update|bump) ([A-Za-z0-9._-]+) dependency to (\S+)$`)

// chartVerFor finds the newest macro dep-bump for svcName whose bumped version
// carries branchNeedle (the sanitized "-<branch>.") or ends with tipSHA — the
// umbrella chart version a hotfix lane's publish produced. "" when none match.
func chartVerFor(commits []glCommit, svcName, branchNeedle, tipSHA string) string {
	for _, c := range commits {
		m := reDepBump.FindStringSubmatch(strings.TrimSpace(c.Title))
		if m == nil || m[1] != svcName {
			continue
		}
		ver := m[2]
		if branchNeedle != "" && strings.Contains(ver, branchNeedle) {
			return ver
		}
		if tipSHA != "" && strings.HasSuffix(ver, tipSHA) {
			return ver
		}
	}
	return ""
}

// cachedChartsCommits lists the macro repo's ~50 newest main commits (120s
// cache) — the dep-bump trail hotfix ChartVer resolution reads. Nil in
// single-app mode (no umbrella to bump).
func (a *api) cachedChartsCommits(ctx context.Context, t topology) []glCommit {
	if !t.hasMacro() {
		return nil
	}
	v, err := a.c.do("mc:"+t.MacroProj, 120*time.Second, func() (any, error) {
		return a.gl.commitsRange(ctx, t.MacroProj, "main", time.Time{}, time.Time{}, 50)
	})
	if err != nil {
		return nil
	}
	cs, _ := v.([]glCommit)
	return cs
}

// orgHotfixRepo is one repo's cell in an org-hotfix row: whether it carries the
// hotfix branch and, if so, that branch's enriched metadata.
type orgHotfixRepo struct {
	Name     string `json:"name"`
	Project  string `json:"project"`
	Has      bool   `json:"has"`
	When     string `json:"when,omitempty"`
	WebURL   string `json:"web_url,omitempty"`
	ChartVer string `json:"chart_ver,omitempty"`
	MacroVer string `json:"macro_ver,omitempty"`
	MacroURL string `json:"macro_url,omitempty"`
	// Who last touched this repo's branch, and its drift from main.
	Author       string `json:"author,omitempty"`
	AuthorAvatar string `json:"author_avatar,omitempty"`
	Ahead        *int   `json:"ahead,omitempty"`
	Behind       *int   `json:"behind,omitempty"`
	CompareURL   string `json:"compare_url,omitempty"`
}

// orgHotfixJSON is one hotfix branch across the whole org: a cell per topology
// service plus the macro repo, so a hotfix that spans repos reads as one row.
type orgHotfixJSON struct {
	Branch string          `json:"branch"`
	When   string          `json:"when"`
	Repos  []orgHotfixRepo `json:"repos"`
}

// assembleOrgHotfixes groups non-stale hotfix branches by name across every
// topology service and the macro repo — each row carries a cell for every
// repo (present or not), newest-first, capped at 6.
func assembleOrgHotfixes(t topology, repos []repoBranches) []orgHotfixJSON {
	byBranch := map[string]map[string]branchJSON{} // branch → project → entry
	newest := map[string]string{}
	for _, rb := range repos {
		for _, b := range rb.Hotfix {
			if b.Stale {
				continue
			}
			if byBranch[b.Name] == nil {
				byBranch[b.Name] = map[string]branchJSON{}
			}
			byBranch[b.Name][rb.Project] = b
			if b.When > newest[b.Name] {
				newest[b.Name] = b.When
			}
		}
	}
	type col struct{ name, project string }
	cols := make([]col, 0, len(t.Services)+1)
	for _, s := range t.Services {
		cols = append(cols, col{shortName(s.Project), s.Project})
	}
	if t.hasMacro() {
		cols = append(cols, col{shortName(t.MacroProj), t.MacroProj})
	}
	names := make([]string, 0, len(byBranch))
	for n := range byBranch {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool { return newest[names[i]] > newest[names[j]] })
	if len(names) > 6 {
		names = names[:6]
	}
	out := make([]orgHotfixJSON, 0, len(names))
	for _, name := range names {
		row := orgHotfixJSON{Branch: name, When: newest[name], Repos: make([]orgHotfixRepo, 0, len(cols))}
		for _, c := range cols {
			cell := orgHotfixRepo{Name: c.name, Project: c.project}
			if b, ok := byBranch[name][c.project]; ok {
				cell.Has = true
				cell.When, cell.WebURL = b.When, b.WebURL
				cell.ChartVer, cell.MacroVer, cell.MacroURL = b.ChartVer, b.MacroVer, b.MacroURL
				cell.Author, cell.AuthorAvatar = b.Author, b.AuthorAvatar
				cell.Ahead, cell.Behind, cell.CompareURL = b.Ahead, b.Behind, b.CompareURL
			}
			row.Repos = append(row.Repos, cell)
		}
		out = append(out, row)
	}
	return out
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
type deliveryJSON struct {
	Env      string `json:"env"`
	Cluster  string `json:"cluster"`
	Project  string `json:"project"`
	App      string `json:"app"`
	Kind     string `json:"kind"`
	Host     string `json:"host"`
	TokenEnv string `json:"token_env"`
	Write    string `json:"write"`
	Branch   string `json:"branch"`
}

func (d deliveryJSON) spec() deliverySpec {
	return deliverySpec{Env: d.Env, Cluster: d.Cluster, Project: d.Project, App: d.App,
		Kind: d.Kind, Host: d.Host, TokenEnv: d.TokenEnv, Write: d.Write, Branch: d.Branch}
}

type topologyJSON struct {
	Services []struct {
		Name     string         `json:"name"`
		Project  string         `json:"project"`
		Chart    string         `json:"chart"`
		Delivery []deliveryJSON `json:"delivery"`
	} `json:"services"`
	Macro struct {
		Name      string `json:"name"`
		Project   string `json:"project"`
		File      string `json:"file"`
		TagPrefix string `json:"tagPrefix"`
	} `json:"macro"`
	Delivery     []deliveryJSON `json:"delivery"`
	BranchPolicy string         `json:"branch_policy"`
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
	// macro is OPTIONAL now (single-app mode) — only services are mandatory
	if err := json.Unmarshal([]byte(raw), &tj); err != nil || len(tj.Services) == 0 {
		log.Printf("TOPOLOGY env invalid (%v) — using the metaphor default", err)
		return defaultTopology()
	}
	t := topology{
		MacroName: tj.Macro.Name, MacroProj: tj.Macro.Project,
		MacroFile: tj.Macro.File, MacroTag: tj.Macro.TagPrefix,
		BranchPolicy: tj.BranchPolicy,
	}
	for _, s := range tj.Services {
		sv := svcSpec{Name: s.Name, Project: s.Project, Chart: s.Chart}
		for _, d := range s.Delivery {
			sv.Delivery = append(sv.Delivery, d.spec())
		}
		t.Services = append(t.Services, sv)
	}
	for _, d := range tj.Delivery {
		t.Delivery = append(t.Delivery, d.spec())
	}
	log.Printf("TOPOLOGY: %d services, macro %q, %d delivery targets, policy %q",
		len(t.Services), t.MacroProj, len(t.Delivery), t.BranchPolicy)
	return t
}

// deliveryFileFor reads a delivery app file with the TARGET's own client —
// per-host, per-credential. An unready target reads as "" (rendered pending).
func (a *api) deliveryFileFor(ctx context.Context, d deliverySpec) string {
	c := a.clientFor(d)
	if c == nil {
		return ""
	}
	// the legacy single cross-group client still wins for un-annotated
	// targets so existing konstruct config keeps working
	if d.TokenEnv == "" && d.Host == "" && a.glDelivery != nil {
		return a.deliveryFile(ctx, d.Project, d.App)
	}
	br := d.Branch
	if br == "" {
		br = "main"
	}
	ttl := ttlEco
	if a.isHot(d.Project) {
		ttl = 5 * time.Second
	}
	v, err := a.c.do("dfile:"+d.Host+":"+d.Project+":"+d.App, ttl, func() (any, error) {
		return c.fileRaw(ctx, d.Project, d.App, br)
	})
	if err != nil {
		return ""
	}
	return v.(string)
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
