# OIDC multi-user mode

An optional `identity:` block turns LocalRouter into a small multi-user
gateway. People sign in through one OpenID Connect provider in a browser.
There they create their own LocalRouter API keys, and applications use those
keys for inference. Without the block, LocalRouter behaves exactly as the
single-user router described in the [README](../README.md).

> **Status.** This page is checked against the current source, fake signed
> IdP fixtures and a locally built binary. No real Entra ID or authentik
> tenant has been tested yet. See [Verification status](#verification-status)
> before you deploy.

Sources: `internal/config/identity.go` (configuration),
`internal/weblogin` (browser login), `internal/identity` (users, keys,
sessions and static-key registry in `identity.db`), `internal/authz` and
`internal/control` (permissions and API), `internal/proxy` (inference relay),
`cmd/localrouter/users.go` (CLI). The example config is
[`config.example.multiuser.yaml`](../config.example.multiuser.yaml).

## 1. Model

| Who | Credential | Role | Can |
|---|---|---|---|
| Person in a browser | `__Host-lr_session` cookie from an OIDC login | `user` or `admin` | Manage their own keys and see their own usage and budget. Admins can also manage users and see global data. |
| Application | `lrk_…` key that a user created in the UI | always `user` | Run inference as that user, and read only that user's usage. |
| Static key file, `role: user` | key file + `owner: u_…` | `user` | Same as a user key, owned by an existing user. |
| Static key file, `role: service` | key file | `service` | Read global status, diagnostics, budgets and all-user usage, and admit with account detail. Ingest only with `ingest: true`. No user administration. |

- **Admin is browser-only.** API keys always authenticate as `user`. A
  static `role: admin` is rejected, and there is no way to promote an admin
  through the API or the CLI.
- **Separate surfaces.** `/v1/*` and `/control/v1/*` accept only bearer keys
  and ignore cookies. `/ui/v1/*` accepts only the session cookie, and a request
  that also carries `Authorization` gets 400.
- **Users are identified by an exact (issuer, `sub`) pair** and receive an
  opaque ID (`u_` followed by 26 base32 characters). Email and display name are
  stored for display only. They are never used for matching, and they never
  appear in the ledger or budgets.
- **There is no SCIM, no IdP push and no refresh token.** LocalRouter learns
  about changes at the IdP only when that person logs in again (see
  [§6](#6-lifetimes-and-revocation)).

## 2. Requirements

- **An HTTPS public origin.** `public_base_url` is the exact origin browsers
  use: https, no path, and lower-cased with `:443` dropped. Its host must be
  loopback (`localhost` is fine) or be listed in `allowed_hosts` with
  `allow_non_loopback: true`. Session cookies are `Secure` with the `__Host-`
  prefix, so a TLS endpoint is required even for `https://localhost`.
- **TLS at the NGINX edge.** This guide assumes the NGINX edge from
  [Container deployment](CONTAINERS.md), which terminates TLS and forwards
  `Host` verbatim. No packaging change is needed. LocalRouter never trusts
  `X-Forwarded-*`. The callback host and the Origin/CSRF checks come only from
  `public_base_url`. With the rootless edge on port 8443, the origin is
  `https://router.example.com:8443`.
- `control.require_auth: true`.
- **A client secret file.** The OIDC client secret is read only from
  `client_secret_file`, never inline or from the environment. The file must be
  a private (0600) regular file of at most 4096 bytes holding one value. It
  must also be a different file from every other key file. The file is re-read
  at each token exchange.
- **Linux or macOS.** Multi-user mode needs an exclusive `identity.lock`, and
  on other platforms it refuses to start. The Windows binary still compiles,
  but nothing has been run on Windows in either mode.

## 3. Configuration

Durations use Go syntax (`2160h`, not `90d`). Lifetimes can only be tightened
below the ceilings.

| Key | Default | Rule |
|---|---|---|
| `identity.public_base_url` | — | Required, see §2. Callback is `<public_base_url>/auth/callback`. |
| `oidc.issuer` | — | Exact `iss`, compared byte for byte. https only. Templated or Entra `common`/`organizations`/`consumers` issuers are rejected. |
| `oidc.client_id` | — | 1..256 printable characters. |
| `oidc.client_secret_file` | — | Required; relative paths resolve from the config directory. |
| `oidc.client_auth` | `client_secret_basic` | Or `client_secret_post`. |
| `oidc.scopes` | `[openid, profile]` | Must include `openid`. `offline_access` is refused. |
| `oidc.signing_algs` | `[RS256]` | RS/ES/PS 256–512 or EdDSA only. HMAC is refused. |
| `oidc.tenant_id` | — | Optional. Must appear in the issuer; the `tid` claim must equal it. |
| `oidc.allow_private_network` | `false` | Lets the IdP resolve to loopback, RFC 1918, CGNAT or ULA addresses. Link-local and metadata addresses stay blocked. |
| `oidc.extra_endpoint_hosts` | `[]` | Bare host names that discovery may use besides the issuer host. |
| `oidc.http_timeout` | `10s` | At most `60s`. |
| `access.claim` | — | Required. The ID-token claim (string or string array) to match, e.g. `groups` or `roles`. Identity and profile claims (`sub`, `email`, `name`, …, `hasgroups`) are refused. |
| `access.user_values` / `admin_values` | — | Exact, case-sensitive values. At least one value overall, and no value in both lists. |
| `access.admin_subjects` | — | Exact `sub` values to promote to admin. Each still needs an allowed claim value. You need `admin_values` or `admin_subjects`. |
| `keys.max_ttl` | `2160h` (90 d) | Ceiling. Default lifetime of a new key. Tightening it also shortens existing keys. |
| `keys.require_login_within` | `720h` (30 d) | Ceiling. A key works only while its owner's last allowed browser login is this recent. |
| `keys.max_per_user` | `10` | 1..100 live keys. |
| `session.absolute_ttl` / `idle_ttl` | `24h` / `8h` | Ceilings. `idle_ttl` ≤ `absolute_ttl`. |
| `clients[].name` | — | Must not start with `k_` or `u_` (reserved for user key IDs and user IDs). |
| `clients[].role` | — | Required in this mode: `service` or `user`. Static clients are optional. |
| `clients[].owner` | — | `role: user` only. Must be a `u_…` ID. `ingest` is not allowed. See §5 and §6 for registration and revocation. |
| `limits.max_concurrent_per_user` | `0` | Per-user cap across all of that user's keys. 0 = unlimited. |
| `budgets.users` | — | Default `daily_usd`/`monthly_usd` for every user (see §7). |

Shorter session lifetimes apply to newly created sessions. Existing sessions
retain their stored deadlines (never beyond the original 24h/8h ceilings);
disable the user to revoke them immediately.

The `identity` block is restart-only. Run `localrouter check -config PATH`
after every edit. It validates the block and checks the secret file's mode,
size and content without printing it. It also rejects a `k_`/`u_` client name
and a static key file that starts with the reserved `lrk_` prefix.

## 4. Identity provider setup

### Any OIDC provider

LocalRouter uses the authorization code flow as a confidential client. Every
login uses PKCE S256, `state`, `nonce`, `max_age=0` and `prompt=login`.
Configure the provider like this:

1. **Client.** A confidential web client. Set the redirect URI to exactly
   `<public_base_url>/auth/callback`; do not use a wildcard or regex.
2. **Signing.** Asymmetric ID-token signing (RS256 unless `signing_algs`
   says otherwise).
3. **`auth_time`.** The ID token must carry `auth_time`. The callback accepts
   it only if it falls between 5 minutes before the login started and 5
   minutes after now. If it is missing or older, the login fails with
   `stale_auth`. This prevents an old IdP session from renewing the 30-day key
   window. A provider that honours neither `max_age=0` nor `prompt=login`
   therefore fails closed for users with an older IdP session.
4. **Access claim.** The ID token itself must carry `access.claim`.
   LocalRouter never calls userinfo and never follows distributed claims. The
   claim is evaluated as follows:
   - A string or array of strings is matched exactly.
   - A missing claim, or one with no allowed value, gives `not_permitted`.
   - Any other JSON type, or more than 1024 values, gives `claim_malformed`.
   - Group overage gives `group_overage` and the login is denied. Overage means
     the claim appears in `_claim_names`, or `hasgroups` is present while
     `claim: groups`.
   - Denying a known user also revokes all of their keys, sessions and owned
     static keys.
5. **Discovery.** Discovery is fetched lazily at the first login, with no
   redirects, a body cap and a timeout. `/readyz` reflects only the local
   identity database, never IdP reachability.

**Pending logins hold no server state.** `GET /auth/login` seals the state,
nonce, PKCE verifier and expiry into the `__Host-lr_login` cookie with
AES-256-GCM, under a random key generated at process start. Nothing is
persisted, and anonymous `/auth/login` requests consume nothing, even from a
single address behind NGINX. The flow expires after 10 minutes. A restart
invalidates logins in progress, and the user starts again.

Each state is exchanged at most once. A small replay cache remembers a state
only after the IdP returned an ID token signed for its nonce, and keeps it
until the flow expires (at most 256 entries). A full cache, or all 4
token-exchange slots busy, gives `503 busy`. Only logins that authenticated
at the IdP can fill the cache, including logins the access policy then
denies. These limits are not configurable.

### Microsoft Entra ID

Facts are from Microsoft Learn, fetched 2026-10-10: [OIDC protocol][e-oidc],
[ID token claims][e-idt], [optional claims][e-opt], [app roles][e-roles].

- Register a **single-tenant** web app. Use the redirect URI above and create a
  client secret. Store the secret only in the secret file.
- Set `issuer: https://login.microsoftonline.com/<tenant-id>/v2.0` and
  `tenant_id: <tenant-id>`. All v2 endpoints are served under that tenant
  authority, so `extra_endpoint_hosts` is not needed.
- **Add the optional ID-token claim `auth_time`** ("Time when the user last
  authenticated") under Token configuration. Without it every login fails
  with `stale_auth`.
- **Prefer app roles over groups:** use `claim: roles`, with
  `user_values`/`admin_values` set to the app roles' **Value** strings. Assign
  the roles to users or groups. With `claim: groups`, a user in more than 200
  groups gets the overage form, and LocalRouter denies the login.
- `sub` is specific to each application, so prefer `admin_values` over
  `admin_subjects`. LocalRouter never displays subjects.
- Microsoft documents `prompt=login` as forcing credential entry. It does not
  document `max_age`. Untested against a real tenant.

### authentik

Facts are from the [OAuth2 provider docs][a-doc] and the authentik source
(`authentik/providers/oauth2/views/authorize.py`, `id_token.py` at `main`
commit `a40205dc`, fetched 2026-10-10).

- Create an OAuth2/OpenID provider with client type **Confidential**.
- Set a strict redirect URI. If the field is left empty, authentik stores
  whichever URI is used first.
- **Select a Signing Key.** Without one, authentik signs with HS256 using the
  client secret, and LocalRouter refuses that.
- The default per-provider issuer is
  `https://<authentik-host>/application/o/<slug>/`, including the trailing
  slash. Copy it exactly.
- Use the `profile` scope (the default), which carries group membership. Set
  `claim: groups` and match group names. For a LAN authentik host, set
  `allow_private_network: true`.
- The authorize view treats `max_age=0` as unset (`if self.params.max_age:`),
  but `prompt=login` makes it re-authenticate the user, and `auth_time` is
  taken from that new login. This is from reading the source; it is untested
  against a real authentik.

## 5. First start, admins and onboarding

There is no first-user admin, no email-domain rule and no CLI command that
grants roles. Admin access comes only from `admin_values` or `admin_subjects`
in the config, and every admin also needs an allowed claim value.

1. Write the config and the secret file (0600), run `localrouter check`, then
   start the server. The first start creates `<data_dir>/identity.db` and pins
   the issuer and client ID into it (§9).
2. An admin opens `<public_base_url>/` (redirects to `/ui/`) and signs in. A
   new user is provisioned at login (JIT), but only after the claim matches.
   Roles are taken from the claim at every login.
3. Each person signs in and creates keys in the UI. Every key has a name and
   an expiry of at most `max_ttl`. The token is shown once and never stored in
   plaintext. Applications send it as `Authorization: Bearer lrk_…` to
   `/v1/*`.
4. **Static key owned by a user (optional).** The owner signs in once. Find
   their ID with `localrouter users list -config PATH` or in the admin UI.
   Add a client with `role: user, owner: u_…`, then reload or restart.
   - Every start and reload registers a digest of each owned key, used or not,
     and binds it to that owner for good.
   - The key authenticates only while the owner is active and their last
     allowed login is within `require_login_within`.
   - An owner ID that does not exist yet does not block startup. The key gets
     401 until that exact user exists.
   - A key added while the owner is disabled, or after re-enable but before
     their next login, is registered already revoked.
   - Moving a key to a different owner is refused: the reload is rejected
     ("client keys invalid") and a start fails.
5. **Service keys.** Give `role: service` only to agents and monitors that
   need global reads. Add `ingest: true` only to agents that push usage.
   Ingested rows are always unowned; a service key cannot set an owner.

## 6. Lifetimes and revocation

- **A key works only while all of these hold:**
  - the key is not revoked and not expired (at most 90 days);
  - the owner is active;
  - the owner's last allowed browser login is within 30 days.

  All three are checked on every request. Logging in again restores the
  30-day window for live keys. Creating a key needs a login within that same
  window; otherwise the API returns `reauth_required`.
- **Sessions** last at most 8 hours idle and 24 hours in total. Logout
  (`POST /auth/logout`) ends only the LocalRouter session, not the IdP session.
- **Local disable and delete take effect at the next request**, including a
  CLI change made while the server runs. They revoke every key, session and
  owned static key for good, and enabling the user again does not bring them
  back. Requests already in flight finish. An admin cannot disable or delete
  themselves (409); the CLI can.
- **Owned static keys after a disable or policy denial.** Restarting,
  reloading or restoring the old key file does not revive them. To give the
  user a static key again:
  1. Enable the user (after a disable).
  2. Wait for the user's next allowed login.
  3. Write a new key to the file and reload.
- **IdP-side removal is not immediate.**
  - If the person attempts a new login after being removed from the allowed
    groups, LocalRouter denies it and revokes everything.
  - Otherwise their keys keep working until the 30-day login window or the
    key's expiry lapses. Their sessions keep working for up to 24 hours.
  - An admin demoted in the IdP keeps admin rights until the session ends or
    they log in again.
  - For immediate cut-off, disable the user locally.

## 7. Budgets and concurrency

- `budgets.users` sets a default day and month ceiling for every identity user
  (user keys and `role: user` static keys). It is charged against the same
  durable reservation as the existing client and account ceilings, in one
  transaction.
  - The bucket is per user and shared across all of that user's keys, so a new
    key does not reset spend.
  - Amounts are exact decimal strings, and `"0"` blocks all spend.
  - `reserve_usd` is a fixed hold per attempt, not a cap on the provider bill
    ([Spend controls](SPEND-CONTROLS.md)).
  - Per-user overrides are not supported yet.
- `limits.max_concurrent_per_user` caps one user's concurrent requests across
  all of their keys.
- User keys carry dynamic names, so `budgets.clients` and
  `max_concurrent_per_client` cannot target them. Use the per-user defaults.
- A user sees only their own usage, analytics, keys and budget. Rows without
  an owner (collectors, ingest, legacy history) appear only in admin and
  service global views.

## 8. Interfaces

**Browser UI:** `/ui/`. Login is `GET /auth/login`, the callback is
`GET /auth/callback`, and logout is `POST /auth/logout`. A failed login shows
one fixed code:

| Code | Status | Meaning |
|---|---|---|
| `login_csrf` | 400 | No valid login cookie in this browser, its state does not match, or the server restarted. |
| `login_expired` | 400 | The 10-minute flow expired, or this state was already used. |
| `stale_auth` | 401 | `auth_time` missing or older than the login (§4). |
| `not_permitted`, `group_overage`, `claim_malformed`, `user_disabled`, `not_provisioned` | 403 | Access policy or local account refused the login. |
| `exchange_failed`, `token_invalid` | 502 | Token endpoint or ID token failed. |
| `busy`, `provider_unavailable`, `login_failed` | 503 | Exchange slots or replay cache full, IdP discovery failed, or a local store error. |

**Session JSON API** (`/ui/v1/*`):
- Responses are `no-store`.
- Bodies must be one JSON object with exact-case, non-repeated member names.
- Unsafe methods require all of the following, and fail with 403 otherwise:
  - `Origin` exactly equal to `public_base_url`;
  - `Sec-Fetch-Site` absent or `same-origin`;
  - `X-LocalRouter-CSRF` taken from `GET /ui/v1/me`.

| Who | Endpoints |
|---|---|
| any session | `GET /ui/v1/me`; `GET`/`POST /ui/v1/me/keys`; `DELETE /ui/v1/me/keys/{id}`; `GET /ui/v1/me/{usage,analytics,analytics/dimensions,budget}` |
| admin session | `GET /ui/v1/admin/users`; `POST /ui/v1/admin/users/{id}/{disable,enable,delete}`; `GET /ui/v1/admin/users/{id}/keys`; `DELETE /ui/v1/admin/users/{uid}/keys/{kid}`; `GET /ui/v1/admin/{audit,status,usage,analytics,analytics/dimensions,diagnostics,budgets}` |

**Bearer API** (`/control/v1/*`):

| Principal | status / diagnostics / budgets | usage / analytics / dimensions | admit | ingest |
|---|---|---|---|---|
| user key, `role: user` static key | 403 | own data only (`filter.user` rejected) | `{class, model}` only, see below | 403 |
| `role: service` | global | all users | full, as in single-user mode | only with `ingest: true` |

For a user key, admit returns only the decision. `account_id` is empty, and
`reason` is `admitted` or `no_admissible_account`. The budget estimate covers
only the caller's own client and user buckets, so a shared account ceiling
that is exhausted does not show there (the proxy still enforces it). The
`{class, account}` form returns 403 whether or not the account exists.

**Inference responses** (`/v1/*`, every principal, multi-user mode only):
- Relayed responses carry only `Content-Type` and a registered
  `Content-Encoding`. Upstream rate-limit, organization, request-ID and
  cookie headers are dropped.
- An upstream status of 400 or above is returned with the same status, a
  validated `Retry-After`, and
  `{"error":{"message":"localrouter: upstream error (HTTP N)","type":"upstream_error"}}`.
  The provider's error message, type and code are not passed on.
- Known gap: error events inside a streamed `200` response, and the bodies
  of 3xx responses, are relayed unchanged.

**CLI** (local break-glass, works directly on `identity.db` while the server
runs, prints no tokens or subjects):

```bash
localrouter users list    -config PATH [-after USER_ID] [-limit N]
localrouter users disable -config PATH USER_ID
localrouter users enable  -config PATH USER_ID
localrouter users keys    -config PATH USER_ID
localrouter users revoke  -config PATH USER_ID KEY_ID
localrouter users delete  -config PATH --yes USER_ID   # irreversible
```

**Recovery when no admin can log in.**
1. Use `users enable` if the admin was disabled.
2. Fix `admin_values`/`admin_subjects` or the IdP assignment, then restart.
3. Sign in again.

A deleted subject can never be re-provisioned, so delete only when that is
intended.

## 9. Data, backup, upgrades and provider changes

- `data_dir` holds `identity.db` (users, key, session and static-key digests,
  audit), `localrouter.db` (ledger), `budgets.db` and their lock files.
- **Upgrading is one-way.** The first start of this version migrates
  `localrouter.db` to schema 5 and `budgets.db` to schema 2, in either mode.
  Multi-user mode also creates `identity.db` at schema 2. Older binaries
  refuse to start on these files ("database schema version … is newer than
  supported"). To roll back, restore the pre-upgrade backup.
- **Owned static keys of users disabled before the upgrade.** Rotate them
  after the owner's next login. The new registry has no record of the older
  disable, so a key it has never seen is accepted if the owner is now active
  and logged in.
- **Back up all three databases together**, using the offline procedure in
  [Network deployment](NETWORK.md#backup-and-restore). Do not run
  `localrouter users` while the backup is being taken.
  - Restoring an older `identity.db` brings back users, keys and sessions that
    were live when it was taken. Repeat any later disable, delete or revoke
    after a restore.
  - Ledger and budget rows refer to user IDs, so keep the databases from the
    same backup.
- **The issuer and client ID are pinned in `identity.db`.** If either changes,
  the server and the CLI refuse to start ("bound to a different OIDC issuer or
  client"). Moving to another provider or client means a new `identity.db`.
  Every person then gets a new user ID: their old keys stop working, the
  `role: user` owners must be updated, and history stays under the old IDs.
  Rotating the client secret needs only a file update.
- **Deletion is not guaranteed erasure.**
  - The user is marked deleted.
  - Email and name are cleared.
  - Every key is revoked, and key names and digests are wiped.
  - Sessions and the owner's static-key digests are deleted.
  - The issuer and subject are replaced by a digest tombstone.

  Removing the old values from the SQLite file and WAL is best effort. While
  another reader (the CLI or a backup) holds an older snapshot, the cleanup is
  recorded as pending. Each later write, readiness probe (`/readyz`) or
  shutdown retries it, for at most about 250 ms per attempt. `users delete`
  does not warn while the cleanup is pending. Backups taken earlier still
  contain the profile.
- Do not reuse a deleted user's static key for another owner. The
  registry no longer holds its digest and would accept it.
- Do not move a revoked user key into a `role: service` client. Service keys
  are operator-controlled credentials outside the user revocation registry;
  reusing one that way grants service permissions. Generate a new service key.

## Verification status

Checked during this documentation review against commit `45f43a2`, with
scratch paths and generated secrets:

- [x] **The example config passes `localrouter check`**, with and without the
  `role: user` client. Eight broken variants are rejected, and no secret
  appears in the output. The variants are `k_`/`u_` names, `role: admin`, an
  `lrk_` static key, `offline_access`, HS256, `idle_ttl` > `absolute_ttl`, and
  `require_auth: false`.
- [x] **The built binary on loopback:**
  - Ready with no IdP contact.
  - `/` redirects to `/ui/`, and `/ui/v1/me` without a cookie returns 401.
  - `/auth/login` returns 503 while the IdP is unreachable.
  - A service key reads global status, and a bogus `lrk_` key gets 401.
  - The data directory ends up with identity schema 2, ledger 5 and budgets 2.
    The `89693a0` binary then refuses to start on it.
- [x] **Cross-builds** for `windows/amd64`, `darwin/amd64` and `darwin/arm64`
  compile. None of them was run.
- [x] **Fake signed IdP tests in `internal/weblogin` and `internal/app`**
  (reported by the implementing workers, not re-run here). They cover:
  - login through to inference;
  - the sealed login cookie and replay cache;
  - `prompt=login`;
  - stale `auth_time`;
  - policy denial revoking credentials;
  - static-key revocation;
  - admit redaction;
  - upstream error redaction.
- [ ] **Browser run on the current tree.** An earlier tree passed a 27-check
  headless Chrome run over HTTPS against a fake signed TLS IdP. That run is
  worker-reported and was not repeated after the final login, static-key,
  admit and proxy changes.
- [ ] **Real Entra ID tenant:** `prompt=login`/`max_age=0`, the optional
  `auth_time` claim, the `roles` claim, overage.
- [ ] **Real authentik:** `prompt=login` re-authentication, the signing-key
  requirement, the `groups` claim.
- [ ] **Browser acceptance through the NGINX edge** with a real certificate.
- [x] **Independent review** of the sealed login cookie, replay cache and
  durable static-key revocation found no remaining high/medium findings.
  The parent re-ran all 11 original authorization regressions under race
  detection and adopted seven additional login-audit probes, including the
  full-cache/in-flight regression. Those probes also pass under race detection.
- [ ] **Windows:** no runtime support claimed.

[e-oidc]: https://learn.microsoft.com/en-us/entra/identity-platform/v2-protocols-oidc
[e-idt]: https://learn.microsoft.com/en-us/entra/identity-platform/id-token-claims-reference
[e-opt]: https://learn.microsoft.com/en-us/entra/identity-platform/optional-claims-reference
[e-roles]: https://learn.microsoft.com/en-us/entra/identity-platform/howto-add-app-roles-in-apps
[a-doc]: https://docs.goauthentik.io/add-secure-apps/providers/oauth2/
