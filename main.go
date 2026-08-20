// foxglider — read-only dashboard for GitLab build metadata.
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
	static, err := fs.Sub(assets, "static")
	if err != nil {
		log.Fatal(err)
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
	if g := os.Getenv("GITLAB_GROUPS"); g != "" {
		for _, s := range strings.Split(g, ",") {
			if s = strings.TrimSpace(s); s != "" {
				groups = append(groups, s)
			}
		}
	}
	log.Printf("foxglider serving on :%s (gitlab=%s groups=%v)", port, gl.base, groups)
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
