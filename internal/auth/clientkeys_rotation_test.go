package auth

import (
	"strings"
	"testing"
)

// Overlapping client key rotation: a single client may hold more than one
// active key (old + new) so keys can be rotated without downtime; every key
// authenticates as that client.
func TestLoadClientKeyFilesOverlap(t *testing.T) {
	dir := t.TempDir()
	k1, _ := GenerateKey()
	k2, _ := GenerateKey()
	k3, _ := GenerateKey()
	ck, err := LoadClientKeyFiles(map[string][]string{
		"alice": {writeKey(t, dir, "a1", k1, 0o600), writeKey(t, dir, "a2", k2, 0o400)},
		"bob":   {writeKey(t, dir, "b1", k3, 0o600)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if n, ok := ck.Lookup(k1); !ok || n != "alice" {
		t.Fatalf("alice key1: %q %v", n, ok)
	}
	if n, ok := ck.Lookup(k2); !ok || n != "alice" {
		t.Fatalf("alice key2: %q %v", n, ok)
	}
	if n, ok := ck.Lookup(k3); !ok || n != "bob" {
		t.Fatalf("bob: %q %v", n, ok)
	}
	if _, ok := ck.Lookup("lr-nope"); ok {
		t.Fatal("unknown key accepted")
	}
}

// After rotation the caller builds a NEW ClientKeys from the reloaded config;
// a key removed from the list must no longer authenticate, while remaining
// keys still do. The previously loaded object is untouched.
func TestLoadClientKeyFilesRemoval(t *testing.T) {
	dir := t.TempDir()
	k1, _ := GenerateKey()
	k2, _ := GenerateKey()
	p1 := writeKey(t, dir, "a1", k1, 0o600)
	p2 := writeKey(t, dir, "a2", k2, 0o600)

	before, err := LoadClientKeyFiles(map[string][]string{"alice": {p1, p2}})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := before.Lookup(k2); !ok {
		t.Fatal("old object should accept k2 before reload")
	}

	// Rotate: drop k2, keep k1.
	after, err := LoadClientKeyFiles(map[string][]string{"alice": {p1}})
	if err != nil {
		t.Fatal(err)
	}
	if n, ok := after.Lookup(k1); !ok || n != "alice" {
		t.Fatalf("kept key rejected after reload: %q %v", n, ok)
	}
	if _, ok := after.Lookup(k2); ok {
		t.Fatal("removed key still accepted after reload")
	}
	// The old object still holds its own view (no shared state).
	if _, ok := before.Lookup(k2); !ok {
		t.Fatal("old object mutated by reload")
	}
}

// The original single-key loader stays valid and equivalent.
func TestLoadClientKeysStillValid(t *testing.T) {
	dir := t.TempDir()
	k1, _ := GenerateKey()
	p1 := writeKey(t, dir, "a1", k1, 0o600)
	ck, err := LoadClientKeys(map[string]string{"alice": p1})
	if err != nil {
		t.Fatal(err)
	}
	if n, ok := ck.Lookup(k1); !ok || n != "alice" {
		t.Fatalf("legacy loader: %q %v", n, ok)
	}
}

func TestLoadClientKeyFilesRejections(t *testing.T) {
	dir := t.TempDir()
	k1, _ := GenerateKey()
	k2, _ := GenerateKey()
	p1 := writeKey(t, dir, "a1", k1, 0o600)
	p1dup := writeKey(t, dir, "a1dup", k1, 0o600) // same key content, different file

	cases := map[string]map[string][]string{
		// A duplicate key across two clients is ambiguous; reject.
		"duplicate cross-client": {"a": {p1}, "b": {p1dup}},
		// A deliberate duplicate within one client adds no auth value; reject.
		"duplicate same-client": {"a": {p1, p1dup}},
		// The same file listed twice is the same duplicate.
		"duplicate file same-client": {"a": {p1, p1}},
		// A client configured with no key files at all is an error.
		"empty list": {"a": {}},
		"nil list":   {"a": nil},
		// Group/world readable files remain rejected.
		"world readable": {"a": {writeKey(t, dir, "w", k2, 0o604)}},
		"short key":      {"a": {writeKey(t, dir, "s", "short", 0o600)}},
		"missing file":   {"a": {dir + "/nope"}},
	}
	for name, files := range cases {
		_, err := LoadClientKeyFiles(files)
		if err == nil {
			t.Errorf("%s: expected error", name)
			continue
		}
		if strings.Contains(err.Error(), k1) || strings.Contains(err.Error(), k2) {
			t.Errorf("%s: error leaks key material", name)
		}
	}
}
