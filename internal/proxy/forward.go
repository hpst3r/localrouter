package proxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hpst3r/localrouter/internal/budget"
	"github.com/hpst3r/localrouter/internal/core"
	"github.com/hpst3r/localrouter/internal/routing"
)

const (
	maxFailureBody  = 64 << 10 // buffered upstream error body kept for relay
	ledgerTimeout   = 5 * time.Second
	denyRetryAfter  = "60"
	streamChunkSize = 32 << 10
)

// forwardedHeaders are the only client request headers sent upstream.
var forwardedHeaders = []string{"Content-Type", "Accept", "OpenAI-Beta", "session_id", "conversation_id", "x-request-id"}

// hopHeaders are stripped from upstream responses.
var hopHeaders = []string{
	"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
	"Proxy-Connection", "Te", "Trailer", "Transfer-Encoding", "Upgrade",
	"Content-Length", "Set-Cookie",
}

// failure is a buffered retryable upstream response, relayed to the client if
// no further account is admissible.
type failure struct {
	status int
	header http.Header
	body   []byte
}

// attemptResult tells forward whether to stop or try another account.
type attemptResult struct {
	done    bool
	failure *failure // set when a buffered upstream response is available
	errMsg  string   // set when the attempt failed without a response
}

// forward runs the admission/attempt/failover loop for one request.
func (p *Proxy) forward(w http.ResponseWriter, r *http.Request, req *request) {
	exclude := map[string]bool{}
	var (
		prevID    string
		rootID    string // record ID of the first attempt; the request correlation ID
		pending   *failure
		lastErr   string
		failovers int
		cur       *onceLease
	)
	// Release the current lease if a panic unwinds past it; the panic keeps
	// propagating so net/http still logs it.
	defer func() {
		if cur != nil {
			cur.Release(core.Outcome{})
		}
	}()
	for {
		l, dec := p.deps.Policy.Acquire(req.class, req.candidates, exclude)
		var lease core.Lease
		if l != nil {
			cur = &onceLease{Lease: l}
			lease = cur
		}
		if lease == nil || !dec.Allow {
			if lease != nil {
				lease.Release(core.Outcome{})
			}
			switch {
			case pending != nil:
				relayFailure(w, pending)
			case lastErr != "":
				writeError(w, http.StatusBadGateway, "localrouter: "+lastErr, "upstream_error")
			default:
				p.log.Info("policy denied", "client", req.client.Name, "class", req.class, "model", req.model, "reason", dec.Reason)
				w.Header().Set("Retry-After", denyRetryAfter)
				writeError(w, http.StatusTooManyRequests, "localrouter: no admissible account: "+dec.Reason, "quota_reserve")
			}
			return
		}
		acctID := lease.AccountID()
		res := p.attempt(w, r, req, lease, &prevID, &rootID, failovers < p.opts.MaxFailovers)
		if res.done {
			return
		}
		exclude[acctID] = true
		failovers++
		if res.failure != nil {
			pending, lastErr = res.failure, ""
		} else if pending == nil {
			lastErr = res.errMsg
		}
	}
}

// onceLease makes Release idempotent so a deferred safety release never
// double-counts.
type onceLease struct {
	core.Lease
	once sync.Once
}

func (l *onceLease) Release(o core.Outcome) {
	l.once.Do(func() { l.Lease.Release(o) })
}

