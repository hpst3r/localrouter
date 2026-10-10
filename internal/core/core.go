// Package core holds the shared types and interfaces for LocalRouter.
// It is the frozen contract between packages; other internal packages import
// only core (never each other). See docs/SPEC.md.
package core

import (
	"context"
	"net/http"
	"time"
)

// Provider identifiers.
const (
	ProviderCodex        = "codex"
	ProviderOllama       = "ollama"
	ProviderOpenAICompat = "openai_compat"
	// ProviderClaude is a quota-only account backed by the official Claude
	// Code login. LocalRouter never proxies inference for it; it only reads
	// quota (read-only, never refreshing the CLI's token) and ingests token
	// usage from Claude Code's local transcripts.
	ProviderClaude = "claude"
	// ProviderOpenRouter is a prepaid OpenRouter API account. Inference is
	// forwarded like openai_compat; quota is a signed USD account balance
	// (GET /credits) and the key's own spend/cap (GET /key), not windows.
	ProviderOpenRouter = "openrouter"
)

// Workload classes.
type Class string

const (
	ClassInteractive Class = "interactive"
	ClassBackground  Class = "background"
)

// Quota window kinds.
const (
	Window5h     = "5h"
	WindowWeekly = "weekly"
)

// Window is one rolling quota window as reported by the provider.
type Window struct {
	Kind          string    `json:"kind"`
	UsedFrac      float64   `json:"used_frac"` // 0..1
	ResetAt       time.Time `json:"reset_at"`  // zero if unknown
	WindowSeconds int64     `json:"window_seconds"`
}

// Snapshot is the latest quota observation for one account.
type Snapshot struct {
	AccountID string    `json:"account_id"`
	FetchedAt time.Time `json:"fetched_at"`
	Source    string    `json:"source"` // "usage_api" | "headers"
	Plan      string    `json:"plan,omitempty"`
	Windows   []Window  `json:"windows"`
	// Allowed is the provider's own gate (Codex rate_limit.allowed); nil if unknown.
	Allowed *bool  `json:"allowed,omitempty"`
	Err     string `json:"error,omitempty"` // last fetch error; snapshot keeps last-good windows
	// ModelRequests are provider-reported per-model request counts per window
	// kind (e.g. Ollama's limits.<window>.models[]). They cover ALL traffic on
	// the account, including clients that bypass LocalRouter. nil if the
	// provider does not report them.
	ModelRequests map[string][]ModelCount `json:"model_requests,omitempty"`
	// Credits is the account-level prepaid balance (openrouter only); nil for
	// providers without one. Key is the API key's own spend and cap
	// (openrouter only). Each carries its own FetchedAt/Err so a partial
	// refresh never makes the other look fresh. For these accounts
	// FetchedAt is the oldest successful observation among the parts.
	Credits *Credits  `json:"credits,omitempty"`
	Key     *KeyUsage `json:"key,omitempty"`
}

// Credits is a provider-reported prepaid USD account balance (OpenRouter GET
// /credits). TotalCreditsUSD and TotalUsageUSD are lifetime totals; BalanceUSD
// = TotalCreditsUSD - TotalUsageUSD and is signed (negative = overdrawn). They
// are the provider's figures, independent of the router's ledger estimates.
// FetchedAt is zero until the first successful fetch: the values are then
// unknown (not zero). Err is the last fetch error; values stay last-good.
type Credits struct {
	TotalCreditsUSD float64   `json:"total_credits_usd"`
	TotalUsageUSD   float64   `json:"total_usage_usd"`
	BalanceUSD      float64   `json:"balance_usd"`
	FetchedAt       time.Time `json:"fetched_at"`
	Err             string    `json:"error,omitempty"`
}

