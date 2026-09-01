package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

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
