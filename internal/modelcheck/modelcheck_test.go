package modelcheck

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
	"github.com/hpst3r/localrouter/internal/discovery"
)

const secretKey = "sk-modelcheck-secret-0123456789"

// llamaRouterListing is a llama.cpp router-mode GET /v1/models listing: every
// id carries its quant suffix, plus the extra status/architecture fields (the
// same shape as discovery's D3 fixture, internal/app/discovery_e2e_test.go).
const llamaRouterListing = `{"object":"list","data":[
 {"id":"unsloth/Qwen3.5-9B-GGUF:Q4_K_M","object":"model","owned_by":"llamacpp","created":1760000000,
  "status":{"value":"loaded","args":["--ctx-size","8192"]},"architecture":{"modality":"text->text"}},
 {"id":"ggml-org/gemma-3-1b-it-GGUF","object":"model","owned_by":"llamacpp","created":1760000001,
  "status":{"value":"unloaded"}}]}`

// singleModelListing is a single-model llama-server GET /v1/models listing:
// one id (the loaded model's alias), with the "models" array newer servers
// add alongside "data" (ignored).
func singleModelListing(alias string) string {
	return `{"models":[{"name":"` + alias + `","model":"` + alias + `"}],"object":"list","data":[{"id":"` + alias +
		`","object":"model","created":1760000000,"owned_by":"llamacpp","meta":{"vocab_type":2,"n_ctx_train":32768}}]}`
}

// ollamaListing is an Ollama OpenAI-compatible GET /v1/models listing: ids are
// name:tag.
const ollamaListing = `{"object":"list","data":[{"id":"qwen3:8b","object":"model","created":1760000000,"owned_by":"library"},{"id":"gpt-oss:20b","object":"model","created":1760000000,"owned_by":"library"}]}`

type fakeCreds struct{ fail map[string]bool }

func (f fakeCreds) Credential(_ context.Context, id string) (core.Credential, error) {
	if f.fail[id] {
		return core.Credential{}, errors.New("no key " + secretKey)
	}
	return core.Credential{Headers: http.Header{"Authorization": {"Bearer " + secretKey}}}, nil
}
func (fakeCreds) Invalidate(string) {}

// upstream fakes several accounts behind one server, keyed by path prefix.
type upstream struct {
	srv *httptest.Server

	mu       sync.Mutex
	listings map[string]string
	status   map[string]int
	calls    map[string]int
	auth     map[string]string
	block    chan struct{} // when non-nil, /models waits on it
	active   atomic.Int32
	peak     atomic.Int32
}

func newUpstream(t *testing.T) *upstream {
	u := &upstream{listings: map[string]string{}, status: map[string]int{}, calls: map[string]int{}, auth: map[string]string{}}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		prefix, rest, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
		if rest != "v1/models" {
			http.NotFound(w, r)
			return
		}
		n := u.active.Add(1)
		defer u.active.Add(-1)
		for p := u.peak.Load(); n > p && !u.peak.CompareAndSwap(p, n); p = u.peak.Load() {
		}
		u.mu.Lock()
		u.calls[prefix]++
		u.auth[prefix] = r.Header.Get("Authorization")
		body, st, block := u.listings[prefix], u.status[prefix], u.block
		u.mu.Unlock()
		if block != nil {
			<-block
		}
		if st != 0 {
			http.Error(w, "upstream body "+secretKey, st)
			return
		}
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(u.srv.Close)
	return u
}

func (u *upstream) set(prefix, body string) {
	u.mu.Lock()
	u.listings[prefix] = body
	u.mu.Unlock()
}

func (u *upstream) setStatus(prefix string, st int) {
	u.mu.Lock()
	u.status[prefix] = st
	u.mu.Unlock()
}

func (u *upstream) callCount(prefix string) int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.calls[prefix]
}

func (u *upstream) account(id, provider, prefix string) core.Account {
	return core.Account{ID: id, Provider: provider, BaseURL: u.srv.URL + "/" + prefix + "/v1"}
}

func accountMap(as ...core.Account) map[string]core.Account {
	m := map[string]core.Account{}
	for _, a := range as {
		m[a.ID] = a
	}
	return m
}

