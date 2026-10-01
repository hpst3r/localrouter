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
	InputTokens       int64
	CachedInputTokens int64 // cache reads
	// CacheCreationInputTokens are prompt tokens written to the cache
	// (Anthropic cache_creation_input_tokens); 0 for providers without it.
	CacheCreationInputTokens int64
	OutputTokens             int64
	ReasoningTokens          int64
}

// RequestRecord is one ledger row. Never contains prompt/response content.
type RequestRecord struct {
	ID               string
	StartedAt        time.Time
	FinishedAt       time.Time
	Client           string
	Class            Class
	Route            string
	Model            string
	Provider         string
	AccountID        string
	UpstreamIdentity string
	Status           int
	FailoverOf       string
	Usage            Usage
	UsageKnown       bool
	LatencyMS        int64
	BytesOut         int64
	Session          string
	Task             string
	Agent            string
	Error            string
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

// Clock allows tests to control time.
type Clock interface{ Now() time.Time }

type SystemClock struct{}

func (SystemClock) Now() time.Time { return time.Now() }
