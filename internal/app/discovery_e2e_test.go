package app_test

// Model-discovery acceptance tests (docs/SPEC.md "Model discovery", D3-D5,
// D9-D12) through the real App HTTP surface. Every upstream is one
// httptest.Server reached over loopback: each account is a path prefix on it
// (/lab, /two, /cloud) with its own /models listing and inference status.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/app"
	"github.com/hpst3r/localrouter/internal/config"
)

// llamaRouterModels is a llama.cpp router-mode GET /v1/models fixture: two ids,
// each with the extra status/architecture fields discovery must ignore.
const llamaRouterModels = `{"object":"list","data":[
 {"id":"unsloth/Qwen3-8B-GGUF:Q4_K_M","object":"model","owned_by":"llamacpp","created":1760000000,
  "status":{"value":"loaded","args":["--ctx-size","8192"]},"architecture":{"modality":"text->text","input_modalities":["text"]}},
 {"id":"ggml-org/gemma-3-1b-it-GGUF","object":"model","owned_by":"llamacpp","created":1760000001,
  "status":{"value":"unloaded"},"architecture":{"modality":"text+image->text"}}]}`

type discHit struct {
	path string
	body []byte
}

// discUpstream fakes several accounts behind one server, keyed by path prefix.
type discUpstream struct {
	srv *httptest.Server

	mu        sync.Mutex
	listings  map[string]string // prefix -> /models body
	listCalls map[string]int    // prefix -> /models calls
	infStatus map[string]int    // prefix -> inference status (0 = 200)
	hits      []discHit         // inference hits
	auth      map[string]string // prefix -> last /models Authorization
}

func newDiscUpstream(t *testing.T) *discUpstream {
	u := &discUpstream{listings: map[string]string{}, listCalls: map[string]int{}, infStatus: map[string]int{}, auth: map[string]string{}}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		prefix, rest, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
		if rest == "v1/models" {
			u.mu.Lock()
			u.listCalls[prefix]++
			u.auth[prefix] = r.Header.Get("Authorization")
			body, ok := u.listings[prefix]
			u.mu.Unlock()
			if !ok {
				http.Error(w, "no listing", http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, body)
			return
		}
		b, _ := io.ReadAll(r.Body)
		u.mu.Lock()
		u.hits = append(u.hits, discHit{path: r.URL.Path, body: b})
		st := u.infStatus[prefix]
		u.mu.Unlock()
		if st != 0 {
			http.Error(w, `{"error":{"message":"down"}}`, st)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"r","object":"chat.completion","usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	t.Cleanup(u.srv.Close)
	return u
}

func (u *discUpstream) setListing(prefix string, ids ...string) {
	data := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		data = append(data, map[string]any{"id": id, "object": "model", "status": map[string]any{"value": "loaded"}})
	}
	b, _ := json.Marshal(map[string]any{"object": "list", "data": data})
	u.mu.Lock()
	u.listings[prefix] = string(b)
	u.mu.Unlock()
}

func (u *discUpstream) setRaw(prefix, body string) {
	u.mu.Lock()
	u.listings[prefix] = body
	u.mu.Unlock()
}

func (u *discUpstream) setStatus(prefix string, st int) {
	u.mu.Lock()
	u.infStatus[prefix] = st
	u.mu.Unlock()
}

func (u *discUpstream) calls(prefix string) int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.listCalls[prefix]
}

func (u *discUpstream) takeHits() []discHit {
	u.mu.Lock()
	defer u.mu.Unlock()
	h := u.hits
	u.hits = nil
	return h
}

// discConfigYAML: a static cloud route, then two discovering routes. "lab" only
// admits llama.cpp-style and m-* ids, so "duo/*" ids reach route "duo".
func discConfigYAML(upstreamURL string) string {
	return fmt.Sprintf(`listen: 127.0.0.1:0
data_dir: data
policy: {max_failovers: 2}
clients:
  - {name: me, class: interactive, key_file: me.key}
accounts:
  - id: ollama-cloud
    provider: ollama
    base_url: %[1]s/cloud/v1
    api_key_env: HR_UPSTREAM_KEY
  - id: llama-lab
    provider: openai_compat
    base_url: %[1]s/lab/v1
    api_key_env: HR_UPSTREAM_KEY
  - id: llama-two
    provider: openai_compat
    base_url: %[1]s/two/v1
    api_key_env: HR_UPSTREAM_KEY
routes:
  - name: cloud
    models: [static-m]
    interactive: [ollama-cloud]
  - name: lab
    discover_models: true
    discover_match: ["unsloth/*", "ggml-org/*", "m-*"]
    interactive: [llama-lab]
    background: [llama-lab]
  - name: duo
    discover_models: true
    discover_interval: 1m
    discover_match: ["duo/*"]
    interactive: [llama-lab, llama-two]
`, upstreamURL)
}

