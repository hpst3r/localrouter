package weblogin

import (
	"net/http"
	"testing"
)

// Bidi overrides/isolates, zero-width and other Unicode format characters
// (category Cf) in informational claims are stripped at ingestion, so an IdP
// display name cannot visually impersonate another user in admin dialogs. A
// value that is only whitespace or format characters is empty, and the
// display name then falls back to preferred_username.
func TestCallbackStripsFormatCharacters(t *testing.T) {
	cases := []struct {
		name                   string
		edit                   func(map[string]any)
		wantEmail, wantDisplay string
	}{
		{"bidi override", func(c map[string]any) { c["name"] = "Alice\u202eecilA\u202c" }, "user@example.test", "AliceecilA"},
		{"isolates and marks", func(c map[string]any) { c["name"] = "\u2066Bob\u2069\u200e\u200f\u061c" }, "user@example.test", "Bob"},
		{"embeddings", func(c map[string]any) { c["name"] = "\u202aA\u202bB\u202dC" }, "user@example.test", "ABC"},
		{"zero width", func(c map[string]any) { c["name"] = "ad\u200bm\u200ci\u200dn\u2060\ufeff\u00ad" }, "user@example.test", "admin"},
		{"email format chars", func(c map[string]any) { c["email"] = "ali\u200bce@example.test\u202e" }, "alice@example.test", "Test User"},
		{"format-only name falls back", func(c map[string]any) {
			c["name"] = "\u200b\u202e\u2066"
			c["preferred_username"] = "eve"
		}, "user@example.test", "eve"},
		{"whitespace-only name falls back", func(c map[string]any) {
			c["name"] = " \u3000\u00a0 "
			c["preferred_username"] = "eve"
		}, "user@example.test", "eve"},
		{"whitespace-only email empty", func(c map[string]any) { c["email"] = " \u200b " }, "", "Test User"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			if w := h.loginWith(c.edit); w.Code != http.StatusSeeOther {
				t.Fatalf("login: %d %q", w.Code, w.Body.String())
			}
			logins, _ := h.hooks.snapshot()
			if logins[0].Email != c.wantEmail || logins[0].DisplayName != c.wantDisplay {
				t.Fatalf("email %q display %q, want %q %q", logins[0].Email, logins[0].DisplayName, c.wantEmail, c.wantDisplay)
			}
		})
	}
}

func TestInfoStringStripsFormatAndControl(t *testing.T) {
	for in, want := range map[string]string{
		"plain":                 "plain",
		"a\x00b\x7fc\u0085d":    "abcd",
		"x\u202ay\u202ez\u2069": "xyz",
		"\ufeff":                "",
		"   ":                   "",
		"\t\n":                  "",
		"tag\U000E0041chars":    "tagchars",
		"keep inner  space":     "keep inner  space",
		"\xffbad utf8":          "bad utf8",
		"\u00e9\u65e5\u672c":    "\u00e9\u65e5\u672c",
	} {
		if got := infoString(in); got != want {
			t.Errorf("infoString(%q) = %q, want %q", in, got, want)
		}
	}
}
