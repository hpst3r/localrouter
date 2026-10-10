package userui

import (
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func get(t *testing.T, h http.Handler, method, target string) *http.Response {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, target, nil))
	return rec.Result()
}

func body(t *testing.T, res *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestIndexServed(t *testing.T) {
	res := get(t, Handler(), http.MethodGet, "/ui/")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Fatalf("Content-Type = %q", ct)
	}
	if b := body(t, res); !strings.Contains(b, `<script src="/ui/assets/app.js" defer></script>`) {
		t.Fatalf("index does not load app.js:\n%s", b)
	}
}

func TestAssetsServedWithExactTypes(t *testing.T) {
	for path, want := range map[string]string{
		"/ui/assets/app.js":    "text/javascript; charset=utf-8",
		"/ui/assets/style.css": "text/css; charset=utf-8",
	} {
		res := get(t, Handler(), http.MethodGet, path)
		if res.StatusCode != http.StatusOK {
			t.Fatalf("%s: status = %d", path, res.StatusCode)
		}
		if ct := res.Header.Get("Content-Type"); ct != want {
			t.Fatalf("%s: Content-Type = %q, want %q", path, ct, want)
		}
		if body(t, res) == "" {
			t.Fatalf("%s: empty body", path)
		}
	}
}

const wantCSP = "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; " +
	"base-uri 'none'; form-action 'self'; frame-ancestors 'none'; " +
	"require-trusted-types-for 'script'; trusted-types 'none'"

func TestSecurityHeadersOnEveryResponse(t *testing.T) {
	want := map[string]string{
		"Content-Security-Policy":      wantCSP,
		"X-Content-Type-Options":       "nosniff",
		"Referrer-Policy":              "no-referrer",
		"X-Frame-Options":              "DENY",
		"Cache-Control":                "no-store",
		"Cross-Origin-Opener-Policy":   "same-origin",
		"Cross-Origin-Resource-Policy": "same-origin",
	}
	cases := []struct{ method, path string }{
		{http.MethodGet, "/ui/"}, {http.MethodGet, "/ui/assets/app.js"},
		{http.MethodHead, "/ui/assets/style.css"}, {http.MethodGet, "/ui/missing"},
		{http.MethodPost, "/ui/"},
	}
	for _, c := range cases {
		res := get(t, Handler(), c.method, c.path)
		for k, v := range want {
			if got := res.Header.Get(k); got != v {
				t.Errorf("%s %s: %s = %q, want %q", c.method, c.path, k, got, v)
			}
		}
		for k := range res.Header {
			if strings.HasPrefix(k, "Access-Control-") || k == "Set-Cookie" {
				t.Errorf("%s %s: unexpected header %s", c.method, c.path, k)
			}
		}
	}
}

func TestOnlyGetAndHeadAllowed(t *testing.T) {
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch, http.MethodOptions, "TRACE"} {
		res := get(t, Handler(), m, "/ui/")
		if res.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s: status = %d, want 405", m, res.StatusCode)
		}
		if a := res.Header.Get("Allow"); a != "GET, HEAD" {
			t.Errorf("%s: Allow = %q", m, a)
		}
		if strings.Contains(body(t, res), "<script") {
			t.Errorf("%s: served the page", m)
		}
	}
	res := get(t, Handler(), http.MethodHead, "/ui/assets/app.js")
	if res.StatusCode != http.StatusOK || res.Header.Get("Content-Type") != "text/javascript; charset=utf-8" {
		t.Fatalf("HEAD: status = %d type = %q", res.StatusCode, res.Header.Get("Content-Type"))
	}
	if b := body(t, res); b != "" {
		t.Fatalf("HEAD body = %q", b)
	}
}

func TestOnlyWhitelistedPathsServed(t *testing.T) {
	for _, path := range []string{
		"/", "/ui", "/ui/index.html", "/ui/assets/", "/ui/assets/app.js/",
		"/ui/assets/APP.JS", "/ui/assets/other.js", "/ui/assets/app.js.map",
		"/ui/static/app.js", "/ui/v1/me", "/ui/../ui/", "/ui//", "/ui/assets//app.js",
		"/ui/assets/%61pp.js", "/auth/login",
	} {
		res := get(t, Handler(), http.MethodGet, path)
		if res.StatusCode != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404", path, res.StatusCode)
		}
	}
}

// TestNoModuleImports keeps the package static: it must not depend on the
// identity store, control API or app wiring.
func TestNoModuleImports(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range f.Imports {
			if path := strings.Trim(imp.Path.Value, `"`); strings.Contains(path, ".") {
				t.Errorf("%s imports %s", name, path)
			}
		}
	}
}
