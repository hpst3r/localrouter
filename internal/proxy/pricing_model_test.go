package proxy

// Verifies the forward.go newRecord change: PricingModel is populated ONLY for a
// capability-constrained route (per-candidate Upstreams), carrying the backend
// the attempt actually resolved; a legacy route leaves it empty so the ledger
// keeps pricing on the client-facing Model. No other proxy behaviour is touched.

import (
	"net/http"
	"testing"

	"github.com/hpst3r/localrouter/internal/core"
)

const pmBody = `{"model":"gpt-x","stream":false,"input":"hi"}`

// Constrained route: both UpstreamModel and PricingModel carry the resolved
// backend alias, so the ledger prices the row by the backend.
func TestNewRecordPricingModelConstrainedRoute(t *testing.T) {
	h := newHarness(t)
	h.upstream("a", core.ProviderOllama, okJSON)
	h.routes = []core.Route{{
		Name:        "cap",
		Models:      []string{"gpt-x"},
		Upstreams:   map[string]core.UpstreamSpec{"a": {UpstreamModel: "alias-a"}},
		Interactive: []string{"a"},
		Background:  []string{"a"},
	}}
	h.start()

	resp := h.post("/v1/responses", clientKey, pmBody, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	rows := h.waitRows(1)
	if len(rows) != 1 {
		t.Fatalf("rows %d", len(rows))
	}
	r := rows[0]
	if r.Model != "gpt-x" || r.UpstreamModel != "alias-a" || r.PricingModel != "alias-a" {
		t.Fatalf("constrained row %+v; want Model=gpt-x UpstreamModel=alias-a PricingModel=alias-a", r)
	}
}

// Legacy route with a route-level upstream model: UpstreamModel still records the
// resolved rewrite, but PricingModel stays empty — pricing keys on Model exactly
// as before, never on the upstream alias.
func TestNewRecordPricingModelLegacyRouteUnchanged(t *testing.T) {
	h := newHarness(t)
	h.upstream("a", core.ProviderOllama, okJSON)
	h.routes = []core.Route{{
		Name:          "main",
		Models:        []string{"gpt-x"},
		UpstreamModel: "route-upstream",
		Interactive:   []string{"a"},
		Background:    []string{"a"},
	}}
	h.start()

	resp := h.post("/v1/responses", clientKey, pmBody, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	rows := h.waitRows(1)
	if len(rows) != 1 {
		t.Fatalf("rows %d", len(rows))
	}
	r := rows[0]
	if r.Model != "gpt-x" || r.UpstreamModel != "route-upstream" {
		t.Fatalf("legacy row %+v; want Model=gpt-x UpstreamModel=route-upstream", r)
	}
	if r.PricingModel != "" {
		t.Fatalf("legacy row PricingModel = %q; want empty (pricing must key on Model)", r.PricingModel)
	}
}

// Plain legacy route (no upstream model at all): UpstreamModel mirrors the client
// model and PricingModel stays empty.
func TestNewRecordPricingModelPlainLegacyRoute(t *testing.T) {
	h := newHarness(t)
	h.upstream("a", core.ProviderOllama, okJSON)
	singleRoute(h, "a")
	h.start()

	resp := h.post("/v1/responses", clientKey, pmBody, nil)
	resp.Body.Close()
	rows := h.waitRows(1)
	if len(rows) != 1 {
		t.Fatalf("rows %d", len(rows))
	}
	r := rows[0]
	if r.UpstreamModel != "gpt-x" || r.PricingModel != "" {
		t.Fatalf("plain legacy row %+v; want UpstreamModel=gpt-x PricingModel empty", r)
	}
}
