package control

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
)

const (
	maxIngestBody    = 4 << 20
	maxIngestRecords = 1000
	maxSnapshotSkew  = 5 * time.Minute
	maxRecordAge     = 400 * 24 * time.Hour
	// Snapshot bounds. Claude agent snapshots carry one snapshot with a
	// handful of windows; the limits only stop schema pollution.
	maxIngestSnapshots   = 16
	maxSnapshotWindows   = 16
	maxModelRequestKeys  = 16
	maxModelRequestsPerK = 256
	// QuotaSourceAgent marks claude accounts whose snapshots come from agents.
	QuotaSourceAgent = "agent"
)

var (
	hostRE       = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
	recordIDRE   = regexp.MustCompile(`^[A-Za-z0-9:._-]{1,128}$`)
	windowKindRE = regexp.MustCompile(`^[a-z0-9_]{1,32}$`)
)

// ingest handles POST /control/v1/ingest (SPEC "Multi-host"). It always
// requires a client key with Ingest permission, independent of RequireAuth.
// Validation is all-or-nothing: any invalid record or snapshot rejects the
// whole request with 400 before anything is written.
func (s *Server) ingest(w http.ResponseWriter, r *http.Request) {
	// In multi-user mode the bearer middleware has already authenticated and
	// authorized the principal (a service client with Ingest); the legacy
	// Authenticate is never consulted there.
	ma, multi := authFrom(r.Context())
	client := ma.principal.Client
	if !multi {
		token, ok := bearer(r.Header.Get("Authorization"))
		if !ok || s.deps.Authenticate == nil {
			writeError(w, http.StatusUnauthorized, "client key required")
			return
		}
		client, ok = s.deps.Authenticate(token)
		if !ok {
			writeError(w, http.StatusUnauthorized, "invalid client key")
			return
		}
		if !client.Ingest {
			writeError(w, http.StatusForbidden, "client key is not permitted to ingest")
			return
		}
	}

	// Unknown fields are accepted for forward compatibility; trailing data
	// after the JSON value is not.
	var req core.IngestRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxIngestBody))
	err := dec.Decode(&req)
	if err == nil {
		if err = dec.Decode(&struct{}{}); err == io.EOF {
			err = nil
		} else if err == nil {
			err = errors.New("trailing data")
		}
	}
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, "ingest body exceeds 4 MiB")
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]string{
			"message": "body must be a single JSON ingest request", "code": CodeInvalidRequest}})
		return
	}
	if err := s.validateIngest(&req, client.Name); err != nil {
		code := CodeInvalidRequest
		var ie *ingestError
		if errors.As(err, &ie) {
			code = ie.code
		}
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]string{"message": err.Error(), "code": code}})
		return
	}
	if multi {
		// Ownership comes only from the authenticated principal, never from
		// the body (the fields are json:"-" as well): overwrite both before
		// anything is written.
		for i := range req.Records {
			req.Records[i].UserID, req.Records[i].KeyID = ma.principal.UserID, ma.principal.KeyID
		}
	}
	if len(req.Records) > 0 && s.deps.Ledger == nil {
		writeError(w, http.StatusServiceUnavailable, "ledger unavailable")
		return
	}
	if len(req.Snapshots) > 0 && s.deps.Ingester == nil {
		writeError(w, http.StatusServiceUnavailable, "snapshot ingest unavailable")
		return
	}

	if err := s.writeRecords(r, req.Records); err != nil {
		slog.Warn("control: ingest records failed", "client", client.Name, "host", req.Host, "err", err)
		writeError(w, http.StatusInternalServerError, "recording usage failed")
		return
	}
	resp := core.IngestResponse{SchemaVersion: SchemaVersion, RecordsAccepted: len(req.Records)}
	for _, snap := range req.Snapshots {
		if s.snapshotIsOld(snap) {
			resp.SnapshotsIgnored++
			continue
		}
		if err := s.deps.Ingester.IngestSnapshot(snap); err != nil {
			if s.isStale(err) {
				resp.SnapshotsIgnored++
				continue
			}
			slog.Warn("control: ingest snapshot failed", "account", snap.AccountID, "host", req.Host, "err", err)
			writeError(w, http.StatusInternalServerError, "storing snapshot failed")
			return
		}
		resp.SnapshotsAccepted++
	}
	writeJSON(w, http.StatusOK, resp)
}

// IngestErrorCode values sent in 400 bodies as error.code. Agents may drop
// (after isolating) only records rejected with CodeInvalidRecord; any other
// 400 is request-wide (schema, host, server configuration) and must be
// retried later without dropping data.
const (
	CodeInvalidRecord  = "invalid_record"
	CodeInvalidRequest = "invalid_request"
)

// ingestError is a validation failure with its scope.
type ingestError struct {
	code string
	msg  string
}

func (e *ingestError) Error() string { return e.msg }

