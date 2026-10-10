package app_test

// Capability-descriptor reload acceptance tests.
//
// Scope: this file only. It exercises the capability/descriptor plumbing that
// the routing generation publishes to the proxy:
//   * an upstream descriptor (per-candidate upstream_model / protocols /
//     input_modalities) is faithfully projected from the loaded config into the
//     core.Route the runtime serves, and that projection is a deep copy — maps
//     and slices cannot be mutated back through the projection;
//   * a descriptor-only edit (changing only a candidate's upstream_model) is a
//     hot reload: it increments the generation and reports no restart-only
//     fields, rather than forcing a process restart;
//   * a request held across that reload keeps the OLD descriptor (old upstream
//     backend/model and old pricing), while a request admitted after the reload
//     uses the NEW descriptor;
//   * a config.Config handed to ReloadConfig is deep-copied (JSON clone), so
//     mutating the caller's copy afterwards — including the route's Upstreams
//     map and the descriptor's Protocols/InputModalities slices — cannot leak
//     into the running generation.
//
// Every upstream is an httptest.Server reached over loopback; no live service,
// credential or outbound network is used. Key material in the surrounding
// harness is fake fixture data, not a real credential.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/config"
)

// capRouteYAML renders a one-account / one-route config whose route carries an
// upstream descriptor for its single candidate. The descriptor's upstream_model
// is what a reload can change to swap the backend without a restart.
func capRouteYAML(upstreamURL, keyFileName, model, backend string) string {
	return fmt.Sprintf(`listen: 127.0.0.1:0
data_dir: data
pricing_file: pricing.yaml
control: {require_auth: true}
limits: {max_concurrent: 4}
clients:
  - {name: ide, class: interactive, key_file: %s}
accounts:
  - id: acct
    provider: openai_compat
    base_url: %s/v1
    api_key_env: HR_UPSTREAM_KEY
routes:
  - name: r
    models: [%s]
    interactive: [acct]
    background: [acct]
    upstreams:
      acct:
        upstream_model: %s
        protocols: [responses]
        input_modalities: [text]
`, keyFileName, upstreamURL, model, backend)
}

// capPricingYAML prices the two backend aliases differently so the ledger cost
// reveals which descriptor (old vs new) each request was admitted under.
func capPricingYAML(inputA, inputB float64) string {
	return fmt.Sprintf("models:\n  backend-a:\n    input: %g\n    output: 0\n  backend-b:\n    input: %g\n    output: 0\n", inputA, inputB)
}

// capUpstreamModel extracts the "model" the proxy actually sent upstream.
func capUpstreamModel(t *testing.T, body []byte) string {
	t.Helper()
	var doc struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("decode upstream body: %v (%s)", err, body)
	}
	return doc.Model
}

// TestHotReloadDescriptorOnlySwapsBackendHeldRequestKeepsOldDescriptor verifies
// that changing only a candidate's upstream descriptor is a hot reload (not a
// restart) and that a request held across it keeps the descriptor it was
// admitted under — including the backend model and therefore the pricing — while
// a request admitted afterwards uses the new descriptor.
func TestHotReloadDescriptorOnlySwapsBackendHeldRequestKeepsOldDescriptor(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HR_UPSTREAM_KEY", hrUpstreamKey)
	keyA := "hr-client-keyA-0123456789"
	hrWriteFile(t, filepath.Join(dir, "a.key"), keyA)
	hrWriteFile(t, filepath.Join(dir, "pricing.yaml"), capPricingYAML(2.0, 5.0)) // $2 / $5 per 1M input

	up := hrNewUpstream(t)
	cfgPath := filepath.Join(dir, "config.yaml")
	hrWriteFile(t, cfgPath, capRouteYAML(up.srv.URL, "a.key", "client-model", "backend-a"))
	e := hrBuild(t, dir, cfgPath, up)

	if g := e.a.ReloadStatus().Generation; g != 1 {
		t.Fatalf("initial generation = %d", g)
	}

	// Hold request #1 under the generation-1 descriptor (backend-a).
	up.arm()
	old := make(chan hrResult, 1)
	go func() { old <- e.responses(keyA, "client-model") }()
	select {
	case <-up.arrived:
	case <-time.After(3 * time.Second):
		t.Fatal("gated request never reached the upstream")
	}
	e.hrWaitActive(keyA, 1)

	// Descriptor-only edit: same account, same base_url, same key file, same
	// limits — only the candidate's upstream_model changes.
	hrWriteFile(t, cfgPath, capRouteYAML(up.srv.URL, "a.key", "client-model", "backend-b"))
	st, err := e.a.Reload(cfgPath)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !st.OK || st.Generation != 2 {
		t.Fatalf("reload status = %#v, want ok generation 2", st)
	}
	if len(st.RestartOnly) != 0 {
		t.Fatalf("descriptor-only reload demanded restart-only fields: %v", st.RestartOnly)
	}

	// Release the held request; it must still complete under generation 1.
	up.release()
	var first hrResult
	select {
	case first = <-old:
	case <-time.After(5 * time.Second):
		t.Fatal("held request never completed")
	}
	if first.status != http.StatusOK {
		t.Fatalf("held request status %d: %s", first.status, first.body)
	}

	// A request admitted after the reload uses the new descriptor.
	if r := e.responses(keyA, "client-model"); r.status != http.StatusOK {
		t.Fatalf("post-reload request status %d: %s", r.status, r.body)
	}

	hits := up.hitsCopy()
	if len(hits) != 2 {
		t.Fatalf("upstream hits = %d, want 2", len(hits))
	}
	if got := capUpstreamModel(t, hits[0].Body); got != "backend-a" {
		t.Fatalf("held request upstream model = %q, want backend-a (old descriptor)", got)
	}
	if got := capUpstreamModel(t, hits[1].Body); got != "backend-b" {
		t.Fatalf("post-reload request upstream model = %q, want backend-b (new descriptor)", got)
	}

	// Cost proves each request was priced under the descriptor it was admitted
	// with: 1000 input tokens at $2/1M (held, old) + $5/1M (new). Had the held
	// request been re-resolved against the new descriptor the sum would be
	// $10/1M, so the exact sum is a real assertion, not a tautology.
	sum := e.summary()["acct"]
	if sum.Requests != 2 {
		t.Fatalf("ledger requests = %d, want 2", sum.Requests)
	}
	if sum.UnpricedRequests != 0 {
		t.Fatalf("unpriced requests = %d, want 0", sum.UnpricedRequests)
	}
	const want = (1000*2.0 + 1000*5.0) / 1e6
	if sum.CostUSD == nil {
		t.Fatal("aggregate cost is nil")
	}
	if got := *sum.CostUSD; got < want-1e-12 || got > want+1e-12 {
		t.Fatalf("aggregate cost = %v, want %v (held request must keep old descriptor pricing)", got, want)
	}
}

