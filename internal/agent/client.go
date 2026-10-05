package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
)

// MaxBatch is the most records sent in one ingest request.
const MaxBatch = 500

// maxErrBody is how much of a server error body is kept in an error.
const maxErrBody = 200

// IngestPath is the server endpoint agents push to.
const IngestPath = "/control/v1/ingest"

// HTTPError is a non-2xx ingest response. Body is truncated, stripped of
// control characters, and has the client key redacted.
type HTTPError struct {
	Status int
	Body   string
	// Code is the server's error.code ("invalid_record", "invalid_request"),
	// "" if absent.
	Code string
}

func (e *HTTPError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("ingest: http %d", e.Status)
	}
	return fmt.Sprintf("ingest: http %d: %s", e.Status, e.Body)
}

// Permanent reports whether specific records in the request are invalid, so
// the caller may isolate (bisect) and drop them. Only a 400 whose error.code
// is "invalid_record", or a 413 (too large: bisecting shrinks it), qualifies.
// A request-wide 400 (bad host/schema, account not configured on the server)
// is NOT permanent: bisecting would drop every valid record, so it is
// retried later instead. Other statuses (401/403, 429, 5xx) are transient.
// Callers such as claudelog detect this via errors.As with
// interface{ Permanent() bool }.
func (e *HTTPError) Permanent() bool {
	switch e.Status {
	case http.StatusRequestEntityTooLarge:
		return true
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		return e.Code == codeInvalidRecord
	}
	return false
}

const codeInvalidRecord = "invalid_record"

// errorCode extracts error.code from a server error body, "" if absent.
func errorCode(b []byte) string {
	var e struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(b, &e) != nil {
		return ""
	}
	return e.Error.Code
}

// Client posts ingest requests to the central server.
type Client struct {
	endpoint string
	host     string
	key      string
	hc       *http.Client
}

// NewClient returns a Client for server (base URL), attributing pushes to
// host and authenticating with key. hc nil uses a client with a 30s timeout.
func NewClient(server, host, key string, hc *http.Client) *Client {
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	return &Client{endpoint: strings.TrimRight(server, "/") + IngestPath, host: host, key: key, hc: hc}
}

// Host is the host name sent with every request.
func (c *Client) Host() string { return c.host }

// Ingest sends one request (SchemaVersion and Host are filled in).
func (c *Client) Ingest(ctx context.Context, req core.IngestRequest) (core.IngestResponse, error) {
	req.SchemaVersion = 1
	req.Host = c.host
	body, err := json.Marshal(req)
	if err != nil {
		return core.IngestResponse{}, err
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return core.IngestResponse{}, errors.New("ingest: bad server url")
	}
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("Authorization", "Bearer "+c.key)
	resp, err := c.hc.Do(hreq)
	if err != nil {
		return core.IngestResponse{}, c.transportErr(ctx, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return core.IngestResponse{}, &HTTPError{Status: resp.StatusCode, Body: c.sanitize(b), Code: errorCode(b)}
	}
	var out core.IngestResponse
	if err := json.Unmarshal(b, &out); err != nil {
		return core.IngestResponse{}, errors.New("ingest: malformed response")
	}
	return out, nil
}

// PushSnapshot sends one quota snapshot (no records).
func (c *Client) PushSnapshot(ctx context.Context, s core.Snapshot) (core.IngestResponse, error) {
	return c.Ingest(ctx, core.IngestRequest{Snapshots: []core.Snapshot{s}})
}

func (c *Client) transportErr(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var ue interface{ Timeout() bool }
	if errors.As(err, &ue) && ue.Timeout() {
		return errors.New("ingest: timeout")
	}
	// url.Error includes only the endpoint URL (validated to carry no
	// credentials) and the transport cause.
	return fmt.Errorf("ingest: %s", c.redact(err.Error()))
}

func (c *Client) sanitize(b []byte) string {
	s := strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, string(b))
	s = c.redact(strings.TrimSpace(s))
	if r := []rune(s); len(r) > maxErrBody {
		s = string(r[:maxErrBody]) + "…"
	}
	return s
}

func (c *Client) redact(s string) string {
	if c.key == "" {
		return s
	}
	return strings.ReplaceAll(s, c.key, "[redacted]")
}

// RemoteLedger is a core.Ledger and core.BatchLedger that forwards records to
// the server's ingest endpoint. It does not buffer: RecordBatch returns nil
// only once every record has been accepted, so callers may then persist
// progress.
type RemoteLedger struct {
	c *Client
}

// NewRemoteLedger returns a RemoteLedger pushing through c.
func NewRemoteLedger(c *Client) *RemoteLedger { return &RemoteLedger{c: c} }

var (
	_ core.Ledger      = (*RemoteLedger)(nil)
	_ core.BatchLedger = (*RemoteLedger)(nil)
)

// ErrSummaryUnsupported is returned by RemoteLedger.Summary.
var ErrSummaryUnsupported = errors.New("agent ledger: summary unsupported")

// Record sends one record (a batch of one).
func (l *RemoteLedger) Record(ctx context.Context, r core.RequestRecord) error {
	return l.RecordBatch(ctx, []core.RequestRecord{r})
}

// RecordBatch sends rs in chunks of at most MaxBatch, stamping each record
// with the client's host. The server dedupes by ID, so a partially delivered
// batch that is retried yields no duplicates. A rejected chunk returns an
// *HTTPError whose Permanent method classifies it.
func (l *RemoteLedger) RecordBatch(ctx context.Context, rs []core.RequestRecord) error {
	for len(rs) > 0 {
		n := min(len(rs), MaxBatch)
		chunk := make([]core.RequestRecord, n)
		copy(chunk, rs[:n])
		for i := range chunk {
			chunk[i].Host = l.c.host
		}
		if _, err := l.c.Ingest(ctx, core.IngestRequest{Records: chunk}); err != nil {
			return err
		}
		rs = rs[n:]
	}
	return nil
}

// Summary is not available on an agent.
func (l *RemoteLedger) Summary(context.Context, time.Time, string) ([]core.UsageRow, error) {
	return nil, ErrSummaryUnsupported
}

// Close is a no-op.
func (l *RemoteLedger) Close() error { return nil }
