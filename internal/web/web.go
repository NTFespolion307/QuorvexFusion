// Package web serves the browser UI, embedded into the binary.
//
// The UI is plain HTML, CSS and JavaScript modules (no build step) plus the
// vendored uPlot charting library, all under static/:
//
//	login.html, public/   reachable without logging in (login page only)
//	index.html, js/, css/, vendor/   require a valid session cookie
//
// The UI talks to the same REST API as the CLI, authenticated by the
// session cookie, and listens to /api/v1/events for live updates.
package web

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

//go:embed static
var staticFS embed.FS

var static, _ = fs.Sub(staticFS, "static")

// Register adds the UI routes. hasSession reports whether a request carries
// a valid login session.
func Register(mux *http.ServeMux, hasSession func(*http.Request) bool) {
	files := http.FileServerFS(static)

	mux.HandleFunc("GET /login", func(w http.ResponseWriter, r *http.Request) {
		if hasSession(r) {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		serveFile(w, r, "login.html")
	})
	mux.HandleFunc("GET /favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		serveFile(w, r, "public/favicon.svg")
	})
	// Public assets of the login page.
	mux.Handle("GET /ui/public/", http.StripPrefix("/ui", noCache(files)))

	// Everything else requires a session.
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		if !hasSession(r) {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		serveFile(w, r, "index.html")
	})
	mux.Handle("GET /ui/", http.StripPrefix("/ui", noCache(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !hasSession(r) {
			http.Error(w, "not logged in", http.StatusUnauthorized)
			return
		}
		files.ServeHTTP(w, r)
	}))))
}

func serveFile(w http.ResponseWriter, r *http.Request, name string) {
	data, err := fs.ReadFile(static, name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	switch path.Ext(name) {
	case ".html":
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
	case ".svg":
		w.Header().Set("Content-Type", "image/svg+xml")
	}
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(data)
}

// noCache makes browsers revalidate assets, so an upgraded controller's UI
// is picked up immediately (the files are small).
func noCache(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/") {
			http.NotFound(w, r) // no directory listings
			return
		}
		w.Header().Set("Cache-Control", "no-cache")
		h.ServeHTTP(w, r)
	})
}