// attempt sends the request to the leased account, retrying the same account
// once after a 401/403 with a refreshed credential. It always releases the
// lease, records one ledger row per upstream try, and emits one structured
// completion event per try.
func (p *Proxy) attempt(w http.ResponseWriter, r *http.Request, req *request, lease core.Lease, prevID, rootID *string, canFailover bool) attemptResult {
	ctx := r.Context()
	acctID := lease.AccountID()
	account, ok := p.deps.Accounts[acctID]
	if !ok {
		p.log.Error("policy returned unknown account", "account", acctID)
		lease.Release(core.Outcome{})
		return p.noResponse(w, canFailover, "account misconfigured")
	}
	authRetried := false
	for {
		// upCtx lets an idle read abort this upstream try without touching
		// the client or a later auth retry.
		upCtx, cancelUp := context.WithCancel(ctx)
		defer cancelUp()
		rec := p.newRecord(req, account, *prevID)
		*prevID = rec.ID
		if *rootID == "" {
			*rootID = rec.ID
		}
		ev := attemptEvent{
			requestID: *rootID,
			attemptID: rec.ID,
			prevID:    rec.FailoverOf,
			client:    req.client.Name,
			account:   account.ID,
			provider:  account.Provider,
			class:     req.class,
		}

		cred, err := p.deps.Creds.Credential(ctx, acctID)
		if err != nil {
			p.log.Warn("credential unavailable", "account", acctID, "err", err)
			lease.Release(core.Outcome{})
			rec.Error = "credential unavailable"
			rec = p.record(ctx, rec)
			ev.outcome = outcomeTransportError
			ev.failoverEligible = canFailover
			ev.latencyMS = rec.LatencyMS
			p.emitAttempt(ev)
			return p.noResponse(w, canFailover, "upstream credential unavailable")
		}
		rec.UpstreamIdentity = cred.Identity

		// Reserve budget AFTER the credential is resolved — so a credential
		// failure never places a hold — and immediately before the send, on
		// every attempt including this loop's credential-refresh retry. A
		// reservation error is terminal: a denied client must not fail over
		// onto another account's budget, and a store error must not be papered
		// over by spending. Neither is settled, because neither was reserved.
		if p.deps.Budget != nil {
			if err := p.deps.Budget.Reserve(ctx, rec); err != nil {
				lease.Release(core.Outcome{})
				if errors.Is(err, budget.ErrExceeded) {
					p.log.Info("budget exceeded", "client", rec.Client, "class", req.class,
						"account", acctID, "model", req.model)
					rec.Error = "budget exceeded"
					p.record(ctx, rec)
					writeError(w, http.StatusTooManyRequests, "localrouter: budget exceeded", "budget_exceeded")
					return attemptResult{done: true}
				}
				// A client that went away mid-Reserve is a disconnect, not a
				// store outage: nothing was reserved and nobody reads a reply.
				if ctx.Err() != nil {
					rec.Error = "client disconnected"
					p.record(ctx, rec)
					return attemptResult{done: true}
				}
				// Log a sanitized class only: the store's own error text can
				// embed filesystem paths or DSNs.
				p.log.Error("budget reserve failed", "client", rec.Client, "account", acctID,
					"class", budgetErrClass(err))
				rec.Error = "budget store error"
				p.record(ctx, rec)
				writeError(w, http.StatusServiceUnavailable, "localrouter: budget store unavailable", "budget_store_error")
				return attemptResult{done: true}
			}
		}

		resp, err := p.send(upCtx, req, account, cred)
		if err != nil {
			if ctx.Err() != nil {
				lease.Release(core.Outcome{})
				rec.Error = "client disconnected"
				rec = p.recordBudget(ctx, rec)
				ev.outcome = outcomeClientCancelled
				ev.latencyMS = rec.LatencyMS
				p.emitAttempt(ev)
				return attemptResult{done: true}
			}
			msg := sanitizeErr(err)
			p.log.Warn("upstream transport error", "account", acctID, "model", req.model, "err", msg)
			lease.Release(core.Outcome{})
			rec.Error = "upstream transport error: " + msg
			rec = p.recordBudget(ctx, rec)
			ev.outcome = outcomeTransportError
			ev.failoverEligible = canFailover
			ev.latencyMS = rec.LatencyMS
			p.emitAttempt(ev)
			return p.noResponse(w, canFailover, "upstream unreachable")
		}
		p.deps.Quota.ObserveHeaders(acctID, resp.Header)
		status := resp.StatusCode

		scoped, final, idledOut := p.classify(account, status, resp, cancelUp)
		if idledOut {
			// The error body stalled: keep what was read, and fail over or
			// relay it.
			body, _ := p.readFailureBody(resp, cancelUp)
			f := &failure{status: status, header: resp.Header.Clone(), body: body}
			out := core.Outcome{Status: status, RequestScoped: scoped}
			if wantsResetHint(account.Provider, status) {
				out.ResetAt = resetHint(resp.Header, p.clock.Now())
			}
			lease.Release(out)
			rec.Status = status
			rec.Error = errIdleErrorBody
			p.log.Warn("upstream idle timeout reading error body", "account", acctID, "model", req.model, "status", status)
			rec = p.recordBudget(ctx, rec)
			eligible := canFailover && retryableFor(account.Provider, status)
			ev.status = status
			ev.outcome = outcomeUpstreamError
			ev.failoverEligible = eligible
			ev.latencyMS = rec.LatencyMS
			p.emitAttempt(ev)
			if eligible {
				return attemptResult{failure: f}
			}
			relayFailure(w, f)
			return attemptResult{done: true}
		}
		if final {
			// The same request would fail the same way on any account, so a
			// replay elsewhere is only amplification: relay it as the answer.
			p.log.Info("upstream rejected request", "account", acctID, "class", req.class, "model", req.model, "status", status)
			p.stream(w, r, req, resp, lease, rec, cancelUp, true, ev)
			return attemptResult{done: true}
		}

		if (status == http.StatusUnauthorized || status == http.StatusForbidden) && !authRetried && p.allowInvalidate(acctID) {
			_, _ = p.readFailureBody(resp, cancelUp)
			cancelUp()
			authRetried = true
			p.deps.Creds.Invalidate(acctID)
			rec.Status = status
			rec.Error = "upstream " + strconv.Itoa(status) + "; retrying with refreshed credential"
			p.log.Info("upstream auth rejected; refreshing credential", "account", acctID, "status", status)
			rec = p.recordBudget(ctx, rec)
			ev.status = status
			ev.outcome = outcomeUpstreamError
			ev.authRetry = true
			ev.latencyMS = rec.LatencyMS
			p.emitAttempt(ev)
			continue
		}

		if retryableFor(account.Provider, status) && canFailover {
			body, timedOut := p.readFailureBody(resp, cancelUp)
			f := &failure{status: status, header: resp.Header.Clone(), body: body}
			out := core.Outcome{Status: status, RequestScoped: scoped}
			if wantsResetHint(account.Provider, status) {
				out.ResetAt = resetHint(resp.Header, p.clock.Now())
			}
			lease.Release(out)
			rec.Status = status
			rec.Error = "upstream " + strconv.Itoa(status) + "; failing over"
			if timedOut {
				rec.Error = errIdleErrorBody
			}
			p.log.Info("upstream failure; failing over", "account", acctID, "class", req.class, "model", req.model, "status", status)
			rec = p.recordBudget(ctx, rec)
			ev.status = status
			ev.outcome = outcomeUpstreamError
			ev.failoverEligible = true
			ev.latencyMS = rec.LatencyMS
			p.emitAttempt(ev)
			return attemptResult{failure: f}
		}

		p.stream(w, r, req, resp, lease, rec, cancelUp, scoped, ev)
		return attemptResult{done: true}
	}
}

