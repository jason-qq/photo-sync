// Package ui serves the owner web UI: a small static single-page app that
// talks to the API with the owner API key (pasted at sign-in and kept in
// the browser's sessionStorage).
//
// The files are embedded into the binary, so the API server needs no
// separate asset directory or build step.
package ui

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

//go:embed static
var files embed.FS

var contentTypes = map[string]string{
	".css":  "text/css; charset=utf-8",
	".html": "text/html; charset=utf-8",
	".js":   "text/javascript; charset=utf-8",
}

// Config configures the UI handler.
type Config struct {
	// ImageOrigins are extra origins images may load from (e.g. the blob
	// store's origin when it differs from the UI's). The UI's own origin
	// is always allowed.
	ImageOrigins []string
}

// Handler returns an http.Handler serving the UI. Mount it at "/" (with no
// method in the pattern, so more specific paths such as "/api/" win).
func Handler(cfg Config) http.Handler {
	static, err := fs.Sub(files, "static")
	if err != nil {
		panic(err) // the embed path is fixed at compile time
	}
	fileServer := http.FileServerFS(static)

	csp := strings.Join([]string{
		"default-src 'self'",
		"img-src " + strings.Join(append([]string{"'self'"}, cfg.ImageOrigins...), " "),
		"object-src 'none'",
		"base-uri 'none'",
		"form-action 'self'",
		"frame-ancestors 'none'",
	}, "; ")

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Robots-Tag", "noindex")
		// Embedded files have no modification time, so revalidate every
		// load; a rebuilt binary is picked up immediately.
		h.Set("Cache-Control", "no-cache")
		// Set types explicitly: Go otherwise consults the OS (on Windows,
		// the registry), and with nosniff a wrong type breaks the page.
		if ct, ok := contentTypes[path.Ext(r.URL.Path)]; ok {
			h.Set("Content-Type", ct)
		}
		fileServer.ServeHTTP(w, r)
	})
}