func probe(accounts map[string]core.Account, routes []core.Route) Result {
	return Probe(context.Background(), fakeCreds{}, accounts, routes, Options{})
}

func onlyVerdict(t *testing.T, res Result) Verdict {
	t.Helper()
	if len(res.Verdicts) != 1 {
		t.Fatalf("verdicts = %+v, want exactly one", res.Verdicts)
	}
	return res.Verdicts[0]
}

// MC3: expected ids per candidate: upstreams[acct].upstream_model, else the
// route upstream_model, else every models id; discovering routes are skipped;
// candidates are interactive ∪ background, deduplicated, first-seen order.
func TestMC3Expectations(t *testing.T) {
	accts := accountMap(
		core.Account{ID: "a", Provider: core.ProviderOpenAICompat},
		core.Account{ID: "b", Provider: core.ProviderOllama},
		core.Account{ID: "cx", Provider: core.ProviderCodex},
	)
	routes := []core.Route{
		{Name: "plain", Models: []string{"m1", "m2"}, Interactive: []string{"a"}, Background: []string{"a", "b"}},
		{Name: "rewrite", Models: []string{"client"}, UpstreamModel: "route-up", Interactive: []string{"b", "a"},
			Upstreams: map[string]core.UpstreamSpec{"a": {UpstreamModel: "acct-up"}, "b": {}}},
		{Name: "disc", DiscoverModels: true, Interactive: []string{"a"}},
		{Name: "codex", Models: []string{"gpt-5"}, Interactive: []string{"cx"}},
		{Name: "unknown", Models: []string{"x"}, Interactive: []string{"gone"}},
	}
	got := Expectations(accts, routes)
	want := []Expectation{
		{"plain", "a", core.ProviderOpenAICompat, "m1"},
		{"plain", "a", core.ProviderOpenAICompat, "m2"},
		{"plain", "b", core.ProviderOllama, "m1"},
		{"plain", "b", core.ProviderOllama, "m2"},
		{"rewrite", "b", core.ProviderOllama, "route-up"},
		{"rewrite", "a", core.ProviderOpenAICompat, "acct-up"},
		{"codex", "cx", core.ProviderCodex, "gpt-5"},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("expectations\n got %+v\nwant %+v", got, want)
	}
	// Codex is ineligible (discovery's rule) and is never fetched.
	vs := Evaluate(got[6:], map[string]Listing{})
	if vs[0].Status != StatusIneligible || vs[0].Provider != core.ProviderCodex {
		t.Fatalf("codex verdict %+v, want ineligible", vs[0])
	}
	if ids := eligibleAccounts(got); !slices.Equal(ids, []string{"a", "b"}) {
		t.Fatalf("eligible accounts %v", ids)
	}
}

// MC4 (incident 1): a llama.cpp router-mode backend lists ids with the quant
// suffix; a route sending the bare repo id is missing, with the suffixed id
// suggested. The exact suffixed id is ok.
func TestMC4LlamaRouterBareRepoID(t *testing.T) {
	u := newUpstream(t)
	u.set("lab", llamaRouterListing)
	accts := accountMap(u.account("lab", core.ProviderOpenAICompat, "lab"))
	res := probe(accts, []core.Route{
		{Name: "qwen", Models: []string{"qwen3.5-9b"}, UpstreamModel: "unsloth/Qwen3.5-9B-GGUF", Interactive: []string{"lab"}},
		{Name: "qwen-ok", Models: []string{"qwen-ok"}, UpstreamModel: "unsloth/Qwen3.5-9B-GGUF:Q4_K_M", Interactive: []string{"lab"}},
	})
	v := res.Verdicts[0]
	if v.Status != StatusMissing || !slices.Equal(v.Suggestions, []string{"unsloth/Qwen3.5-9B-GGUF:Q4_K_M"}) || v.SingleModel {
		t.Fatalf("bare repo id verdict %+v", v)
	}
	if res.Verdicts[1].Status != StatusOK {
		t.Fatalf("suffixed id verdict %+v, want ok", res.Verdicts[1])
	}
	if res.Summary != (Summary{OK: 1, Missing: 1}) || u.callCount("lab") != 1 {
		t.Fatalf("summary %+v, calls %d", res.Summary, u.callCount("lab"))
	}
	u.mu.Lock()
	auth := u.auth["lab"]
	u.mu.Unlock()
	if auth != "Bearer "+secretKey {
		t.Fatalf("listing fetched without the account credential")
	}
}