type discEnv struct {
	t   *testing.T
	up  *discUpstream
	a   *app.App
	srv *httptest.Server
	dir string
}

func newDiscEnv(t *testing.T, up *discUpstream, cfgYAML string, start bool) *discEnv {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HR_UPSTREAM_KEY", hrUpstreamKey)
	hrWriteFile(t, filepath.Join(dir, "me.key"), goldenKey)
	cfgPath := filepath.Join(dir, "config.yaml")
	hrWriteFile(t, cfgPath, cfgYAML)
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	a, err := app.Build(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), app.Overrides{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })
	e := &discEnv{t: t, up: up, a: a, srv: httptest.NewServer(a.Handler), dir: dir}
	t.Cleanup(e.srv.Close)
	if start {
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		a.Start(ctx)
	}
	return e
}

func (e *discEnv) do(method, path, body string) (int, []byte) {
	e.t.Helper()
	st, b, err := e.doRaw(method, path, body)
	if err != nil {
		e.t.Fatal(err)
	}
	return st, b
}

func (e *discEnv) doRaw(method, path, body string) (int, []byte, error) {
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, _ := http.NewRequest(method, e.srv.URL+path, rd)
	req.Header.Set("Authorization", "Bearer "+goldenKey)
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return resp.StatusCode, b, err
}

func (e *discEnv) models() []string {
	_, b := e.do("GET", "/v1/models", "")
	return modelIDs(e.t, b)
}

