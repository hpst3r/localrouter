package app

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/identity"
)

const (
	policyWide   = "    claim: groups\n    user_values: [lr-users, contractors]\n    admin_values: [lr-admins]\n"
	policyNarrow = "    claim: groups\n    user_values: [lr-users]\n    admin_values: [lr-ops]\n"
)

// restart closes the app and builds it again from the fixture with access.
func (e *loginEnv) restart(access string) {
	e.t.Helper()
	_ = e.a.Close()
	e.f.access = access
	e.a = e.f.build(e.idp.overrides(e.clock))
}

// Narrowing the access policy in configuration and restarting must not leave
// credentials minted under the old policy alive: every session is gone,
// every admin is demoted until a fresh login re-grants the role, and keys
// (user and owned static) stay dead until their owner logs in again under
// the new policy.
func TestAccessPolicyNarrowingRevokesOldCredentials(t *testing.T) {
	e := newLoginEnv(t)
	e.restart(policyWide)

	malloryCookie, _ := e.login("mallory", map[string]any{"groups": []string{"lr-admins"}})
	opCookie, _ := e.login("operator", map[string]any{"groups": []string{"lr-admins"}})
	contrCookie, _ := e.login("contractor", map[string]any{"groups": []string{"contractors"}})
	userCookie, _ := e.login("nora", map[string]any{"groups": []string{"lr-users"}})
	if malloryCookie == "" || opCookie == "" || contrCookie == "" || userCookie == "" {
		t.Fatal("setup logins failed")
	}
	ctx := context.Background()
	opID := e.session(opCookie).Principal.UserID
	malloryID := e.session(malloryCookie).Principal.UserID
	contrKey, err := e.a.Identity.CreateKey(ctx, e.session(contrCookie).Principal.UserID, "ci", 0)
	if err != nil {
		t.Fatal(err)
	}
	noraKey, err := e.a.Identity.CreateKey(ctx, e.session(userCookie).Principal.UserID, "laptop", 0)
	if err != nil {
		t.Fatal(err)
	}

	// A sub-second offset: the provider's auth_time has second precision,
	// so a fresh login after the restart may carry an auth_time that is
	// earlier than the narrowing itself.
	e.clock.Advance(1500 * time.Millisecond)
	e.restart(policyNarrow)

	// The demoted admin's session is gone; it cannot reach admin routes or
	// delete the operator.
	if w := serve(e.a, "GET", "/ui/v1/admin/users", "", sessionReq(malloryCookie)); w.Code != http.StatusUnauthorized {
		t.Fatalf("demoted admin session on admin route = %d, want 401", w.Code)
	}
	if w := serve(e.a, "POST", "/ui/v1/admin/users/"+opID+"/delete", "", sessionReq(malloryCookie)); w.Code != http.StatusUnauthorized {
		t.Fatalf("demoted admin deleting operator = %d %s, want 401", w.Code, w.Body)
	}
	if u, err := e.a.Identity.User(ctx, opID); err != nil || u.Status != identity.StatusActive {
		t.Fatalf("operator after narrowing = %+v, %v", u, err)
	}
	for _, id := range []string{opID, malloryID} {
		if u, _ := e.a.Identity.User(ctx, id); u.Role != "user" {
			t.Fatalf("admin %s not demoted: role %q", id, u.Role)
		}
	}

	// The removed group's key and session no longer authenticate.
	if w := serve(e.a, "GET", "/v1/models", "", withBearer(contrKey.Token)); w.Code != http.StatusUnauthorized {
		t.Fatalf("removed group's key = %d, want 401", w.Code)
	}
	if w := serve(e.a, "GET", "/ui/v1/me", "", sessionReq(contrCookie)); w.Code != http.StatusUnauthorized {
		t.Fatalf("removed group's session = %d, want 401", w.Code)
	}
	// A still-allowed user's key waits for a fresh login under the new policy.
	if w := serve(e.a, "GET", "/v1/models", "", withBearer(noraKey.Token)); w.Code != http.StatusUnauthorized {
		t.Fatalf("still-allowed user's key before re-login = %d, want 401", w.Code)
	}
	noraCookie, w := e.login("nora", map[string]any{"groups": []string{"lr-users"}})
	if noraCookie == "" {
		t.Fatalf("re-login under the new policy: %d %s", w.Code, w.Body)
	}
	if w := serve(e.a, "GET", "/v1/models", "", withBearer(noraKey.Token)); w.Code != http.StatusOK {
		t.Fatalf("still-allowed user's key after re-login = %d, want 200", w.Code)
	}
	// The operator regains admin by logging in with the new admin group.
	opCookie, _ = e.login("operator", map[string]any{"groups": []string{"lr-ops"}})
	if w := serve(e.a, "GET", "/ui/v1/admin/users", "", sessionReq(opCookie)); w.Code != http.StatusOK {
		t.Fatalf("operator admin after re-login = %d", w.Code)
	}

	// Restarting with the identical policy revokes nothing.
	e.restart(policyNarrow)
	if w := serve(e.a, "GET", "/ui/v1/admin/users", "", sessionReq(opCookie)); w.Code != http.StatusOK {
		t.Fatalf("operator session after identical restart = %d", w.Code)
	}
	if w := serve(e.a, "GET", "/v1/models", "", withBearer(noraKey.Token)); w.Code != http.StatusOK {
		t.Fatalf("key after identical restart = %d", w.Code)
	}

	// A pure widening (a new user value, a new admin value) revokes nothing.
	e.restart("    claim: groups\n    user_values: [lr-users, interns]\n    admin_values: [lr-ops, lr-root]\n")
	if w := serve(e.a, "GET", "/ui/v1/admin/users", "", sessionReq(opCookie)); w.Code != http.StatusOK {
		t.Fatalf("operator session after widening = %d", w.Code)
	}
	if w := serve(e.a, "GET", "/v1/models", "", withBearer(noraKey.Token)); w.Code != http.StatusOK {
		t.Fatalf("key after widening = %d", w.Code)
	}
	if w := serve(e.a, "GET", "/ui/v1/me", "", sessionReq(noraCookie)); w.Code != http.StatusOK {
		t.Fatalf("session after widening = %d", w.Code)
	}
}
