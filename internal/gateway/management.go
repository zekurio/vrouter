package gateway

import (
	"io/fs"
	"net/http"
	"strings"
)

// staticHandler serves the embedded frontend. Only files are served, and
// index.html is revalidated on every load so a new build is picked up.
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
			http.NotFound(w, r)
			return
		}
		if path == "index.html" {
			w.Header().Set("Cache-Control", "no-cache")
		}
		files.ServeHTTP(w, r)
	}
}

// authStatus tells the signed-out dashboard where sign-in leads.
func (s *server) authStatus(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, map[string]string{"publicUrl": s.cfg.PublicURL, "issuer": s.oidc.issuerHost()})
}