// invalidateGap is the minimum interval between credential refreshes forced
// by upstream 401/403 answers on one account.
const invalidateGap = 60 * time.Second

// errIdleErrorBody is the ledger error for an upstream error body that
// stalled for StreamIdleTimeout before the response was relayed.
const errIdleErrorBody = "upstream idle timeout reading error body"

// classify decides how an upstream status counts against the account.
// scoped means the failure was caused by this request, not the account, so
// policy must not cool the account down; final additionally means no other
// account would answer differently, so the response is relayed without
// credential refresh or failover. idledOut reports that the body read
// needed for classification stalled and the upstream was canceled. Only
// OpenRouter distinguishes these; other providers' 401/403/429 stay
// account-level.
func (p *Proxy) classify(account core.Account, status int, resp *http.Response, cancelUp context.CancelFunc) (scoped, final, idledOut bool) {
	if account.Provider != core.ProviderOpenRouter {
		return false, false, false
	}
	switch status {
	case http.StatusForbidden:
		// Moderation-flagged input; auth failures are 401.
		return true, true, false
	case http.StatusPaymentRequired:
		// The affordability preflight depends on this request (max_tokens,
		// prompt size, model price), so it does not cool the account down;
		// another account may have more credit, so it is not final either.
		// Any other 402 ("Insufficient credits", unknown or unreadable body)
		// is the account being out of credit. A positive balance snapshot
		// proves nothing about current funds; a known exhausted one wins.
		if snap, ok := p.deps.Quota.Latest(account.ID); ok && orExhausted(snap) {
			return false, false, false
		}
		body, timedOut := p.peekBody(resp, cancelUp)
		if timedOut {
			return false, false, true
		}
		return isAffordability(body), false, false
	case http.StatusTooManyRequests:
		// A per-model or upstream-provider rate limit, unless the account
		// itself is known exhausted. Another key may not be limited.
		snap, ok := p.deps.Quota.Latest(account.ID)
		return !(ok && orExhausted(snap)), false, false
	}
	return false, false, false
}

