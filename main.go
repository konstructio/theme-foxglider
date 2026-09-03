// theme-foxglider — read-only dashboard for GitLab build metadata.
// Seed forked from civo/konstruct/theme-foxglider; the delivery-view build
// (releases/tags/package-registry + guarded pipeline triggers) lands in #37.
// Borrows the theme-starter shape: embedded static frontend + tiny server.
package main

import (
	"embed"
	"io/fs"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

//go:embed static
var assets embed.FS

func newMux(api http.Handler) http.Handler {
	// THEME_STATIC_DIR serves the static assets from disk (dev): edits to
	// index.html show on a plain browser refresh, no rebuild. Unset = embedded.
	var static fs.FS
	if dir := os.Getenv("THEME_STATIC_DIR"); dir != "" {
		static = os.DirFS(dir)
		log.Printf("dev: serving static from disk dir %q", dir)
	} else {
		sub, err := fs.Sub(assets, "static")
		if err != nil {
			log.Fatal(err)
		}
		static = sub
	}
	mux := http.NewServeMux()
	mux.Handle("/api/", api)
	mux.Handle("/", http.FileServer(http.FS(static)))
	return requestLog(mux)
}

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	gl := newGLClient(envOr("GITLAB_HOST", "https://gitlab.com"), os.Getenv("GITLAB_TOKEN"))
	var groups []string
	// Org-pinned by default: the metaphor theme scopes to civo/metaphor unless
	// GITLAB_GROUPS overrides it with a comma-separated list of group paths.
	for _, s := range strings.Split(envOr("GITLAB_GROUPS", "civo/metaphor"), ",") {
		if s = strings.TrimSpace(s); s != "" {
			groups = append(groups, s)
		}
	}
	log.Printf("theme-foxglider serving on :%s (gitlab=%s groups=%v)", port, gl.base, groups)
	log.Fatal(http.ListenAndServe(":"+port, newMux(newAPI(gl, groups))))
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// requestLog keeps the platform runtime-logs panel truthful: one line per request.
func requestLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		log.Printf("%s %s %d %s", r.Method, r.URL.Path, rec.status, time.Since(start).Round(time.Millisecond))
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}
