package main

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// The dashboard-manager component on dev-33 is the richest real-world shape:
// an ingress host, a browser-facing env URL, an in-cluster env URL, and a
// repoURL that must NOT surface as an app hostname.
const dashMgrYAML = `
spec:
  source:
    repoURL: europe-west2-docker.pkg.dev/civo-com/metaphor-charts
    chart: metaphor-dashboard-manager
    helm:
      valuesObject:
        ingress:
          hosts:
            - host: metaphor-dashboard.development-33.civo-platform.com
              paths:
                - path: /
        env:
          - name: NEXT_PUBLIC_REMOTE_METAPHOR
            value: https://metaphor-micro-frontend.development-33.civo-platform.com
          - name: REMOTE_METAPHOR_URL
            value: http://metaphor-micro-frontend.metaphor.svc.cluster.local:8080
          - name: APP_URL
            value: https://metaphor-dashboard.development-33.civo-platform.com
`

func TestScanHostsRealComponentShape(t *testing.T) {
	got := scanHosts("infra/autopilot:dash.yaml", dashMgrYAML)
	byHost := map[string]hostFinding{}
	for _, f := range got {
		byHost[f.Host] = f
	}
	dash, ok := byHost["metaphor-dashboard.development-33.civo-platform.com"]
	if !ok {
		t.Fatalf("ingress host not found in %+v", got)
	}
	// first sighting wins: the ingress host line comes before the APP_URL env
	if dash.Field != "ingress host" {
		t.Fatalf("dash field = %q, want ingress host (first provenance wins)", dash.Field)
	}
	mfe, ok := byHost["metaphor-micro-frontend.development-33.civo-platform.com"]
	if !ok || mfe.Field != "env NEXT_PUBLIC_REMOTE_METAPHOR" {
		t.Fatalf("mfe finding = %+v, want env NEXT_PUBLIC_REMOTE_METAPHOR", mfe)
	}
	svc, ok := byHost["metaphor-micro-frontend.metaphor.svc.cluster.local"]
	if !ok || svc.DNS != "internal" || svc.URL != "" {
		t.Fatalf("svc finding = %+v, want internal with no URL", svc)
	}
	for h := range byHost {
		if strings.Contains(h, "pkg.dev") {
			t.Fatalf("repoURL leaked into findings: %s", h)
		}
	}
	if len(got) != 3 {
		t.Fatalf("findings = %d (%+v), want 3 (dedupe within file)", len(got), got)
	}
}

func TestScanHostsListFormAndComments(t *testing.T) {
	raw := `
ingress:
  enabled: false
  hosts:
    - host: zippy.example.com
# a commented decoy must never surface:
#    - host: ghost.example.com
tls:
  - hosts:
      - zippy.example.com
`
	got := scanHosts("chart:values.yaml", raw)
	if len(got) != 1 || got[0].Host != "zippy.example.com" {
		t.Fatalf("findings = %+v, want exactly zippy.example.com", got)
	}
	if got[0].URL != "https://zippy.example.com" {
		t.Fatalf("url = %q, want https form", got[0].URL)
	}
}

func TestDedupeHostsDropsDeclaredAppURL(t *testing.T) {
	fs := []hostFinding{
		{Host: "app.example.com", Source: "a"},
		{Host: "other.example.com", Source: "a"},
		{Host: "other.example.com", Source: "b"}, // cross-file dup
	}
	got := dedupeHosts(fs, "https://app.example.com")
	if len(got) != 1 || got[0].Host != "other.example.com" || got[0].Source != "a" {
		t.Fatalf("dedupe = %+v, want only other.example.com from source a", got)
	}
}

func TestDNSVerdictInjection(t *testing.T) {
	a := &api{c: newCache(), lookup: func(_ context.Context, h string) error {
		if h == "real.example.com" {
			return nil
		}
		return errors.New("NXDOMAIN")
	}}
	if v := a.dnsProbe("real.example.com"); v != "ok" {
		t.Fatalf("dns real = %q, want ok", v)
	}
	if v := a.dnsProbe("zippy-development.feedkray.com"); v != "none" {
		t.Fatalf("dns dangling = %q, want none", v)
	}
	// the batch fill answers instantly-resolvable stubs within its budget
	got := a.dnsFill([]string{"real.example.com", "gone.example.com"})
	if got["real.example.com"] != "ok" || got["gone.example.com"] != "none" {
		t.Fatalf("dnsFill = %+v", got)
	}
}

func TestScanHostsNormalization(t *testing.T) {
	raw := `
ingress:
  hosts:
    - host: LOUD.Example.COM
notes: see https://loud.example.com/docs).
also: https://trail.example.com,
`
	got := scanHosts("f", raw)
	byHost := map[string]hostFinding{}
	for _, f := range got {
		byHost[f.Host] = f
	}
	if len(got) != 2 {
		t.Fatalf("findings = %+v, want loud+trail deduped case-insensitively", got)
	}
	if _, ok := byHost["loud.example.com"]; !ok {
		t.Fatalf("uppercase host not normalized: %+v", got)
	}
	tr, ok := byHost["trail.example.com"]
	if !ok || strings.HasSuffix(tr.URL, ",") {
		t.Fatalf("trailing punctuation survived: %+v", tr)
	}
}

func TestTopologyManifestsParse(t *testing.T) {
	d := deliveryJSON{Env: "dev", Project: "org/gitops", App: "app.yaml",
		Manifests: []struct {
			Project string `json:"project"`
			Path    string `json:"path"`
		}{{Project: "org/infra", Path: "c/app.yaml"}, {Path: "sibling.yaml"}}}
	s := d.spec()
	if len(s.Manifests) != 2 || s.Manifests[0].Project != "org/infra" || s.Manifests[1].Project != "" {
		t.Fatalf("manifests = %+v", s.Manifests)
	}
}
