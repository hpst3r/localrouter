Packages: internal/control, internal/ledger, internal/proxy, internal/app (hostguard.go + app.go wiring ONLY for the items below), cmd/localrouter/main.go (serve TLS only).
Read docs/SPEC.md section 'Multi-host (central server + per-host agents)' FIRST; it is authoritative. core and config are frozen (already contain the new fields/types: RequestRecord.Host + JSON tags, BatchLedger, SnapshotIngester, IngestRequest/Response, Client.Host/Ingest, Account.QuotaSource; config allowed_hosts/tls_*/clients[].host/ingest/accounts[].quota_source).

Implement:
1. ledger: migration v3 host column; Record stores Host; Summary group=host; RecordBatch (one tx, max 1000, idempotent). Ledger must satisfy core.BatchLedger (add a compile-time assertion).
2. proxy: set RequestRecord.Host = client.Host for proxied requests (core.Client now has Host). Find where Client is built from authenticate.
3. control: POST /control/v1/ingest exactly per SPEC (auth -> 401/403 rules, validation, overwrite Host/Client/Provider, whole-request 400 on any invalid record/snapshot, size limit, future-skew check, uses Ledger RecordBatch when available else Record loop, SnapshotIngester dep). Add Deps.Ingester core.SnapshotIngester and Deps.ClaudeAccounts or derive from Deps.Accounts (Provider==claude, QuotaSource==agent for snapshots). usage groups add "host". Authenticate returns core.Client which now carries Ingest.
   Widget: add "By host (24h)" table (group=host; key "" displayed as "(server)").
4. app: hostguard accepts loopback + cfg.AllowedHosts when AllowNonLoopback (guard is now ALWAYS on); authenticate closure populates Host/Ingest from cfg.Clients; wire control Deps.Ingester = the quota manager IF it implements core.SnapshotIngester (use a type assertion so you compile before the quota worker's code lands: `if ing, ok := any(qm).(core.SnapshotIngester); ok {...}`).
5. main.go serve: if cfg.TLSCertFile != "" use srv.ListenAndServeTLS(cert,key) (keep everything else).
Tests: MH2, MH3, MH4, MH5, MH8 + ledger migration from v2 DB, RecordBatch dedupe, group=host. Use fakes for SnapshotIngester.
Do NOT edit internal/quota, internal/claudelog, internal/agent.