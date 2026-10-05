package agent

import (
	"context"
	"fmt"
	"testing"
)

func TestKeychainExplicitAccount(t *testing.T) {
	var got []string
	k := KeychainCredentials{Service: "svc", Account: "alice", Run: func(_ context.Context, name string, args ...string) ([]byte, error) {
		got = append([]string{name}, args...)
		return []byte(`{"claudeAiOauth":{"accessToken":"x"}}`), nil
	}}
	if _, err := k.ReadCredentials(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(got) != "[security find-generic-password -a alice -s svc -w]" {
		t.Fatalf("args = %v", got)
	}
}

func TestDefaultKeychainAccountMirrorsClaudeCode(t *testing.T) {
	t.Setenv("USER", "bob.smith-1")
	if a := DefaultKeychainAccount(); a != "bob.smith-1" {
		t.Fatalf("got %q", a)
	}
	t.Setenv("USER", "bad user!")
	if a := DefaultKeychainAccount(); a != "claude-code-user" {
		t.Fatalf("invalid username should map to claude-code-user, got %q", a)
	}
}
