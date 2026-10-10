package control

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
	"github.com/hpst3r/localrouter/internal/routing"
)

//go:embed static/index.html
var indexHTML []byte

// SchemaVersion is the version of the status/usage JSON documents.
const SchemaVersion = 1

const (
	defaultStaleAfter = 10 * time.Minute
	defaultSince      = 24 * time.Hour
	maxSince          = 400 * 24 * time.Hour
	maxAdmitBody      = 64 << 10
)

// widgetCSP forbids all external loads; the page only talks to its own origin.
const widgetCSP = "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'; img-src data:; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

// Deps are the runtime collaborators of the control server.
type Deps struct {
	// Accounts in configured order; status lists them in this order.
	Accounts []core.Account
	Quota    core.QuotaSource
	Policy   core.Policy
	Ledger   core.Ledger
	Routes   []core.Route
	// Clients are the registered clients in config order (status only).
	Clients []ClientInfo
	// Authenticate validates a client bearer key. Required when RequireAuth
	// and always for POST /control/v1/ingest (which also needs Client.Ingest).
	Authenticate func(bearer string) (core.Client, bool)
	// Ingester stores agent-pushed quota snapshots; nil rejects snapshots
	// with 503. Records go to Ledger (via core.BatchLedger when implemented).
	Ingester core.SnapshotIngester
	// IsSnapshotStale reports whether an Ingester error means "not newer than
	// the stored snapshot" (counted as ignored, not a failure). Nil falls back
	// to matching "stale" in the error text.
	IsSnapshotStale func(error) bool
	// Storage probes local storage health for /readyz and diagnostics. Nil
	// means storage health is not observed (reported as unconfigured).
	Storage StoragePinger
	// Inflight reports inference concurrency for diagnostics. Nil reports
	// zero capacity.
	Inflight InflightReporter
	// ReloadStatus reports the sanitized last configuration-reload attempt for
	// the diagnostics document. Nil omits the reload block entirely, so a
	// server without a reload seam never claims a reload state it cannot
	// observe. It is consulted per request (a live view), not sampled once.
	ReloadStatus func() core.ReloadStatus
	// Ready reports whether this instance is serving. Nil defaults to ready.
	// It folds in process shutdown so /readyz and diagnostics flip not-ready
	// once the process stops accepting work, independently of storage health.
	Ready func() bool
	// Clock defaults to core.SystemClock.
	Clock core.Clock
}

// Options tune the control server.
type Options struct {
	// RequireAuth makes /control/v1/* require a valid client bearer key.
	RequireAuth bool
	// StaleAfter marks snapshots older than this as stale (default 10m).
	StaleAfter time.Duration
}

// Server implements the control API and widget.
type Server struct {
	deps Deps
	opts Options
}

// New builds a control server. Nil Clock defaults to the system clock and a
// zero StaleAfter defaults to 10 minutes.
func New(deps Deps, opts Options) *Server {
	if deps.Clock == nil {
		deps.Clock = core.SystemClock{}
	}
	if opts.StaleAfter <= 0 {
		opts.StaleAfter = defaultStaleAfter
	}
	return &Server{deps: deps, opts: opts}
}

// Handler returns the HTTP handler for all control routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("GET /readyz", s.readyz)
	mux.HandleFunc("GET /{$}", s.widget)
	mux.Handle("GET /control/v1/status", s.auth(http.HandlerFunc(s.status)))
	mux.Handle("GET /control/v1/usage", s.auth(http.HandlerFunc(s.usage)))
	mux.Handle("GET /control/v1/analytics", s.auth(http.HandlerFunc(s.analytics)))
	mux.Handle("GET /control/v1/analytics/dimensions", s.auth(http.HandlerFunc(s.analyticsDimensions)))
	mux.Handle("GET /control/v1/diagnostics", s.auth(http.HandlerFunc(s.diagnostics)))
	mux.Handle("POST /control/v1/admit", s.auth(http.HandlerFunc(s.admit)))
	mux.HandleFunc("POST /control/v1/ingest", s.ingest)
	return mux
}

func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte("ok"))
}

