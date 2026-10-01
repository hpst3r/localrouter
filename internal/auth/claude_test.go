package auth

import (
	"context"
	"strings"
	"testing"

	"github.com/hpst3r/localrouter/internal/core"
)

func TestClaudeAccountNeverYieldsCredential(t *testing.T) {
	h := newHarness(t, core.Account{ID: "cl", Provider: core.ProviderClaude})
	_, err := h.m.Credential(context.Background(), "cl")
	if err == nil || !strings.Contains(err.Error(), "quota-only") {
		t.Fatalf("want quota-only error, got %v", err)
	}
	h.iss.mu.Lock()
	n := h.iss.refreshCalls
	h.iss.mu.Unlock()
	if n != 0 {
		t.Fatalf("issuer contacted for claude account")
	}
}