// affordRE matches OpenRouter's affordability preflight 402 ("This request
// requires more credits, or fewer max_tokens. You requested up to N tokens,
// but can only afford M."). M may be 0 for a large enough prompt.
var affordRE = regexp.MustCompile(`requires more credits, or fewer max_tokens|can only afford [0-9]+`)

func isAffordability(body []byte) bool { return affordRE.Match(body) }

// copyIdle copies up to limit bytes from src to dst. If no bytes arrive for
// idle (0 disables), it calls cancelUp to abort the upstream and reports
// timedOut, with the same semantics as stream's idle timer.
func copyIdle(dst io.Writer, src io.Reader, limit int64, idle time.Duration, cancelUp context.CancelFunc) (timedOut bool) {
	var fired atomic.Bool
	var timer *time.Timer
	if idle > 0 {
		timer = time.AfterFunc(idle, func() { fired.Store(true); cancelUp() })
		defer timer.Stop()
	}
	buf := make([]byte, 4<<10)
	for limit > 0 {
		if timer != nil {
			timer.Reset(idle)
		}
		n, err := src.Read(buf[:min(int64(len(buf)), limit)])
		if timer != nil {
			timer.Stop()
		}
		_, _ = dst.Write(buf[:n])
		limit -= int64(n)
		if err != nil {
			return fired.Load()
		}
	}
	return false
}

// peekBody reads up to maxFailureBody bytes of resp.Body, idle-bounded, and
// puts them back in front of the remainder so the response can still be
// streamed or buffered unchanged.
func (p *Proxy) peekBody(resp *http.Response, cancelUp context.CancelFunc) ([]byte, bool) {
	var b bytes.Buffer
	timedOut := copyIdle(&b, resp.Body, maxFailureBody, p.opts.StreamIdleTimeout, cancelUp)
	resp.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(b.Bytes()), resp.Body), resp.Body}
	return b.Bytes(), timedOut
}

// readFailureBody buffers up to maxFailureBody bytes of an upstream error
// body, discards up to as much again so the connection can be reused, and
// closes it. Reads are idle-bounded like stream; on timeout the upstream is
// canceled and the bytes read so far are returned.
func (p *Proxy) readFailureBody(resp *http.Response, cancelUp context.CancelFunc) ([]byte, bool) {
	defer resp.Body.Close()
	idle := p.opts.StreamIdleTimeout
	var b bytes.Buffer
	if copyIdle(&b, resp.Body, maxFailureBody, idle, cancelUp) {
		return b.Bytes(), true
	}
	return b.Bytes(), copyIdle(io.Discard, resp.Body, maxFailureBody, idle, cancelUp)
}