// MC5 (incident 2): a single-model llama-server whose loaded model was swapped
// lists only the new alias; the old route's id is missing with single_model
// naming the loaded id.
func TestMC5SingleModelSwap(t *testing.T) {
	u := newUpstream(t)
	u.set("single", singleModelListing("gemma-3-27b"))
	accts := accountMap(u.account("single", core.ProviderOpenAICompat, "single"))
	v := onlyVerdict(t, probe(accts, []core.Route{{Name: "old", Models: []string{"qwen3-32b"}, Interactive: []string{"single"}}}))
	if v.Status != StatusMissing || !v.SingleModel || v.Loaded != "gemma-3-27b" || len(v.Suggestions) != 0 {
		t.Fatalf("swapped single-model verdict %+v", v)
	}
	// The route for the loaded alias is ok and is not flagged.
	v = onlyVerdict(t, probe(accts, []core.Route{{Name: "new", Models: []string{"gemma-3-27b"}, Interactive: []string{"single"}}}))
	if v.Status != StatusOK || v.SingleModel {
		t.Fatalf("loaded alias verdict %+v", v)
	}
}

// MC6: suggestion rules: suffix stripped on either side, case-insensitive
// repo path, at most 3, deterministic order (suffix matches first).
func TestMC6Suggestions(t *testing.T) {
	listed := []string{"x/Repo:Q8_0", "x/repo:Q2_K", "x/Repo:Q4_K_M", "x/Repo", "x/REPO:F16", "y/Repo:Q4_K_M", "other"}
	for _, c := range []struct {
		expected string
		want     []string
	}{
		{"x/Repo:Q5_K_M", []string{"x/Repo", "x/Repo:Q4_K_M", "x/Repo:Q8_0"}},
		{"x/repo", []string{"x/repo:Q2_K", "x/REPO:F16", "x/Repo"}},
		{"x/Rep", nil},
		{"qwen3", []string{"qwen3:8b"}},
	} {
		l := listed
		if c.expected == "qwen3" {
			l = []string{"gpt-oss:20b", "qwen3:8b"}
		}
		if got := Suggest(c.expected, l); !slices.Equal(got, c.want) {
			t.Errorf("Suggest(%q) = %q, want %q", c.expected, got, c.want)
		}
	}
	// Ollama: a route sending the bare name gets the tagged id suggested.
	u := newUpstream(t)
	u.set("ol", ollamaListing)
	v := onlyVerdict(t, probe(accountMap(u.account("ol", core.ProviderOllama, "ol")),
		[]core.Route{{Name: "q", Models: []string{"qwen3"}, Interactive: []string{"ol"}}}))
	if v.Status != StatusMissing || !slices.Equal(v.Suggestions, []string{"qwen3:8b"}) || v.SingleModel {
		t.Fatalf("ollama verdict %+v", v)
	}
}

