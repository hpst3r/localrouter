package proxy

import "github.com/hpst3r/localrouter/internal/core"

// Stable, sanitized per-attempt outcome classes. Exactly one is reported per
// ledger row, including a credential-unavailable attempt that never reached
// upstream. These strings are part of the observability contract and must not
// change without a schema-version bump.
const (
	outcomeSuccess         = "success"          // upstream answered with a non-error status
	outcomeUpstreamError   = "upstream_error"   // upstream answered 4xx/5xx (or a buffered retryable status)
	outcomeTransportError  = "transport_error"  // no usable upstream response (dial/TLS/mid-stream failure, credential unavailable)
	outcomeClientCancelled = "client_cancelled" // the downstream client went away
)

// eventRoutingAttemptCompleted is the value of the "event" attribute carried by
// every structured per-attempt completion record. It is a stable machine key:
// operators filter on it, so the value is intentionally verbose and fixed.
const eventRoutingAttemptCompleted = "routing_attempt_completed"

// attemptEvent is the privacy-safe structured payload of one upstream attempt
// completion.
//
// Every field is either a configured name (client, account, provider, class),
// a stable enum (outcome) or an existing ledger identifier (the attempt's
// record ID, its failover_of, and the first attempt's record ID). Deliberately
// absent: the client model string, route aliases and upstream model names
// (deferred to the chunk 2 integration), prompts/response bodies, headers,
// credentials, URLs and raw error text.
//
// failoverEligible deliberately reports eligibility, never occurrence:
// eligibility means the failover budget still permitted another account
// (canFailover) and this attempt was retryable. It does NOT assert that a
// different account was actually leased and tried next. An *actual* failover is
// evidenced only by the NEXT completed event, which carries failover_of (the
// predecessor record ID) together with a different `account`.
type attemptEvent struct {
	requestID string     // record ID of the first attempt for this client request
	attemptID string     // this attempt's record ID
	prevID    string     // this attempt's failover_of (predecessor record ID), "" for the first attempt
	client    string     // configured client name
	account   string     // configured account ID
	provider  string     // configured provider
	class     core.Class // interactive or background
	status    int        // observed upstream HTTP status (0 when none)
	outcome   string     // one of the outcome* constants
	authRetry bool       // this attempt observed 401/403 and triggered an in-place credential refresh
	// failoverEligible: canFailover (failover budget permission) + a retryable
	// attempt — NOT an actual next lease. See the type comment.
	failoverEligible bool
	latencyMS        int64 // finalized ledger latency (core.RequestRecord.LatencyMS)
}

// emitAttempt writes one structured completion record through the proxy's
// injected logger. It never allocates queues, globals or persisted state. The
// emitted key is failover_eligible (eligibility), keeping the distinction from
// an actual failover explicit; latency_ms is the same value the ledger row
// carries, derived once by record() (no second clock read).
func (p *Proxy) emitAttempt(ev attemptEvent) {
	attrs := []any{
		"event", eventRoutingAttemptCompleted,
		"request_id", ev.requestID,
		"attempt_id", ev.attemptID,
		"client", ev.client,
		"account", ev.account,
		"provider", ev.provider,
		"class", ev.class,
		"status", ev.status,
		"outcome", ev.outcome,
		"auth_retry", ev.authRetry,
		"failover_eligible", ev.failoverEligible,
		"latency_ms", ev.latencyMS,
	}
	if ev.prevID != "" {
		attrs = append(attrs, "failover_of", ev.prevID)
	}
	p.log.Info("attempt completed", attrs...)
}