// orExhausted reports a known non-positive balance or an exhausted key cap.
func orExhausted(s core.Snapshot) bool {
	c := s.Credits
	return c != nil && !c.FetchedAt.IsZero() && c.BalanceUSD <= 0 || keyCapExhausted(s.Key)
}

// keyCapExhausted mirrors the policy's rule: a known cap with no spend left.
func keyCapExhausted(k *core.KeyUsage) bool {
	return k != nil && !k.FetchedAt.IsZero() && k.LimitUSD != nil && k.LimitRemainingUSD != nil &&
		*k.LimitRemainingUSD <= 0
}

// allowInvalidate reports whether an upstream 401/403 on accountID may force
// a credential refresh now, and records it if so. At most one refresh per
// account per invalidateGap, so a client cannot drive refresh traffic; a
// later auth failure inside the gap is handled as an account-level failure.
func (p *Proxy) allowInvalidate(accountID string) bool {
	now := p.clock.Now()
	p.invMu.Lock()
	defer p.invMu.Unlock()
	if last, ok := p.lastInvalidate[accountID]; ok && now.Sub(last) < invalidateGap {
		return false
	}
	p.lastInvalidate[accountID] = now
	return true
}

// noResponse either signals failover or, if none is allowed, answers 502.
func (p *Proxy) noResponse(w http.ResponseWriter, canFailover bool, msg string) attemptResult {
	if canFailover {
		return attemptResult{errMsg: msg}
	}
	writeError(w, http.StatusBadGateway, "localrouter: "+msg, "upstream_error")
	return attemptResult{done: true}
}

// send builds and performs the upstream request for one attempt. The upstream
// model is resolved per attempt from the leased account, so failing over to a
// different backend sends that backend's alias; and the body is rebuilt from the
// unmodified client body on every attempt, so no model rewrite, include_usage
// injection or Codex normalisation can leak between attempts.
func (p *Proxy) send(ctx context.Context, req *request, account core.Account, cred core.Credential) (*http.Response, error) {
	u := strings.TrimRight(account.BaseURL, "/") + req.endpoint
	// Only an actual override rewrites the body: a resolved model equal to the
	// client model is semantically the same request, so it is left byte-identical
	// (the legacy no-rewrite fast path). The rewrite target is the leased
	// candidate's alias, else the route-wide alias, else the client model.
	override := ""
	if model := routing.ResolveModel(req.route, account.ID, req.model); model != req.model {
		override = model
	}
	body, err := rewriteBody(req.body, override, req.endpoint == "/chat/completions" && req.stream)
	if err != nil {
		return nil, err
	}
	if account.Provider == core.ProviderCodex {
		body = codexBody(body)
	}
	up, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for _, h := range forwardedHeaders {
		if v, ok := req.header[http.CanonicalHeaderKey(h)]; ok {
			up.Header[http.CanonicalHeaderKey(h)] = append([]string(nil), v...)
		} else if v, ok := req.header[h]; ok {
			up.Header[h] = append([]string(nil), v...)
		}
	}
	if up.Header.Get("Content-Type") == "" {
		up.Header.Set("Content-Type", "application/json")
	}
	for k, v := range cred.Headers {
		up.Header[k] = append([]string(nil), v...)
	}
	return p.opts.HTTPClient.Do(up)
}

