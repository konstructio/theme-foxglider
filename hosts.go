package main

// Environment host discovery — what does this environment actually answer on?
//
// The theme is generic and drops into any org, so nothing here may know a
// platform's hostnames. Instead they are DISCOVERED: the delivery Application
// file the dashboard already reads, plus any extra manifests the TOPOLOGY
// declares, are scanned for ingress `host:` fields and URL-valued strings.
// Every finding carries its provenance — which file, which field — and a
// server-side DNS verdict, because a hostname rendered confidently that
// resolves nowhere is exactly the failure this exists to expose (the zippy
// feedkray ingress sat dead on dev-33 for weeks with nothing surfacing it).

import (
	"context"
	"net"
	"regexp"
	"strings"
	"time"
)

// manifestRef is an extra manifest scanned for hostnames, read with the
// delivery target's own client and credentials. Project "" = the target's
// own gitops repo.
type manifestRef struct {
	Project string
	Path    string
}

// hostFinding is one discovered hostname with the story of how it was found.
type hostFinding struct {
	Host   string `json:"host"`
	URL    string `json:"url,omitempty"` // clickable form; empty for in-cluster names
	Source string `json:"source"`        // "<project>:<path>"
	Field  string `json:"field"`         // "ingress host", "env FOO", or the yaml key
	DNS    string `json:"dns,omitempty"` // "ok" | "none" | "internal" | "" (unchecked)
}

var (
	reHostLine = regexp.MustCompile(`^\s*(?:-\s*)?host:\s*"?([A-Za-z0-9][A-Za-z0-9._-]*\.[A-Za-z0-9-]+)"?\s*$`)
	reURLTok   = regexp.MustCompile(`https?://[A-Za-z0-9._-]+(?::\d+)?[^\s"']*`)
	reNameItem = regexp.MustCompile(`^\s*-\s*name:\s*"?([A-Za-z0-9_.-]+)"?\s*$`)
	reYamlKey  = regexp.MustCompile(`^\s*(?:-\s*)?([A-Za-z0-9_-]+):`)
)

// scanHosts extracts hostname findings from one manifest. Line/regex over
// YAML like every parser in this repo — no yaml dependency, comments stripped
// first so a documented example never becomes a "discovered" hostname.
func scanHosts(source, raw string) []hostFinding {
	var out []hostFinding
	seen := map[string]bool{}
	add := func(f hostFinding) {
		if f.Host == "" || seen[f.Host] {
			return
		}
		seen[f.Host] = true
		out = append(out, f)
	}
	lastName, lastNameAt := "", -10
	for i, ln := range strings.Split(stripComments(raw), "\n") {
		if m := reNameItem.FindStringSubmatch(ln); m != nil {
			// remember `- name: FOO` so a `value: https://…` a line or two
			// below can say WHICH env var carried the URL
			lastName, lastNameAt = m[1], i
			continue
		}
		if m := reHostLine.FindStringSubmatch(ln); m != nil {
			add(classifyHost(hostFinding{Host: m[1], Source: source, Field: "ingress host"}))
			continue
		}
		for _, u := range reURLTok.FindAllString(ln, -1) {
			u = strings.TrimRight(u, ".,;)]}'\"!") // prose punctuation, not the URL
			h := hostOfURL(u)
			if h == "" {
				continue
			}
			key := ""
			if k := reYamlKey.FindStringSubmatch(ln); k != nil {
				key = k[1]
			}
			// not apps the environment serves: chart/repo coordinates and the
			// ArgoCD destination API server (kubernetes.default.svc noise)
			if key == "repoURL" || key == "repository" || key == "server" {
				continue
			}
			field := key
			if key == "value" && lastName != "" && i-lastNameAt <= 2 {
				field = "env " + lastName
			}
			if field == "" {
				field = "url"
			}
			add(classifyHost(hostFinding{Host: h, URL: u, Source: source, Field: field}))
		}
	}
	return out
}

// classifyHost marks in-cluster names: public DNS doesn't apply to them and a
// browser can't open them, so they render informational, never as links.
// Hostnames are case-insensitive — normalize here so dedupe can't be fooled
// by `host: EXAMPLE.COM` meeting `https://example.com`.
func classifyHost(f hostFinding) hostFinding {
	f.Host = strings.ToLower(f.Host)
	if strings.Contains(f.Host, ".svc.cluster.local") || strings.HasSuffix(f.Host, ".svc") {
		f.DNS = "internal"
		f.URL = ""
		return f
	}
	if f.URL == "" {
		f.URL = "https://" + f.Host
	}
	return f
}