func (s *Server) widget(w http.ResponseWriter, _ *http.Request) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Security-Policy", widgetCSP)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Cache-Control", "no-store")
	_, _ = w.Write(indexHTML)
}

// auth enforces a client bearer key on the wrapped handler when RequireAuth.
func (s *Server) auth(next http.Handler) http.Handler {
	if !s.opts.RequireAuth {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := bearer(r.Header.Get("Authorization"))
		if !ok || s.deps.Authenticate == nil {
			writeError(w, http.StatusUnauthorized, "client key required")
			return
		}
		if _, ok := s.deps.Authenticate(token); !ok {
			writeError(w, http.StatusUnauthorized, "invalid client key")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func bearer(h string) (string, bool) {
	const prefix = "bearer "
	if len(h) < len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return "", false
	}
	t := strings.TrimSpace(h[len(prefix):])
	return t, t != ""
}

// statusDoc is the GET /control/v1/status response.
type statusDoc struct {
	SchemaVersion int             `json:"schema_version"`
	Now           time.Time       `json:"now"`
	Accounts      []accountStatus `json:"accounts"`
	// Clients lists every registered client (never key material) so the
	// dashboard can show per-client analytics including unused clients.
	Clients []ClientInfo `json:"clients"`
}

// ClientInfo describes a registered client for the status document.
type ClientInfo struct {
	Name   string `json:"name"`
	Class  string `json:"class"`
	Host   string `json:"host,omitempty"`
	Ingest bool   `json:"ingest"`
}

type accountStatus struct {
	ID                    string             `json:"id"`
	Provider              string             `json:"provider"`
	Healthy               bool               `json:"healthy"`
	CooldownUntil         *time.Time         `json:"cooldown_until"`
	Inflight              int                `json:"inflight"`
	Reserve               map[string]float64 `json:"reserve"`
	Windows               []windowStatus     `json:"windows"`
	BackgroundAdmissible  bool               `json:"background_admissible"`
	InteractiveAdmissible bool               `json:"interactive_admissible"`
	SnapshotAgeS          *int64             `json:"snapshot_age_s"`
	Stale                 bool               `json:"stale"`
	Error                 *string            `json:"error"`
	// Reason explains why a class is not admissible (policy wording; no secrets).
	Reason string `json:"reason,omitempty"`
	// ModelRequests are provider-reported per-model request counts per window
	// kind; they include traffic that bypassed LocalRouter.
	ModelRequests map[string][]core.ModelCount `json:"model_requests,omitempty"`
	// Credits and Key are present only for openrouter accounts: the signed
	// USD account balance and the API key's own spend/cap, each with its own
	// freshness. Provider figures, independent of ledger cost estimates.
	Credits *creditsStatus `json:"credits,omitempty"`
	Key     *keyStatus     `json:"key,omitempty"`
}

// creditsStatus is the account-level prepaid balance. Amounts are null while
// unavailable (never fetched, or no permission); BalanceUSD is signed.
type creditsStatus struct {
	Available       bool     `json:"available"`
	BalanceUSD      *float64 `json:"balance_usd"`
	TotalCreditsUSD *float64 `json:"total_credits_usd"`
	TotalUsageUSD   *float64 `json:"total_usage_usd"`
	// Exhausted is true when the known balance is at or below zero.
	Exhausted bool    `json:"exhausted"`
	AgeS      *int64  `json:"age_s"`
	Stale     bool    `json:"stale"`
	Error     *string `json:"error"`
}

// keyStatus is the API key's spend and cap. Unlimited is true when the
// provider reports no cap (limit null); a zero cap is a real cap.
type keyStatus struct {
	Available           bool       `json:"available"`
	Unlimited           bool       `json:"unlimited"`
	LimitUSD            *float64   `json:"limit_usd"`
	LimitRemainingUSD   *float64   `json:"limit_remaining_usd"`
	LimitReset          string     `json:"limit_reset,omitempty"`
	LimitResetAt        *time.Time `json:"limit_reset_at"`
	Exhausted           bool       `json:"exhausted"`
	UsageUSD            *float64   `json:"usage_usd"`
	UsageDailyUSD       *float64   `json:"usage_daily_usd"`
	UsageWeeklyUSD      *float64   `json:"usage_weekly_usd"`
	UsageMonthlyUSD     *float64   `json:"usage_monthly_usd"`
	BYOKUsageUSD        *float64   `json:"byok_usage_usd"`
	BYOKUsageDailyUSD   *float64   `json:"byok_usage_daily_usd"`
	BYOKUsageWeeklyUSD  *float64   `json:"byok_usage_weekly_usd"`
	BYOKUsageMonthlyUSD *float64   `json:"byok_usage_monthly_usd"`
	IncludeBYOKInLimit  *bool      `json:"include_byok_in_limit"`
	IsFreeTier          *bool      `json:"is_free_tier"`
	AgeS                *int64     `json:"age_s"`
	Stale               bool       `json:"stale"`
	Error               *string    `json:"error"`
}

type windowStatus struct {
	Kind          string     `json:"kind"`
	UsedFrac      float64    `json:"used_frac"`
	RemainingFrac float64    `json:"remaining_frac"`
	ResetAt       *time.Time `json:"reset_at"`
	WindowSeconds int64      `json:"window_seconds"`
	// Rolled is true when ResetAt has passed; UsedFrac is then reported as 0.
	Rolled bool `json:"rolled"`
}

func (s *Server) status(w http.ResponseWriter, _ *http.Request) {
	now := s.deps.Clock.Now()
	doc := statusDoc{SchemaVersion: SchemaVersion, Now: now.UTC(), Accounts: make([]accountStatus, 0, len(s.deps.Accounts)),
		Clients: append([]ClientInfo{}, s.deps.Clients...)}
	for _, a := range s.deps.Accounts {
		doc.Accounts = append(doc.Accounts, s.accountStatus(a, now))
	}
	writeJSON(w, http.StatusOK, doc)
}

func (s *Server) accountStatus(a core.Account, now time.Time) accountStatus {
	st := accountStatus{
		ID:       a.ID,
		Provider: a.Provider,
		Reserve:  make(map[string]float64, len(a.Reserve)),
		Windows:  []windowStatus{},
	}
	for k, v := range a.Reserve {
		st.Reserve[k] = v
	}

	var ps core.AccountState
	if s.deps.Policy != nil {
		ps = s.deps.Policy.Status(a.ID)
	}
	st.Inflight = ps.Inflight
	st.BackgroundAdmissible = ps.BackgroundAdmissible
	st.InteractiveAdmissible = ps.InteractiveAdmissible
	st.Reason = ps.Reason
	st.Stale = ps.Stale
	coolingDown := !ps.CooldownUntil.IsZero() && now.Before(ps.CooldownUntil)
	if coolingDown {
		t := ps.CooldownUntil.UTC()
		st.CooldownUntil = &t
	}

	var snap core.Snapshot
	var have bool
	if s.deps.Quota != nil {
		snap, have = s.deps.Quota.Latest(a.ID)
	}
	if !have {
		st.Stale = true
	} else {
		if snap.FetchedAt.IsZero() {
			// Never successfully fetched (first fetch failed): no age.
			st.Stale = true
		} else {
			age := int64(max(now.Sub(snap.FetchedAt), 0) / time.Second)
			st.SnapshotAgeS = &age
			if now.Sub(snap.FetchedAt) > s.opts.StaleAfter {
				st.Stale = true
			}
		}
		if snap.Err != "" {
			e := snap.Err
			st.Error = &e
		}
		for _, win := range snap.Windows {
			st.Windows = append(st.Windows, windowView(win, now))
		}
		st.ModelRequests = snap.ModelRequests
	}
	if a.Provider == core.ProviderOpenRouter {
		st.Credits, st.Key = s.creditsView(snap.Credits, now), s.keyView(snap.Key, now)
	}
	st.Healthy = !coolingDown && st.Error == nil
	return st
}

// partAge returns the age and staleness of a part fetched at t (nil, true if
// never fetched).
func (s *Server) partAge(t, now time.Time) (*int64, bool) {
	if t.IsZero() {
		return nil, true
	}
	age := int64(max(now.Sub(t), 0) / time.Second)
	return &age, now.Sub(t) > s.opts.StaleAfter
}

func errPtr(e string) *string {
	if e == "" {
		return nil
	}
	return &e
}

func (s *Server) creditsView(c *core.Credits, now time.Time) *creditsStatus {
	if c == nil {
		c = &core.Credits{}
	}
	v := &creditsStatus{Error: errPtr(c.Err)}
	v.AgeS, v.Stale = s.partAge(c.FetchedAt, now)
	if !c.FetchedAt.IsZero() {
		bal, tc, tu := c.BalanceUSD, c.TotalCreditsUSD, c.TotalUsageUSD
		v.Available, v.BalanceUSD, v.TotalCreditsUSD, v.TotalUsageUSD = true, &bal, &tc, &tu
		v.Exhausted = bal <= 0
	}
	return v
}

func (s *Server) keyView(k *core.KeyUsage, now time.Time) *keyStatus {
	if k == nil {
		k = &core.KeyUsage{}
	}
	v := &keyStatus{Error: errPtr(k.Err)}
	v.AgeS, v.Stale = s.partAge(k.FetchedAt, now)
	if k.FetchedAt.IsZero() {
		return v
	}
	// The snapshot comes from Latest (a private deep copy), so its pointers
	// can be shared with the response.
	usage := k.UsageUSD
	v.Available, v.UsageUSD = true, &usage
	v.Unlimited = k.LimitUSD == nil
	v.LimitUSD, v.LimitRemainingUSD, v.LimitReset = k.LimitUSD, k.LimitRemainingUSD, k.LimitReset
	v.UsageDailyUSD, v.UsageWeeklyUSD, v.UsageMonthlyUSD = k.UsageDailyUSD, k.UsageWeeklyUSD, k.UsageMonthlyUSD
	v.BYOKUsageUSD, v.BYOKUsageDailyUSD = k.BYOKUsageUSD, k.BYOKUsageDailyUSD
	v.BYOKUsageWeeklyUSD, v.BYOKUsageMonthlyUSD = k.BYOKUsageWeeklyUSD, k.BYOKUsageMonthlyUSD
	v.IncludeBYOKInLimit, v.IsFreeTier = k.IncludeBYOKInLimit, k.IsFreeTier
	if !k.LimitResetAt.IsZero() {
		t := k.LimitResetAt.UTC()
		v.LimitResetAt = &t
	}
	// LimitResetAt is informational only: reaching the predicted reset does not
	// restore spending credit, so a zero-remaining cap stays exhausted until a
	// fresh /key observation reports positive remaining. A finite cap with a
	// null remaining is a malformed pair the parser rejects, not exhaustion.
	v.Exhausted = k.LimitUSD != nil && k.LimitRemainingUSD != nil && *k.LimitRemainingUSD <= 0
	return v
}

func windowView(win core.Window, now time.Time) windowStatus {
	v := windowStatus{Kind: win.Kind, WindowSeconds: win.WindowSeconds}
	used := clamp01(win.UsedFrac)
	if !win.ResetAt.IsZero() {
		t := win.ResetAt.UTC()
		v.ResetAt = &t
		if !now.Before(win.ResetAt) {
			v.Rolled = true
			used = 0
		}
	}
	v.UsedFrac = used
	v.RemainingFrac = clamp01(1 - used)
	return v
}

func clamp01(f float64) float64 {
	if math.IsNaN(f) || f < 0 {
		return 0
	}
	return min(f, 1)
}

// usageDoc is the GET /control/v1/usage response.
type usageDoc struct {
	SchemaVersion int             `json:"schema_version"`
	Since         time.Time       `json:"since"`
	Group         string          `json:"group"`
	Rows          []core.UsageRow `json:"rows"`
}

var usageGroups = []string{"account", "model", "class", "client", "host", "route", "task", "agent"}

func (s *Server) usage(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	since := defaultSince
	if v := q.Get("since"); v != "" {
		d, err := parseSince(v)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		since = d
	}
	group := q.Get("group")
	if group == "" {
		group = "account"
	}
	if !slices.Contains(usageGroups, group) {
		writeError(w, http.StatusBadRequest, "group must be one of "+strings.Join(usageGroups, ", "))
		return
	}
	if s.deps.Ledger == nil {
		writeError(w, http.StatusServiceUnavailable, "ledger unavailable")
		return
	}
	from := s.deps.Clock.Now().Add(-since)
	rows, err := s.deps.Ledger.Summary(r.Context(), from, group)
	if err != nil {
		slog.Warn("control: usage summary failed", "group", group, "err", err)
		writeError(w, http.StatusInternalServerError, "usage summary failed")
		return
	}
	if rows == nil {
		rows = []core.UsageRow{}
	}
	writeJSON(w, http.StatusOK, usageDoc{SchemaVersion: SchemaVersion, Since: from.UTC(), Group: group, Rows: rows})
}

// parseSince accepts a Go duration ("24h", "168h") or whole days ("7d").
// The result must be positive and at most 400 days.
func parseSince(v string) (time.Duration, error) {
	var d time.Duration
	if n, ok := strings.CutSuffix(v, "d"); ok {
		days, err := strconv.Atoi(n)
		if err != nil || days <= 0 || days > 400 {
			return 0, errors.New("since: days must be an integer between 1 and 400")
		}
		d = time.Duration(days) * 24 * time.Hour
	} else {
		var err error
		d, err = time.ParseDuration(v)
		if err != nil {
			return 0, errors.New("since: expected a duration like 24h or 7d")
		}
	}
	if d <= 0 || d > maxSince {
		return 0, errors.New("since: must be positive and at most 400d")
	}
	return d, nil
}

// admitRequest names exactly one of Model (dry-run the route's candidates) or
// Account (dry-run that single account, e.g. a quota-only claude account).
//
// Requirements is optional. A route that has opted into capability routing
// (core.Route.Upstreams non-empty) requires it: the dry run then mirrors the
// inference path — the route's candidates are narrowed by routing.Filter before
// the policy is consulted — so a caller learns, before sending a real request,
// whether any candidate can serve the capability profile it declares. A legacy
// route (no descriptors) has no capability boundary to compare against, so a
// profile cannot narrow it by capability, exactly as the inference path ignores
// a body it cannot classify for such a route; the profile's vocabulary is still
// checked there, and a chat protocol still drops Codex accounts as the
// inference path does for /v1/chat/completions.
type admitRequest struct {
	Class        string             `json:"class"`
	Model        string             `json:"model"`
	Account      string             `json:"account"`
	Requirements *capabilityRequest `json:"requirements"`
}

// capabilityRequest is the control API's wire form of routing.Requirements.
// routing.Requirements is an internal value type with no JSON tags, so the
// endpoint declares its own explicitly-tagged mirror and converts, rather than
// decoding straight into the routing type. The field names are the
// lowercase-underscore spellings used by the equivalent config keys — notably
// json_schema.
type capabilityRequest struct {
	Protocol   string   `json:"protocol"`
	Modalities []string `json:"modalities"`
	Tools      bool     `json:"tools"`
	JSONSchema bool     `json:"json_schema"`
	Stream     bool     `json:"stream"`
}

// Capability admission failures, surfaced as the stable error "type" the client
// branches on. These names are part of the control API contract; the inference
// path reports the same capability_unsupported condition.
const (
	// errCapabilityRequirementsRequired: a model dry run against a route that
	// has opted into capability routing omitted — or under-specified — the
	// explicit capability profile that route needs.
	errCapabilityRequirementsRequired = "capability_requirements_required"
	// errCapabilityUnsupported: the declared capability profile rules out every
	// candidate, or names a protocol or modality the router does not model.
	errCapabilityUnsupported = "capability_unsupported"
)

// capabilityError is a refusal from the capability stage of a model dry run. It
// carries the HTTP status and the stable error type so callers can report both.
type capabilityError struct {
	status int
	typ    string
	msg    string
}

// toRouting validates the declared profile and converts it to the value
// routing.Filter consumes, normalizing it the way routing.Infer normalizes a
// real request: the protocol is required and must be chat or responses; at least
// one modality must be named; and every modality must be one the router models
// (text, image, audio or file, where audio and file are always unsatisfiable and
// so fail closed). Vocabulary is checked before completeness, so an
// errCapabilityRequirementsRequired error still returns a validated (possibly
// empty) Protocol. The declared modalities are used verbatim — deduplicated, with
// blanks ignored — and text is NEVER folded in implicitly: an image-only profile
// asks for image alone, exactly as Infer reports an image-only body, so a
// vision-only candidate can serve it. No default is invented for a field the
// caller left out: an incomplete profile is refused, never widened.
func (c *capabilityRequest) toRouting() (routing.Requirements, *capabilityError) {
	var reqs routing.Requirements
	reqs.Protocol = strings.TrimSpace(c.Protocol)
	if reqs.Protocol != "" && reqs.Protocol != routing.ProtocolChat && reqs.Protocol != routing.ProtocolResponses {
		return routing.Requirements{}, &capabilityError{http.StatusBadRequest, errCapabilityUnsupported,
			fmt.Sprintf("localrouter: unknown protocol %q (want chat or responses)", reqs.Protocol)}
	}
	seen := map[string]bool{}
	declared := 0
	for _, m := range c.Modalities {
		m = strings.TrimSpace(m)
		if m == "" {
			continue
		}
		declared++
		switch m {
		case routing.ModalityText, routing.ModalityImage, routing.ModalityAudio, routing.ModalityFile:
		default:
			return routing.Requirements{}, &capabilityError{http.StatusBadRequest, errCapabilityUnsupported,
				fmt.Sprintf("localrouter: unknown input modality %q (want text, image, audio or file)", m)}
		}
		if !seen[m] {
			seen[m] = true
			reqs.Modalities = append(reqs.Modalities, m)
		}
	}
	if reqs.Protocol == "" {
		return reqs, &capabilityError{http.StatusBadRequest, errCapabilityRequirementsRequired,
			"localrouter: capability requirements must name a protocol (chat or responses)"}
	}
	if declared == 0 {
		return reqs, &capabilityError{http.StatusBadRequest, errCapabilityRequirementsRequired,
			"localrouter: capability requirements must name at least one input modality"}
	}
	reqs.Tools, reqs.JSONSchema, reqs.Stream = c.Tools, c.JSONSchema, c.Stream
	return reqs, nil
}

// filterRoute applies capability-aware narrowing to a model dry run, mirroring
// the inference path as closely as a route lookup without a request body
// allows. An opted-in route demands an explicit profile (absent or incomplete is
// a 400 capability_requirements_required — never a silent fall back to the
// legacy permissive path). The profile is then run through routing.Filter, which
// only ever removes candidates and keeps the operator's order, and the policy
// dry run sees exactly that narrowed set. If the narrowing removes every
// candidate that existed (matching the proxy, which only refuses when it had a
// candidate to lose), the request is a 400 capability_unsupported.
//
// A legacy route keeps the candidates unchanged by capability; a profile given
// for it must still use the modelled vocabulary (capability_unsupported
// otherwise) but need not be complete. On either kind of route a chat protocol
// first drops Codex accounts, exactly as the proxy does for
// /v1/chat/completions, and refuses the request when only Codex accounts were
// configured.
func filterRoute(route core.Route, candidates []string, req *capabilityRequest, isCodex func(string) bool) ([]string, *capabilityError) {
	constrained := len(route.Upstreams) > 0
	if req == nil {
		if !constrained {
			return candidates, nil
		}
		return nil, &capabilityError{http.StatusBadRequest, errCapabilityRequirementsRequired,
			"localrouter: capability requirements are required for this route"}
	}
	reqs, cerr := req.toRouting()
	if cerr != nil && (constrained || cerr.typ != errCapabilityRequirementsRequired) {
		return nil, cerr
	}
	if reqs.Protocol == routing.ProtocolChat {
		var keep []string
		for _, id := range candidates {
			if !isCodex(id) {
				keep = append(keep, id)
			}
		}
		if len(keep) == 0 && len(candidates) > 0 {
			return nil, &capabilityError{http.StatusBadRequest, errCapabilityUnsupported,
				"localrouter: codex accounts only serve /v1/responses"}
		}
		candidates = keep
	}
	if !constrained {
		return candidates, nil
	}
	filtered := routing.Filter(route, candidates, reqs)
	if len(filtered) == 0 && len(candidates) > 0 {
		return nil, &capabilityError{http.StatusBadRequest, errCapabilityUnsupported,
			"localrouter: no account supports the capabilities this request needs"}
	}
	return filtered, nil
}

type admitResponse struct {
	Decision  string `json:"decision"`
	AccountID string `json:"account_id"`
	Reason    string `json:"reason"`
}

func (s *Server) admit(w http.ResponseWriter, r *http.Request) {
	var req admitRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxAdmitBody)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "body must be JSON {class, model} or {class, account}")
		return
	}
	class := core.Class(req.Class)
	if class != core.ClassInteractive && class != core.ClassBackground {
		writeError(w, http.StatusBadRequest, "class must be interactive or background")
		return
	}
	if (req.Model == "") == (req.Account == "") {
		writeError(w, http.StatusBadRequest, "exactly one of model or account is required")
		return
	}
	if req.Account != "" && req.Requirements != nil {
		// A single-account dry run has no route, so requirements have nothing to
		// filter and silently ignoring them would be a false guarantee: refuse
		// the ambiguous request rather than guess which meaning was intended.
		writeError(w, http.StatusBadRequest, "requirements apply only to a model request")
		return
	}
	var candidates []string
	if req.Account != "" {
		if !slices.ContainsFunc(s.deps.Accounts, func(a core.Account) bool { return a.ID == req.Account }) {
			writeError(w, http.StatusNotFound, fmt.Sprintf("unknown account %q", req.Account))
			return
		}
		candidates = []string{req.Account}
	} else {
		route, ok := s.findRoute(req.Model)
		if !ok {
			writeError(w, http.StatusNotFound, fmt.Sprintf("no route for model %q", req.Model))
			return
		}
		candidates = slices.Clone(route.Interactive)
		if class == core.ClassBackground {
			candidates = slices.Clone(route.Background)
		}
		isCodex := func(id string) bool {
			return slices.ContainsFunc(s.deps.Accounts, func(a core.Account) bool {
				return a.ID == id && a.Provider == core.ProviderCodex
			})
		}
		filtered, cerr := filterRoute(route, candidates, req.Requirements, isCodex)
		if cerr != nil {
			writeErrorCode(w, cerr.status, cerr.msg, cerr.typ)
			return
		}
		candidates = filtered
	}
	if s.deps.Policy == nil {
		writeError(w, http.StatusServiceUnavailable, "policy unavailable")
		return
	}
	d := s.deps.Policy.DryRun(class, candidates)
	resp := admitResponse{Decision: "deny", AccountID: d.AccountID, Reason: d.Reason}
	if resp.AccountID == "" {
		resp.AccountID = req.Account
	}
	if d.Allow {
		resp.Decision = "allow"
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) findRoute(model string) (core.Route, bool) {
	if model == "" {
		return core.Route{}, false
	}
	for _, rt := range s.deps.Routes {
		if slices.Contains(rt.Models, model) {
			return rt, true
		}
	}
	return core.Route{}, false
}

// writeJSON encodes v before writing the header, so a value json cannot
// encode (e.g. a +Inf float) yields a 500 error instead of an empty body.
func writeJSON(w http.ResponseWriter, code int, v any) {
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(v); err != nil {
		slog.Warn("control: encoding response failed", "err", err)
		code = http.StatusInternalServerError
		buf.Reset()
		_ = json.NewEncoder(&buf).Encode(map[string]any{"error": map[string]string{"message": "encoding response failed"}})
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_, _ = w.Write(buf.Bytes())
}

// errorBody is the control API's error envelope. It mirrors the proxy's
// ({error:{message,type}}) so a client that already branches on a proxy error
// type can branch on capability_requirements_required / capability_unsupported
// here too. Type is omitted for the capability-neutral errors, keeping their
// bodies byte for byte what they were before this envelope carried a type.
type errorBody struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type,omitempty"`
	} `json:"error"`
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeErrorCode(w, code, msg, "")
}

func writeErrorCode(w http.ResponseWriter, code int, msg, typ string) {
	var b errorBody
	b.Error.Message, b.Error.Type = msg, typ
	writeJSON(w, code, b)
}
