package main

import (
	"slices"
	"testing"

	"github.com/hpst3r/localrouter/internal/config"
)

// TestR1CheckPoolInfo: Amendment R1.4, an info line (never an error) for a
// pool with more members than max_failovers + 1 attempts.
func TestR1CheckPoolInfo(t *testing.T) {
	members := func(ids ...string) *config.BalanceConfig {
		m := map[string]config.BalanceMember{}
		for _, id := range ids {
			m[id] = config.BalanceMember{}
		}
		return &config.BalanceConfig{Strategy: "least_inflight", Identity: "x", Members: m}
	}
	cfg := &config.Config{Routes: []config.RouteConfig{
		{Name: "plain", Interactive: []string{"a", "b", "c", "d"}},
		{Name: "fits", Balance: members("a", "b", "c")},
		{Name: "big", Balance: members("a", "b", "c", "d")},
	}}
	cfg.Policy.MaxFailovers = 2
	want := []string{"info: route big pool has 4 members but at most 3 attempts per request (policy.max_failovers)"}
	if got := poolInfo(cfg); !slices.Equal(got, want) {
		t.Fatalf("poolInfo = %q", got)
	}
	cfg.Policy.MaxFailovers = 3
	if got := poolInfo(cfg); got != nil {
		t.Fatalf("poolInfo with 4 attempts = %q", got)
	}
}
