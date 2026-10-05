package quota

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
)

// MH6: an agent-sourced claude account is never polled; ingested snapshots
// appear in Latest; older ones are ignored; non-agent accounts are rejected.
func TestAgentSourcedClaudeNeverPolled(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Write([]byte(claudeUsageBody))
	}))
	t.Cleanup(srv.Close)
	var credReads atomic.Int32
	clk := &fakeClock{t: t0}
	m := New([]core.Account{
		{ID: "cl-agent", Provider: core.ProviderClaude, QuotaSource: QuotaSourceAgent},
		{ID: "cl-local", Provider: core.ProviderClaude, QuotaSource: "local"},
	}, nil, Options{
		Clock:          clk,
		HTTPClient:     srv.Client(),
		ClaudeUsageURL: srv.URL,
		ClaudeCredentialsFile: func(id string) string {
			if id == "cl-agent" {
				credReads.Add(1)
			}
			return "" // local account fails with a sanitized Err; no HTTP
		},
		PollInterval: time.Hour,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.Start(ctx)
	m.RequestRefresh("cl-agent", true)
	m.RequestRefresh("cl-agent", false)
	m.refresh(ctx, "cl-agent", true)
	m.wait()
	// Let the local poller run so a stray agent poll would have happened too.
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, ok := m.Latest("cl-local"); ok || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if hits.Load() != 0 || credReads.Load() != 0 {
		t.Fatalf("agent account polled: hits=%d credReads=%d", hits.Load(), credReads.Load())
	}
	if _, ok := m.Latest("cl-agent"); ok {
		t.Fatal("Latest ok before any ingest")
	}

	allowed := true
	snap := core.Snapshot{AccountID: "cl-agent", FetchedAt: t0, Source: SourceUsageAPI, Plan: "max", Allowed: &allowed,
		Windows: []core.Window{{Kind: core.Window5h, UsedFrac: 0.4, WindowSeconds: 18000}}}
	if err := m.IngestSnapshot(snap); err != nil {
		t.Fatal(err)
	}
	snap.Windows[0].UsedFrac = 0.9 // caller mutation must not leak in
	*snap.Allowed = false
	got, ok := m.Latest("cl-agent")
	if !ok || !got.FetchedAt.Equal(t0) || got.Plan != "max" || len(got.Windows) != 1 || got.Windows[0].UsedFrac != 0.4 || got.Allowed == nil || !*got.Allowed {
		t.Fatalf("latest %+v ok=%v", got, ok)
	}

	for _, at := range []time.Time{t0, t0.Add(-time.Minute)} {
		old := core.Snapshot{AccountID: "cl-agent", FetchedAt: at, Windows: []core.Window{{Kind: core.Window5h, UsedFrac: 0.1}}}
		if err := m.IngestSnapshot(old); !errors.Is(err, ErrSnapshotStale) {
			t.Fatalf("FetchedAt %v: err=%v, want ErrSnapshotStale", at, err)
		}
	}
	if got, _ := m.Latest("cl-agent"); got.Windows[0].UsedFrac != 0.4 {
		t.Fatalf("stale snapshot replaced stored one: %+v", got)
	}

	clk.Advance(time.Minute) // FetchedAt is capped at the manager's clock
	newer := core.Snapshot{AccountID: "cl-agent", FetchedAt: t0.Add(time.Minute), Err: "claude token expired; run claude to refresh"}
	if err := m.IngestSnapshot(newer); err != nil {
		t.Fatal(err)
	}
	if got, _ := m.Latest("cl-agent"); got.Err != newer.Err || len(got.Windows) != 0 {
		t.Fatalf("agent-reported Err not preserved: %+v", got)
	}

	for _, id := range []string{"cl-local", "nope"} {
		if err := m.IngestSnapshot(core.Snapshot{AccountID: id, FetchedAt: t0.Add(time.Hour)}); err == nil || errors.Is(err, ErrSnapshotStale) {
			t.Fatalf("ingest for %q: err=%v, want rejection", id, err)
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("usage server hit %d times", hits.Load())
	}
}

func TestIngestRejectsNonClaudeAgentAccount(t *testing.T) {
	// quota_source is only meaningful for claude; a codex account with it set
	// is still polled and must not accept ingests.
	m := New([]core.Account{{ID: "cx", Provider: core.ProviderCodex, QuotaSource: QuotaSourceAgent}}, nil, Options{})
	if err := m.IngestSnapshot(core.Snapshot{AccountID: "cx", FetchedAt: t0}); err == nil {
		t.Fatal("codex ingest accepted")
	}
}

func TestFetchClaudeSnapshot(t *testing.T) {
	var hits atomic.Int32
	var status atomic.Int32
	status.Store(http.StatusOK)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Header.Get("Authorization") != "Bearer "+claudeToken || r.Header.Get("User-Agent") != "agent-ua" {
			t.Errorf("bad headers")
		}
		w.WriteHeader(int(status.Load()))
		if status.Load() == http.StatusOK {
			w.Write([]byte(claudeUsageBody))
		} else {
			w.Write([]byte(`{"error":"` + claudeToken + `"}`))
		}
	}))
	t.Cleanup(srv.Close)
	cred := ClaudeCredential{AccessToken: claudeToken, ExpiresAt: t0.Add(time.Hour).UnixMilli(), SubscriptionType: "max"}
	ctx := context.Background()

	s, err := FetchClaudeSnapshot(ctx, srv.Client(), srv.URL, "agent-ua", cred, "acct", t0)
	if err != nil {
		t.Fatal(err)
	}
	if s.AccountID != "acct" || !s.FetchedAt.Equal(t0) || s.Plan != "max" || s.Source != SourceUsageAPI || len(s.Windows) != 3 {
		t.Fatalf("snapshot %+v", s)
	}
	if w := findWindow(t, s, core.Window5h); w.UsedFrac != 0.27 {
		t.Fatalf("5h %+v", w)
	}

	// Expired: exactly at expiresAt, no request.
	before := hits.Load()
	if _, err := FetchClaudeSnapshot(ctx, srv.Client(), srv.URL, "agent-ua", cred, "acct", t0.Add(time.Hour)); !errors.Is(err, ErrClaudeTokenExpired) {
		t.Fatalf("expired: err=%v", err)
	}
	if hits.Load() != before {
		t.Fatal("expired token hit the network")
	}

	status.Store(http.StatusUnauthorized)
	_, err = FetchClaudeSnapshot(ctx, srv.Client(), srv.URL, "agent-ua", cred, "acct", t0)
	if !errors.Is(err, ErrClaudeTokenRejected) || strings.Contains(err.Error(), claudeToken) {
		t.Fatalf("401: err=%v", err)
	}

	status.Store(http.StatusInternalServerError)
	_, err = FetchClaudeSnapshot(ctx, srv.Client(), srv.URL, "agent-ua", cred, "acct", t0)
	if err == nil || err.Error() != "usage api: http 500" {
		t.Fatalf("500: err=%v", err)
	}
}