// MC7: a failed fetch is unchecked with discovery's error class, never ok, and
// neither the result nor its JSON carries the URL, key or body.
func TestMC7UncheckedNeverOK(t *testing.T) {
	u := newUpstream(t)
	u.setStatus("st", http.StatusInternalServerError)
	u.set("bad", `not json `+secretKey)
	u.set("ok", llamaRouterListing)
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()
	accts := accountMap(
		u.account("st", core.ProviderOpenAICompat, "st"),
		u.account("bad", core.ProviderOpenAICompat, "bad"),
		u.account("cred", core.ProviderOpenAICompat, "ok"),
		core.Account{ID: "dead", Provider: core.ProviderOpenAICompat, BaseURL: deadURL + "/v1"},
	)
	var routes []core.Route
	for _, id := range []string{"st", "bad", "cred", "dead"} {
		routes = append(routes, core.Route{Name: id, Models: []string{"unsloth/Qwen3.5-9B-GGUF:Q4_K_M"}, Interactive: []string{id}})
	}
	res := Probe(context.Background(), fakeCreds{fail: map[string]bool{"cred": true}}, accts, routes, Options{})
	want := map[string]string{"st": discovery.ErrClassStatus, "bad": discovery.ErrClassMalformed,
		"cred": discovery.ErrClassCredential, "dead": discovery.ErrClassTransport}
	for _, v := range res.Verdicts {
		if v.Status != StatusUnchecked || v.ErrorClass != want[v.Account] {
			t.Errorf("verdict %+v, want unchecked %s", v, want[v.Account])
		}
	}
	if res.Summary != (Summary{Unchecked: 4}) {
		t.Fatalf("summary %+v", res.Summary)
	}
	b, _ := json.Marshal(res)
	for _, leak := range []string{secretKey, u.srv.URL, deadURL, "127.0.0.1", "not json"} {
		if bytes.Contains(b, []byte(leak)) {
			t.Fatalf("result leaks %q: %s", leak, b)
		}
	}
}

// MC8: Probe fetches each eligible account once however many routes use it,
// at most 4 at a time.
func TestMC8ProbeFetchesOncePerAccountBounded(t *testing.T) {
	u := newUpstream(t)
	block := make(chan struct{})
	u.block = block
	var as []core.Account
	var routes []core.Route
	for _, p := range []string{"a", "b", "c", "d", "e", "f", "g"} {
		u.set(p, singleModelListing("m-"+p))
		as = append(as, u.account(p, core.ProviderOpenAICompat, p))
		routes = append(routes,
			core.Route{Name: "r1-" + p, Models: []string{"m-" + p}, Interactive: []string{p}},
			core.Route{Name: "r2-" + p, Models: []string{"n-" + p}, Interactive: []string{p}, Background: []string{p}})
	}
	done := make(chan Result)
	go func() { done <- probe(accountMap(as...), routes) }()
	deadline := time.Now().Add(5 * time.Second)
	for u.active.Load() < DefaultConcurrency && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	close(block)
	res := <-done
	if p := u.peak.Load(); p != DefaultConcurrency {
		t.Fatalf("peak concurrent fetches %d, want %d", p, DefaultConcurrency)
	}
	for _, a := range as {
		if n := u.callCount(a.ID); n != 1 {
			t.Fatalf("account %s fetched %d times", a.ID, n)
		}
	}
	if res.Summary != (Summary{OK: 7, Missing: 7, SingleModel: 7}) || len(res.Accounts) != 7 {
		t.Fatalf("summary %+v accounts %d", res.Summary, len(res.Accounts))
	}
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) take() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.b.String()
	s.b.Reset()
	return out
}

// MC11: the checker logs one warn line per change into missing or
// single_model, one info line on return to ok, nothing while unchanged, and an
// unchecked fetch does not reset the state.
func TestMC11CheckerLogsVerdictChanges(t *testing.T) {
	u := newUpstream(t)
	u.set("lab", singleModelListing("qwen-a"))
	logs := &syncBuffer{}
	c := NewChecker(fakeCreds{}, nil, time.Minute, slog.New(slog.NewTextHandler(logs, nil)), Options{})
	defer c.Close()
	c.SetTargets(accountMap(u.account("lab", core.ProviderOpenAICompat, "lab")),
		[]core.Route{{Name: "r", Models: []string{"qwen-a"}, Interactive: []string{"lab"}}})
	ctx := context.Background()

	step := func(name, wantLevel, wantMsg string) {
		t.Helper()
		c.Check(ctx)
		got := logs.take()
		if wantMsg == "" {
			if got != "" {
				t.Fatalf("%s: unexpected log %q", name, got)
			}
			return
		}
		if strings.Count(got, "\n") != 1 || !strings.Contains(got, "level="+wantLevel) || !strings.Contains(got, wantMsg) {
			t.Fatalf("%s: log %q, want one %s %q line", name, got, wantLevel, wantMsg)
		}
		for _, leak := range []string{secretKey, u.srv.URL} {
			if strings.Contains(got, leak) {
				t.Fatalf("%s: log leaks %q", name, leak)
			}
		}
	}
	step("ok at start", "", "")
	u.set("lab", singleModelListing("qwen-b"))
	step("swap", "WARN", `"backend serves a different single model" route=r account=lab expected=qwen-a loaded=qwen-b`)
	step("unchanged", "", "")
	u.setStatus("lab", http.StatusBadGateway)
	step("unchecked keeps state", "", "")
	u.setStatus("lab", 0)
	step("still swapped", "", "")
	u.set("lab", `{"data":[{"id":"qwen-b"},{"id":"qwen-c"}]}`)
	step("missing", "WARN", `"backend model missing" route=r account=lab expected=qwen-a`)
	u.set("lab", `{"data":[{"id":"qwen-b"},{"id":"qwen-a"}]}`)
	step("recovered", "INFO", `"backend model check ok" route=r account=lab expected=qwen-a`)
	step("ok unchanged", "", "")

	d := c.Snapshot()
	if d.Interval != "1m0s" || d.CheckedAt == nil || len(d.Verdicts) != 1 || d.Verdicts[0].Status != StatusOK {
		t.Fatalf("snapshot %+v", d)
	}
}