// TestHotReloadConfigCloneIsolation verifies ReloadConfig deep-copies the
// candidate config: mutating the caller's copy afterwards — the route model
// list, the Upstreams map, and the descriptor's Protocols/InputModalities
// slices — must not reach the running generation.
func TestHotReloadConfigCloneIsolation(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HR_UPSTREAM_KEY", hrUpstreamKey)
	keyA := "hr-client-keyA-0123456789"
	hrWriteFile(t, filepath.Join(dir, "a.key"), keyA)
	hrWriteFile(t, filepath.Join(dir, "pricing.yaml"), capPricingYAML(2.0, 5.0))

	up := hrNewUpstream(t)
	cfgPath := filepath.Join(dir, "config.yaml")
	hrWriteFile(t, cfgPath, capRouteYAML(up.srv.URL, "a.key", "client-model", "backend-a"))
	e := hrBuild(t, dir, cfgPath, up)

	cand, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("load candidate: %v", err)
	}
	if _, err := e.a.ReloadConfig(cand); err != nil {
		t.Fatalf("reload candidate: %v", err)
	}

	// Mutate the caller's copy in place across maps and slices.
	cand.Routes[0].Models[0] = "MUTATED"
	cand.Routes[0].Interactive[0] = "MUTATED"
	spec := cand.Routes[0].Upstreams["acct"]
	spec.UpstreamModel = "MUTATED"
	spec.Protocols[0] = "chat"
	spec.InputModalities[0] = "image"
	cand.Routes[0].Upstreams["acct"] = spec
	cand.Routes[0].Upstreams["ghost"] = spec

	if ids := modelIDs(t, e.do("GET", "/v1/models", keyA, "").body); len(ids) != 1 || ids[0] != "client-model" {
		t.Fatalf("served models after caller mutation = %v, want [client-model]", ids)
	}
	if r := e.responses(keyA, "client-model"); r.status != http.StatusOK {
		t.Fatalf("request after caller mutation status %d: %s", r.status, r.body)
	}
	hits := up.hitsCopy()
	if len(hits) != 1 {
		t.Fatalf("upstream hits = %d, want 1", len(hits))
	}
	if got := capUpstreamModel(t, hits[0].Body); got != "backend-a" {
		t.Fatalf("upstream model after caller mutation = %q, want backend-a (clone must isolate)", got)
	}
}

// TestCoreRoutesDescriptorProjectionIsDeepCopy checks that config.CoreRoutes
// builds the runtime route with its own maps and slices: mutating the returned
// projection cannot reach back into the loaded config's model list, candidate
// list or descriptor slices.
func TestCoreRoutesDescriptorProjectionIsDeepCopy(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	hrWriteFile(t, cfgPath, capRouteYAML("http://127.0.0.1:1", "a.key", "client-model", "backend-a"))

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	routes := cfg.CoreRoutes()
	if len(routes) != 1 {
		t.Fatalf("projected routes = %d, want 1", len(routes))
	}
	got, ok := routes[0].Upstreams["acct"]
	if !ok || got.UpstreamModel != "backend-a" {
		t.Fatalf("projected descriptor = %+v (present=%v)", got, ok)
	}

	// Mutate the projection across the descriptor's slice fields and the
	// route's own model/candidate slices.
	got.UpstreamModel = "MUTATED"
	got.Protocols[0] = "chat"
	got.InputModalities[0] = "image"
	routes[0].Upstreams["acct"] = got
	routes[0].Models[0] = "MUTATED"
	routes[0].Interactive[0] = "MUTATED"

	if cfg.Routes[0].Models[0] != "client-model" || cfg.Routes[0].Interactive[0] != "acct" {
		t.Fatalf("projection aliased config slices: models=%v interactive=%v", cfg.Routes[0].Models, cfg.Routes[0].Interactive)
	}
	src := cfg.Routes[0].Upstreams["acct"]
	if src.UpstreamModel != "backend-a" || src.Protocols[0] != "responses" || src.InputModalities[0] != "text" {
		t.Fatalf("projection aliased descriptor: %+v", src)
	}
}