func reqErr(format string, a ...any) error {
	return &ingestError{code: CodeInvalidRequest, msg: fmt.Sprintf(format, a...)}
}

func recErr(format string, a ...any) error {
	return &ingestError{code: CodeInvalidRecord, msg: fmt.Sprintf(format, a...)}
}

// validateIngest checks req and normalizes it in place: record Host,
// Client, Provider are overwritten and snapshot FetchedAt is capped at now.
// client is the registered client whose key pushed the request; ingested
// usage is attributed to it (Route/Provider still mark it as Claude Code).
func (s *Server) validateIngest(req *core.IngestRequest, client string) error {
	if req.SchemaVersion != 1 {
		return reqErr("schema_version must be 1")
	}
	if !hostRE.MatchString(req.Host) {
		return reqErr("host must match [A-Za-z0-9._-]{1,64}")
	}
	if len(req.Records) > maxIngestRecords {
		return reqErr("at most %d records per request", maxIngestRecords)
	}
	if len(req.Snapshots) > maxIngestSnapshots {
		return reqErr("at most %d snapshots per request", maxIngestSnapshots)
	}
	claude := map[string]core.Account{}
	for _, a := range s.deps.Accounts {
		if a.Provider == core.ProviderClaude {
			claude[a.ID] = a
		}
	}
	now := s.deps.Clock.Now()
	for i := range req.Records {
		rec := &req.Records[i]
		if !recordIDRE.MatchString(rec.ID) {
			return recErr("records[%d]: id must match [A-Za-z0-9:._-]{1,128}", i)
		}
		if _, ok := claude[rec.AccountID]; !ok {
			return reqErr("records[%d]: account_id %q is not a configured claude account", i, rec.AccountID)
		}
		if !rec.UsageKnown {
			return recErr("records[%d]: usage_known must be true", i)
		}
		u := rec.Usage
		for _, n := range []int64{u.InputTokens, u.CachedInputTokens, u.CacheCreationInputTokens,
			u.OutputTokens, u.ReasoningTokens} {
			if n < 0 || n > core.MaxRecordTokens {
				return recErr("records[%d]: usage token counts must be between 0 and %d", i, core.MaxRecordTokens)
			}
		}
		if err := validateRecordFields(i, rec); err != nil {
			return err
		}
		switch {
		case rec.StartedAt.IsZero():
			return recErr("records[%d]: started_at is required", i)
		case rec.StartedAt.After(now.Add(maxSnapshotSkew)):
			return recErr("records[%d]: started_at is more than 5 minutes in the future", i)
		case rec.StartedAt.Before(now.Add(-maxRecordAge)):
			return recErr("records[%d]: started_at is more than 400 days old", i)
		case !rec.FinishedAt.IsZero() && rec.FinishedAt.Before(rec.StartedAt):
			return recErr("records[%d]: finished_at is before started_at", i)
		}
		rec.Host = req.Host
		rec.Client = client
		rec.Provider = core.ProviderClaude
		rec.Route = "claude"
	}
	limit := now.Add(maxSnapshotSkew)
	for i := range req.Snapshots {
		snap := &req.Snapshots[i]
		a, ok := claude[snap.AccountID]
		if !ok || a.QuotaSource != QuotaSourceAgent {
			return reqErr("snapshots[%d]: account_id %q is not a claude account with quota_source agent", i, snap.AccountID)
		}
		if snap.FetchedAt.IsZero() {
			return reqErr("snapshots[%d]: fetched_at is required", i)
		}
		if snap.FetchedAt.After(limit) {
			return reqErr("snapshots[%d]: fetched_at is more than 5 minutes in the future", i)
		}
		if err := validateSnapshotFields(i, snap); err != nil {
			return err
		}
		kinds := make(map[string]bool, len(snap.Windows))
		for j, win := range snap.Windows {
			if !windowKindRE.MatchString(win.Kind) {
				return reqErr("snapshots[%d].windows[%d]: kind must match [a-z0-9_]{1,32}", i, j)
			}
			if kinds[win.Kind] {
				return reqErr("snapshots[%d].windows[%d]: duplicate kind %q", i, j, win.Kind)
			}
			kinds[win.Kind] = true
			if math.IsNaN(win.UsedFrac) || win.UsedFrac < 0 || win.UsedFrac > 1 {
				return reqErr("snapshots[%d].windows[%d]: used_frac must be between 0 and 1", i, j)
			}
			if win.WindowSeconds < 0 {
				return reqErr("snapshots[%d].windows[%d]: window_seconds must be non-negative", i, j)
			}
		}
		// Store at most server now: a skewed future FetchedAt would make
		// every genuine snapshot until then look stale.
		if snap.FetchedAt.After(now) {
			snap.FetchedAt = now
		}
	}
	return nil
}

