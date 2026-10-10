package control

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
	"github.com/hpst3r/localrouter/internal/quota"
)

func secAgentSnapshot() core.Snapshot {
	return core.Snapshot{AccountID: "claude-max", FetchedAt: t0, Source: quota.SourceUsageAPI, Plan: "max",
		Windows: []core.Window{{Kind: core.Window5h, UsedFrac: 0.2, WindowSeconds: 18000},
			{Kind: core.WindowWeekly, UsedFrac: 0.1, WindowSeconds: 604800}}}
}

// Snapshot labels, collections and model counts are bounded before anything
// is stored (Sol issue 5 / CTL-2): a request that inflates the stored
// snapshot (and so every status response) is rejected whole with 400.
func TestSecIngestSnapshotLabelsAndCollectionsBounded(t *testing.T) {
	accounts := []core.Account{{ID: "claude-max", Provider: core.ProviderClaude, QuotaSource: "agent"}}
	q := quota.New(accounts, nil, quota.Options{Clock: fakeClock{t0}}) // never Start: no provider calls
	h := New(Deps{Accounts: accounts, Quota: q, Ingester: q, Clock: fakeClock{t0}, Authenticate: func(key string) (core.Client, bool) {
		return core.Client{Name: "sol-agent", Ingest: true}, key == ingestKey
	}}, Options{}).Handler()
	f := &ingestFixture{h: h}

	big := strings.Repeat("A", 1<<20)
	windows := make([]core.Window, 1000)
	for i := range windows {
		windows[i] = core.Window{Kind: "5h", UsedFrac: 0.1}
	}
	snap := core.Snapshot{AccountID: "claude-max", FetchedAt: t0, Err: big, Plan: big, Source: big, Windows: windows,
		ModelRequests: map[string][]core.ModelCount{"5h": {{Model: strings.Repeat("m", 1024), Requests: -1}}}}
	w := f.post(t, ingestKey, core.IngestRequest{SchemaVersion: 1, Host: "vm1", Snapshots: []core.Snapshot{snap}})
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), CodeInvalidRequest) {
		t.Fatalf("ingest %d %s, want 400 invalid_request", w.Code, w.Body)
	}
	if _, ok := q.Latest("claude-max"); ok {
		t.Fatal("oversized snapshot was stored")
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/control/v1/status", nil))
	if w.Code != 200 || w.Body.Len() >= 64<<10 {
		t.Fatalf("status %d size=%d", w.Code, w.Body.Len())
	}
}

// Each limit individually rejects the request; nothing is stored.
func TestSecIngestSnapshotLimits(t *testing.T) {
	label := func(n int) string { return strings.Repeat("x", n) }
	mr := func(n int) map[string][]core.ModelCount {
		out := map[string][]core.ModelCount{}
		for i := range n {
			out[fmt.Sprintf("k%d", i)] = []core.ModelCount{{Model: "m", Requests: 1}}
		}
		return out
	}
	no := false
	cases := map[string]func(s *core.Snapshot){
		"windows>16": func(s *core.Snapshot) {
			s.Windows = nil
			for i := range 17 {
				s.Windows = append(s.Windows, core.Window{Kind: fmt.Sprintf("w%d", i)})
			}
		},
		"duplicate kind":    func(s *core.Snapshot) { s.Windows = append(s.Windows, core.Window{Kind: core.Window5h}) },
		"plan too long":     func(s *core.Snapshot) { s.Plan = label(core.MaxLabelBytes + 1) },
		"plan control":      func(s *core.Snapshot) { s.Plan = "max\n" },
		"source too long":   func(s *core.Snapshot) { s.Source = label(core.MaxLabelBytes + 1) },
		"source c1 control": func(s *core.Snapshot) { s.Source = "usage\u0085" }, // JSON already maps invalid UTF-8 to U+FFFD
		"err too long":      func(s *core.Snapshot) { s.Err = label(core.MaxErrorBytes + 1) },
		"err control":       func(s *core.Snapshot) { s.Err = "a\x1bb" },
		"model keys>16":     func(s *core.Snapshot) { s.ModelRequests = mr(17) },
		"model key bad":     func(s *core.Snapshot) { s.ModelRequests = map[string][]core.ModelCount{"5H!": {{Model: "m"}}} },
		"model entries>256": func(s *core.Snapshot) {
			s.ModelRequests = map[string][]core.ModelCount{"5h": make([]core.ModelCount, 257)}
			for i := range s.ModelRequests["5h"] {
				s.ModelRequests["5h"][i].Model = fmt.Sprintf("m%d", i)
			}
		},
		"model label long": func(s *core.Snapshot) {
			s.ModelRequests = map[string][]core.ModelCount{"5h": {{Model: label(core.MaxLabelBytes + 1)}}}
		},
		"model label ctl": func(s *core.Snapshot) { s.ModelRequests = map[string][]core.ModelCount{"5h": {{Model: "m\x00"}}} },
		"requests<0": func(s *core.Snapshot) {
			s.ModelRequests = map[string][]core.ModelCount{"5h": {{Model: "m", Requests: -1}}}
		},
		"requests>max": func(s *core.Snapshot) {
			s.ModelRequests = map[string][]core.ModelCount{"5h": {{Model: "m", Requests: core.MaxRecordTokens + 1}}}
		},
		"credits": func(s *core.Snapshot) { s.Credits = &core.Credits{} },
		"key":     func(s *core.Snapshot) { s.Key = &core.KeyUsage{} },
		"allowed": func(s *core.Snapshot) { s.Allowed = &no },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f := newIngestFixture(t, true, true)
			s := secAgentSnapshot()
			mutate(&s)
			w := f.post(t, ingestKey, core.IngestRequest{SchemaVersion: 1, Host: "vm1", Snapshots: []core.Snapshot{s}})
			if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), CodeInvalidRequest) {
				t.Fatalf("status %d %s, want 400 invalid_request", w.Code, w.Body)
			}
			if f.ing.calls != 0 {
				t.Fatal("snapshot reached the ingester")
			}
		})
	}
	t.Run("snapshots>16", func(t *testing.T) {
		f := newIngestFixture(t, true, true)
		snaps := make([]core.Snapshot, 17)
		for i := range snaps {
			snaps[i] = secAgentSnapshot()
			snaps[i].FetchedAt = t0.Add(-time.Duration(i) * time.Second)
		}
		w := f.post(t, ingestKey, core.IngestRequest{SchemaVersion: 1, Host: "vm1", Snapshots: snaps})
		if w.Code != http.StatusBadRequest || f.ing.calls != 0 {
			t.Fatalf("status %d calls=%d, want 400 and no ingest", w.Code, f.ing.calls)
		}
	})
}

