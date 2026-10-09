package control

import (
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
	v.Exhausted = k.LimitUSD != nil && k.LimitRemainingUSD != nil && *k.LimitRemainingUSD <= 0 &&
		(k.LimitResetAt.IsZero() || now.Before(k.LimitResetAt))
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

// admitRequest names exactly one of Model (dry-run the route's candidates)
// or Account (dry-run that single account, e.g. a quota-only claude account).
type admitRequest struct {
	Class   string `json:"class"`
	Model   string `json:"model"`
	Account string `json:"account"`
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

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"error": map[string]string{"message": msg}})
}
