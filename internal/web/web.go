// Package web serves the embedded single-file admin interface. The same
// index.html is committed at internal/web/index.html so it can also be opened
// directly in a browser and pointed at a running gateway.
package web

import (
	_ "embed"
	"net/http"
)

//go:embed index.html
var indexHTML []byte

// Handler serves the UI. Only the exact index paths return HTML so stray API
// typos keep their JSON 404s.
func Handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" && r.URL.Path != "/index.html" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(indexHTML)
	}
}
