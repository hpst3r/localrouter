package control

import (
	"strings"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
	"github.com/hpst3r/localrouter/internal/policy"
)

// B11: /control/v1/status reports each account's breaker additively, and the
// admit dry run names a breaker denial.
func TestB11StatusAndAdmitReportBreaker(t *testing.T) {
	accts := []core.Account{{ID: "dead", Provider: core.ProviderOllama}, {ID: "fine", Provider: core.ProviderOllama}}
	now := t0
	clk := stepClock{t: &now}
	q := &fakeQuota{snaps: map[string]core.Snapshot{}}
	pol := policy.New(accts, q, policy.Options{Clock: clk,
		Breaker: policy.BreakerOptions{Threshold: 2, Base: 5 * time.Second, Max: time.Minute}})
	for range 2 {
		l, _ := pol.Acquire(core.ClassInteractive, []string{"dead"}, nil)
		l.Release(core.Outcome{Unreachable: "dns"})
	}
	l, _ := pol.Acquire(core.ClassInteractive, []string{"fine"}, nil)
	l.Release(core.Outcome{Unreachable: "dial"})
	newSrv := func(p core.Policy) *Server {
		return New(Deps{
			Accounts: accts, Quota: q, Policy: p, Ledger: &fakeLedger{}, Storage: fakePinger{}, Clock: clk,
			Routes: []core.Route{{Name: "local", Models: []string{"llama"}, Interactive: []string{"dead"}}},
		}, Options{StaleAfter: 10 * time.Minute})
	}
	f := &fixture{srv: newSrv(pol)}
	f.h = f.srv.Handler()

	byID := func() map[string]map[string]any {
		out := map[string]map[string]any{}
		for _, a := range decode(t, f.do(t, "GET", "/control/v1/status", "", ""))["accounts"].([]any) {
			m := a.(map[string]any)
			out[m["id"].(string)] = m
		}
		return out
	}
	st := byID()
	dead, fine := st["dead"]["breaker"].(map[string]any), st["fine"]["breaker"].(map[string]any)
	if dead["state"] != "open" || dead["consecutive_failures"] != float64(2) || dead["open_until"] != "2026-10-01T12:00:05Z" {
		t.Fatalf("open breaker = %v", dead)
	}
	if st["dead"]["healthy"] != false || st["dead"]["interactive_admissible"] != false {
		t.Fatalf("open account flags = %v", st["dead"])
	}
	if _, has := fine["open_until"]; has || fine["state"] != "closed" || fine["consecutive_failures"] != float64(1) {
		t.Fatalf("closed breaker = %v", fine)
	}

	m := decode(t, f.do(t, "POST", "/control/v1/admit", `{"class":"interactive","model":"llama"}`, ""))
	if m["decision"] != "deny" || !strings.Contains(m["reason"].(string), "dead: upstream unreachable (breaker open until 2026-10-01T12:00:05Z)") {
		t.Fatalf("admit = %v", m)
	}

	now = now.Add(5 * time.Second)
	if b := byID()["dead"]["breaker"].(map[string]any); b["state"] != "half_open" || b["open_until"] != "2026-10-01T12:00:05Z" {
		t.Fatalf("half-open breaker = %v", b)
	}

	// Disabled breaker (or a policy without one): no breaker key at all.
	off := policy.New(accts, q, policy.Options{Clock: clk})
	f = &fixture{srv: newSrv(off)}
	f.h = f.srv.Handler()
	if _, has := byID()["dead"]["breaker"]; has {
		t.Fatal("disabled breaker reported in status")
	}
}