// Values at the limits are accepted.
func TestSecIngestSnapshotAtLimitsAccepted(t *testing.T) {
	f := newIngestFixture(t, true, true)
	s := secAgentSnapshot()
	s.Windows = nil
	for i := range 16 {
		s.Windows = append(s.Windows, core.Window{Kind: fmt.Sprintf("w%d", i), UsedFrac: 0.5})
	}
	s.Plan = strings.Repeat("p", core.MaxLabelBytes)
	s.Source = strings.Repeat("s", core.MaxLabelBytes)
	s.Err = strings.Repeat("é", core.MaxErrorBytes/2)
	s.ModelRequests = map[string][]core.ModelCount{}
	for i := range 16 {
		counts := make([]core.ModelCount, 256)
		for j := range counts {
			counts[j] = core.ModelCount{Model: fmt.Sprintf("model-%d", j), Requests: core.MaxRecordTokens}
		}
		s.ModelRequests[fmt.Sprintf("k%d", i)] = counts
	}
	w := f.post(t, ingestKey, core.IngestRequest{SchemaVersion: 1, Host: "vm1", Snapshots: []core.Snapshot{s}})
	if w.Code != http.StatusOK || f.ing.calls != 1 {
		t.Fatalf("status %d %s calls=%d", w.Code, w.Body, f.ing.calls)
	}
}

// A snapshot built by the agent's own code path (quota.FetchClaudeSnapshot
// against a realistic usage API body) is accepted.
func TestSecIngestRealisticAgentSnapshotAccepted(t *testing.T) {
	const body = `{"five_hour":{"utilization":27.0,"resets_at":"2026-10-01T23:00:00.506990+00:00"},
 "seven_day":{"utilization":7.0,"resets_at":"2026-10-02T20:00:00.507011+00:00"},
 "seven_day_oauth_apps":null,"seven_day_opus":{"utilization":3.0,"resets_at":null},"seven_day_sonnet":null,
 "extra_usage":{"is_enabled":false},
 "limits":[{"kind":"session","group":"session","percent":27,"resets_at":"2026-10-01T23:00:00.506990+00:00"},
  {"kind":"weekly_scoped","group":"weekly","percent":0,"resets_at":"2026-10-02T20:00:00.507187+00:00",
   "scope":{"model":{"id":null,"display_name":"Fable"},"surface":null}}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(body)) }))
	t.Cleanup(srv.Close)
	cred := quota.ClaudeCredential{AccessToken: "tok", ExpiresAt: t0.Add(time.Hour).UnixMilli(), SubscriptionType: "max"}
	snap, err := quota.FetchClaudeSnapshot(context.Background(), srv.Client(), srv.URL, "ua", cred, "ignored", t0)
	if err != nil {
		t.Fatal(err)
	}
	snap.AccountID = "claude-max" // as agent.pushQuota does
	if len(snap.Windows) < 3 {
		t.Fatalf("unexpected windows %+v", snap.Windows)
	}
	f := newIngestFixture(t, true, true)
	w := f.post(t, ingestKey, core.IngestRequest{SchemaVersion: 1, Host: "vm1", Snapshots: []core.Snapshot{snap}})
	if w.Code != http.StatusOK || f.ing.calls != 1 {
		t.Fatalf("status %d %s calls=%d", w.Code, w.Body, f.ing.calls)
	}
}