// hostOfURL returns the bare hostname of a URL token, "" when it isn't
// hostname-shaped (a dot is required — bare service names are noise here;
// dotted in-cluster names survive and are classified internal by the caller).
func hostOfURL(u string) string {
	u = strings.TrimPrefix(strings.TrimPrefix(u, "https://"), "http://")
	if i := strings.IndexAny(u, "/?#"); i >= 0 {
		u = u[:i]
	}
	if i := strings.IndexByte(u, ':'); i >= 0 {
		u = u[:i]
	}
	// URL tokens lifted out of prose can drag trailing punctuation along
	// ("see https://x.com)." → "x.com)."); hostnames end alphanumeric.
	u = strings.TrimRight(u, ".,;)]}'\"!")
	if !strings.Contains(u, ".") {
		return ""
	}
	return strings.ToLower(u)
}

// dedupeHosts merges findings from several files (first provenance wins) and
// drops the topology-declared app URL's host — that one is already the card's
// "↗ app" link; chips are for what was discovered, not declared.
func dedupeHosts(findings []hostFinding, appURL string) []hostFinding {
	declared := hostOfURL(appURL)
	var out []hostFinding
	seen := map[string]bool{}
	for _, f := range findings {
		if f.Host == declared || seen[f.Host] {
			continue
		}
		seen[f.Host] = true
		out = append(out, f)
	}
	return out
}

// dnsProbe resolves one hostname to "ok"/"none", cached well past the payload
// TTL — DNS moves slowly, and these lookups are the only network calls in
// this codebase that leave GitLab. The lookup runs on a DETACHED context: a
// dying request must never bank a false "none" for 15 minutes.
func (a *api) dnsProbe(host string) string {
	v, err := a.c.do("dns:"+host, 15*time.Minute, func() (any, error) {
		lctx, cancel := context.WithTimeout(context.Background(), 1200*time.Millisecond)
		defer cancel()
		lk := a.lookup
		if lk == nil {
			lk = func(c context.Context, h string) error {
				_, e := net.DefaultResolver.LookupHost(c, h)
				return e
			}
		}
		if e := lk(lctx, host); e != nil {
			return "none", nil
		}
		return "ok", nil
	})
	if err != nil {
		return ""
	}
	return v.(string)
}

// dnsFill resolves a batch of hostnames in parallel, bounded: the payload
// handler waits at most ~1.5s for cold verdicts; stragglers keep resolving
// detached and warm the cache for the next poll, so the view fills in rather
// than the handler stalling (the same deal the fan-out fetches get).
func (a *api) dnsFill(hosts []string) map[string]string {
	out := make(map[string]string, len(hosts))
	pending := hosts[:0:0]
	for _, h := range hosts {
		if v, ok := a.c.peek("dns:" + h); ok {
			out[h] = v.(string)
			continue
		}
		pending = append(pending, h)
	}
	if len(pending) == 0 {
		return out
	}
	type verdict struct{ host, v string }
	ch := make(chan verdict, len(pending))
	for _, h := range pending {
		h := h
		go func() { ch <- verdict{h, a.dnsProbe(h)} }()
	}
	deadline := time.After(1500 * time.Millisecond)
	for range pending {
		select {
		case v := <-ch:
			out[v.host] = v.v
		case <-deadline:
			return out // the rest land in cache for the next poll
		}
	}
	return out
}

// manifestRaw pairs a scan manifest with its fetched content ("" = unreadable
// with this target's credentials — the caller counts these so the card can
// say so instead of silently showing fewer hosts).
type manifestRaw struct {
	ref manifestRef
	raw string
}

// targetReadClient picks the read client for a delivery target: the target's
// own (host, token) client, except legacy un-annotated targets keep reading
// via the dedicated cross-group delivery credential when one is configured —
// the same selection deliveryFileFor makes, in one place.
func (a *api) targetReadClient(d deliverySpec) *glClient {
	if d.TokenEnv == "" && d.Host == "" && a.glDelivery != nil {
		return a.glDelivery
	}
	return a.clientFor(d)
}

// manifestFiles reads a target's extra scan manifests with the target's own
// client, mirroring deliveryFileFor's credential selection.
func (a *api) manifestFiles(ctx context.Context, d deliverySpec) []manifestRaw {
	if len(d.Manifests) == 0 {
		return nil
	}
	c := a.targetReadClient(d)
	if c == nil {
		return nil // pending creds — the card already says so
	}
	out := make([]manifestRaw, 0, len(d.Manifests))
	for _, m := range d.Manifests {
		proj := m.Project
		if proj == "" {
			proj = d.Project
		}
		// the target's branch only means something in the target's own repo
		ref := "main"
		if proj == d.Project && d.Branch != "" {
			ref = d.Branch
		}
		mr := manifestRaw{ref: manifestRef{Project: proj, Path: m.Path}}
		// ref is part of the key: two targets pinning the same repo on
		// different branches must not read each other's cached copy
		v, err := a.c.do("mfile:"+d.Host+":"+proj+"@"+ref+":"+m.Path, ttlEco, func() (any, error) {
			return c.fileRaw(ctx, proj, m.Path, ref)
		})
		if err == nil {
			mr.raw = v.(string)
		}
		out = append(out, mr)
	}
	return out
}
