package weblogin

import (
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc/oidctest"
)

// loginWith runs the flow with the fake IdP's claims edited by edit.
func (h *harness) loginWith(edit func(map[string]any)) *httptest.ResponseRecorder {
	h.t.Helper()
	h.idp.mu.Lock()
	h.idp.claimsEdit = edit
	h.idp.mu.Unlock()
	return h.login()
}

func (h *harness) signWith(sign func(payload []byte) string) {
	h.idp.mu.Lock()
	h.idp.sign = sign
	h.idp.mu.Unlock()
}

func TestTokenSignatureIssuerAudienceExpiryEnforced(t *testing.T) {
	unpublished := newECKey()
	cases := map[string]func(h *harness){
		"wrong issuer":   func(h *harness) { h.idp.claimsEdit = func(c map[string]any) { c["iss"] = h.idp.issuer + "/" } },
		"wrong audience": func(h *harness) { h.idp.claimsEdit = func(c map[string]any) { c["aud"] = "other-client" } },
		"expired": func(h *harness) {
			h.idp.claimsEdit = func(c map[string]any) { c["exp"] = h.clock.Now().Add(-time.Second).Unix() }
		},
		"not yet valid": func(h *harness) {
			h.idp.claimsEdit = func(c map[string]any) { c["nbf"] = h.clock.Now().Add(10 * time.Minute).Unix() }
		},
		"unknown signing key": func(h *harness) {
			h.signWith(func(p []byte) string { return oidctest.SignIDToken(unpublished, "rsa-1", "ES256", string(p)) })
		},
		"HS256 with client secret": func(h *harness) {
			h.signWith(func(p []byte) string {
				return oidctest.SignIDToken([]byte(testClientSecret), "rsa-1", "HS256", string(p))
			})
		},
		"alg none": func(h *harness) {
			h.signWith(func(p []byte) string {
				enc := base64.RawURLEncoding.EncodeToString
				return enc([]byte(`{"alg":"none","kid":"rsa-1"}`)) + "." + enc(p) + "."
			})
		},
		"alg not configured (PS256 by published key)": func(h *harness) {
			h.signWith(func(p []byte) string { return oidctest.SignIDToken(testRSAKey(0), "rsa-1", "PS256", string(p)) })
		},
		"garbage token": func(h *harness) { h.signWith(func([]byte) string { return "a.b.c" }) },
		"no id_token": func(h *harness) {
			h.idp.tokenRaw = func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(`{"access_token":"x","token_type":"Bearer"}`))
			}
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			setup(h)
			h.expectFailure(h.login(), http.StatusBadGateway, "token_invalid")
		})
	}
}

func TestTokenNonceMustMatchFlow(t *testing.T) {
	for name, edit := range map[string]func(map[string]any){
		"other nonce":   func(c map[string]any) { c["nonce"] = randToken() },
		"missing nonce": func(c map[string]any) { delete(c, "nonce") },
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.expectFailure(h.loginWith(edit), http.StatusBadGateway, "token_invalid")
		})
	}
}

func TestTokenAuthorizedParty(t *testing.T) {
	bad := map[string]func(map[string]any){
		"multi aud without azp": func(c map[string]any) { c["aud"] = []string{testClientID, "other"} },
		"multi aud other azp": func(c map[string]any) {
			c["aud"] = []string{testClientID, "other"}
			c["azp"] = "other"
		},
		"single aud other azp": func(c map[string]any) { c["azp"] = "other" },
		"azp not a string":     func(c map[string]any) { c["azp"] = 5 },
	}
	for name, edit := range bad {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.expectFailure(h.loginWith(edit), http.StatusBadGateway, "token_invalid")
		})
	}
	h := newHarness(t)
	w := h.loginWith(func(c map[string]any) {
		c["aud"] = []string{testClientID, "other"}
		c["azp"] = testClientID
	})
	if w.Code != http.StatusSeeOther {
		t.Fatalf("multi aud with matching azp: %d %q", w.Code, w.Body.String())
	}
}

func TestTokenIssuedAtWithinFlowWindow(t *testing.T) {
	for name, edit := range map[string]func(h *harness) func(map[string]any){
		"missing": func(h *harness) func(map[string]any) { return func(c map[string]any) { delete(c, "iat") } },
		"far future": func(h *harness) func(map[string]any) {
			return func(c map[string]any) { c["iat"] = h.clock.Now().Add(6 * time.Minute).Unix() }
		},
		"before flow window": func(h *harness) func(map[string]any) {
			return func(c map[string]any) { c["iat"] = h.clock.Now().Add(-6 * time.Minute).Unix() }
		},
		"not a number": func(h *harness) func(map[string]any) { return func(c map[string]any) { c["iat"] = "yesterday" } },
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.expectFailure(h.loginWith(edit(h)), http.StatusBadGateway, "token_invalid")
		})
	}
	h := newHarness(t)
	if w := h.loginWith(func(c map[string]any) { c["iat"] = h.clock.Now().Add(4 * time.Minute).Unix() }); w.Code != http.StatusSeeOther {
		t.Fatalf("iat within skew rejected: %d %q", w.Code, w.Body.String())
	}
}

