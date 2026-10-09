package auth

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
)

func skipIfNoModeBits(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX mode bits not enforced on windows")
	}
}

// CRED-1: upstream key files (api_key_file / management_key_file) that are
// group/world accessible are refused, like client key files.
func TestSecStaticKeyFileWorldReadableRejected(t *testing.T) {
	skipIfNoModeBits(t)
	p := filepath.Join(t.TempDir(), "openrouter.key")
	if err := os.WriteFile(p, []byte("sk-or-v1-secret-upstream-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := New([]core.Account{{ID: "or", Provider: core.ProviderOpenRouter}},
		map[string]StaticKey{"or": {File: p}}, nil, Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if _, err := m.Credential(context.Background(), "or"); err != nil {
		t.Fatalf("0600 key rejected: %v", err)
	}
	// Loosening the mode after the key was cached must also be caught.
	for _, mode := range []os.FileMode{0o644, 0o640, 0o604} {
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
		_, err := m.Credential(context.Background(), "or")
		if err == nil {
			t.Fatalf("mode %#o key file accepted", mode)
		}
		if strings.Contains(err.Error(), "sk-or-v1") {
			t.Fatalf("error leaks key: %v", err)
		}
	}
}

// CRED-1: client key paths must be regular files.
func TestSecClientKeyFileMustBeRegular(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "keydir")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := LoadClientKeyFiles(map[string][]string{"c": {dir}})
	if err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("want not-a-regular-file error, got %v", err)
	}
}

// CRED-4: when persisting a refreshed token fails, the rotated refresh token
// stays in memory and persistence is retried on the next Credential call,
// without another refresh (the old refresh token is already consumed).
func TestSecPersistFailureKeepsRotatedToken(t *testing.T) {
	skipIfNoModeBits(t)
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	h := newHarness(t)
	h.seed(t, "a1", t0.Add(time.Minute), "chatgpt-1", "old")
	h.iss.newAccess = makeJWT(t0.Add(time.Hour), "chatgpt-1", "access-new")
	h.iss.newRefresh = "rt-new"

	if err := os.Chmod(h.store.Dir(), 0o500); err != nil {
		t.Fatal(err)
	}
	_, err := h.m.Credential(context.Background(), "a1")
	if err := os.Chmod(h.store.Dir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err == nil {
		t.Fatal("persist failure not reported")
	}
	if strings.Contains(err.Error(), "rt-new") || strings.Contains(h.logs.String(), "rt-new") {
		t.Fatal("refresh token leaked in error or logs")
	}

	// Next call: persistence retried and succeeds; no second refresh.
	c, err := h.m.Credential(context.Background(), "a1")
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if h.iss.calls() != 1 {
		t.Fatalf("refresh calls = %d, want 1", h.iss.calls())
	}
	if c.Headers.Get("Authorization") != "Bearer "+h.iss.newAccess {
		t.Fatal("did not return the refreshed access token")
	}
	saved, err := h.store.Load("a1")
	if err != nil {
		t.Fatal(err)
	}
	if saved.RefreshToken != "rt-new" {
		t.Fatalf("rotated refresh token not persisted: %q", saved.RefreshToken)
	}
}

// CRED-4: while persistence keeps failing, a refresh that becomes due again
// uses the rotated (in-memory) refresh token, not the consumed one on disk.
func TestSecPersistFailureLaterRefreshUsesRotatedToken(t *testing.T) {
	skipIfNoModeBits(t)
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	h := newHarness(t)
	h.seed(t, "a1", t0.Add(time.Minute), "chatgpt-1", "old")
	h.iss.newAccess = makeJWT(t0.Add(time.Hour), "chatgpt-1", "access-new")
	h.iss.newRefresh = "rt-new"
	if err := os.Chmod(h.store.Dir(), 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(h.store.Dir(), 0o700) })
	if _, err := h.m.Credential(context.Background(), "a1"); err == nil {
		t.Fatal("persist failure not reported")
	}
	if _, err := h.m.Credential(context.Background(), "a1"); err == nil {
		t.Fatal("persist still failing; want error")
	}
	if err := os.Chmod(h.store.Dir(), 0o700); err != nil {
		t.Fatal(err)
	}
	h.clock.advance(2 * time.Hour) // new token now expired too
	h.iss.mu.Lock()
	h.iss.newAccess = makeJWT(t0.Add(3*time.Hour), "chatgpt-1", "access-newer")
	h.iss.newRefresh = "rt-newer"
	h.iss.mu.Unlock()
	if _, err := h.m.Credential(context.Background(), "a1"); err != nil {
		t.Fatal(err)
	}
	h.iss.mu.Lock()
	sent := append([]string(nil), h.iss.gotRefresh...)
	h.iss.mu.Unlock()
	if len(sent) != 2 || sent[1] != "rt-new" {
		t.Fatalf("refresh tokens sent = %q, want second to be rt-new", sent)
	}
	saved, _ := h.store.Load("a1")
	if saved.RefreshToken != "rt-newer" {
		t.Fatalf("persisted refresh token = %q", saved.RefreshToken)
	}
}
