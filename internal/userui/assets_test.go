package userui

import (
	"regexp"
	"slices"
	"strings"
	"testing"
)

// frozenPaths is the PHASE2 session API used by the UI. Path parameters are
// written as {id}/{uid}/{kid}.
var frozenPaths = []string{
	"/ui/v1/me",
	"/ui/v1/me/keys",
	"/ui/v1/me/keys/{id}",
	"/ui/v1/me/usage",
	"/ui/v1/me/analytics",
	"/ui/v1/me/dimensions",
	"/ui/v1/me/budget",
	"/ui/v1/admin/users",
	"/ui/v1/admin/users/{id}/disable",
	"/ui/v1/admin/users/{id}/enable",
	"/ui/v1/admin/users/{id}/delete",
	"/ui/v1/admin/users/{id}/keys",
	"/ui/v1/admin/users/{uid}/keys/{kid}",
	"/ui/v1/admin/audit",
	"/ui/v1/admin/status",
	"/ui/v1/admin/usage",
	"/ui/v1/admin/analytics",
	"/ui/v1/admin/dimensions",
	"/ui/v1/admin/diagnostics",
	"/ui/v1/admin/budgets",
	"/auth/logout",
}

func TestIndexHasNoInlineCode(t *testing.T) {
	html := string(indexHTML)
	scripts := regexp.MustCompile(`(?is)<script\b([^>]*)>(.*?)</script>`).FindAllStringSubmatch(html, -1)
	if len(scripts) != 1 || scripts[0][1] != ` src="/ui/assets/app.js" defer` || scripts[0][2] != "" {
		t.Fatalf("scripts = %q, want exactly one external app.js", scripts)
	}
	for _, re := range []string{
		`(?i)<style\b`, `(?i)\sstyle\s*=`, `(?i)\son[a-z]+\s*=`, `(?i)javascript:`,
		`(?i)<base\b`, `(?i)<iframe\b`, `(?i)<object\b`, `(?i)<embed\b`, `(?i)http-equiv`,
	} {
		if m := regexp.MustCompile(re).FindString(html); m != "" {
			t.Errorf("index.html contains %q", m)
		}
	}
	refs := regexp.MustCompile(`(?i)\s(?:src|href|action)\s*=\s*"([^"]*)"`).FindAllStringSubmatch(html, -1)
	var got []string
	for _, r := range refs {
		got = append(got, r[1])
	}
	slices.Sort(got)
	got = slices.Compact(got)
	want := []string{"#main", "/auth/login", "/ui/assets/app.js", "/ui/assets/style.css"}
	if !slices.Equal(got, want) {
		t.Fatalf("references = %q, want %q", got, want)
	}
}

func TestAppJSAvoidsUnsafeSinksAndStorage(t *testing.T) {
	js := string(appJS)
	for _, bad := range []string{
		"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "DOMParser",
		"createContextualFragment", "srcdoc", "eval(", "Function(", "setTimeout(\"", "setInterval(",
		"localStorage", "sessionStorage", "indexedDB", "caches.", "document.cookie", "console.",
		"location", "history.", "window.open", "postMessage", "sendBeacon", "XMLHttpRequest",
		"importScripts", "import(", ".style.", "setAttribute(\"style", "setAttribute(\"on",
	} {
		if strings.Contains(js, bad) {
			t.Errorf("app.js contains %q", bad)
		}
	}
}

func TestStyleCSSHasNoExternalLoads(t *testing.T) {
	css := string(styleCSS)
	for _, bad := range []string{"@import", "url(", "expression("} {
		if strings.Contains(css, bad) {
			t.Errorf("style.css contains %q", bad)
		}
	}
}

func TestAppJSUsesOnlyFrozenPaths(t *testing.T) {
	js := string(appJS)
	lits := regexp.MustCompile(`"(/(?:ui/v1|auth)/[^"]*)"`).FindAllStringSubmatch(js, -1)
	if len(lits) == 0 {
		t.Fatal("no API path literals found")
	}
	// Literals are either complete paths or prefixes joined with an
	// encodeURIComponent()'d id followed by a fixed suffix literal.
	seen := map[string]bool{}
	for _, l := range lits {
		seen[l[1]] = true
	}
	allowedPieces := map[string]bool{}
	for _, p := range frozenPaths {
		allowedPieces[p] = true
		// prefix up to the first parameter
		if i := strings.Index(p, "{"); i > 0 {
			allowedPieces[p[:i]] = true
		}
	}
	for s := range seen {
		if !allowedPieces[s] {
			t.Errorf("app.js uses non-frozen path literal %q", s)
		}
	}
	// Each frozen endpoint must be reachable: its fixed prefix appears.
	for _, p := range frozenPaths {
		pre := p
		if i := strings.Index(p, "{"); i > 0 {
			pre = p[:i]
		}
		if !seen[pre] {
			t.Errorf("app.js never calls %s", p)
		}
	}
	if !strings.Contains(js, `"X-LocalRouter-CSRF"`) {
		t.Error("app.js does not send the X-LocalRouter-CSRF header")
	}
	if strings.Contains(js, `credentials: "include"`) {
		t.Error("app.js sends cross-origin credentials")
	}
}