// stream relays the upstream response to the client, capturing usage, then
// releases the lease, records the ledger row and emits the completion event.
// If no upstream bytes arrive for StreamIdleTimeout, cancelUp aborts the
// upstream request. scoped is passed to policy as Outcome.RequestScoped.
func (p *Proxy) stream(w http.ResponseWriter, r *http.Request, req *request, resp *http.Response, lease core.Lease, rec core.RequestRecord, cancelUp context.CancelFunc, scoped bool, ev attemptEvent) {
	defer resp.Body.Close()
	ctx := r.Context()
	rc := http.NewResponseController(w)

	// The idle timer only runs while waiting on an upstream Read, so a slow
	// client write does not count as upstream idleness.
	var idle atomic.Bool
	var timer *time.Timer
	if d := p.opts.StreamIdleTimeout; d > 0 {
		timer = time.AfterFunc(d, func() { idle.Store(true); cancelUp() })
		timer.Stop()
		defer timer.Stop()
	}

	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	_ = rc.Flush()

	capture := newUsageCapture(resp.Header.Get("Content-Type"))
	buf := make([]byte, streamChunkSize)
	var (
		written  int64
		aborted  bool
		idledOut bool
		readErr  error
	)
	for {
		if timer != nil {
			timer.Reset(p.opts.StreamIdleTimeout)
		}
		n, err := resp.Body.Read(buf)
		if timer != nil {
			timer.Stop()
		}
		if n > 0 {
			_, _ = capture.Write(buf[:n])
			wn, werr := w.Write(buf[:n])
			written += int64(wn)
			if werr != nil {
				aborted = true
				break
			}
			_ = rc.Flush()
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			switch {
			case ctx.Err() != nil:
				aborted = true
			case idle.Load():
				idledOut = true
			default:
				readErr = err
			}
			break
		}
	}

	usage, known := capture.Result()
	incomplete := aborted || idledOut || readErr != nil
	if incomplete {
		known = false
	}
	out := core.Outcome{Status: resp.StatusCode, UsageKnown: known, BytesToClient: written, RequestScoped: scoped}
	if wantsResetHint(rec.Provider, resp.StatusCode) {
		out.ResetAt = resetHint(resp.Header, p.clock.Now())
	}
	lease.Release(out)

	rec.Status = resp.StatusCode
	rec.BytesOut = written
	rec.UsageKnown = known
	if known {
		rec.Usage = usage
	}
	// Provider-reported cost is an independent observation and is only trusted
	// from OpenRouter; cost fields from other providers are ignored. Read it
	// after Result above has flushed any final buffered event. On an incomplete
	// stream the ledger still keeps it, but it may come from an intermediate
	// usage record, so the budget settles it only as a lower bound (below).
	if rec.Provider == core.ProviderOpenRouter {
		rec.ReportedCostUSD = capture.ReportedCost()
	}
	switch {
	case aborted:
		rec.Error = "client disconnected"
		ev.outcome = outcomeClientCancelled
	case idledOut:
		p.log.Warn("upstream stream idle timeout", "account", rec.AccountID, "model", req.model)
		rec.Error = "stream idle timeout"
		ev.outcome = outcomeTransportError
	case readErr != nil:
		rec.Error = "upstream stream error: " + sanitizeErr(readErr)
		ev.outcome = outcomeTransportError
	case resp.StatusCode >= 400:
		rec.Error = "upstream " + strconv.Itoa(resp.StatusCode)
		ev.outcome = outcomeUpstreamError
	default:
		ev.outcome = outcomeSuccess
	}
	ev.status = resp.StatusCode
	if incomplete {
		rec = p.recordIncompleteBudget(ctx, rec)
	} else {
		rec = p.recordBudget(ctx, rec)
	}
	ev.latencyMS = rec.LatencyMS
	p.emitAttempt(ev)
}

// newRecord starts a ledger row for one upstream try.
func (p *Proxy) newRecord(req *request, account core.Account, failoverOf string) core.RequestRecord {
	resolved := routing.ResolveModel(req.route, account.ID, req.model)
	rec := core.RequestRecord{
		ID:        newID(),
		StartedAt: p.clock.Now(),
		Client:    req.client.Name,
		Class:     req.class,
		Route:     req.route.Name,
		Model:     req.model,
		// UpstreamModel is what this attempt actually sent upstream: the
		// candidate descriptor's alias, the route-level upstream model, or the
		// client model when no rewrite applies.
		UpstreamModel: resolved,
		Provider:      account.Provider,
		AccountID:     account.ID,
		FailoverOf:    failoverOf,
		Session:       req.session,
		Task:          req.task,
		Agent:         req.agent,
		Host:          req.client.Host,
	}
	// A capability-constrained route (per-candidate Upstreams) attributes cost
	// to the backend this attempt actually resolved, so two candidates serving
	// the same client model are priced at their own backend. A legacy route
	// keeps the exact legacy pricing path: PricingModel stays empty and the
	// ledger keys on Model. This never falls back from an unpriced backend to
	// the client alias — an unpriced backend stays honestly unpriced.
	if len(req.route.Upstreams) > 0 {
		rec.PricingModel = resolved
	}
	return rec
}

