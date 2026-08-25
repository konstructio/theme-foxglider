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
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// themeVersion is the human-visible build marker. Bump it with every change
// worth seeing land — the header badge surfaces it so you can tell at a glance
// which build of the theme is actually serving.
const themeVersion = "1.0.0"

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
}

type macroNode struct {
	Name         string        `json:"name"`
	Project      string        `json:"project"`
	WebURL       string        `json:"web_url"`
	BaseVer      string        `json:"base_version"`
	PublishedRC  string        `json:"published_rc"`
	PublishedTag string        `json:"published_tag"`
	Pipeline     *pipelineJSON `json:"pipeline,omitempty"`
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
	v, err := a.c.do("file:"+proj+":"+file, ttlEco, func() (any, error) {
		return a.gl.fileRaw(ctx, proj, file, "main")
	})
	if err != nil {
		return ""
	}
	return v.(string)
}

func (a *api) cachedTags(ctx context.Context, proj string) []glTag {
	v, err := a.c.do("tags:"+proj, ttlEco, func() (any, error) {
		return a.gl.tags(ctx, proj, 100)
	})
	if err != nil {
		return nil
	}
	return v.([]glTag)
}

func (a *api) latestPipe(ctx context.Context, proj string) *pipelineJSON {
	v, err := a.c.do("lp:"+proj, ttlEco, func() (any, error) {
		return a.gl.latestPipeline(ctx, proj)
	})
	if err != nil {
		return nil
	}
	pl := v.(glPipeline)
	if pl.ID == 0 {
		return nil
	}
	pj := toPipelineJSON(pl)
	return &pj
}

// ecosystem renders the metaphor supply chain: services → umbrella → delivered.
func (a *api) ecosystem(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	t := a.topo

	// Macro first — its deps are the source of truth for what's bundled.
	macroRaw := a.rawFile(ctx, t.MacroProj, t.MacroFile)
	deps := macroDeps(macroRaw)
	publishedTag := newestTag(a.cachedTags(ctx, t.MacroProj), t.MacroTag)
	publishedRC := strings.TrimPrefix(publishedTag, t.MacroTag)

	macro := macroNode{
		Name: t.MacroName, Project: t.MacroProj,
		WebURL:  a.gl.base + "/" + t.MacroProj,
		BaseVer: chartVersion(macroRaw), PublishedRC: publishedRC, PublishedTag: publishedTag,
		Pipeline: a.latestPipe(ctx, t.MacroProj),
	}

	var mu sync.Mutex
	var wg sync.WaitGroup

	services := make([]svcNode, len(t.Services))
	for i, s := range t.Services {
		wg.Add(1)
		go func(i int, s svcSpec) {
			defer wg.Done()
			n := svcNode{
				Name: s.Name, Project: s.Project,
				WebURL:  a.gl.base + "/" + s.Project,
				BaseVer: chartVersion(a.rawFile(ctx, s.Project, s.Chart)),
				Bundled: deps[s.Name],
			}
			n.Pipeline = a.latestPipe(ctx, s.Project)
			mu.Lock()
			services[i] = n
			mu.Unlock()
		}(i, s)
	}

	delivery := make([]deliveryNode, len(t.Delivery))
	for i, d := range t.Delivery {
		wg.Add(1)
		go func(i int, d deliverySpec) {
			defer wg.Done()
			delivered := targetRevision(a.rawFile(ctx, d.Project, d.App))
			state, behind := drift(delivered, publishedRC)
			mu.Lock()
			delivery[i] = deliveryNode{
				Env: d.Env, Cluster: d.Cluster, Delivered: delivered,
				WebURL: a.gl.base + "/" + d.Project + "/-/blob/main/" + d.App,
				State:  state, Behind: behind,
			}
			mu.Unlock()
		}(i, d)
	}
	wg.Wait()

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
		"theme":       "metaphor",
		"version":     themeVersion,
		"gitlab_host": a.gl.base,
		"groups":      a.groups,
	})
}