func TestParseClaudeCredentials(t *testing.T) {
	good := `{"claudeAiOauth":{"accessToken":"` + claudeToken + `","refreshToken":"sk-ant-ort01-REFRESH","expiresAt":1790000000000,"subscriptionType":"max"}}`
	c, err := ParseClaudeCredentials([]byte(good))
	if err != nil {
		t.Fatal(err)
	}
	if c != (ClaudeCredential{AccessToken: claudeToken, ExpiresAt: 1790000000000, SubscriptionType: "max"}) {
		t.Fatalf("cred mismatch")
	}
	for _, bad := range []string{
		``,
		`not json ` + claudeToken,
		`{}`,
		`{"claudeAiOauth":null}`,
		`{"claudeAiOauth":{"accessToken":""}}`,
		`{"claudeAiOauth":{"accessToken":42}}`,
	} {
		_, err := ParseClaudeCredentials([]byte(bad))
		if !errors.Is(err, ErrClaudeCredentialsMalformed) || strings.Contains(err.Error(), claudeToken) {
			t.Fatalf("%q: err=%v", bad, err)
		}
	}
}

// A pushed FetchedAt ahead of the manager's clock is capped at now, so a
// host with a fast clock cannot make later genuine snapshots look stale.
func TestIngestSnapshotCapsFutureFetchedAt(t *testing.T) {
	clk := &fakeClock{t: t0}
	m := New([]core.Account{{ID: "cl", Provider: core.ProviderClaude, QuotaSource: QuotaSourceAgent}}, nil, Options{Clock: clk})
	if err := m.IngestSnapshot(core.Snapshot{AccountID: "cl", FetchedAt: t0.Add(4 * time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if got, _ := m.Latest("cl"); !got.FetchedAt.Equal(t0) {
		t.Fatalf("FetchedAt %v, want %v", got.FetchedAt, t0)
	}
	clk.Advance(time.Minute)
	if err := m.IngestSnapshot(core.Snapshot{AccountID: "cl", FetchedAt: t0.Add(time.Minute)}); err != nil {
		t.Fatalf("later genuine snapshot: %v", err)
	}
}
