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

func TestNoToken503(t *testing.T) {
	api := newAPI(newGLClient("http://unused", ""), nil)
	srv := httptest.NewServer(api)
	defer srv.Close()
	res, _ := http.Get(srv.URL + "/api/branches")
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