// record finalizes timing, logs a summary, and writes the ledger row. Ledger
// failures are logged and never affect the response.
func (p *Proxy) record(ctx context.Context, rec core.RequestRecord) core.RequestRecord {
	// Errors can carry upstream/transport text: bound and sanitize them.
	rec.Error = core.TruncateError(rec.Error)
	rec.FinishedAt = p.clock.Now()
	rec.LatencyMS = rec.FinishedAt.Sub(rec.StartedAt).Milliseconds()
	p.log.Info("request",
		"id", rec.ID, "client", rec.Client, "class", rec.Class, "model", rec.Model,
		"account", rec.AccountID, "status", rec.Status, "latency_ms", rec.LatencyMS,
		"usage_known", rec.UsageKnown, "input_tokens", rec.Usage.InputTokens,
		"cached_input_tokens", rec.Usage.CachedInputTokens, "output_tokens", rec.Usage.OutputTokens,
		"reasoning_tokens", rec.Usage.ReasoningTokens, "bytes_out", rec.BytesOut,
		"failover_of", rec.FailoverOf, "error", rec.Error)
	if p.deps.Ledger == nil {
		return rec
	}
	lctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), ledgerTimeout)
	defer cancel()
	if err := p.deps.Ledger.Record(lctx, rec); err != nil {
		p.log.Error("ledger record failed", "id", rec.ID, "err", err)
	}
	return rec
}

// recordBudget settles the attempt's budget reservation — when one was taken —
// on a fresh context, then writes the ledger row. Reserve and Settle are
// paired here for every path that follows a successful Reserve, including the
// post-401 credential-refresh retry, each failover account, a transport
// failure or client cancellation, and the stream relay.
//
// Budget is optional: with no Budget installed no reservation was ever placed,
// so recordBudget is exactly record and the legacy path is unchanged.
//
// The settlement context is detached from the request (a cancelled client must
// never lose its hold) with the same bounded budget the ledger write uses.
// Settling an attempt the store never reserved returns ErrUnknownReservation;
// that is a wiring fault and is logged rather than silently absorbed. Any
// other failure leaves the durable hold in place for the reconciler and is
// logged with a sanitized class only — the store's own error is not surfaced.
//
// Every attempt that reached Settle without a usable cost — an auth retry, a
// failover, a transport error, an upstream error response — is charged at its
// full hold under the unknown basis. That is deliberate conservative policy:
// the proxy cannot prove an attempt that reached the upstream was not billed.
//
// It returns the finalized row record wrote, so the caller's completion event
// carries the same latency the ledger does.
func (p *Proxy) recordBudget(ctx context.Context, rec core.RequestRecord) core.RequestRecord {
	if p.deps.Budget != nil {
		p.settleBudget(ctx, rec, p.deps.Budget.Settle)
	}
	return p.record(ctx, rec)
}

// recordIncompleteBudget is recordBudget for a response relay that ended early
// (client disconnect, idle timeout, upstream read error). The ledger row keeps
// any provider-reported cost seen so far, but that cost may come from an
// intermediate usage record, so the budget settles it with SettleIncomplete:
// as unknown, charged at max(hold, observed), never as a final cost.
func (p *Proxy) recordIncompleteBudget(ctx context.Context, rec core.RequestRecord) core.RequestRecord {
	if p.deps.Budget != nil {
		p.settleBudget(ctx, rec, p.deps.Budget.SettleIncomplete)
	}
	return p.record(ctx, rec)
}