// KeyUsage is the spend and spending cap of one API key (OpenRouter GET
// /key), in USD. A nil pointer means the provider reported null (LimitUSD /
// LimitRemainingUSD: no cap) or did not report the field; zero is a real
// zero. BYOK figures are kept separate from UsageUSD and never summed into
// it. LimitResetAt is the next cap reset derived from LimitReset ("daily",
// "weekly", "monthly"; zero if none or unknown). FetchedAt/Err as Credits.
type KeyUsage struct {
	LimitUSD            *float64  `json:"limit_usd"`
	LimitRemainingUSD   *float64  `json:"limit_remaining_usd"`
	LimitReset          string    `json:"limit_reset,omitempty"`
	LimitResetAt        time.Time `json:"limit_reset_at"`
	IncludeBYOKInLimit  *bool     `json:"include_byok_in_limit,omitempty"`
	UsageUSD            float64   `json:"usage_usd"`
	UsageDailyUSD       *float64  `json:"usage_daily_usd"`
	UsageWeeklyUSD      *float64  `json:"usage_weekly_usd"`
	UsageMonthlyUSD     *float64  `json:"usage_monthly_usd"`
	BYOKUsageUSD        *float64  `json:"byok_usage_usd"`
	BYOKUsageDailyUSD   *float64  `json:"byok_usage_daily_usd"`
	BYOKUsageWeeklyUSD  *float64  `json:"byok_usage_weekly_usd"`
	BYOKUsageMonthlyUSD *float64  `json:"byok_usage_monthly_usd"`
	IsFreeTier          *bool     `json:"is_free_tier,omitempty"`
	FetchedAt           time.Time `json:"fetched_at"`
	Err                 string    `json:"error,omitempty"`
}

// ModelCount is one provider-reported per-model request count.
type ModelCount struct {
	Model    string `json:"model"`
	Requests int64  `json:"requests"`
}

// Account is the static configuration of one upstream account.
type Account struct {
	ID       string
	Provider string
	BaseURL  string // e.g. https://chatgpt.com/backend-api/codex, https://ollama.com/v1
	// Reserve fraction per window kind protected from background work (0..1).
	Reserve map[string]float64
	// Price basis for ledger cost: "api_equivalent" or "metered".
	CostBasis string
	// QuotaSource for claude accounts: "local" (poll using the local Claude
	// Code credentials; default) or "agent" (never poll; snapshots arrive via
	// SnapshotIngester from agents on the hosts where Claude Code runs).
	QuotaSource string
}

// Credential is the material the proxy attaches upstream.
type Credential struct {
	Headers http.Header
	// Identity is the provider-side account identity actually used
	// (e.g. chatgpt_account_id). Recorded in the ledger. Never secret.
	Identity string
}

// CredentialSource provides upstream credentials per account.
type CredentialSource interface {
	// Credential returns a currently valid credential, refreshing if needed.
	Credential(ctx context.Context, accountID string) (Credential, error)
	// Invalidate forces the next Credential call to refresh (after a 401).
	Invalidate(accountID string)
}

// QuotaSource provides quota snapshots.
type QuotaSource interface {
	// Latest returns the most recent snapshot; ok=false if none ever fetched.
	Latest(accountID string) (Snapshot, bool)
	// ObserveHeaders lets the proxy feed upstream response headers.
	ObserveHeaders(accountID string, h http.Header)
	// RequestRefresh asks for a (debounced) refresh, e.g. after a request or 429.
	RequestRefresh(accountID string, urgent bool)
}

// Decision is an admission result.
type Decision struct {
	Allow     bool   `json:"allow"`
	AccountID string `json:"account_id,omitempty"`
	Reason    string `json:"reason"`
}

// Outcome is reported when a lease is released.
type Outcome struct {
	Status        int // upstream HTTP status (0 on transport error)
	UsageKnown    bool
	BytesToClient int64
	ResetAt       time.Time // from 429 handling if known
	// RequestScoped marks a failure caused by this request rather than by the
	// account (e.g. an unaffordable max_tokens 402 while the balance is known
	// positive, a moderation 403, or a 429 without account-exhaustion
	// evidence). The policy must not put the account into cooldown for it,
	// so one client cannot lock every other client out of an account.
	RequestScoped bool
}

// Input bounds shared by every ledger writer (proxy, ingest, local
// collectors) so no path accepts values another path would reject.
const (
	// MaxLabelBytes bounds free-form labels (session, task, agent, model,
	// route, class) stored in the ledger. Longer values are truncated on a
	// UTF-8 boundary by trusted local writers and rejected at ingest.
	MaxLabelBytes = 128
	// MaxErrorBytes bounds a stored error string.
	MaxErrorBytes = 256
	// MaxRecordTokens bounds each usage token field of one record.
	MaxRecordTokens int64 = 1_000_000_000_000
	// MaxReportedCostUSD bounds a provider-reported per-request cost.
	// Larger (or non-finite/negative) values are treated as unusable.
	MaxReportedCostUSD = 1e6
	// MaxPricePerMTokUSD bounds any configured or imported per-1M-token price.
	MaxPricePerMTokUSD = 1e6
	// AnalyticsMaxBreakdown caps AnalyticsResult.Breakdown.
	AnalyticsMaxBreakdown = 200
)