// validateSnapshotFields bounds a snapshot's collections and free-form
// fields (same text rules as validateRecordFields) and rejects members that
// only openrouter/codex snapshots carry.
func validateSnapshotFields(i int, snap *core.Snapshot) error {
	if snap.Credits != nil || snap.Key != nil || snap.Allowed != nil {
		return reqErr("snapshots[%d]: credits, key and allowed are not accepted from agents", i)
	}
	if len(snap.Windows) > maxSnapshotWindows {
		return reqErr("snapshots[%d]: at most %d windows", i, maxSnapshotWindows)
	}
	if core.TruncateLabel(snap.Plan) != snap.Plan || core.TruncateLabel(snap.Source) != snap.Source {
		return reqErr("snapshots[%d]: plan and source must be at most %d bytes of UTF-8 without control characters",
			i, core.MaxLabelBytes)
	}
	if core.TruncateError(snap.Err) != snap.Err {
		return reqErr("snapshots[%d]: error must be at most %d bytes of UTF-8 without control characters",
			i, core.MaxErrorBytes)
	}
	if len(snap.ModelRequests) > maxModelRequestKeys {
		return reqErr("snapshots[%d]: model_requests has at most %d window kinds", i, maxModelRequestKeys)
	}
	for k, counts := range snap.ModelRequests {
		if !windowKindRE.MatchString(k) {
			return reqErr("snapshots[%d].model_requests: kind must match [a-z0-9_]{1,32}", i)
		}
		if len(counts) > maxModelRequestsPerK {
			return reqErr("snapshots[%d].model_requests[%q]: at most %d models", i, k, maxModelRequestsPerK)
		}
		for j, c := range counts {
			if core.TruncateLabel(c.Model) != c.Model {
				return reqErr("snapshots[%d].model_requests[%q][%d]: model must be at most %d bytes of UTF-8 without control characters",
					i, k, j, core.MaxLabelBytes)
			}
			if c.Requests < 0 || c.Requests > core.MaxRecordTokens {
				return reqErr("snapshots[%d].model_requests[%q][%d]: requests must be between 0 and %d",
					i, k, j, core.MaxRecordTokens)
			}
		}
	}
	return nil
}

// validateRecordFields bounds the free-form fields of an ingested record the
// way the proxy bounds its own: labels at most core.MaxLabelBytes and the
// error at most core.MaxErrorBytes of valid UTF-8 without control
// characters (the core truncation helpers leave such values unchanged).
func validateRecordFields(i int, rec *core.RequestRecord) error {
	switch rec.Class {
	case "", core.ClassInteractive, core.ClassBackground:
	default:
		return recErr("records[%d]: class must be empty, interactive or background", i)
	}
	for _, f := range []struct{ name, v string }{
		{"model", rec.Model}, {"session", rec.Session}, {"task", rec.Task}, {"agent", rec.Agent},
		{"upstream_identity", rec.UpstreamIdentity}, {"failover_of", rec.FailoverOf},
		{"upstream_model", rec.UpstreamModel}, {"pricing_model", rec.PricingModel},
	} {
		if core.TruncateLabel(f.v) != f.v {
			return recErr("records[%d]: %s must be at most %d bytes of UTF-8 without control characters",
				i, f.name, core.MaxLabelBytes)
		}
	}
	if core.TruncateError(rec.Error) != rec.Error {
		return recErr("records[%d]: error must be at most %d bytes of UTF-8 without control characters",
			i, core.MaxErrorBytes)
	}
	if rec.Status < 0 || rec.LatencyMS < 0 || rec.BytesOut < 0 {
		return recErr("records[%d]: status, latency_ms and bytes_out must be non-negative", i)
	}
	return nil
}

// writeRecords stores recs in one batch when the ledger supports it.
func (s *Server) writeRecords(r *http.Request, recs []core.RequestRecord) error {
	if len(recs) == 0 {
		return nil
	}
	if bl, ok := s.deps.Ledger.(core.BatchLedger); ok {
		return bl.RecordBatch(r.Context(), recs)
	}
	for _, rec := range recs {
		if err := s.deps.Ledger.Record(r.Context(), rec); err != nil {
			return err
		}
	}
	return nil
}

// snapshotIsOld reports whether the quota source already holds a snapshot
// at least as new as snap.
func (s *Server) snapshotIsOld(snap core.Snapshot) bool {
	if s.deps.Quota == nil {
		return false
	}
	cur, ok := s.deps.Quota.Latest(snap.AccountID)
	return ok && !snap.FetchedAt.After(cur.FetchedAt)
}

// isStale classifies an IngestSnapshot error as "older than stored".
func (s *Server) isStale(err error) bool {
	if s.deps.IsSnapshotStale != nil {
		return s.deps.IsSnapshotStale(err)
	}
	return strings.Contains(strings.ToLower(err.Error()), "stale")
}
