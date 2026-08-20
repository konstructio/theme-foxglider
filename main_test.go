package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestStaticServed(t *testing.T) {
	srv := httptest.NewServer(newMux(http.NotFoundHandler()))
	defer srv.Close()
	res, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("status = %d", res.StatusCode)
	}
	buf := make([]byte, 4096)
	n, _ := res.Body.Read(buf)
	if !strings.Contains(string(buf[:n]), "Foxglider") {
		t.Fatal("index.html should mention Foxglider")
	}
}

func TestAPIRouted(t *testing.T) {
	api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(299) })
	srv := httptest.NewServer(newMux(api))
	defer srv.Close()
	res, _ := http.Get(srv.URL + "/api/overview")
	if res.StatusCode != 299 {
		t.Fatalf("api not routed, status = %d", res.StatusCode)
	}
}