// Lease is an admitted in-flight request on one account.
type Lease interface {
	AccountID() string
	Release(o Outcome)
}

// Policy decides admission and account selection.
type Policy interface {
	// Acquire atomically selects the first admissible account from candidates
	// (in order, skipping exclude) and creates a lease.
	Acquire(class Class, candidates []string, exclude map[string]bool) (Lease, Decision)
	// DryRun evaluates without creating a lease.
	DryRun(class Class, candidates []string) Decision
	// Status describes each account's admission state for the control API.
	Status(accountID string) AccountState
}

// AccountState is a policy view of one account.
type AccountState struct {
	Inflight              int       `json:"inflight"`
	CooldownUntil         time.Time `json:"cooldown_until"`
	Stale                 bool      `json:"stale"`
	BackgroundAdmissible  bool      `json:"background_admissible"`
	InteractiveAdmissible bool      `json:"interactive_admissible"`
	Reason                string    `json:"reason,omitempty"`
}

// Usage is authoritative provider-reported token usage.
type Usage struct {
	// InputTokens is the TOTAL prompt tokens, including CachedInputTokens and
	// CacheCreationInputTokens (OpenAI convention). Producers whose provider
	// reports these separately (Anthropic) must sum them into InputTokens.
	InputTokens       int64 `json:"input_tokens"`
	CachedInputTokens int64 `json:"cached_input_tokens"` // cache reads
	// CacheCreationInputTokens are prompt tokens written to the cache
	// (Anthropic cache_creation_input_tokens); 0 for providers without it.
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	ReasoningTokens          int64 `json:"reasoning_tokens"`
}

