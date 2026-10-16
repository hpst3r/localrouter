package mcpserver

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestValidateBaseURLPolicy(t *testing.T) {
	ok := map[string]string{
		"http://127.0.0.1:8787":            "http://127.0.0.1:8787",
		"http://127.0.0.1:8787/":           "http://127.0.0.1:8787",
		"http://localhost:8787":            "http://localhost:8787",
		"http://[::1]:8787":                "http://[::1]:8787",
		"https://router.internal:8443":     "https://router.internal:8443",
		"  https://router.internal:8443  ": "https://router.internal:8443",
	}
	for in, want := range ok {
		got, err := ValidateBaseURL(in)
		if err != nil {
			t.Errorf("ValidateBaseURL(%q) = error %v, want %q", in, err, want)
			continue
		}
		if got != want {
			t.Errorf("ValidateBaseURL(%q) = %q, want %q", in, got, want)
		}
	}

	bad := map[string]string{
		"plain http to a remote host": "http://router.example.com:8787",
		"plain http to any address":   "http://0.0.0.0:8787",
		"credentials in the url":      "http://user:pass@127.0.0.1:8787",
		"query string":                "http://127.0.0.1:8787/?token=abc",
		"fragment":                    "http://127.0.0.1:8787/#x",
		"path":                        "http://127.0.0.1:8787/control",
		"missing scheme":              "127.0.0.1:8787",
		"unsupported scheme":          "ftp://127.0.0.1:8787",
		"missing host":                "http:///control",
		"garbage":                     "://nope",
	}
	for name, in := range bad {
		if got, err := ValidateBaseURL(in); err == nil {
			t.Errorf("%s: ValidateBaseURL(%q) = %q, want an error", name, in, got)
		}
	}
}

func TestConfigDefaultsAndBounds(t *testing.T) {
	cfg := Config{}
	if err := cfg.validate(); err != nil {
		t.Fatalf("default config rejected: %v", err)
	}
	if cfg.URL != DefaultURL || cfg.Timeout != DefaultTimeout || cfg.MaxResultChars != DefaultMaxResultChars {
		t.Fatalf("defaults not applied: %+v", cfg)
	}

	cases := map[string]Config{
		"negative timeout":        {Timeout: -time.Second},
		"timeout over the cap":    {Timeout: MaxTimeout + time.Second},
		"result budget too small": {MaxResultChars: 1},
		"insecure remote url":     {URL: "http://router.example.com:8787"},
	}
	for name, c := range cases {
		cc := c
		if err := cc.validate(); err == nil {
			t.Errorf("%s: config accepted, want rejection", name)
		}
	}
}

func TestNewReadsKeyFileOnce(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "client.key")
	if err := os.WriteFile(keyPath, []byte("  abc123 \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv, err := New(Config{URL: "http://127.0.0.1:8787", KeyFile: keyPath})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if srv.up.key != "abc123" {
		t.Fatalf("key = %q, want the trimmed file contents", srv.up.key)
	}

	empty := filepath.Join(dir, "empty.key")
	if err := os.WriteFile(empty, []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(Config{KeyFile: empty}); err == nil {
		t.Error("an empty key file must be rejected")
	}
	if _, err := New(Config{KeyFile: filepath.Join(dir, "absent.key")}); err == nil {
		t.Error("a missing key file must be rejected")
	}
	if _, err := New(Config{URL: "http://router.example.com"}); err == nil {
		t.Error("an insecure remote url must be rejected before any request")
	}
}

func TestStrictArgDecoding(t *testing.T) {
	if _, err := decodeArgs([]byte(`{"a":1}`), "b"); err == nil {
		t.Error("unknown field must be rejected")
	}
	if _, err := decodeArgs([]byte(`[1]`), "a"); err == nil {
		t.Error("a non-object argument list must be rejected")
	}
	if _, err := decodeArgs([]byte(`{"a":1}{"b":2}`), "a"); err == nil {
		t.Error("trailing content must be rejected")
	}
	bag, err := decodeArgs(nil, "a")
	if err != nil {
		t.Fatalf("absent arguments: %v", err)
	}
	if bag.present("a") {
		t.Error("absent arguments must decode to an empty bag")
	}
	bag, err = decodeArgs([]byte(`{"a":null}`), "a")
	if err != nil {
		t.Fatalf("explicit null: %v", err)
	}
	if bag.present("a") {
		t.Error("explicit null must read as not present")
	}
	if _, _, err = bag.integer("a"); err == nil {
		t.Error("an explicit null must be rejected, never defaulted")
	}
	bag, _ = decodeArgs([]byte(`{"a":"7"}`), "a")
	if _, _, err := bag.integer("a"); err == nil {
		t.Error("a numeric string must not be coerced to an integer")
	}
	if _, err := bag.str("a", false); err != nil {
		t.Errorf("string argument rejected: %v", err)
	}
	bag, _ = decodeArgs([]byte(`{"a":true}`), "a")
	if _, err := bag.str("a", false); err == nil {
		t.Error("a boolean must never be accepted where a string is expected")
	}
	if _, err := bag.str("b", true); err == nil {
		t.Error("a required argument that is absent must be rejected")
	}
}

func TestValidateSinceBounds(t *testing.T) {
	for _, v := range []string{"24h", "7d", "90d", "1h30m", "2160h"} {
		if err := validateSince(v); err != nil {
			t.Errorf("validateSince(%q) = %v, want nil", v, err)
		}
	}
	for _, v := range []string{"", "0", "0d", "91d", "-24h", "2161h", "abc", "24"} {
		if err := validateSince(v); err == nil {
			t.Errorf("validateSince(%q) = nil, want an error", v)
		}
	}
}

func TestErrorCodesAreStable(t *testing.T) {
	want := []ErrorCode{
		CodeInvalidArguments, CodeAuthRequired, CodeForbidden, CodeServerUnreachable,
		CodeServerTimeout, CodeBadResponse, CodeEndpointUnavailable, CodeNotFound,
		CodeCapabilityUnsupported, CodeCapabilityRequirementsRequired,
	}
	for _, c := range want {
		if strings.TrimSpace(string(c)) == "" {
			t.Errorf("empty error code in %v", want)
		}
	}
}
