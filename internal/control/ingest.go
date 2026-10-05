package control

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
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
	// ingestClient is forced on every ingested record.
	ingestClient = "claude-code"
	// QuotaSourceAgent marks claude accounts whose snapshots come from agents.
	QuotaSourceAgent = "agent"
)

var hostRE = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// ingest handles POST /control/v1/ingest (SPEC "Multi-host"). It always
// requires a client key with Ingest permission, independent of RequireAuth.
// Validation is all-or-nothing: any invalid record or snapshot rejects the
// whole request with 400 before anything is written.
func (s *Server) ingest(w http.ResponseWriter, r *http.Request) {
	token, ok := bearer(r.Header.Get("Authorization"))
	if !ok || s.deps.Authenticate == nil {
		writeError(w, http.StatusUnauthorized, "client key required")
		return
	}
	client, ok := s.deps.Authenticate(token)
	if !ok {
		writeError(w, http.StatusUnauthorized, "invalid client key")
		return
	}
	if !client.Ingest {
		writeError(w, http.StatusForbidden, "client key is not permitted to ingest")
		return
	}

	var req core.IngestRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxIngestBody)).Decode(&req); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, "ingest body exceeds 4 MiB")
			return
		}
		writeError(w, http.StatusBadRequest, "body must be a JSON ingest request")
		return
	}
	if err := s.validateIngest(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
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

// validateIngest checks req and normalizes its records in place (Host,
// Client, Provider are overwritten).
func (s *Server) validateIngest(req *core.IngestRequest) error {
	if req.SchemaVersion != 1 {
		return errors.New("schema_version must be 1")
	}
	if !hostRE.MatchString(req.Host) {
		return errors.New("host must match [A-Za-z0-9._-]{1,64}")
	}
	if len(req.Records) > maxIngestRecords {
		return fmt.Errorf("at most %d records per request", maxIngestRecords)
	}
	claude := map[string]core.Account{}
	for _, a := range s.deps.Accounts {
		if a.Provider == core.ProviderClaude {
			claude[a.ID] = a
		}
	}
	for i := range req.Records {
		rec := &req.Records[i]
		if rec.ID == "" {
			return fmt.Errorf("records[%d]: id is required", i)
		}
		if _, ok := claude[rec.AccountID]; !ok {
			return fmt.Errorf("records[%d]: account_id %q is not a configured claude account", i, rec.AccountID)
		}
		if !rec.UsageKnown {
			return fmt.Errorf("records[%d]: usage_known must be true", i)
		}
		u := rec.Usage
		if u.InputTokens < 0 || u.CachedInputTokens < 0 || u.CacheCreationInputTokens < 0 ||
			u.OutputTokens < 0 || u.ReasoningTokens < 0 {
			return fmt.Errorf("records[%d]: usage must be non-negative", i)
		}
		rec.Host = req.Host
		rec.Client = ingestClient
		rec.Provider = core.ProviderClaude
	}
	limit := s.deps.Clock.Now().Add(maxSnapshotSkew)
	for i, snap := range req.Snapshots {
		a, ok := claude[snap.AccountID]
		if !ok || a.QuotaSource != QuotaSourceAgent {
			return fmt.Errorf("snapshots[%d]: account_id %q is not a claude account with quota_source agent", i, snap.AccountID)
		}
		if snap.FetchedAt.IsZero() {
			return fmt.Errorf("snapshots[%d]: fetched_at is required", i)
		}
		if snap.FetchedAt.After(limit) {
			return fmt.Errorf("snapshots[%d]: fetched_at is more than 5 minutes in the future", i)
		}
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