// MC-1 (Amendment R1.1): a backend that lists more ids than discovery's cap,
// with the expected id beyond it, is unchecked "truncated", never missing; an
// expected id that survived the cap is still ok. The shared (discovery) path
// carries the dropped count the same way.
func TestMCFixMC1TruncatedListingUnchecked(t *testing.T) {
	var b strings.Builder
	b.WriteString(`{"object":"list","data":[`)
	for i := range 1000 {
		fmt.Fprintf(&b, `{"id":"filler-%04d"},`, i)
	}
	b.WriteString(`{"id":"wanted-model"}]}`)
	u := newUpstream(t)
	u.set("big", b.String())

	res := probe(accountMap(u.account("big", core.ProviderOpenAICompat, "big")), []core.Route{
		{Name: "r", Models: []string{"wanted-model"}, Interactive: []string{"big"}},
		{Name: "f", Models: []string{"filler-0000"}, Interactive: []string{"big"}},
	})
	if len(res.Verdicts) != 2 {
		t.Fatalf("verdicts = %+v", res.Verdicts)
	}
	if v := res.Verdicts[0]; v.Status != StatusUnchecked || v.ErrorClass != ErrClassTruncated || len(v.Suggestions) != 0 || v.SingleModel {
		t.Fatalf("id beyond the cap: verdict %+v, want unchecked/truncated", v)
	}
	if v := res.Verdicts[1]; v.Status != StatusOK {
		t.Fatalf("id within the cap: verdict %+v, want ok", v)
	}
	if res.Summary.Missing != 0 || res.Summary.Unchecked != 1 {
		t.Fatalf("summary %+v", res.Summary)
	}

	shared := &fakeShared{ids: map[string][]string{"disc": {"other"}}, dropped: map[string]int{"disc": 1}, at: time.Now(), errs: map[string]string{}}
	c := NewChecker(fakeCreds{}, shared, time.Minute, slog.New(slog.NewTextHandler(io.Discard, nil)), Options{})
	defer c.Close()
	c.SetTargets(accountMap(u.account("disc", core.ProviderOpenAICompat, "disc")),
		[]core.Route{{Name: "r", Models: []string{"wanted-model"}, Interactive: []string{"disc"}}})
	if v := c.Check(context.Background()).Verdicts[0]; v.Status != StatusUnchecked || v.ErrorClass != ErrClassTruncated {
		t.Fatalf("shared truncated listing: verdict %+v, want unchecked/truncated", v)
	}
	if u.callCount("disc") != 0 {
		t.Fatal("checker fetched an account discovery already polls")
	}
}

type fakeShared struct {
	mu      sync.Mutex
	ids     map[string][]string
	dropped map[string]int
	at      time.Time
	errs    map[string]string
}

func (f *fakeShared) Listing(id string) ([]string, int, time.Time, string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ids, ok := f.ids[id]
	return ids, f.dropped[id], f.at, f.errs[id], ok
}

