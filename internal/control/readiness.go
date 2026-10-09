package control

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
)

// StoragePinger probes local storage health with a bounded, read-only check
// (for SQLite, a connection ping). It must not mutate state. A nil Storage in
// Deps means storage health is not observed and diagnostics reports it as
// unconfigured rather than claiming it is healthy.
type StoragePinger interface {
	Ping(ctx context.Context) error
}

// InflightReporter snapshots inference concurrency for diagnostics. It never
// exposes client keys or request content.
type InflightReporter interface {
	InflightStats() core.InflightStats
}

// storagePingTimeout bounds the readiness storage probe so readiness checks
// stay fast; a slow or hung database must not make the probe itself hang.
const storagePingTimeout = 2 * time.Second

// readyz reports only whether this instance is ready to serve, as a minimal
// unauthenticated document. Readiness is local serving and storage health: it
// deliberately says nothing about upstream provider health (an upstream
// outage is not a local readiness failure — the router still serves, it just
// reports per-account reasons in diagnostics). No account data is included.
func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	code := s.readyStatus()
	writeJSON(w, code, readyDoc{Ready: code == http.StatusOK})
}

// readyDoc is the minimal readiness document: a single boolean, no version,
// no dependency detail and no account data.
type readyDoc struct {
	Ready bool `json:"ready"`
}

// readyFlag folds in the optional process-serving predicate. A nil Ready
// defaults to ready so a server constructed without the seam still reports
// ready from storage alone.
func (s *Server) readyFlag() bool {
	if s.deps.Ready == nil {
		return true
	}
	return s.deps.Ready()
}

// pingStorage performs the single bounded, read-only storage probe used by
// readiness. A nil Storage is not ready: readiness must not claim health it
// cannot observe.
func (s *Server) pingStorage() bool {
	if s.deps.Storage == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), storagePingTimeout)
	defer cancel()
	return s.deps.Storage.Ping(ctx) == nil
}

func (s *Server) readyStatus() int {
	if !s.readyFlag() || !s.pingStorage() {
		return http.StatusServiceUnavailable
	}
	return http.StatusOK
}

// diagnosticsDoc is the authenticated GET /control/v1/diagnostics response.
type diagnosticsDoc struct {
	SchemaVersion int                `json:"schema_version"`
	Now           time.Time          `json:"now"`
	Ready         bool               `json:"ready"`
	Storage       storageHealth      `json:"storage"`
	Inflight      core.InflightStats `json:"inflight"`
	Accounts      []diagAccount      `json:"accounts"`
}

type storageHealth struct {
	// Configured is false when no storage probe is wired; OK is then false and
	// no health is claimed.
	Configured bool   `json:"configured"`
	OK         bool   `json:"ok"`
	Error      string `json:"error,omitempty"`
}

// diagAccount is a compact per-account health view: availability, staleness
// and the reason a class is inadmissible. It never carries keys, filesystem
// secret paths, prompt text or per-account quota windows.
type diagAccount struct {
	ID           string `json:"id"`
	Provider     string `json:"provider"`
	Healthy      bool   `json:"healthy"`
	Stale        bool   `json:"stale"`
	Cooldown     bool   `json:"cooldown"`
	Reason       string `json:"reason,omitempty"`
	SnapshotAgeS *int64 `json:"snapshot_age_s"`
}

func (s *Server) diagnostics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	now := s.deps.Clock.Now()
	// Diagnostics performs a single storage probe and derives both the
	// readiness flag and storage.ok from it, so the two can never disagree and
	// a hung or private database is never probed twice per request.
	sh := s.storageHealth()
	ready := s.readyFlag() && sh.Configured && sh.OK
	doc := diagnosticsDoc{
		SchemaVersion: SchemaVersion,
		Now:           now.UTC(),
		Ready:         ready,
		Storage:       sh,
		Inflight:      s.inflightStats(),
		Accounts:      make([]diagAccount, 0, len(s.deps.Accounts)),
	}
	for _, a := range s.deps.Accounts {
		doc.Accounts = append(doc.Accounts, s.diagAccount(a, now))
	}
	writeJSON(w, http.StatusOK, doc)
}

func (s *Server) inflightStats() core.InflightStats {
	if s.deps.Inflight == nil {
		return core.InflightStats{}
	}
	st := s.deps.Inflight.InflightStats()
	if st.Clients == nil {
		st.Clients = []core.ClientInflight{}
	}
	return st
}

func (s *Server) storageHealth() storageHealth {
	if s.deps.Storage == nil {
		return storageHealth{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), storagePingTimeout)
	defer cancel()
	if err := s.deps.Storage.Ping(ctx); err != nil {
		return storageHealth{Configured: true, OK: false, Error: boundedError(err)}
	}
	return storageHealth{Configured: true, OK: true}
}

// storageErrorUnavailable is the only non-timeout reason reported for a
// failed storage probe. Raw storage errors can embed filesystem paths, DSNs
// and credentials, so diagnostics never echoes them.
const storageErrorUnavailable = "storage unavailable"

func (s *Server) diagAccount(a core.Account, now time.Time) diagAccount {
	d := diagAccount{ID: a.ID, Provider: a.Provider}
	var ps core.AccountState
	if s.deps.Policy != nil {
		ps = s.deps.Policy.Status(a.ID)
	}
	d.Stale = ps.Stale
	d.Cooldown = !ps.CooldownUntil.IsZero() && now.Before(ps.CooldownUntil)
	d.Reason = ps.Reason
	var snap core.Snapshot
	var have bool
	if s.deps.Quota != nil {
		snap, have = s.deps.Quota.Latest(a.ID)
	}
	snapErr := have && snap.Err != ""
	if !have {
		d.Stale = true
	} else if snap.FetchedAt.IsZero() {
		d.Stale = true
	} else {
		age := int64(max(now.Sub(snap.FetchedAt), 0) / time.Second)
		d.SnapshotAgeS = &age
		if now.Sub(snap.FetchedAt) > s.opts.StaleAfter {
			d.Stale = true
		}
	}
	// Healthy mirrors the status document: the account is admissible for
	// interactive work — not stale, not cooling down, no reported error and
	// an empty reason — and no snapshot error is outstanding.
	d.Healthy = !d.Cooldown && ps.InteractiveAdmissible && !d.Stale && !snapErr
	if d.Reason == "" && snapErr {
		// Never echo raw snapshot errors: they can carry URLs, keys or paths.
		d.Reason = snapshotErrorUnavailable
	}
	return d
}

// snapshotErrorUnavailable is the generic reason reported when a quota
// snapshot fetch failed. The raw snap.Err is never surfaced.
const snapshotErrorUnavailable = "quota snapshot unavailable"

// boundedError sanitizes a storage error for the diagnostics document. Only
// two generic reasons are ever returned: a deadline/timeout probe and a
// generic unavailable. The raw error text (which may carry paths or
// credentials) is never included.
func boundedError(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return "storage probe timed out"
	}
	return storageErrorUnavailable
}
