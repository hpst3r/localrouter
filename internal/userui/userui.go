// Package userui serves the static browser UI for multi-user mode: the page
// at /ui/ and its two assets. It has no server-side state and imports no
// identity, control or app code; the page talks to the session API under
// /ui/v1/ and to /auth/ on the same origin.
//
// Mount it on "/ui/" next to the session API on "/ui/v1/" (the longer
// pattern wins). Every path other than /ui/, /ui/assets/app.js and
// /ui/assets/style.css is 404, and only GET and HEAD are allowed.
package userui

import (
	_ "embed"
	"net/http"
	"strconv"
)

var (
	//go:embed static/index.html
	indexHTML []byte
	//go:embed static/app.js
	appJS []byte
	//go:embed static/style.css
	styleCSS []byte
)

type asset struct {
	contentType string
	body        []byte
}

// assets is the complete whitelist, keyed by exact request path.
var assets = map[string]asset{
	"/ui/":                 {"text/html; charset=utf-8", indexHTML},
	"/ui/assets/app.js":    {"text/javascript; charset=utf-8", appJS},
	"/ui/assets/style.css": {"text/css; charset=utf-8", styleCSS},
}

// csp allows only same-origin script, style and fetch; no inline code, no
// framing, no base rewriting, and Trusted Types forbid HTML string sinks.
const csp = "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; " +
	"base-uri 'none'; form-action 'self'; frame-ancestors 'none'; " +
	"require-trusted-types-for 'script'; trusted-types 'none'"

// Handler returns the static UI handler.
func Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Cache-Control", "no-store")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		a, ok := assets[r.URL.Path]
		if !ok || r.URL.RawPath != "" {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", a.contentType)
		w.Header().Set("Content-Length", strconv.Itoa(len(a.body)))
		if r.Method == http.MethodHead {
			return
		}
		_, _ = w.Write(a.body)
	})
}
