package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

type overviewResp struct {
	Groups []struct {
		Path     string `json:"path"`
		Projects []struct {
			ID        int    `json:"id"`
			Name      string `json:"name"`
			Pipelines []struct {
				Status    string  `json:"status"`
				ShortSHA  string  `json:"short_sha"`
				DurationS float64 `json:"duration_s"`
			} `json:"pipelines"`
		} `json:"projects"`
	} `json:"groups"`
}

func TestOverview(t *testing.T) {
	gl := fakeGitLab(t)
	defer gl.Close()
	api := newAPI(newGLClient(gl.URL, "tok"), nil)
	srv := httptest.NewServer(api)
	defer srv.Close()

	res, err := http.Get(srv.URL + "/api/overview")
	if err != nil {
		t.Fatal(err)
	}
	var o overviewResp
	if err := json.NewDecoder(res.Body).Decode(&o); err != nil {
		t.Fatal(err)
	}
	if len(o.Groups) != 2 { // platform, themes — sorted by path
		t.Fatalf("groups = %+v", o.Groups)
	}
	p := o.Groups[0].Projects[0]
	if p.Name != "alpha" || len(p.Pipelines) != 1 || p.Pipelines[0].Status != "success" {
		t.Fatalf("project = %+v", p)
	}
	if p.Pipelines[0].DurationS != 200 || p.Pipelines[0].ShortSHA != "abc123" {
		t.Fatalf("pipeline mapping = %+v", p.Pipelines[0])
	}
}

func TestOverviewGroupFilter(t *testing.T) {
	gl := fakeGitLab(t)
	defer gl.Close()
	api := newAPI(newGLClient(gl.URL, "tok"), []string{"platform"})
	srv := httptest.NewServer(api)
	defer srv.Close()
	res, _ := http.Get(srv.URL + "/api/overview")
	var o overviewResp
	json.NewDecoder(res.Body).Decode(&o)
	if len(o.Groups) != 1 || o.Groups[0].Path != "platform" {
		t.Fatalf("filter failed: %+v", o.Groups)
	}
}

func TestNoToken503(t *testing.T) {
	api := newAPI(newGLClient("http://unused", ""), nil)
	srv := httptest.NewServer(api)
	defer srv.Close()
	res, _ := http.Get(srv.URL + "/api/overview")
	if res.StatusCode != 503 {
		t.Fatalf("status = %d, want 503", res.StatusCode)
	}
	var e struct {
		Error string `json:"error"`
	}
	json.NewDecoder(res.Body).Decode(&e)
	if e.Error == "" {
		t.Fatal("want explicit error body")
	}
}

func TestProjectPipelines(t *testing.T) {
	gl := fakeGitLab(t)
	defer gl.Close()
	srv := httptest.NewServer(newAPI(newGLClient(gl.URL, "tok"), nil))
	defer srv.Close()
	res, _ := http.Get(srv.URL + "/api/projects/1/pipelines")
	var o struct {
		Pipelines []pipelineJSON `json:"pipelines"`
	}
	json.NewDecoder(res.Body).Decode(&o)
	if len(o.Pipelines) != 1 || o.Pipelines[0].ID != 11 {
		t.Fatalf("pipelines = %+v", o.Pipelines)
	}
}

func TestPipelineDetail(t *testing.T) {
	gl := fakeGitLab(t)
	defer gl.Close()
	srv := httptest.NewServer(newAPI(newGLClient(gl.URL, "tok"), nil))
	defer srv.Close()
	res, _ := http.Get(srv.URL + "/api/pipelines/1/11")
	var d struct {
		DurationS float64 `json:"duration_s"`
		Jobs      []struct {
			Stage     string  `json:"stage"`
			DurationS float64 `json:"duration_s"`
		} `json:"jobs"`
	}
	json.NewDecoder(res.Body).Decode(&d)
	if d.DurationS != 190 || len(d.Jobs) != 1 || d.Jobs[0].DurationS != 110 {
		t.Fatalf("detail = %+v", d)
	}
}

func TestPipelineDetailBadID(t *testing.T) {
	gl := fakeGitLab(t)
	defer gl.Close()
	srv := httptest.NewServer(newAPI(newGLClient(gl.URL, "tok"), nil))
	defer srv.Close()
	res, _ := http.Get(srv.URL + "/api/pipelines/1/notanumber")
	if res.StatusCode != 400 {
		t.Fatalf("status = %d, want 400", res.StatusCode)
	}
}

func TestActivityMergesAndSorts(t *testing.T) {
	gl := fakeGitLab(t)
	defer gl.Close()
	srv := httptest.NewServer(newAPI(newGLClient(gl.URL, "tok"), nil))
	defer srv.Close()
	res, _ := http.Get(srv.URL + "/api/activity?hours=168")
	var o struct {
		Items []struct {
			Type    string `json:"type"`
			Project string `json:"project"`
			WebURL  string `json:"web_url"`
			At      string `json:"at"`
		} `json:"items"`
	}
	json.NewDecoder(res.Body).Decode(&o)
	// fake has: pipeline 11 (alpha), pipeline 21 (beta), 1 push event (alpha)
	if len(o.Items) < 3 {
		t.Fatalf("items = %+v", o.Items)
	}
	for i := 1; i < len(o.Items); i++ {
		if o.Items[i].At > o.Items[i-1].At {
			t.Fatal("not sorted newest-first")
		}
	}
	var sawPush, sawPipeline bool
	for _, it := range o.Items {
		if it.Type == "push" {
			sawPush = true
			if it.WebURL != "http://gl/platform/alpha/-/commits/main" {
				t.Fatalf("push url = %s", it.WebURL)
			}
		}
		if it.Type == "pipeline" {
			sawPipeline = true
		}
	}
	if !sawPush || !sawPipeline {
		t.Fatalf("missing types: %+v", o.Items)
	}
}

func TestActivityHoursClamp(t *testing.T) {
	gl := fakeGitLab(t)
	defer gl.Close()
	srv := httptest.NewServer(newAPI(newGLClient(gl.URL, "tok"), nil))
	defer srv.Close()
	res, _ := http.Get(srv.URL + "/api/activity?hours=9999")
	if res.StatusCode != 200 { // clamped to 168, not an error
		t.Fatalf("status = %d", res.StatusCode)
	}
}