func TestTokenAuthTimeMustBeFresh(t *testing.T) {
	cases := []struct {
		name   string
		edit   func(h *harness) func(map[string]any)
		status int
		code   string
	}{
		{"missing", func(h *harness) func(map[string]any) { return func(c map[string]any) { delete(c, "auth_time") } }, http.StatusUnauthorized, "stale_auth"},
		{"old idp session", func(h *harness) func(map[string]any) {
			return func(c map[string]any) { c["auth_time"] = h.clock.Now().Add(-time.Hour).Unix() }
		}, http.StatusUnauthorized, "stale_auth"},
		{"just outside skew", func(h *harness) func(map[string]any) {
			return func(c map[string]any) { c["auth_time"] = h.clock.Now().Add(-5*time.Minute - time.Second).Unix() }
		}, http.StatusUnauthorized, "stale_auth"},
		{"future", func(h *harness) func(map[string]any) {
			return func(c map[string]any) { c["auth_time"] = h.clock.Now().Add(time.Hour).Unix() }
		}, http.StatusBadGateway, "token_invalid"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			h.expectFailure(h.loginWith(c.edit(h)), c.status, c.code)
			if _, denials := h.hooks.snapshot(); len(denials) != 0 {
				t.Fatalf("LoginDenied called for unverified freshness: %v", denials)
			}
		})
	}
	// The verified auth_time is what reaches the hook.
	h := newHarness(t)
	at := h.clock.Now().Add(-2 * time.Minute)
	if w := h.loginWith(func(c map[string]any) { c["auth_time"] = at.Unix() }); w.Code != http.StatusSeeOther {
		t.Fatalf("fresh auth_time rejected: %d", w.Code)
	}
	if logins, _ := h.hooks.snapshot(); !logins[0].AuthTime.Equal(at) {
		t.Fatalf("AuthTime = %v, want %v", logins[0].AuthTime, at)
	}
}

func TestTokenSubjectAndTenantChecked(t *testing.T) {
	const tenant = "tenant" // contained in the fixture issuer path
	withTenant := func(cfg *Config) { cfg.TenantID = tenant }
	bad := map[string]func(map[string]any){
		"empty sub":      func(c map[string]any) { c["sub"] = "" },
		"missing sub":    func(c map[string]any) { delete(c, "sub") },
		"long sub":       func(c map[string]any) { c["sub"] = strings.Repeat("s", 256) },
		"control sub":    func(c map[string]any) { c["sub"] = "a\nb" },
		"tid mismatch":   func(c map[string]any) { c["tid"] = "other-tenant" },
		"tid missing":    func(c map[string]any) { delete(c, "tid") },
		"tid not string": func(c map[string]any) { c["tid"] = 1 },
	}
	for name, edit := range bad {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, withTenant)
			h.expectFailure(h.loginWith(func(c map[string]any) {
				c["tid"] = tenant
				edit(c)
			}), http.StatusBadGateway, "token_invalid")
		})
	}
	h := newHarness(t, withTenant)
	if w := h.loginWith(func(c map[string]any) { c["tid"] = tenant }); w.Code != http.StatusSeeOther {
		t.Fatalf("matching tid rejected: %d %q", w.Code, w.Body.String())
	}
	// Without a configured tenant the tid claim is not consulted.
	h = newHarness(t)
	if w := h.loginWith(func(c map[string]any) { c["tid"] = "anything" }); w.Code != http.StatusSeeOther {
		t.Fatalf("tid checked without TenantID: %d", w.Code)
	}
}

func TestExchangeFailureLogsOnlyFixedErrorClass(t *testing.T) {
	cases := map[string]struct {
		body, want string
	}{
		"invalid_grant": {`{"error":"invalid_grant","error_description":"raw-idp-body-marker"}`, "oauth_error=invalid_grant"},
		"hostile code":  {`{"error":"raw-idp-body-marker<script>","error_description":"x"}`, "oauth_error=other"},
		"not json":      {`raw-idp-body-marker`, "oauth_error=other"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.idp.tokenRaw = func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				w.Write([]byte(c.body))
			}
			w := h.login()
			h.expectFailure(w, http.StatusBadGateway, "exchange_failed")
			logs := h.logs.String()
			if !strings.Contains(logs, c.want) || strings.Contains(logs+w.Body.String(), "raw-idp-body-marker") {
				t.Fatalf("logs = %s", logs)
			}
		})
	}
}

