package control

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
	"github.com/hpst3r/localrouter/internal/ledger"
)

// TestSecAnalyticsMemoryBounded: thousands of distinct keys cost one
// analytics GET memory proportional to top N (not keys x buckets), and the
// response carries at most core.AnalyticsMaxBreakdown breakdown rows.
func TestSecAnalyticsMemoryBounded(t *testing.T) {
	led, err := ledger.Open(filepath.Join(t.TempDir(), "l.db"), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer led.Close()
	h := New(Deps{
		Accounts: []core.Account{{ID: "claude-max", Provider: core.ProviderClaude}},
		Ledger:   led,
		Authenticate: func(string) (core.Client, bool) {
			return core.Client{Name: "agent1", Class: core.ClassBackground, Ingest: true}, true
		},
	}, Options{}).Handler()

	const reqs, perReq = 5, 1000
	now := time.Now().Add(-time.Minute)
	for r := range reqs {
		recs := make([]core.RequestRecord, perReq)
		for i := range recs {
			recs[i] = core.RequestRecord{ID: fmt.Sprintf("x:%d:%d", r, i), AccountID: "claude-max", UsageKnown: true,
				StartedAt: now, Task: fmt.Sprintf("t-%d-%d", r, i), Usage: core.Usage{InputTokens: int64(1 + r*perReq + i)}}
		}
		body, _ := json.Marshal(core.IngestRequest{SchemaVersion: 1, Host: "h", Records: recs})
		req := httptest.NewRequest("POST", "/control/v1/ingest", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer k")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != 200 {
			t.Fatalf("ingest: %d %s", w.Code, w.Body)
		}
	}

	from := time.Now().Add(-31 * 24 * time.Hour).UTC().Format(time.RFC3339)
	to := time.Now().UTC().Format(time.RFC3339)
	req := httptest.NewRequest("GET", "/control/v1/analytics?group=task&bucket=hour&top=1&from="+from+"&to="+to, nil)
	w := httptest.NewRecorder()
	var m0, m1 runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&m0)
	h.ServeHTTP(w, req)
	runtime.ReadMemStats(&m1)
	if w.Code != http.StatusOK {
		t.Fatalf("analytics: %d %s", w.Code, w.Body)
	}
	var doc core.AnalyticsResult
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	alloc := m1.TotalAlloc - m0.TotalAlloc
	t.Logf("keys=%d breakdown=%d omitted=%d response=%d B alloc=%d KiB",
		reqs*perReq, len(doc.Breakdown), doc.BreakdownOmitted, w.Body.Len(), alloc>>10)
	// Unbounded: ~744 buckets x ~96 B x 5000 keys ≈ 340 MiB.
	if alloc > 40<<20 {
		t.Errorf("analytics allocated %d MiB", alloc>>20)
	}
	if len(doc.Breakdown) != core.AnalyticsMaxBreakdown || doc.BreakdownOmitted != reqs*perReq-core.AnalyticsMaxBreakdown {
		t.Fatalf("breakdown len=%d omitted=%d", len(doc.Breakdown), doc.BreakdownOmitted)
	}
	if len(doc.Series) != 2 || doc.Series[0].Key != "t-4-999" || doc.Series[1].Key != core.AnalyticsOtherKey {
		t.Fatalf("series = %d", len(doc.Series))
	}
	if doc.Series[1].Total.InputTokens != doc.Totals.InputTokens-doc.Series[0].Total.InputTokens ||
		doc.Series[1].Total.Requests != doc.Totals.Requests-1 || doc.Totals.Requests != reqs*perReq {
		t.Fatalf("other = %+v totals = %+v", doc.Series[1].Total, doc.Totals)
	}

	// dimensions: capped per dimension.
	req = httptest.NewRequest("GET", "/control/v1/analytics/dimensions", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	var dims dimensionsDoc
	if err := json.Unmarshal(w.Body.Bytes(), &dims); err != nil || w.Code != 200 {
		t.Fatalf("dimensions: %d %v", w.Code, err)
	}
	if len(dims.Dimensions["task"]) != dimensionsTop || dims.Dimensions["task"][0].Key != "t-4-999" {
		t.Fatalf("task dimension = %d", len(dims.Dimensions["task"]))
	}
}

// TestSecIngestFieldValidation: ingest rejects (invalid_record) unknown
// classes, oversized or malformed labels, oversized errors and negative
// counters; the proxy truncates the same fields to the same bounds.
func TestSecIngestFieldValidation(t *testing.T) {
	long := strings.Repeat("a", core.MaxLabelBytes+1)
	for name, mut := range map[string]func(*core.RequestRecord){
		"class":              func(r *core.RequestRecord) { r.Class = "root-override" },
		"task long":          func(r *core.RequestRecord) { r.Task = strings.Repeat("A", 1<<20) },
		"model long":         func(r *core.RequestRecord) { r.Model = long },
		"agent long":         func(r *core.RequestRecord) { r.Agent = long },
		"session long":       func(r *core.RequestRecord) { r.Session = long },
		"upstream long":      func(r *core.RequestRecord) { r.UpstreamIdentity = long },
		"failover long":      func(r *core.RequestRecord) { r.FailoverOf = long },
		"task control":       func(r *core.RequestRecord) { r.Task = "a\nb" },
		"session control":    func(r *core.RequestRecord) { r.Session = "a\x00" },
		"agent del":          func(r *core.RequestRecord) { r.Agent = "a\x7f" },
		"error long":         func(r *core.RequestRecord) { r.Error = strings.Repeat("e", core.MaxErrorBytes+1) },
		"status negative":    func(r *core.RequestRecord) { r.Status = -5 },
		"latency negative":   func(r *core.RequestRecord) { r.LatencyMS = -1 },
		"bytes_out negative": func(r *core.RequestRecord) { r.BytesOut = -1 },
	} {
		f := newIngestFixture(t, true, true)
		r := goodRecord("r1")
		mut(&r)
		rec := f.post(t, ingestKey, core.IngestRequest{SchemaVersion: 1, Host: "vm1", Records: []core.RequestRecord{r}})
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), CodeInvalidRecord) {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
		if len(f.rows()) != 0 {
			t.Errorf("%s: stored", name)
		}
	}

	// encoding/json turns invalid UTF-8 into U+FFFD on the wire, so check
	// the validator directly.
	for _, mut := range []func(*core.RequestRecord){
		func(r *core.RequestRecord) { r.Model = "a\xffb" },
		func(r *core.RequestRecord) { r.Error = "\xff" },
	} {
		r := goodRecord("r1")
		mut(&r)
		if err := validateRecordFields(0, &r); err == nil {
			t.Errorf("invalid UTF-8 accepted: %+v", r)
		}
	}

	// At the bounds, with the allowed classes, is accepted.
	for _, class := range []core.Class{"", core.ClassInteractive, core.ClassBackground} {
		f := newIngestFixture(t, true, true)
		r := goodRecord("r1")
		r.Class = class
		r.Task = strings.Repeat("é", core.MaxLabelBytes/2)
		r.Model, r.Agent, r.Session = long[1:], "main", "s-1"
		r.Error = strings.Repeat("e", core.MaxErrorBytes)
		rec := f.post(t, ingestKey, core.IngestRequest{SchemaVersion: 1, Host: "vm1", Records: []core.RequestRecord{r}})
		if rec.Code != http.StatusOK || len(f.rows()) != 1 {
			t.Fatalf("class %q: %d %s", class, rec.Code, rec.Body)
		}
	}
}

// TestSecWriteJSONEncodeError: a value json cannot encode yields a 500 error
// body instead of an empty 200.
func TestSecWriteJSONEncodeError(t *testing.T) {
	w := httptest.NewRecorder()
	writeJSON(w, http.StatusOK, map[string]float64{"x": math.Inf(1)})
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("code = %d body %q", w.Code, w.Body)
	}
	var body struct {
		Error struct{ Message string } `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body.Error.Message == "" {
		t.Fatalf("body %q err %v", w.Body, err)
	}

	// End to end: an analytics doc with +Inf cost.
	f, al := newAnalyticsFixture(false)
	inf := math.Inf(1)
	al.result = func(q core.AnalyticsQuery) core.AnalyticsResult {
		return core.AnalyticsResult{Totals: core.UsageRow{CostUSD: &inf}}
	}
	if rec := f.do(t, "GET", "/control/v1/analytics", "", ""); rec.Code != http.StatusInternalServerError ||
		!strings.Contains(rec.Body.String(), "error") {
		t.Fatalf("analytics: %d %q", rec.Code, rec.Body)
	}
}
