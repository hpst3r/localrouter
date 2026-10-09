package proxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
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
		res := p.attempt(w, r, req, lease, &prevID, failovers < p.opts.MaxFailovers)
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
// lease, and records one ledger row per upstream try.
func (p *Proxy) attempt(w http.ResponseWriter, r *http.Request, req *request, lease core.Lease, prevID *string, canFailover bool) attemptResult {
	ctx := r.Context()
	acctID := lease.AccountID()
	account, ok := p.deps.Accounts[acctID]
	if !ok {
		p.log.Error("policy returned unknown account", "account", acctID)
		lease.Release(core.Outcome{})
		return p.noResponse(w, canFailover, "account misconfigured")
	}
	// upCtx lets stream abort an idle upstream without touching the client.
	upCtx, cancelUp := context.WithCancel(ctx)
	defer cancelUp()
	authRetried := false
	for {
		rec := p.newRecord(req, account, *prevID)
		*prevID = rec.ID

		cred, err := p.deps.Creds.Credential(ctx, acctID)
		if err != nil {
			p.log.Warn("credential unavailable", "account", acctID, "err", err)
			lease.Release(core.Outcome{})
			rec.Error = "credential unavailable"
			p.record(ctx, rec)
			return p.noResponse(w, canFailover, "upstream credential unavailable")
		}
		rec.UpstreamIdentity = cred.Identity

		resp, err := p.send(upCtx, req, account, cred)
		if err != nil {
			if ctx.Err() != nil {
				lease.Release(core.Outcome{})
				rec.Error = "client disconnected"
				p.record(ctx, rec)
				return attemptResult{done: true}
			}
			msg := sanitizeErr(err)
			p.log.Warn("upstream transport error", "account", acctID, "model", req.model, "err", msg)
			lease.Release(core.Outcome{})
			rec.Error = "upstream transport error: " + msg
			p.record(ctx, rec)
			return p.noResponse(w, canFailover, "upstream unreachable")
		}
		p.deps.Quota.ObserveHeaders(acctID, resp.Header)
		status := resp.StatusCode

		scoped, final := p.requestScoped(account, status)
		if final {
			// The same request would fail the same way on any account, so a
			// replay elsewhere is only amplification: relay it as the answer.
			p.log.Info("upstream rejected request", "account", acctID, "class", req.class, "model", req.model, "status", status)
			p.stream(w, r, req, resp, lease, rec, cancelUp, true)
			return attemptResult{done: true}
		}

		if (status == http.StatusUnauthorized || status == http.StatusForbidden) && !authRetried && p.allowInvalidate(acctID) {
			drain(resp)
			authRetried = true
			p.deps.Creds.Invalidate(acctID)
			rec.Status = status
			rec.Error = "upstream " + strconv.Itoa(status) + "; retrying with refreshed credential"
			p.log.Info("upstream auth rejected; refreshing credential", "account", acctID, "status", status)
			p.record(ctx, rec)
			continue
		}

		if retryableFor(account.Provider, status) && canFailover {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, maxFailureBody))
			drain(resp)
			f := &failure{status: status, header: resp.Header.Clone(), body: body}
			out := core.Outcome{Status: status, RequestScoped: scoped}
			if wantsResetHint(account.Provider, status) {
				out.ResetAt = resetHint(resp.Header, p.clock.Now())
			}
			lease.Release(out)
			rec.Status = status
			rec.Error = "upstream " + strconv.Itoa(status) + "; failing over"
			p.log.Info("upstream failure; failing over", "account", acctID, "class", req.class, "model", req.model, "status", status)
			p.record(ctx, rec)
			return attemptResult{failure: f}
		}

		p.stream(w, r, req, resp, lease, rec, cancelUp, scoped)
		return attemptResult{done: true}
	}
}

// requestScopedFresh bounds how old a credits observation may be to count
// as proof that an OpenRouter account still has funds. It matches the
// policy's default StaleAfter.
const requestScopedFresh = 10 * time.Minute

// invalidateGap is the minimum interval between credential refreshes forced
// by upstream 401/403 answers on one account.
const invalidateGap = 60 * time.Second