func TestExchangeSecretErrorStopsBeforeTokenEndpoint(t *testing.T) {
	h := newHarness(t, func(c *Config) {
		c.ClientSecret = func() (string, error) { return "", errors.New("secret file mode 0644: secret-path-marker") }
	})
	h.expectFailure(h.login(), http.StatusBadGateway, "exchange_failed")
	if n := h.idp.hitCount("/tenant/token"); n != 0 {
		t.Fatalf("token endpoint called %d times", n)
	}
	if strings.Contains(h.logs.String(), "secret-path-marker") {
		t.Fatal("secret error logged")
	}
}

func TestTokenECDSASignedLogin(t *testing.T) {
	h := newHarness(t)
	h.idp.rotate(signingKey{kid: "ec-1", alg: "ES256", priv: newECKey()}, false)
	if w := h.login(); w.Code != http.StatusSeeOther {
		t.Fatalf("ES256 login: %d %q", w.Code, w.Body.String())
	}
}

func TestJWKSRotationRefetchesUnknownKeyID(t *testing.T) {
	h := newHarness(t)
	if w := h.login(); w.Code != http.StatusSeeOther {
		t.Fatalf("first login: %d", w.Code)
	}
	if n := h.idp.hitCount("/tenant/jwks"); n != 1 {
		t.Fatalf("jwks hits = %d, want 1", n)
	}
	// Cached keys serve the next login without a refetch.
	if w := h.login(); w.Code != http.StatusSeeOther || h.idp.hitCount("/tenant/jwks") != 1 {
		t.Fatalf("cached login: %d, jwks hits %d", w.Code, h.idp.hitCount("/tenant/jwks"))
	}
	h.idp.rotate(signingKey{kid: "rsa-2", alg: "RS256", priv: testRSAKey(1)}, false)
	if w := h.login(); w.Code != http.StatusSeeOther {
		t.Fatalf("login after rotation: %d %q", w.Code, w.Body.String())
	}
	if n := h.idp.hitCount("/tenant/jwks"); n != 2 {
		t.Fatalf("jwks hits after rotation = %d, want 2", n)
	}
}

func TestClientSecretPostAuthStyle(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.ClientAuth = ClientSecretPost })
	h.idp.authStyle = "post"
	if w := h.login(); w.Code != http.StatusSeeOther {
		t.Fatalf("post login: %d; idp failures %v", w.Code, h.idp.failureList())
	}
	// And a basic-auth IdP refuses a post-configured client (no auto-detect
	// retry that would send the secret twice).
	h2 := newHarness(t, func(c *Config) { c.ClientAuth = ClientSecretPost })
	h2.expectFailure(h2.login(), http.StatusBadGateway, "exchange_failed")
	if n := h2.idp.hitCount("/tenant/token"); n != 1 {
		t.Fatalf("token hits = %d, want exactly 1", n)
	}
}

func TestUserinfoAndClaimSourcesNeverFetched(t *testing.T) {
	h := newHarness(t)
	h.login()
	h.loginWith(func(c map[string]any) {
		c["_claim_names"] = map[string]any{"groups": "src1"}
		c["_claim_sources"] = map[string]any{"src1": map[string]any{"endpoint": h.idp.url("/tenant/userinfo")}}
	})
	if n := h.idp.hitCount("/tenant/userinfo"); n != 0 {
		t.Fatalf("userinfo fetched %d times", n)
	}
}

func TestVerifierConfigNeverSkipsChecks(t *testing.T) {
	h := newHarness(t)
	c := h.svc.verifierConfig()
	if c.SkipClientIDCheck || c.SkipExpiryCheck || c.SkipIssuerCheck || c.InsecureSkipSignatureCheck ||
		c.ClientID != testClientID || c.Now == nil || strings.Join(c.SupportedSigningAlgs, ",") != "RS256,ES256" {
		t.Fatalf("verifier config = %+v", c)
	}
}

func TestHangingTokenEndpointIsBounded(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.Outbound.Timeout = 300 * time.Millisecond })
	release := make(chan struct{})
	defer close(release)
	h.idp.tokenRaw = func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}
	start := time.Now()
	h.expectFailure(h.login(), http.StatusBadGateway, "exchange_failed")
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("exchange took %v", d)
	}
}