// MC12: an account discovery already polls is read from the shared listing
// and never fetched by the checker; before discovery's first poll it is
// unchecked "pending"; a failed discovery poll is unchecked with its class.
func TestMC12CheckerSharesDiscoveryListing(t *testing.T) {
	u := newUpstream(t)
	u.set("own", llamaRouterListing)
	u.set("disc", llamaRouterListing)
	shared := &fakeShared{ids: map[string][]string{"disc": {"unsloth/Qwen3.5-9B-GGUF:Q4_K_M"}}, errs: map[string]string{}}
	c := NewChecker(fakeCreds{}, shared, time.Minute, slog.New(slog.NewTextHandler(io.Discard, nil)), Options{})
	defer c.Close()
	c.SetTargets(accountMap(u.account("own", core.ProviderOpenAICompat, "own"), u.account("disc", core.ProviderOpenAICompat, "disc")),
		[]core.Route{{Name: "r", Models: []string{"unsloth/Qwen3.5-9B-GGUF:Q4_K_M"}, Interactive: []string{"own", "disc"}}})

	res := c.Check(context.Background())
	if res.Verdicts[1].Status != StatusUnchecked || res.Verdicts[1].ErrorClass != ErrClassPending || !res.pending() {
		t.Fatalf("pending shared verdict %+v", res.Verdicts[1])
	}
	shared.mu.Lock()
	shared.at = time.Now()
	shared.mu.Unlock()
	res = c.Check(context.Background())
	if res.Verdicts[0].Status != StatusOK || res.Verdicts[1].Status != StatusOK || res.Accounts[0].Source != "discovery" || res.Accounts[1].Source != "fetch" {
		t.Fatalf("shared check %+v", res)
	}
	shared.mu.Lock()
	shared.errs["disc"] = discovery.ErrClassStatus
	shared.mu.Unlock()
	res = c.Check(context.Background())
	if v := res.Verdicts[1]; v.Status != StatusUnchecked || v.ErrorClass != discovery.ErrClassStatus {
		t.Fatalf("failed shared poll verdict %+v", v)
	}
	if u.callCount("disc") != 0 || u.callCount("own") != 3 {
		t.Fatalf("fetches disc=%d own=%d, want 0 and 3", u.callCount("disc"), u.callCount("own"))
	}
}

// MC13: once started, a re-target (reload) re-checks at once, re-fetching only
// accounts without a cached listing; Close stops the goroutine.
func TestMC13CheckerRetargetAndClose(t *testing.T) {
	before := runtime.NumGoroutine()
	u := newUpstream(t)
	u.set("a", singleModelListing("m-a"))
	u.set("b", singleModelListing("m-b"))
	accts := accountMap(u.account("a", core.ProviderOpenAICompat, "a"), u.account("b", core.ProviderOpenAICompat, "b"))
	c := NewChecker(fakeCreds{}, nil, time.Hour, slog.New(slog.NewTextHandler(io.Discard, nil)), Options{})
	c.SetTargets(accts, []core.Route{{Name: "ra", Models: []string{"m-a"}, Interactive: []string{"a"}}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.Start(ctx)
	waitFor(t, func() bool { d := c.Snapshot(); return d.CheckedAt != nil && len(d.Verdicts) == 1 })
	c.SetTargets(accts, []core.Route{
		{Name: "ra", Models: []string{"m-a"}, Interactive: []string{"a"}},
		{Name: "rb", Models: []string{"m-x"}, Interactive: []string{"b"}},
	})
	waitFor(t, func() bool { return len(c.Snapshot().Verdicts) == 2 })
	if v := c.Snapshot().Verdicts[1]; v.Status != StatusMissing || !v.SingleModel || v.Loaded != "m-b" {
		t.Fatalf("re-targeted verdict %+v", v)
	}
	if u.callCount("a") != 1 || u.callCount("b") != 1 {
		t.Fatalf("fetches a=%d b=%d, want 1 each", u.callCount("a"), u.callCount("b"))
	}
	c.Close()
	c.Close()
	u.srv.Close()
	waitFor(t, func() bool { return runtime.NumGoroutine() <= before })
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
