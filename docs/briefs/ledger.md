Package: internal/ledger  (also internal/ledger/pricing.go in same package)

Implement core.Ledger on SQLite (modernc.org/sqlite, driver name "sqlite").

- `Open(path string, pricing *Pricing, basis func(accountID string) string) (*Ledger, error)`
  creates dir 0700, opens with WAL + busy_timeout(5000) + foreign_keys, creates schema
  via a tiny migration table (`schema_version`). Single table `requests` with exactly
  SPEC columns (id TEXT PK; times as INTEGER unix millis; cost_usd REAL NULL;
  cost_basis TEXT NULL). Indexes on started_at, account_id. File mode 0600.
- Record computes cost: if pricing has the model AND UsageKnown -> cost per SPEC
  formula, cost_basis = basis(accountID); else cost_usd NULL, cost_basis NULL.
  Insert must be safe for concurrent callers (serialize writes with a mutex or
  SetMaxOpenConns(1) for the writer).
- Summary(since, group) where group in account|model|class|client|route|day
  (day = local date YYYY-MM-DD); return core.UsageRow with SUMs; CostUSD = SUM of
  non-null costs (nil if zero priced rows); UnknownUsageRequests = count where
  usage_known=0; UnpricedRequests = count where cost_usd IS NULL and usage_known=1.
  Reject unknown group with an error. Order by key.
- Pricing: `LoadPricing(path string) (*Pricing, error)` YAML
  `models: {<name>: {input: <usd/1M>, cached_input: <usd/1M>, output: <usd/1M>}}`.
  Missing file => empty pricing, no error. cached_input missing => use input.
  `(*Pricing).Cost(model string, u core.Usage) (float64, bool)`.
  Lookup exact model name, then a lowercase match; no fuzzy guessing.
- `ImportLiteLLM(r io.Reader) (map[string]ModelPrice, error)` parses LiteLLM's
  model_prices_and_context_window.json (per-token fields input_cost_per_token,
  output_cost_per_token, cache_read_input_token_cost) -> per-1M values; skip entries
  lacking input/output; strip a leading "provider/" when the key contains one ONLY as
  an additional alias (keep original key too). `WritePricing(path, map)` writes YAML
  atomically (temp+rename, 0600), sorted keys.
- Ship NO price numbers in code or files.
- Tests: schema has no content columns (acceptance 10 — assert column list exactly),
  cost math incl. cached split and unknown price -> NULL, summary grouping and counts,
  concurrent Record with -race (50 goroutines), LiteLLM import from a small fixture,
  pricing roundtrip.
