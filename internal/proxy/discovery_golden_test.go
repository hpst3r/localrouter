package proxy

// D1 (flags-off) golden: the routing decision table and the /v1/models body for
// a config.example.yaml-like route set with no discovering route. The goldens
// were captured before model discovery existed; run with -update to rewrite
// them (only ever on a pre-change build).

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/hpst3r/localrouter/internal/core"
)

var updateGolden = flag.Bool("update", false, "rewrite golden files in testdata")

// recordingPolicy admits the first non-excluded candidate and records the
// candidate list each Acquire saw.
type recordingPolicy struct {
	mu    sync.Mutex
	cands [][]string
}

type recLease struct{ id string }

func (l recLease) AccountID() string                       { return l.id }
func (l recLease) Release(core.Outcome)                    {}
func (p *recordingPolicy) Status(string) core.AccountState { return core.AccountState{} }
func (p *recordingPolicy) DryRun(core.Class, []string) core.Decision {
	return core.Decision{}
}
func (p *recordingPolicy) Acquire(_ core.Class, cands []string, exclude map[string]bool) (core.Lease, core.Decision) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cands = append(p.cands, append([]string{}, cands...))
	for _, c := range cands {
		if !exclude[c] {
			return recLease{c}, core.Decision{Allow: true, AccountID: c, Reason: "ok"}
		}
	}
	return nil, core.Decision{Reason: testReason}
}
func (p *recordingPolicy) take() [][]string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := p.cands
	p.cands = nil
	return out
}

// goldenRoutes mirrors config.example.yaml plus the two rewrite shapes
// (route-wide upstream_model and capability upstreams).
func goldenRoutes() []core.Route {
	return []core.Route{
		{Name: "gpt", Models: []string{"gpt-5.6-sol", "gpt-5.6-luna"},
			Interactive: []string{"codex-primary", "codex-secondary"}, Background: []string{"codex-secondary", "codex-primary"}},
		{Name: "ollama", Models: []string{"deepseek-v4.1-flash", "kimi-k3"},
			Interactive: []string{"ollama-cloud"}, Background: []string{"ollama-cloud"}},
		{Name: "alias", Models: []string{"alias-model"}, UpstreamModel: "backend-alias",
			Interactive: []string{"compat-a", "ollama-cloud"}, Background: []string{"compat-a"}},
		{Name: "cap", Models: []string{"cap-model"},
			Interactive: []string{"compat-a", "compat-b"}, Background: []string{"compat-b"},
			Upstreams: map[string]core.UpstreamSpec{
				"compat-a": {UpstreamModel: "cap-a", Protocols: []string{"responses"}, InputModalities: []string{"text"}},
				"compat-b": {Protocols: []string{"chat", "responses"}, InputModalities: []string{"text"}},
			}},
	}
}

type goldenRow struct {
	Model      string     `json:"model"`
	Class      string     `json:"class"`
	Endpoint   string     `json:"endpoint"`
	Status     int        `json:"status"`
	ErrType    string     `json:"err_type,omitempty"`
	Route      string     `json:"route,omitempty"`
	Candidates [][]string `json:"candidates,omitempty"`
	Account    string     `json:"account,omitempty"`
	Forwarded  string     `json:"forwarded,omitempty"`
}

// goldenHarness builds a proxy over goldenRoutes with one httptest upstream per
// account that records the forwarded model.
func goldenHarness(t *testing.T) (*harness, *recordingPolicy, func() (string, string)) {
	h := newHarness(t)
	pol := &recordingPolicy{}
	h.pol = pol
	var mu sync.Mutex
	var lastAcct, lastModel string
	for _, a := range []struct{ id, provider string }{
		{"codex-primary", core.ProviderCodex}, {"codex-secondary", core.ProviderCodex},
		{"ollama-cloud", core.ProviderOllama}, {"compat-a", core.ProviderOpenAICompat},
		{"compat-b", core.ProviderOpenAICompat},
	} {
		id := a.id
		h.upstream(id, a.provider, func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			var doc struct {
				Model string `json:"model"`
			}
			_ = json.Unmarshal(b, &doc)
			mu.Lock()
			lastAcct, lastModel = id, doc.Model
			mu.Unlock()
			okJSON(w, r)
		})
	}
	h.routes = goldenRoutes()
	h.start()
	return h, pol, func() (string, string) {
		mu.Lock()
		defer mu.Unlock()
		a, m := lastAcct, lastModel
		lastAcct, lastModel = "", ""
		return a, m
	}
}

// goldenTable drives every model x class x endpoint through h.
func goldenTable(t *testing.T, h *harness, pol *recordingPolicy, last func() (string, string)) []goldenRow {
	t.Helper()
	models := []string{"gpt-5.6-sol", "gpt-5.6-luna", "deepseek-v4.1-flash", "kimi-k3", "alias-model", "cap-model", "unknown-model", "unsloth/qwen3"}
	var rows []goldenRow
	n := 0
	for _, m := range models {
		for _, key := range []string{clientKey, bgKey} {
			for _, ep := range []string{pathResponses, pathChat} {
				resp := h.post(ep, key, fmt.Sprintf(`{"model":%q,"input":"hi","messages":[{"role":"user","content":"hi"}]}`, m), nil)
				body, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				n++
				h.waitHandled(n)
				row := goldenRow{Model: m, Class: "interactive", Endpoint: ep, Status: resp.StatusCode}
				if key == bgKey {
					row.Class = "background"
				}
				if resp.StatusCode != http.StatusOK {
					var eb errorBody
					_ = json.Unmarshal(body, &eb)
					row.ErrType = eb.Error.Type
				}
				row.Candidates = pol.take()
				row.Account, row.Forwarded = last()
				for _, rec := range h.ledger.all() {
					row.Route = rec.Route
				}
				h.ledger.mu.Lock()
				h.ledger.rows = nil
				h.ledger.mu.Unlock()
				rows = append(rows, row)
			}
		}
	}
	return rows
}

func goldenModels(t *testing.T, h *harness) []byte {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, h.srv.URL+pathModels, nil)
	req.Header.Set("Authorization", "Bearer "+clientKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return b
}

func checkGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *updateGolden {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s differs from golden:\n got: %s\nwant: %s", name, got, want)
	}
}

func TestD1GoldenFlagsOffRoutingAndModels(t *testing.T) {
	h, pol, last := goldenHarness(t)
	checkGolden(t, "d1_models.json", goldenModels(t, h))
	rows := goldenTable(t, h, pol, last)
	b, err := json.MarshalIndent(rows, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	checkGolden(t, "d1_routing.json", append(b, '\n'))
}
