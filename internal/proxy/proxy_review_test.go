package proxy

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hpst3r/localrouter/internal/core"
)

// The default upstream client has no ResponseHeaderTimeout: an upstream that
// accepts the request but never answers holds the lease (and the client)
// until the client gives up. internal/app does not pass an HTTPClient.
func TestReviewDefaultClientHasResponseHeaderTimeout(t *testing.T) {
	p := New(Deps{}, Options{})
	tr, ok := p.opts.HTTPClient.Transport.(*http.Transport)
	if !ok || tr == nil {
		tr = http.DefaultTransport.(*http.Transport)
	}
	if tr.ResponseHeaderTimeout <= 0 {
		t.Fatalf("ResponseHeaderTimeout = %v; want > 0", tr.ResponseHeaderTimeout)
	}
}

type panicCreds struct{}

func (panicCreds) Credential(context.Context, string) (core.Credential, error) { panic("boom") }
func (panicCreds) Invalidate(string)                                           {}

// A panic anywhere between Acquire and Release (credential source, ledger,
// quota observer, usage parser) is recovered by net/http, but the lease is
// never released, permanently inflating inflight(A).
func TestReviewLeaseReleasedOnPanic(t *testing.T) {
	pol := newFakePolicy()
	p := New(Deps{
		Accounts: map[string]core.Account{"a": {ID: "a", Provider: core.ProviderOllama, BaseURL: "http://127.0.0.1:1"}},
		Routes:   []core.Route{{Name: "r", Models: []string{"m"}, Interactive: []string{"a"}, Background: []string{"a"}}},
		Creds:    panicCreds{}, Quota: newFakeQuota(), Policy: pol, Ledger: newFakeLedger(),
		Logger:       slog.New(slog.DiscardHandler),
		Authenticate: func(string) (core.Client, bool) { return core.Client{Name: "c", Class: core.ClassInteractive}, true },
	}, Options{})
	srv := httptest.NewUnstartedServer(p.Handler())
	srv.Config.ErrorLog = nil
	srv.Start()
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/responses", strings.NewReader(`{"model":"m"}`))
	req.Header.Set("Authorization", "Bearer k")
	if resp, err := http.DefaultClient.Do(req); err == nil {
		resp.Body.Close()
	}
	pol.mu.Lock()
	defer pol.mu.Unlock()
	if len(pol.leases) != 1 {
		t.Fatalf("leases = %d", len(pol.leases))
	}
	l := pol.leases[0]
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.released == 0 {
		t.Fatal("lease leaked after panic")
	}
}
