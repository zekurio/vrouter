package gateway

import (
	"io/fs"
	"net/http"
	"strings"
)

// staticHandler serves the embedded frontend: its files, and index.html for
// the dashboard's page addresses. index.html is revalidated on every load so
// a new build is picked up.
func staticHandler(assets fs.FS) http.HandlerFunc {
	files := http.FileServer(http.FS(assets))
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		path := strings.TrimPrefix(r.URL.Path, "/")
		if path == "" {
			path = "index.html"
		}
		if info, err := fs.Stat(assets, path); err != nil || info.IsDir() {
			// A page address such as /usage belongs to the dashboard, which
			// reads it once loaded. A missing file stays missing.
			if strings.ContainsAny(path, "./") {
				http.NotFound(w, r)
				return
			}
			path = "index.html"
			r = r.Clone(r.Context())
			r.URL.Path = "/"
		}
		if path == "index.html" {
			w.Header().Set("Cache-Control", "no-cache")
		}
		files.ServeHTTP(w, r)
	}
}