// RequestRecord is one ledger row. Never contains prompt/response content.
// JSON tags define the ingest wire format (agents -> server).
type RequestRecord struct {
	ID         string    `json:"id"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
	Client     string    `json:"client"`
	Class      Class     `json:"class"`
	Route      string    `json:"route"`
	Model      string    `json:"model"`
	// UpstreamModel is the resolved backend model actually sent upstream for
	// this attempt (the candidate descriptor's upstream_model, else the
	// route's upstream_model, else the client model). It is additive metadata
	// for the ledger/pricing path; Model stays the client-facing id so pricing
	// and analytics keep working against the route's advertised name.
	UpstreamModel string `json:"upstream_model,omitempty"`
	// PricingModel is the ledger's cost-attribution key for this attempt. It is
	// populated ONLY for a capability-constrained route (one with per-candidate
	// Upstreams), and then carries the backend model actually resolved for the
	// attempt (the same value as UpstreamModel). Cost attribution uses
	// PricingModel when non-empty, else Model. For a legacy/unconstrained route
	// it stays empty and pricing keys on Model exactly as before — and it is
	// never backfilled from the client alias for an unpriced backend: on a
	// constrained route a backend with no price stays honestly unpriced (NULL
	// cost) rather than being costed at the client model's price.
	PricingModel     string `json:"pricing_model,omitempty"`
	Provider         string `json:"provider"`
	AccountID        string `json:"account_id"`
	UpstreamIdentity string `json:"upstream_identity,omitempty"`
	Status           int    `json:"status"`
	FailoverOf       string `json:"failover_of,omitempty"`
	Usage            Usage  `json:"usage"`
	UsageKnown       bool   `json:"usage_known"`
	LatencyMS        int64  `json:"latency_ms"`
	BytesOut         int64  `json:"bytes_out"`
	Session          string `json:"session,omitempty"`
	Task             string `json:"task,omitempty"`
	Agent            string `json:"agent,omitempty"`
	Error            string `json:"error,omitempty"`
	// Host is the machine the usage happened on ("" = the server itself for
	// proxied requests from clients without a configured host).
	Host string `json:"host,omitempty"`
	// ReportedCostUSD is the cost the upstream provider itself reported for
	// this request, in USD, when it returns one (e.g. OpenRouter's
	// usage.cost). It is an independent observation, not derived from tokens:
	// nil means the provider reported no cost, or one that was unusable
	// (null, non-numeric, negative, or non-finite). An explicit zero is a
	// valid, known cost. The proxy only populates this for
	// core.ProviderOpenRouter; cost fields from other providers are ignored.
	// The ledger persists it as cost_usd with cost_basis "provider_reported",
	// which takes precedence over the local pricing table and is never
	// overwritten by Reprice.
	ReportedCostUSD *float64 `json:"reported_cost_usd,omitempty"`
	// CostUSD/CostBasis are resolved by the ledger: valid OpenRouter reported
	// costs take precedence; otherwise known token usage uses local pricing.
}

// UsageRow is one aggregate row.
type UsageRow struct {
	Key                      string   `json:"key"`
	Requests                 int64    `json:"requests"`
	InputTokens              int64    `json:"input_tokens"`
	CachedInputTokens        int64    `json:"cached_input_tokens"`
	CacheCreationInputTokens int64    `json:"cache_creation_input_tokens"`
	OutputTokens             int64    `json:"output_tokens"`
	ReasoningTokens          int64    `json:"reasoning_tokens"`
	CostUSD                  *float64 `json:"cost_usd"` // nil if any priced component unknown and nothing priced
	UnknownUsageRequests     int64    `json:"unknown_usage_requests"`
	UnpricedRequests         int64    `json:"unpriced_requests"`
}

// BatchLedger is optionally implemented by ledgers that can record many rows
// in one round trip (e.g. the agent's remote ledger). Producers such as the
// Claude transcript collector use it when available and must only advance
// their persisted progress after RecordBatch returns nil. Like Record, it is
// idempotent on non-empty IDs.
type BatchLedger interface {
	RecordBatch(ctx context.Context, rs []RequestRecord) error
}

// SnapshotIngester accepts quota snapshots pushed from elsewhere (agents) for
// accounts whose quota_source is "agent". Implementations keep the snapshot
// with the newest FetchedAt and ignore older ones.
type SnapshotIngester interface {
	IngestSnapshot(s Snapshot) error
}

// IngestRequest is the POST /control/v1/ingest body sent by agents.
type IngestRequest struct {
	SchemaVersion int             `json:"schema_version"` // 1
	Host          string          `json:"host"`
	Records       []RequestRecord `json:"records,omitempty"`
	Snapshots     []Snapshot      `json:"snapshots,omitempty"`
}

// IngestResponse is the POST /control/v1/ingest reply.
type IngestResponse struct {
	SchemaVersion     int `json:"schema_version"`
	RecordsAccepted   int `json:"records_accepted"`
	SnapshotsAccepted int `json:"snapshots_accepted"`
	SnapshotsIgnored  int `json:"snapshots_ignored"` // older than what the server has
}

// AnalyticsDimensions are the ledger columns analytics can group/filter by.
// "task" is the project (Claude Code project dir slug or X-LocalRouter-Task);
// "agent" is main/subagent or X-LocalRouter-Agent.
var AnalyticsDimensions = []string{"host", "account", "model", "client", "class", "route", "task", "agent"}

// AnalyticsQuery selects ledger rows for time-series analytics.
type AnalyticsQuery struct {
	From, To time.Time
	// Bucket is "hour" or "day" (local calendar days, DST-correct).
	Bucket string
	// Group is one of AnalyticsDimensions.
	Group string
	// Filters are exact matches on AnalyticsDimensions; a present key with ""
	// matches rows whose column is empty.
	Filters map[string]string
	// TopN series are returned individually; the rest are summed into a
	// series with Key AnalyticsOtherKey. 0 = 8.
	TopN int
}

// AnalyticsOtherKey names the aggregate of groups outside the top N.
const AnalyticsOtherKey = "__other__"

// AnalyticsSeries is one group's time series aligned with BucketStarts.
type AnalyticsSeries struct {
	Key    string     `json:"key"`
	Total  UsageRow   `json:"total"`
	Points []UsageRow `json:"points"` // len == len(BucketStarts); Key empty
}

// AnalyticsResult answers an AnalyticsQuery. Ranking ("top") is by total
// tokens = InputTokens + OutputTokens, ties by key.
type AnalyticsResult struct {
	SchemaVersion int               `json:"schema_version"`
	From          time.Time         `json:"from"`
	To            time.Time         `json:"to"`
	Bucket        string            `json:"bucket"`
	BucketStarts  []time.Time       `json:"bucket_starts"`
	Group         string            `json:"group"`
	Filters       map[string]string `json:"filters"`
	Totals        UsageRow          `json:"totals"`
	// Breakdown has the highest-ranked group keys in range, at most
	// AnalyticsMaxBreakdown of them; BreakdownOmitted counts the rest (their
	// usage is still included in Totals and the "other" series).
	Breakdown        []UsageRow `json:"breakdown"`
	BreakdownOmitted int        `json:"breakdown_omitted"`
	// Series has the top N keys in rank order, then AnalyticsOtherKey if any.
	Series []AnalyticsSeries `json:"series"`
}

// AnalyticsLedger is implemented by ledgers that support time-series analytics.
type AnalyticsLedger interface {
	Analytics(ctx context.Context, q AnalyticsQuery) (AnalyticsResult, error)
}

// Ledger persists request records and answers summaries.
type Ledger interface {
	// Record inserts r. It is idempotent on a non-empty r.ID: re-recording an
	// existing ID is a no-op returning nil (used by transcript ingestion).
	Record(ctx context.Context, r RequestRecord) error
	Summary(ctx context.Context, since time.Time, group string) ([]UsageRow, error)
	Close() error
}

// Client is an authenticated downstream caller.
type Client struct {
	Name  string
	Class Class
	// Host attributes this client's proxied requests to a machine (optional).
	Host string
	// Ingest permits POST /control/v1/ingest with this client's key.
	Ingest bool
}

// UpstreamSpec describes one candidate account's upstream binding within an
// opted-in route (core.Route.Upstreams). Every field is optional and the zero
// value reproduces today's behaviour: empty UpstreamModel inherits
// Route.UpstreamModel (which, when also empty, forwards the client model
// unchanged), nil Protocols is provider-derived, nil InputModalities is
// text-only, and the false booleans mean the optional feature is unsupported.
// The descriptor is a declaration, not a security boundary.
type UpstreamSpec struct {
	// UpstreamModel is the backend model sent upstream for this candidate;
	// "" inherits Route.UpstreamModel.
	UpstreamModel string
	// Protocols is the non-empty subset of {"chat", "responses"} this candidate
	// may serve when the route opts into capability routing.
	Protocols []string
	// InputModalities is the non-empty subset of {"text", "image"} this
	// candidate accepts. Declaring only "image" (a vision-only model) does not
	// imply "text". nil means text-only.
	InputModalities []string
	// Tools reports whether the candidate supports a non-empty top-level
	// "tools" array. false = unsupported.
	Tools bool
	// JSONSchema reports whether the candidate supports structured output
	// (json_schema response format). false = unsupported.
	JSONSchema bool
	// Stream reports whether the candidate supports streaming. false =
	// unsupported.
	Stream bool
}

// Route maps a model name to ordered candidate accounts per class, the
// upstream model name to send, and (optionally) per-candidate capability
// descriptors.
type Route struct {
	Name          string
	Models        []string
	UpstreamModel string // empty = forward client model unchanged
	Interactive   []string
	Background    []string
	// Upstreams, when non-empty, opts the route into capability routing: it
	// MUST carry one descriptor for every candidate in interactive ∪
	// background, and unknown keys are rejected. A nil/empty map means the
	// route is unconstrained (legacy permissive behaviour).
	Upstreams map[string]UpstreamSpec
}

// ClientInflight is one client's concurrency usage. Limit 0 means unlimited.
type ClientInflight struct {
	Name   string `json:"name"`
	Limit  int    `json:"limit"`
	Active int    `json:"active"`
}

// InflightStats is a snapshot of inference concurrency, reported by the
// authenticated diagnostics endpoint. GlobalLimit 0 means unlimited.
type InflightStats struct {
	GlobalLimit  int              `json:"global_limit"`
	GlobalActive int              `json:"global_active"`
	GlobalPeak   int              `json:"global_peak"`
	Clients      []ClientInflight `json:"clients"`
}

// ReloadStatus is the sanitized result of one configuration reload attempt,
// surfaced by the authenticated diagnostics endpoint and CLI logs. It carries
// no secrets: Reason is a fixed cause class and RestartOnly names config keys,
// never values, key material or filesystem secret paths. On a failed attempt
// Generation reports the still-serving generation (it does not advance), so a
// reader can never observe a generation that is not the one serving requests.
type ReloadStatus struct {
	// Generation counts successful publishes: 1 after startup, +1 per
	// accepted reload. It never lags the published handler.
	Generation uint64 `json:"generation"`
	// OK is true when the last attempt published a new generation.
	OK bool `json:"ok"`
	// At is when the last attempt was made (UTC in the diagnostics document).
	At time.Time `json:"at"`
	// Reason is a sanitized failure cause ("restart required", "busy",
	// "config parse", …); empty on success. Never a raw error or secret path.
	Reason string `json:"reason,omitempty"`
	// RestartOnly lists the config keys that forced a rejection because they
	// are applied only on restart (names, never values).
	RestartOnly []string `json:"restart_only,omitempty"`
}

// Clock allows tests to control time.
type Clock interface{ Now() time.Time }

type SystemClock struct{}

func (SystemClock) Now() time.Time { return time.Now() }
