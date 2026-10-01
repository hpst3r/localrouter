// Package auth provides upstream credentials for LocalRouter accounts and
// authenticates downstream clients.
//
// Public API wired by cmd/localrouter:
//
//   - NewStore(dir) opens the Codex token store (<dir>/<account>.json,
//     dir 0700, files 0600, atomic writes).
//   - New(accounts, keys, store, opts) returns a *Manager implementing
//     core.CredentialSource. Codex accounts use OAuth tokens from the store
//     with single-flight refresh; other providers use a StaticKey (file or
//     env var).
//   - (*Manager).Login / LoginWithOptions run the Codex device-code login
//     for `localrouter login <account-id>`.
//   - LoadClientKeys(map[name]keyFile) returns *ClientKeys whose Lookup
//     maps a client bearer token to a client name in constant time.
//   - GenerateKey() makes a new random client key ("lr-" prefix).
//
// Secrets (access/refresh/id tokens, API keys, client keys) never appear in
// returned errors or log records.
package auth
