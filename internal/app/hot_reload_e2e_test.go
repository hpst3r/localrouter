package app_test

// Additional chunk2 end-to-end acceptance tests for hot reload, driven through
// the real App HTTP surface (App.Handler + httptest) with a fake upstream.
//
// Scope: this file only. It exercises the atomic single-pointer publication of
// a runtime generation by observing, from outside the process, that
//   * an in-flight request admitted under generation N keeps generation N's
//     frozen pricing and keeps its concurrency slot even after a reload, while
//     requests admitted after the reload use the new pricing and the new limits;
//   * a request refused by the newly published (smaller) limit is rejected with
//     the concurrency error while the previously admitted request is untouched;
//   * under concurrent reloads, client authentication, routing and the served
//     handler always come from the same generation (no cross-generation tear).
//
// The upstream is gated with a one-shot channel so a request can be held
// mid-stream across a reload; all goroutines are finite, all client calls carry
// deadlines, and the gate is always closed before the test returns.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/app"
	"github.com/hpst3r/localrouter/internal/config"
	"github.com/hpst3r/localrouter/internal/core"
)

// The upstream API key is a synthetic placeholder supplied through the
// environment (never written into a config file). Key material in this file is
// fake fixture data, not a real credential.
const (
	hrUpstreamKey = "hr-upstream-key-000000000000"
	hrPricedModel = "hr-priced"
)

type hrHit struct {
	Path string
	Auth string
	Body []byte
}

// hrUpstream is a fake openai_compat upstream. It records every hit and can
// hold exactly one request open on a gate so a test can reload while that
// request is in flight.
type hrUpstream struct {
	srv *httptest.Server

	mu      sync.Mutex
	hits    []hrHit
	gate    chan struct{} // if non-nil, the next request blocks until closed
	arrived chan struct{} // signalled once when a request reaches the upstream
	usageIn int64
}

func hrNewUpstream(t *testing.T) *hrUpstream {
	t.Helper()
	u := &hrUpstream{arrived: make(chan struct{}, 1), usageIn: 1000}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		u.mu.Lock()
		gate := u.gate
		u.hits = append(u.hits, hrHit{
			Path: r.URL.Path,
			Auth: r.Header.Get("Authorization"),
			Body: body,
		})
		in := u.usageIn
		u.mu.Unlock()

		select {
		case u.arrived <- struct{}{}:
		default:
		}
		if gate != nil {
			<-gate // released by the test before it returns
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":    "hr-resp",
			"object": "response",
			"usage": map[string]any{
				"input_tokens":  in,
				"output_tokens": 0,
			},
		})
	})
	u.srv = httptest.NewServer(mux)
	t.Cleanup(u.srv.Close)
	return u
}

func (u *hrUpstream) arm() { u.mu.Lock(); u.gate = make(chan struct{}); u.mu.Unlock() }

func (u *hrUpstream) release() {
	u.mu.Lock()
	g := u.gate
	u.gate = nil
	u.mu.Unlock()
	if g != nil {
		close(g)
	}
}

func (u *hrUpstream) hitsCopy() []hrHit {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]hrHit(nil), u.hits...)
}

func hrWriteFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// hrConfigYAML renders a minimal config: one openai_compat account, one
// interactive client whose key file is swappable, one route whose model list is
// the reload marker, and a reloadable max_concurrent limit.
func hrConfigYAML(upstreamURL, keyFileName, model string, maxConc int) string {
	return fmt.Sprintf(`listen: 127.0.0.1:0
data_dir: data
pricing_file: pricing.yaml
control: {require_auth: true}
limits: {max_concurrent: %d}
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
`, maxConc, keyFileName, upstreamURL, model)
}

func hrPricingYAML(input float64) string {
	return fmt.Sprintf("models:\n  %s:\n    input: %g\n    output: 0\n", hrPricedModel, input)
}

type hrResult struct {
	status int
	header http.Header
	body   []byte
}

type hrEnv struct {
	t       *testing.T
	dir     string
	cfgPath string
	up      *hrUpstream
	a       *app.App
	srv     *httptest.Server
	client  *http.Client
}

