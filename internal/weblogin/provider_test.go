package weblogin

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestDiscoveryRejectsUnsafeOrInvalidMetadata(t *testing.T) {
	cases := map[string]func(p *fakeIdP){
		"jwks on other host": func(p *fakeIdP) {
			p.discoveryEdit = func(d map[string]any) { d["jwks_uri"] = hostURL(otherHost, p.srv, "/tenant/jwks") }
		},
		"token on other host": func(p *fakeIdP) {
			p.discoveryEdit = func(d map[string]any) { d["token_endpoint"] = hostURL(otherHost, p.srv, "/t") }
		},
		"authorize on http": func(p *fakeIdP) {
			p.discoveryEdit = func(d map[string]any) { d["authorization_endpoint"] = "http://" + idpHost + "/a" }
		},
		"token other port": func(p *fakeIdP) {
			p.discoveryEdit = func(d map[string]any) { d["token_endpoint"] = "https://" + idpHost + ":1/t" }
		},
		"token userinfo": func(p *fakeIdP) {
			p.discoveryEdit = func(d map[string]any) { d["token_endpoint"] = "https://u@" + idpHost + "/t" }
		},
		"missing jwks": func(p *fakeIdP) { p.discoveryEdit = func(d map[string]any) { delete(d, "jwks_uri") } },
		"no code response type": func(p *fakeIdP) {
			p.discoveryEdit = func(d map[string]any) { d["response_types_supported"] = []string{"id_token"} }
		},
		"no common signing alg": func(p *fakeIdP) {
			p.discoveryEdit = func(d map[string]any) { d["id_token_signing_alg_values_supported"] = []string{"HS256"} }
		},
		"issuer mismatch": func(p *fakeIdP) { p.discoveryEdit = func(d map[string]any) { d["issuer"] = p.issuer + "/" } },
		"redirect": func(p *fakeIdP) {
			p.discoveryRaw = func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, "/tenant/jwks", http.StatusFound)
			}
		},
		"oversized body": func(p *fakeIdP) {
			p.discoveryRaw = func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte(`{"issuer":"` + strings.Repeat("x", 2<<20) + `"}`))
			}
		},
		"server error": func(p *fakeIdP) {
			p.discoveryRaw = func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "raw-idp-body-marker", http.StatusInternalServerError)
			}
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			setup(h.idp)
			if err := h.svc.Discover(context.Background()); err != errProviderUnavailable {
				t.Fatalf("Discover err = %v, want errProviderUnavailable", err)
			}
			if h.svc.Ready() {
				t.Fatal("ready after invalid discovery")
			}
			if h.idp.hitCount("/tenant/jwks") != 0 {
				t.Fatal("redirect target or jwks fetched")
			}
			logs := h.logs.String()
			if !strings.Contains(logs, "provider discovery failed") || strings.Contains(logs, "raw-idp-body-marker") ||
				strings.Contains(logs, idpHost) || strings.Contains(logs, "xxxx") {
				t.Fatalf("unexpected discovery log: %s", logs)
			}
		})
	}
}

func TestDiscoveryRetryGapAndRecovery(t *testing.T) {
	h := newHarness(t)
	h.idp.discoveryRaw = func(w http.ResponseWriter, r *http.Request) { http.Error(w, "down", 503) }
	const path = "/tenant/.well-known/openid-configuration"
	if h.svc.Discover(context.Background()) == nil {
		t.Fatal("discovery succeeded against failing provider")
	}
	h.idp.mu.Lock()
	h.idp.discoveryRaw = nil
	h.idp.mu.Unlock()
	h.clock.Advance(29 * time.Second)
	if h.svc.Discover(context.Background()) == nil || h.idp.hitCount(path) != 1 {
		t.Fatalf("retried inside the gap: hits=%d", h.idp.hitCount(path))
	}
	h.clock.Advance(time.Second)
	if err := h.svc.Discover(context.Background()); err != nil || !h.svc.Ready() {
		t.Fatalf("no recovery after gap: %v", err)
	}
	if h.idp.hitCount(path) != 2 {
		t.Fatalf("hits=%d, want 2", h.idp.hitCount(path))
	}
}

func TestDiscoverySucceedsAgainstFixtureIssuer(t *testing.T) {
	h := newHarness(t)
	if h.svc.Ready() {
		t.Fatal("ready before discovery")
	}
	if err := h.svc.Discover(context.Background()); err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if !h.svc.Ready() {
		t.Fatal("not ready after discovery")
	}
	if n := h.idp.hitCount("/tenant/.well-known/openid-configuration"); n != 1 {
		t.Fatalf("discovery hits = %d, want 1", n)
	}
	// Success is cached for the process lifetime.
	if err := h.svc.Discover(context.Background()); err != nil {
		t.Fatalf("second Discover: %v", err)
	}
	if n := h.idp.hitCount("/tenant/.well-known/openid-configuration"); n != 1 {
		t.Fatalf("discovery hits after cache = %d, want 1", n)
	}
}

func TestDiscoveryNotAbortedByCanceledRequest(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := h.svc.Discover(ctx); err != nil || !h.svc.Ready() {
		t.Fatalf("a disconnecting browser aborted discovery: %v", err)
	}
}