// waitModels waits until /v1/models lists every id in want.
func (e *discEnv) waitModels(want ...string) {
	e.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got := e.models()
		ok := true
		for _, w := range want {
			ok = ok && slices.Contains(got, w)
		}
		if ok {
			return
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("/v1/models = %v, want it to include %v", got, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func chatBody(model string) string {
	return fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}],"temperature":0.2}`, model)
}

// D3: a llama.cpp router listing makes both ids appear in /v1/models and route
// to the account; the forwarded body (and so its model) is byte-identical.
func TestDiscoveryD3LlamaRouterListing(t *testing.T) {
	up := newDiscUpstream(t)
	up.setRaw("lab", llamaRouterModels)
	e := newDiscEnv(t, up, discConfigYAML(up.srv.URL), true)
	e.waitModels("unsloth/Qwen3-8B-GGUF:Q4_K_M", "ggml-org/gemma-3-1b-it-GGUF")
	if got, want := e.models(), []string{"ggml-org/gemma-3-1b-it-GGUF", "static-m", "unsloth/Qwen3-8B-GGUF:Q4_K_M"}; !slices.Equal(got, want) {
		t.Fatalf("/v1/models = %v, want %v", got, want)
	}
	up.mu.Lock()
	a := up.auth["lab"]
	up.mu.Unlock()
	if a != "Bearer "+hrUpstreamKey {
		t.Fatalf("/models Authorization = %q, want the account bearer", a)
	}
	for _, id := range []string{"unsloth/Qwen3-8B-GGUF:Q4_K_M", "ggml-org/gemma-3-1b-it-GGUF"} {
		body := chatBody(id)
		st, resp := e.do("POST", "/v1/chat/completions", body)
		if st != http.StatusOK {
			t.Fatalf("%s: status %d %s", id, st, resp)
		}
		hits := up.takeHits()
		if len(hits) != 1 || hits[0].path != "/lab/v1/chat/completions" {
			t.Fatalf("%s: hits = %+v, want one on /lab", id, hits)
		}
		if string(hits[0].body) != body {
			t.Fatalf("%s: forwarded body %s, want byte-identical %s", id, hits[0].body, body)
		}
	}
}

// D5: a discovering route whose only candidate is down never fails over
// outside the route, and an unknown model never falls through to a
// non-discovering route.
func TestDiscoveryD5FailClosed(t *testing.T) {
	up := newDiscUpstream(t)
	up.setListing("lab", "m-local")
	up.setListing("cloud", "m-local", "nope") // never consulted: cloud does not discover
	up.setStatus("lab", http.StatusServiceUnavailable)
	e := newDiscEnv(t, up, discConfigYAML(up.srv.URL), true)
	e.waitModels("m-local")

	st, body := e.do("POST", "/v1/chat/completions", chatBody("m-local"))
	if st != http.StatusServiceUnavailable {
		t.Fatalf("m-local: status %d %s, want the lab 503 relayed", st, body)
	}
	for _, h := range up.takeHits() {
		if !strings.HasPrefix(h.path, "/lab/") {
			t.Fatalf("down discovering route failed over outside the route: %s", h.path)
		}
	}
	st, body = e.do("POST", "/v1/chat/completions", chatBody("nope"))
	if st != http.StatusNotFound || !strings.Contains(string(body), "model_not_found") {
		t.Fatalf("unknown model: %d %s, want 404 model_not_found", st, body)
	}
	if h := up.takeHits(); len(h) != 0 {
		t.Fatalf("unknown model reached an upstream: %+v", h)
	}
	if n := up.calls("cloud"); n != 0 {
		t.Fatalf("non-discovering account polled %d times", n)
	}
}

// D9: a model that appears upstream after the last poll resolves on the first
// request; concurrent misses share at most one /models call per window.
func TestDiscoveryD9OnDemandRefresh(t *testing.T) {
	up := newDiscUpstream(t)
	up.setListing("lab", "m-early")
	up.setListing("two")
	e := newDiscEnv(t, up, discConfigYAML(up.srv.URL), true)
	e.waitModels("m-early")
	before := up.calls("lab")

	up.setListing("lab", "m-early", "m-late")
	st, body := e.do("POST", "/v1/chat/completions", chatBody("m-late"))
	if st != http.StatusOK {
		t.Fatalf("m-late on first request: %d %s, want 200 after on-demand refresh", st, body)
	}
	if got := up.calls("lab") - before; got != 1 {
		t.Fatalf("on-demand refresh made %d /models calls, want 1", got)
	}

	// The refresh window is now spent: 100 concurrent misses add no call.
	before = up.calls("lab")
	var wg sync.WaitGroup
	errs := make(chan string, 100)
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			st, b, err := e.doRaw("POST", "/v1/chat/completions", chatBody("m-ghost"))
			if err != nil || st != http.StatusNotFound {
				errs <- fmt.Sprintf("%d %s %v", st, b, err)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for s := range errs {
		t.Fatalf("ghost miss: %s", s)
	}
	if got := up.calls("lab") - before; got > 1 {
		t.Fatalf("100 concurrent misses made %d /models calls, want at most 1", got)
	}
}

// D9 (cold): 100 concurrent misses on a fresh router cause exactly one
// on-demand /models call per account.
func TestDiscoveryD9ConcurrentMissesSingleFlight(t *testing.T) {
	up := newDiscUpstream(t)
	up.setListing("lab", "m-early")
	up.setListing("two")
	e := newDiscEnv(t, up, discConfigYAML(up.srv.URL), false) // no background poller
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, _ = e.doRaw("POST", "/v1/chat/completions", chatBody("m-ghost"))
		}()
	}
	wg.Wait()
	if got := up.calls("lab"); got != 1 {
		t.Fatalf("100 concurrent misses made %d /models calls to lab, want exactly 1", got)
	}
	if got := up.calls("two"); got != 1 {
		t.Fatalf("100 concurrent misses made %d /models calls to two, want exactly 1", got)
	}
}

type admitDoc struct {
	Decision  string `json:"decision"`
	AccountID string `json:"account_id"`
	Reason    string `json:"reason"`
}

// D4 + D11: on a two-account discovering route each model is served only by
// the account that lists it, and check_admission resolves the same route and
// narrowed candidates as the proxy.
func TestDiscoveryD4D11NarrowedCandidates(t *testing.T) {
	up := newDiscUpstream(t)
	up.setListing("lab", "duo/a", "duo/both")
	up.setListing("two", "duo/b", "duo/both")
	e := newDiscEnv(t, up, discConfigYAML(up.srv.URL), true)
	e.waitModels("duo/a", "duo/b", "duo/both")

	for _, c := range []struct{ model, acct, prefix string }{
		{"duo/a", "llama-lab", "/lab/"}, {"duo/b", "llama-two", "/two/"}, {"duo/both", "llama-lab", "/lab/"},
	} {
		_, b := e.do("POST", "/control/v1/admit", fmt.Sprintf(`{"class":"interactive","model":%q}`, c.model))
		var d admitDoc
		if err := json.Unmarshal(b, &d); err != nil || d.Decision != "allow" || d.AccountID != c.acct {
			t.Fatalf("admit %s = %s, want allow on %s", c.model, b, c.acct)
		}
		st, body := e.do("POST", "/v1/chat/completions", chatBody(c.model))
		hits := up.takeHits()
		if st != http.StatusOK || len(hits) != 1 || !strings.HasPrefix(hits[0].path, c.prefix) {
			t.Fatalf("%s: %d %s hits %+v, want served by %s", c.model, st, body, hits, c.acct)
		}
	}

	// duo/b is listed only by llama-two: with it down the request must not
	// fail over to llama-lab (outside the narrowed list).
	up.setStatus("two", http.StatusServiceUnavailable)
	if st, _ := e.do("POST", "/v1/chat/completions", chatBody("duo/b")); st != http.StatusServiceUnavailable {
		t.Fatalf("duo/b with two down: status %d, want 503", st)
	}
	for _, h := range up.takeHits() {
		if !strings.HasPrefix(h.path, "/two/") {
			t.Fatalf("duo/b failed over outside its listing accounts: %s", h.path)
		}
	}
	// duo/both: two is down... and lab lists it first anyway; with lab down
	// failover stays within lab -> two in configured order.
	up.setStatus("two", 0)
	up.setStatus("lab", http.StatusServiceUnavailable)
	if st, b := e.do("POST", "/v1/chat/completions", chatBody("duo/both")); st != http.StatusOK {
		t.Fatalf("duo/both with lab down: %d %s, want failover to two", st, b)
	}
	var paths []string
	for _, h := range up.takeHits() {
		paths = append(paths, h.path)
	}
	if want := []string{"/lab/v1/chat/completions", "/two/v1/chat/completions"}; !slices.Equal(paths, want) {
		t.Fatalf("duo/both failover order %v, want %v", paths, want)
	}
}

// D12: the discovery block appears only with a discovering route and never
// contains URLs, keys or upstream bodies.
func TestDiscoveryD12Diagnostics(t *testing.T) {
	up := newDiscUpstream(t)
	up.setRaw("lab", llamaRouterModels)
	up.setListing("two", "duo/b")
	e := newDiscEnv(t, up, discConfigYAML(up.srv.URL), true)
	e.waitModels("unsloth/Qwen3-8B-GGUF:Q4_K_M", "duo/b")
	_, b := e.do("GET", "/control/v1/diagnostics", "")
	var doc struct {
		Discovery *struct {
			Accounts []struct {
				Account       string   `json:"account"`
				Routes        []string `json:"routes"`
				LastSuccessAt *string  `json:"last_success_at"`
				ModelCount    int      `json:"model_count"`
			} `json:"accounts"`
			Routes []struct {
				Route  string   `json:"route"`
				Models []string `json:"models"`
			} `json:"routes"`
		} `json:"discovery"`
	}
	if err := json.Unmarshal(b, &doc); err != nil || doc.Discovery == nil {
		t.Fatalf("diagnostics lacks a discovery block: %s", b)
	}
	s := string(b)
	for _, leak := range []string{up.srv.URL, "127.0.0.1", hrUpstreamKey, "llamacpp", "loaded", "architecture"} {
		if strings.Contains(s, leak) {
			t.Fatalf("diagnostics leaks %q: %s", leak, s)
		}
	}
	if len(doc.Discovery.Accounts) != 2 || doc.Discovery.Accounts[0].Account != "llama-lab" ||
		!slices.Equal(doc.Discovery.Accounts[0].Routes, []string{"lab", "duo"}) ||
		doc.Discovery.Accounts[0].ModelCount != 2 || doc.Discovery.Accounts[0].LastSuccessAt == nil {
		t.Fatalf("discovery accounts = %+v", doc.Discovery.Accounts)
	}
	if len(doc.Discovery.Routes) != 2 || !slices.Equal(doc.Discovery.Routes[1].Models, []string{"duo/b"}) {
		t.Fatalf("discovery routes = %+v", doc.Discovery.Routes)
	}
}

// D10: reloads re-target polling without losing listings of accounts still
// referenced, and Close leaves no discovery goroutine behind.
func TestDiscoveryD10ReloadRetargetsAndCloseLeaksNothing(t *testing.T) {
	base := runtime.NumGoroutine()
	up := newDiscUpstream(t)
	up.setListing("lab", "m-one")
	up.setListing("two", "duo/b")
	dir := t.TempDir()
	t.Setenv("HR_UPSTREAM_KEY", hrUpstreamKey)
	hrWriteFile(t, filepath.Join(dir, "me.key"), goldenKey)
	cfgPath := filepath.Join(dir, "config.yaml")
	hrWriteFile(t, cfgPath, discConfigYAML(up.srv.URL))
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	a, err := app.Build(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), app.Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	e := &discEnv{t: t, up: up, a: a, srv: httptest.NewServer(a.Handler), dir: dir}
	ctx, cancel := context.WithCancel(context.Background())
	a.Start(ctx)
	e.waitModels("m-one", "duo/b")

	// Remove route "duo" (llama-two no longer referenced) and make the lab
	// listing fail upstream: the lab listing must survive the reload.
	up.setRaw("lab", "not json")
	noDuo := strings.Split(discConfigYAML(up.srv.URL), "  - name: duo\n")[0]
	hrWriteFile(t, cfgPath, noDuo)
	if st, err := a.Reload(cfgPath); err != nil || !st.OK {
		t.Fatalf("reload: %+v %v", st, err)
	}
	if got := e.models(); !slices.Equal(got, []string{"m-one", "static-m"}) {
		t.Fatalf("after removing duo, /v1/models = %v, want lab listing kept and duo/b dropped", got)
	}
	twoCalls := up.calls("two")

	// Re-add duo: llama-two is polled again (a fresh listing is fetched).
	up.setListing("lab", "m-one")
	hrWriteFile(t, cfgPath, discConfigYAML(up.srv.URL))
	if st, err := a.Reload(cfgPath); err != nil || !st.OK {
		t.Fatalf("reload: %+v %v", st, err)
	}
	e.waitModels("duo/b")
	if up.calls("two") <= twoCalls {
		t.Fatal("re-added account was not polled")
	}

	e.srv.Close()
	cancel()
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	up.srv.Close() // also drops the proxy's pooled upstream connections
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > base+1 {
		if time.Now().After(deadline) {
			buf := make([]byte, 1<<16)
			n := runtime.Stack(buf, true)
			t.Fatalf("goroutines %d > base %d after Close:\n%s", runtime.NumGoroutine(), base, buf[:n])
		}
		time.Sleep(10 * time.Millisecond)
	}
	buf := make([]byte, 1<<20)
	if n := runtime.Stack(buf, true); strings.Contains(string(buf[:n]), "internal/discovery") {
		t.Fatalf("discovery goroutine alive after Close:\n%s", buf[:n])
	}
}

// Race: requests resolving discovered ids, /v1/models and diagnostics run while
// reloads re-target discovery and polls run (meaningful under -race).
func TestDiscoveryConcurrentResolvesDuringReloadAndPoll(t *testing.T) {
	up := newDiscUpstream(t)
	up.setListing("lab", "m-one")
	up.setListing("two", "duo/b")
	e := newDiscEnv(t, up, discConfigYAML(up.srv.URL), true)
	e.waitModels("m-one", "duo/b")
	cfgPath := filepath.Join(e.dir, "config.yaml")
	full := discConfigYAML(up.srv.URL)
	noDuo := strings.Split(full, "  - name: duo\n")[0]

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_, _, _ = e.doRaw("POST", "/v1/chat/completions", chatBody("m-one"))
				_, _, _ = e.doRaw("POST", "/v1/chat/completions", chatBody("m-miss"))
				_, _, _ = e.doRaw("GET", "/v1/models", "")
				_, _, _ = e.doRaw("GET", "/control/v1/diagnostics", "")
			}
		}()
	}
	for i := 0; i < 10; i++ {
		body := full
		if i%2 == 0 {
			body = noDuo
		}
		hrWriteFile(t, cfgPath, body)
		if st, err := e.a.Reload(cfgPath); err != nil || !st.OK {
			close(stop)
			wg.Wait()
			t.Fatalf("reload %d: %+v %v", i, st, err)
		}
		e.a.Discovery.PollDue(context.Background())
	}
	close(stop)
	wg.Wait()
	if st, body := e.do("POST", "/v1/chat/completions", chatBody("m-one")); st != http.StatusOK {
		t.Fatalf("m-one after reload churn: %d %s", st, body)
	}
}