// requestScoped classifies an upstream status. scoped means the failure was
// caused by this request, not the account, so policy must not cool the
// account down; final additionally means no other account would answer
// differently, so the response is relayed without credential refresh or
// failover. Only OpenRouter distinguishes these; other providers' 401/403/429
// stay account-level.
func (p *Proxy) requestScoped(account core.Account, status int) (scoped, final bool) {
	if account.Provider != core.ProviderOpenRouter {
		return false, false
	}
	switch status {
	case http.StatusForbidden:
		// Moderation-flagged input; auth failures are 401.
		return true, true
	case http.StatusPaymentRequired:
		// With funds known to be available, a 402 is the request being
		// unaffordable (e.g. max_tokens), not the account being empty.
		snap, ok := p.deps.Quota.Latest(account.ID)
		funded := ok && orFunded(snap, p.clock.Now())
		return funded, funded
	case http.StatusTooManyRequests:
		// A per-model or upstream-provider rate limit, unless the account
		// itself is known exhausted. Another key may not be limited.
		snap, ok := p.deps.Quota.Latest(account.ID)
		return !(ok && orExhausted(snap)), false
	}
	return false, false
}

// orFunded reports a fresh, positive known balance with no exhausted key cap.
func orFunded(s core.Snapshot, now time.Time) bool {
	c := s.Credits
	return c != nil && !c.FetchedAt.IsZero() && now.Sub(c.FetchedAt) <= requestScopedFresh &&
		c.BalanceUSD > 0 && !keyCapExhausted(s.Key)
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

// send builds and performs the upstream request.
func (p *Proxy) send(ctx context.Context, req *request, account core.Account, cred core.Credential) (*http.Response, error) {
	u := strings.TrimRight(account.BaseURL, "/") + req.endpoint
	body := req.body
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
// releases the lease and records the ledger row. If no upstream bytes arrive
// for StreamIdleTimeout, cancelUp aborts the upstream request. scoped is
// passed to policy as Outcome.RequestScoped.
func (p *Proxy) stream(w http.ResponseWriter, r *http.Request, req *request, resp *http.Response, lease core.Lease, rec core.RequestRecord, cancelUp context.CancelFunc, scoped bool) {
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
	if aborted || idledOut || readErr != nil {
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
	// after Result above has flushed any final buffered event.
	if rec.Provider == core.ProviderOpenRouter {
		rec.ReportedCostUSD = capture.ReportedCost()
	}
	switch {
	case aborted:
		rec.Error = "client disconnected"
	case idledOut:
		p.log.Warn("upstream stream idle timeout", "account", rec.AccountID, "model", req.model)
		rec.Error = "stream idle timeout"
	case readErr != nil:
		rec.Error = "upstream stream error: " + sanitizeErr(readErr)
	case resp.StatusCode >= 400:
		rec.Error = "upstream " + strconv.Itoa(resp.StatusCode)
	}
	p.record(ctx, rec)
}

// newRecord starts a ledger row for one upstream try.
func (p *Proxy) newRecord(req *request, account core.Account, failoverOf string) core.RequestRecord {
	return core.RequestRecord{
		ID:         newID(),
		StartedAt:  p.clock.Now(),
		Client:     req.client.Name,
		Class:      req.class,
		Route:      req.route.Name,
		Model:      req.model,
		Provider:   account.Provider,
		AccountID:  account.ID,
		FailoverOf: failoverOf,
		Session:    req.session,
		Task:       req.task,
		Agent:      req.agent,
		Host:       req.client.Host,
	}
}

// record finalizes timing, logs a summary, and writes the ledger row. Ledger
// failures are logged and never affect the response.
func (p *Proxy) record(ctx context.Context, rec core.RequestRecord) {
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
		return
	}
	lctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), ledgerTimeout)
	defer cancel()
	if err := p.deps.Ledger.Record(lctx, rec); err != nil {
		p.log.Error("ledger record failed", "id", rec.ID, "err", err)
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
// account-level OpenRouter 402 (out of credit) is account-specific, so
// another account may still serve; for other providers 402 stays a final
// answer. Request-scoped final answers are filtered out before this check.
func retryableFor(provider string, status int) bool {
	return retryable(status) || provider == core.ProviderOpenRouter && status == http.StatusPaymentRequired
}

// wantsResetHint reports whether the outcome should carry resetHint.
func wantsResetHint(provider string, status int) bool {
	return status == http.StatusTooManyRequests || provider == core.ProviderOpenRouter && status == http.StatusPaymentRequired
}

func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxFailureBody))
	_ = resp.Body.Close()
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
