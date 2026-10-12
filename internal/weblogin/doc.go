// Package weblogin implements the browser OpenID Connect login flow for
// LocalRouter's optional multi-user mode: GET /auth/login, GET /auth/callback
// and the local POST /auth/logout.
//
// The flow is the authorization-code grant for a confidential client with
// PKCE (S256), state, nonce, a browser-bound login cookie, and both
// max_age=0 and prompt=login to ask for a fresh IdP authentication. Which of
// the two a provider honours varies, and no real provider has been tested;
// the returned auth_time is always checked, so a provider that ignores both
// yields a stale_auth failure, never a renewed login. ID token signatures,
// issuer, audience and expiry are verified by github.com/coreos/go-oidc/v3;
// this package adds the checks go-oidc leaves to the caller (nonce, azp, iat,
// auth_time freshness, tid) and evaluates the configured access Policy before
// any Hooks callback runs. Nothing here verifies a JWT by hand.
//
// Pending flows are stateless: GET /auth/login seals state, nonce, PKCE
// verifier, creation and expiry into the __Host-lr_login cookie with
// AES-256-GCM under a process-random key, and the callback accepts only a
// state equal to the one sealed in that browser's cookie. Anonymous logins
// therefore hold no server state, from one address or many, and browsers
// sharing a reverse proxy's address do not affect each other. A restart
// invalidates flows in progress. Each state is exchanged at most once: a
// bounded cache refuses replays and concurrent callbacks before the token
// endpoint, keeps a state until its flow expires only after the IdP returned
// an ID token signed for its nonce, and forgets it if the exchange fails, so
// only IdP-authenticated logins (MaxRedeemedLogins within LoginTimeout) can
// fill it, and a full cache fails closed with 503. The IdP's own single-use
// codes remain the primary guarantee.
//
// weblogin has no storage. A verified, policy-admitted login is handed to
// Hooks.CompleteLogin, which the app adapter implements over the identity
// store (resolve or JIT-provision the user, create a session). The adapter
// returns the session cookie secret; weblogin only sets and clears cookies.
//
// All IdP traffic (discovery, JWKS, token endpoint) goes through one guarded
// *http.Client: https only, issuer-host allowlist, no redirects, a body cap,
// timeouts, no proxy, and a dial-time IP guard that always refuses link-local
// and cloud metadata addresses. The userinfo endpoint and distributed-claim
// sources are never fetched.
//
// Nothing in this package logs codes, states, nonces, verifiers, tokens,
// client secrets, subjects, emails, names, URL queries or IdP response bodies.
// Failures are logged and reported by fixed reason codes only.
package weblogin
