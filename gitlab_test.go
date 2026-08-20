package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func fakeGitLab(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	auth := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("PRIVATE-TOKEN") != "tok" {
				w.WriteHeader(401)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			h(w, r)
		}
	}
	mux.HandleFunc("/api/v4/projects", auth(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("page") {
		case "", "1":
			w.Header().Set("x-next-page", "2")
			fmt.Fprint(w, `[{"id":1,"name":"alpha","path_with_namespace":"platform/alpha","web_url":"http://gl/platform/alpha","default_branch":"main","namespace":{"full_path":"platform"}}]`)
		case "2":
			w.Header().Set("x-next-page", "")
			fmt.Fprint(w, `[{"id":2,"name":"beta","path_with_namespace":"themes/beta","web_url":"http://gl/themes/beta","default_branch":"main","namespace":{"full_path":"themes"}}]`)
		}
	}))
	mux.HandleFunc("/api/v4/projects/1/pipelines", auth(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `[{"id":11,"status":"success","ref":"main","sha":"abc123","web_url":"http://gl/platform/alpha/-/pipelines/11","created_at":"2026-08-20T10:00:00Z","updated_at":"2026-08-20T10:03:20Z"}]`)
	}))
	mux.HandleFunc("/api/v4/projects/1/pipelines/11", auth(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"id":11,"status":"success","ref":"main","sha":"abc123","web_url":"http://gl/platform/alpha/-/pipelines/11","created_at":"2026-08-20T10:00:00Z","updated_at":"2026-08-20T10:03:20Z","started_at":"2026-08-20T10:00:10Z","finished_at":"2026-08-20T10:03:20Z","duration":190}`)
	}))
	mux.HandleFunc("/api/v4/projects/1/pipelines/11/jobs", auth(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `[{"name":"build","stage":"build","status":"success","started_at":"2026-08-20T10:00:10Z","finished_at":"2026-08-20T10:02:00Z","duration":110,"web_url":"http://gl/platform/alpha/-/jobs/101"}]`)
	}))
	mux.HandleFunc("/api/v4/projects/1/events", auth(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `[{"action_name":"pushed to","created_at":"2026-08-20T09:59:00Z","author":{"username":"john"},"push_data":{"ref":"main","commit_title":"feat: x","commit_count":1}}]`)
	}))
	mux.HandleFunc("/api/v4/projects/2/pipelines", auth(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `[{"id":21,"status":"failed","ref":"main","sha":"def456","web_url":"http://gl/themes/beta/-/pipelines/21","created_at":"2026-08-20T11:00:00Z","updated_at":"2026-08-20T11:01:00Z"}]`)
	}))
	mux.HandleFunc("/api/v4/projects/2/events", auth(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `[]`)
	}))
	return httptest.NewServer(mux)
}

func TestClientPaginatesProjects(t *testing.T) {
	gl := fakeGitLab(t)
	defer gl.Close()
	c := newGLClient(gl.URL, "tok")
	ps, err := c.projects(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 2 || ps[0].Name != "alpha" || ps[1].Namespace.FullPath != "themes" {
		t.Fatalf("bad projects: %+v", ps)
	}
}

func TestClientPipelinesJobsEvents(t *testing.T) {
	gl := fakeGitLab(t)
	defer gl.Close()
	c := newGLClient(gl.URL, "tok")
	pl, err := c.pipelines(context.Background(), 1, 20)
	if err != nil || len(pl) != 1 || pl[0].Status != "success" {
		t.Fatalf("pipelines: %v %+v", err, pl)
	}
	d, err := c.pipeline(context.Background(), 1, 11)
	if err != nil || d.Duration != 190 {
		t.Fatalf("pipeline detail: %v %+v", err, d)
	}
	js, err := c.jobs(context.Background(), 1, 11)
	if err != nil || len(js) != 1 || js[0].Stage != "build" {
		t.Fatalf("jobs: %v %+v", err, js)
	}
	ev, err := c.events(context.Background(), 1, 50)
	if err != nil || len(ev) != 1 || ev[0].PushData == nil {
		t.Fatalf("events: %v %+v", err, ev)
	}
}

func TestClientBadToken(t *testing.T) {
	gl := fakeGitLab(t)
	defer gl.Close()
	c := newGLClient(gl.URL, "wrong")
	if _, err := c.projects(context.Background()); err == nil {
		t.Fatal("expected error on 401")
	}
}