func (p *Proxy) settleBudget(ctx context.Context, rec core.RequestRecord, settle func(context.Context, core.RequestRecord) error) {
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), ledgerTimeout)
	err := settle(sctx, rec)
	cancel()
	switch {
	case err == nil:
	case errors.Is(err, budget.ErrUnknownReservation):
		p.log.Error("budget settlement found no reservation", "id", rec.ID)
	default:
		p.log.Error("budget settlement failed; reservation left held for reconciliation",
			"id", rec.ID, "class", budgetErrClass(err))
	}
}

// budgetErrClass maps a budget error to a fixed, sanitized class for logs, so
// the store's own error text (paths, DSNs, SQLite messages) never reaches them.
func budgetErrClass(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, budget.ErrClosed):
		return "closed"
	case errors.Is(err, budget.ErrNoStore):
		return "no_store"
	case errors.Is(err, budget.ErrInvalid):
		return "invalid"
	case errors.Is(err, budget.ErrConflict):
		return "conflict"
	default:
		return "store"
	}
}

func relayFailure(w http.ResponseWriter, f *failure) {
	copyHeaders(w.Header(), f.header)
	w.WriteHeader(f.status)
	_, _ = w.Write(f.body)
}

func copyHeaders(dst, src http.Header) {
	skip := map[string]bool{}
	for _, h := range hopHeaders {
		skip[http.CanonicalHeaderKey(h)] = true
	}
	for _, v := range src.Values("Connection") {
		for _, f := range strings.Split(v, ",") {
			if f = strings.TrimSpace(f); f != "" {
				skip[http.CanonicalHeaderKey(f)] = true
			}
		}
	}
	for k, v := range src {
		if !skip[http.CanonicalHeaderKey(k)] {
			dst[k] = append([]string(nil), v...)
		}
	}
}

func retryable(status int) bool {
	return status == http.StatusTooManyRequests || status == http.StatusUnauthorized ||
		status == http.StatusForbidden || status >= 500
}

// retryableFor adds provider-specific failover statuses to retryable. An
// OpenRouter 402 (out of credit, or this request unaffordable on this key)
// is account-specific, so another account may still serve; for other
// providers 402 stays a final answer. Final answers (classify) are filtered
// out before this check.
func retryableFor(provider string, status int) bool {
	return retryable(status) || provider == core.ProviderOpenRouter && status == http.StatusPaymentRequired
}

// wantsResetHint reports whether the outcome should carry resetHint.
func wantsResetHint(provider string, status int) bool {
	return status == http.StatusTooManyRequests || provider == core.ProviderOpenRouter && status == http.StatusPaymentRequired
}

// resetHint derives when an exhausted account may be retried from
// Retry-After and the x-codex-* window headers (windows at >= 100% used).
// It returns the latest hint, or zero if none is present.
func resetHint(h http.Header, now time.Time) time.Time {
	var best time.Time
	consider := func(t time.Time) {
		if t.After(best) {
			best = t
		}
	}
	if ra := strings.TrimSpace(h.Get("Retry-After")); ra != "" {
		if secs, err := strconv.ParseFloat(ra, 64); err == nil && secs >= 0 {
			consider(now.Add(time.Duration(secs * float64(time.Second))))
		} else if t, err := http.ParseTime(ra); err == nil {
			consider(t)
		}
	}
	for _, w := range []string{"primary", "secondary"} {
		used, err := strconv.ParseFloat(strings.TrimSpace(h.Get("x-codex-"+w+"-used-percent")), 64)
		if err != nil || used < 100 {
			continue
		}
		if secs, err := strconv.ParseFloat(strings.TrimSpace(h.Get("x-codex-"+w+"-reset-after-seconds")), 64); err == nil && secs >= 0 {
			consider(now.Add(time.Duration(secs * float64(time.Second))))
		}
	}
	return best
}

// sanitizeErr strips the request URL from transport errors.
func sanitizeErr(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	s := err.Error()
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}