func hrBuild(t *testing.T, dir, cfgPath string, up *hrUpstream) *hrEnv {
	t.Helper()
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	a, err := app.Build(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), app.Overrides{})
	if err != nil {
		t.Fatalf("build app: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })
	e := &hrEnv{
		t: t, dir: dir, cfgPath: cfgPath, up: up, a: a,
		srv: httptest.NewServer(a.Handler),
		// Every client call is bounded so a stuck path fails fast.
		client: &http.Client{Timeout: 20 * time.Second},
	}
	t.Cleanup(e.srv.Close)
	return e
}

func (e *hrEnv) doRaw(method, path, key, body string) (hrResult, error) {
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, e.srv.URL+path, rd)
	if err != nil {
		return hrResult{}, err
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	if rd != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return hrResult{}, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return hrResult{}, err
	}
	return hrResult{status: resp.StatusCode, header: resp.Header, body: b}, nil
}

func (e *hrEnv) do(method, path, key string, body string) hrResult {
	e.t.Helper()
	r, err := e.doRaw(method, path, key, body)
	if err != nil {
		e.t.Fatal(err)
	}
	return r
}

func (e *hrEnv) responses(key, model string) hrResult {
	return e.do("POST", "/v1/responses", key, fmt.Sprintf(`{"model":%q,"input":"hi"}`, model))
}

func (e *hrEnv) summary() map[string]core.UsageRow {
	e.t.Helper()
	rows, err := e.a.Ledger.Summary(context.Background(), time.Now().Add(-time.Hour), "account")
	if err != nil {
		e.t.Fatal(err)
	}
	out := map[string]core.UsageRow{}
	for _, r := range rows {
		out[r.Key] = r
	}
	return out
}

type hrDiag struct {
	Ready    bool `json:"ready"`
	Inflight struct {
		GlobalLimit  int `json:"global_limit"`
		GlobalActive int `json:"global_active"`
		GlobalPeak   int `json:"global_peak"`
	} `json:"inflight"`
	Reload *struct {
		Generation uint64 `json:"generation"`
		OK         bool   `json:"ok"`
	} `json:"reload"`
}

func (e *hrEnv) diagnostics(key string) hrDiag {
	e.t.Helper()
	r := e.do("GET", "/control/v1/diagnostics", key, "")
	if r.status != 200 {
		e.t.Fatalf("diagnostics: %d %s", r.status, r.body)
	}
	var d hrDiag
	if err := json.Unmarshal(r.body, &d); err != nil {
		e.t.Fatalf("diagnostics decode: %v (%s)", err, r.body)
	}
	return d
}

// hrWaitActive polls diagnostics until the global active count reaches want.
func (e *hrEnv) hrWaitActive(key string, want int) {
	e.t.Helper()
	if err := e.hrWaitActiveRaw(key, want); err != nil {
		e.t.Fatal(err)
	}
}

