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
}

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
	ID               string    `json:"id"`
	StartedAt        time.Time `json:"started_at"`
	FinishedAt       time.Time `json:"finished_at"`
	Client           string    `json:"client"`
	Class            Class     `json:"class"`
	Route            string    `json:"route"`
	Model            string    `json:"model"`
	Provider         string    `json:"provider"`
	AccountID        string    `json:"account_id"`
	UpstreamIdentity string    `json:"upstream_identity,omitempty"`
	Status           int       `json:"status"`
	FailoverOf       string    `json:"failover_of,omitempty"`
	Usage            Usage     `json:"usage"`
	UsageKnown       bool      `json:"usage_known"`
	LatencyMS        int64     `json:"latency_ms"`
	BytesOut         int64     `json:"bytes_out"`
	Session          string    `json:"session,omitempty"`
	Task             string    `json:"task,omitempty"`
	Agent            string    `json:"agent,omitempty"`
	Error            string    `json:"error,omitempty"`
	// Host is the machine the usage happened on ("" = the server itself for
	// proxied requests from clients without a configured host).
	Host string `json:"host,omitempty"`
	// CostUSD/CostBasis are computed by the ledger from pricing; callers leave zero.
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
	// Breakdown has every group key in range (not truncated), ranked.
	Breakdown []UsageRow `json:"breakdown"`
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

// Route maps a model name to ordered candidate accounts per class, and the
// upstream model name to send.
type Route struct {
	Name          string
	Models        []string
	UpstreamModel string // empty = forward client model unchanged
	Interactive   []string
	Background    []string
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

// Clock allows tests to control time.
type Clock interface{ Now() time.Time }

type SystemClock struct{}

func (SystemClock) Now() time.Time { return time.Now() }