// hrWaitActiveRaw is the goroutine-safe form of hrWaitActive.
func (e *hrEnv) hrWaitActiveRaw(key string, want int) error {
	deadline := time.Now().Add(3 * time.Second)
	for {
		r, err := e.doRaw("GET", "/control/v1/diagnostics", key, "")
		if err != nil {
			return err
		}
		if r.status != 200 {
			return fmt.Errorf("diagnostics: %d %s", r.status, r.body)
		}
		var d hrDiag
		if err := json.Unmarshal(r.body, &d); err != nil {
			return err
		}
		if d.Inflight.GlobalActive == want {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("global_active never reached %d", want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// modelIDs extracts the id list from a GET /v1/models body.
func modelIDs(t *testing.T, body []byte) []string {
	t.Helper()
	ids, err := modelIDsRaw(body)
	if err != nil {
		t.Fatalf("models decode: %v (%s)", err, body)
	}
	return ids
}

// modelIDsRaw is the error-returning form safe to call from a goroutine.
func modelIDsRaw(body []byte) ([]string, error) {
	var doc struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(doc.Data))
	for _, d := range doc.Data {
		out = append(out, d.ID)
	}
	return out, nil
}

// TestHotReloadOldStreamKeepsOldPricingNewRequestsUseNewPricing holds one
// request mid-stream under generation 1's pricing and key, then rewrites the
// pricing file and the client key file and reloads. The in-flight request must
// still complete and be costed at the OLD price; a request admitted after the
// reload is costed at the NEW price; a request presenting the removed key is
// rejected and does not disturb the stream; the aggregated ledger cost is the
// exact per-generation sum.
func TestHotReloadOldStreamKeepsOldPricingNewRequestsUseNewPricing(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HR_UPSTREAM_KEY", hrUpstreamKey)
	keyA := "hr-client-keyA-0123456789"
	keyB := "hr-client-keyB-0123456789"
	hrWriteFile(t, filepath.Join(dir, "a.key"), keyA)
	hrWriteFile(t, filepath.Join(dir, "b.key"), keyB)
	hrWriteFile(t, filepath.Join(dir, "pricing.yaml"), hrPricingYAML(1.0)) // $1 / 1M input

	up := hrNewUpstream(t)
	cfgPath := filepath.Join(dir, "config.yaml")
	hrWriteFile(t, cfgPath, hrConfigYAML(up.srv.URL, "a.key", hrPricedModel, 4))
	e := hrBuild(t, dir, cfgPath, up)

	if g := e.a.ReloadStatus().Generation; g != 1 {
		t.Fatalf("initial generation = %d", g)
	}

	// Hold request #1 (old generation) mid-stream.
	up.arm()
	type done struct {
		r hrResult
	}
	old := make(chan done, 1)
	go func() { old <- done{e.responses(keyA, hrPricedModel)} }()
	select {
	case <-up.arrived:
	case <-time.After(3 * time.Second):
		t.Fatal("gated request never reached the upstream")
	}
	e.hrWaitActive(keyA, 1)

	// Change the price file and the client key file, then reload (no restart
	// fields: same paths, same accounts, same limits).
	hrWriteFile(t, filepath.Join(dir, "pricing.yaml"), hrPricingYAML(3.0)) // $3 / 1M input
	hrWriteFile(t, cfgPath, hrConfigYAML(up.srv.URL, "b.key", hrPricedModel, 4))
	st, err := e.a.Reload(cfgPath)
	if err != nil || !st.OK || st.Generation != 2 {
		t.Fatalf("reload = %#v, %v", st, err)
	}

	// The removed key must no longer authenticate.
	if r := e.responses(keyA, hrPricedModel); r.status != 401 {
		t.Fatalf("removed key accepted: %d %s", r.status, r.body)
	}

	// Release the held stream; it is still generation 1 and must succeed.
	up.release()
	var first hrResult
	select {
	case res := <-old:
		first = res.r
	case <-time.After(5 * time.Second):
		t.Fatal("held request never completed")
	}
	if first.status != 200 {
		t.Fatalf("held stream status %d: %s", first.status, first.body)
	}

	// A request admitted after the reload uses the new key and new pricing.
	if r := e.responses(keyB, hrPricedModel); r.status != 200 {
		t.Fatalf("new-key request status %d: %s", r.status, r.body)
	}

	sum := e.summary()["acct"]
	if sum.Requests != 2 {
		t.Fatalf("ledger requests = %d, want 2", sum.Requests)
	}
	// 1000 input tokens at $1/1M (old) + $3/1M (new).
	const want = (1000*1.0 + 1000*3.0) / 1e6
	if sum.CostUSD == nil {
		t.Fatalf("aggregate cost is nil")
	}
	if got := *sum.CostUSD; got < want-1e-12 || got > want+1e-12 {
		t.Fatalf("aggregate cost = %v, want %v (old price must apply to the held stream)", got, want)
	}
	// The old stream must have been attributed to the account it was admitted on.
	hits := up.hitsCopy()
	if len(hits) != 2 {
		t.Fatalf("upstream hits = %d, want 2 (the removed-key probe must not reach upstream)", len(hits))
	}
	for i, hit := range hits {
		if !strings.HasPrefix(hit.Auth, "Bearer ") || strings.Contains(hit.Auth, keyA) || strings.Contains(hit.Auth, keyB) {
			t.Fatalf("hit %d upstream auth not the account credential: %q", i, hit.Auth)
		}
	}
}

// TestHotReloadCapacityChangeSeesOldActiveRequest arms a stream under a loose
// limit, shrinks the limit via reload, and checks that a freshly admitted
// request is refused by the new limit while the already-active request keeps
// its slot (shared counters are not rebuilt) and the slot is reusable after it
// completes.
func TestHotReloadCapacityChangeSeesOldActiveRequest(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HR_UPSTREAM_KEY", hrUpstreamKey)
	keyA := "hr-client-keyA-0123456789"
	hrWriteFile(t, filepath.Join(dir, "a.key"), keyA)
	hrWriteFile(t, filepath.Join(dir, "pricing.yaml"), hrPricingYAML(1.0))

	up := hrNewUpstream(t)
	cfgPath := filepath.Join(dir, "config.yaml")
	hrWriteFile(t, cfgPath, hrConfigYAML(up.srv.URL, "a.key", hrPricedModel, 3))
	e := hrBuild(t, dir, cfgPath, up)

	up.arm()
	old := make(chan hrResult, 1)
	go func() { old <- e.responses(keyA, hrPricedModel) }()
	select {
	case <-up.arrived:
	case <-time.After(3 * time.Second):
		t.Fatal("gated request never reached the upstream")
	}
	e.hrWaitActive(keyA, 1)

	// Shrink the reloadable limit to 1 and publish generation 2.
	hrWriteFile(t, cfgPath, hrConfigYAML(up.srv.URL, "a.key", hrPricedModel, 1))
	if st, err := e.a.Reload(cfgPath); err != nil || !st.OK || st.Generation != 2 {
		t.Fatalf("reload = %#v, %v", st, err)
	}

	// Diagnostics now report the new limit against the shared live count: the
	// old request still occupies one slot.
	d := e.diagnostics(keyA)
	if d.Inflight.GlobalLimit != 1 || d.Inflight.GlobalActive != 1 {
		t.Fatalf("inflight after reload = %+v, want limit 1 active 1", d.Inflight)
	}

	// A new request is refused by the newly published limit.
	r := e.responses(keyA, hrPricedModel)
	if r.status != http.StatusTooManyRequests {
		t.Fatalf("new request status %d, want 429: %s", r.status, r.body)
	}
	if !strings.Contains(string(r.body), "concurrency_limit_exceeded") {
		t.Fatalf("429 body did not name the concurrency limit: %s", r.body)
	}
	if got := r.header.Get("Retry-After"); got == "" {
		t.Fatalf("concurrency rejection missing Retry-After")
	}

	// The request admitted before the reload is untouched and completes.
	up.release()
	var first hrResult
	select {
	case first = <-old:
	case <-time.After(5 * time.Second):
		t.Fatal("held request never completed")
	}
	if first.status != 200 {
		t.Fatalf("held request status %d: %s", first.status, first.body)
	}
	e.hrWaitActive(keyA, 0)

	// The freed slot is reusable under generation 2's limit of 1.
	if r := e.responses(keyA, hrPricedModel); r.status != 200 {
		t.Fatalf("post-release request status %d: %s", r.status, r.body)
	}
	d = e.diagnostics(keyA)
	if d.Inflight.GlobalActive != 0 || d.Inflight.GlobalPeak < 1 {
		t.Fatalf("inflight settled state = %+v", d.Inflight)
	}
	if d.Reload == nil || d.Reload.Generation != 2 || !d.Reload.OK {
		t.Fatalf("diagnostics reload block = %+v", d.Reload)
	}
}

// TestHotReloadConcurrentAtomicAuthRoutes alternates two valid configurations
// (each with its own client key AND its own route model) while many workers
// fetch /v1/models with either key. Because auth, routing and the serving
// handler are all published by one atomic pointer, a request that authenticates
// under a key must see exactly that key's generation's model list — never a
// mixed view. The reload generation must advance monotonically.
func TestHotReloadConcurrentAtomicAuthRoutes(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HR_UPSTREAM_KEY", hrUpstreamKey)
	keyA := "hr-client-keyA-0123456789"
	keyB := "hr-client-keyB-0123456789"
	hrWriteFile(t, filepath.Join(dir, "a.key"), keyA)
	hrWriteFile(t, filepath.Join(dir, "b.key"), keyB)
	// Both markers are priced so no request is accidentally unpriced; the
	// /v1/models surface does not depend on pricing, but keep the configs valid.
	hrWriteFile(t, filepath.Join(dir, "pricing.yaml"), hrPricingYAML(1.0))

	up := hrNewUpstream(t)
	pathA := filepath.Join(dir, "config.yaml")
	pathB := filepath.Join(dir, "config-b.yaml")
	hrWriteFile(t, pathA, hrConfigYAML(up.srv.URL, "a.key", "marker-A", 4))
	hrWriteFile(t, pathB, hrConfigYAML(up.srv.URL, "b.key", "marker-B", 4))

	cfgB, err := config.Load(pathB)
	if err != nil {
		t.Fatalf("load B: %v", err)
	}
	e := hrBuild(t, dir, pathA, up)

	type observation struct {
		key, model string
		status     int
	}
	const workers = 6
	const perWorker = 25
	const reloads = 60

	var (
		mu      sync.Mutex
		obs     []observation
		gens    []uint64
		errs    []string
		reloadE error
		wg      sync.WaitGroup
	)
	// Reloader: alternate A and B (both valid) until it has published reloads
	// generations. Generation numbering is strictly serialized by ReloadConfig.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < reloads; i++ {
			c := cfgB
			if i%2 == 1 {
				cfgA, err := config.Load(pathA)
				if err != nil {
					mu.Lock()
					reloadE = err
					mu.Unlock()
					return
				}
				c = cfgA
			}
			st, err := e.a.ReloadConfig(c)
			mu.Lock()
			if err != nil || !st.OK {
				if reloadE == nil {
					reloadE = fmt.Errorf("reload %d: %#v %v", i, st, err)
				}
			} else {
				gens = append(gens, st.Generation)
			}
			mu.Unlock()
		}
	}()

	// Workers alternate keys so both generations are exercised simultaneously.
	// They never call t.Fatal/t.Errorf (only the test goroutine may); problems
	// are recorded and asserted after wg.Wait.
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				keyAUsed := (w+i)%2 == 0
				key, want := keyB, "marker-B"
				if keyAUsed {
					key, want = keyA, "marker-A"
				}
				r, err := e.doRaw("GET", "/v1/models", key, "")
				var local []string
				if err != nil {
					local = append(local, fmt.Sprintf("request error: %v", err))
				} else if r.status == 200 {
					ids, derr := modelIDsRaw(r.body)
					if derr != nil {
						local = append(local, fmt.Sprintf("models decode: %v", derr))
					} else if len(ids) != 1 {
						local = append(local, fmt.Sprintf("200 listed %d models %v", len(ids), ids))
					} else if ids[0] != want {
						local = append(local, fmt.Sprintf("key %s served model %q, want %q (cross-generation tear)", key, ids[0], want))
					} else {
						mu.Lock()
						obs = append(obs, observation{key: key, model: ids[0], status: 200})
						mu.Unlock()
					}
				} else if r.status != 401 {
					local = append(local, fmt.Sprintf("unexpected status %d: %s", r.status, r.body))
				}
				if len(local) > 0 {
					mu.Lock()
					errs = append(errs, local...)
					mu.Unlock()
				}
			}
		}(w)
	}
	wg.Wait()

	mu.Lock()
	problems := append([]string(nil), errs...)
	genCopy := append([]uint64(nil), gens...)
	obsCopy := append([]observation(nil), obs...)
	re := reloadE
	mu.Unlock()

	if re != nil {
		t.Fatalf("reload error: %v", re)
	}
	for _, p := range problems {
		t.Errorf("%s", p)
	}
	// Generation must advance monotonically, one per successful reload.
	for i := 1; i < len(genCopy); i++ {
		if genCopy[i] != genCopy[i-1]+1 {
			t.Fatalf("generations not monotonic: %v", genCopy)
		}
	}
	if len(genCopy) != reloads {
		t.Fatalf("successful reloads = %d, want %d", len(genCopy), reloads)
	}

	// Every 200 must have carried exactly its own key's model.
	var sawA, sawB bool
	for _, o := range obsCopy {
		switch o.key {
		case keyA:
			if o.model != "marker-A" {
				t.Fatalf("keyA served %q", o.model)
			}
			sawA = true
		case keyB:
			if o.model != "marker-B" {
				t.Fatalf("keyB served %q", o.model)
			}
			sawB = true
		}
	}
	if !sawA || !sawB {
		t.Fatalf("concurrent window did not accept both keys (A=%v B=%v)", sawA, sawB)
	}

	// Deterministic tail: pin each generation and confirm the coupling once more.
	if st, err := e.a.ReloadConfig(cfgB); err != nil || !st.OK {
		t.Fatalf("pin B: %#v %v", st, err)
	}
	if ids := modelIDs(t, e.do("GET", "/v1/models", keyB, "").body); len(ids) != 1 || ids[0] != "marker-B" {
		t.Fatalf("pinned B models = %v", ids)
	}
	if r := e.do("GET", "/v1/models", keyA, ""); r.status != 401 {
		t.Fatalf("keyA accepted while B pinned: %d", r.status)
	}
	cfgA, err := config.Load(pathA)
	if err != nil {
		t.Fatalf("load A: %v", err)
	}
	if st, err := e.a.ReloadConfig(cfgA); err != nil || !st.OK {
		t.Fatalf("pin A: %#v %v", st, err)
	}
	if ids := modelIDs(t, e.do("GET", "/v1/models", keyA, "").body); len(ids) != 1 || ids[0] != "marker-A" {
		t.Fatalf("pinned A models = %v", ids)
	}
	if r := e.do("GET", "/v1/models", keyB, ""); r.status != 401 {
		t.Fatalf("keyB accepted while A pinned: %d", r.status)
	}
}
