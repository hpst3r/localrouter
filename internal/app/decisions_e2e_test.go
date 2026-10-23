package app_test

// Typed-decision end-to-end tests (docs/SPEC.md "Typed decisions"): the full
// app over a loopback fake OpenRouter that replays the saved Decisions API
// example (design/or-decisions-ref.md lines 180-211). No live network.

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const decE2EAnswer = `{"id":"gen-dec-1","model":"typesafe/jev-1.13-20260917","provider":"TypeSafe",` +
	`"answers":{"q":{"type":"noul","noul":0.98}},"usage":{"input_tokens":275,"output_tokens":20,"cost":0.00003}}`

type decFakeOR struct {
	srv   *httptest.Server
	mu    sync.Mutex
	paths []string
}

func newDecFakeOR(t *testing.T) *decFakeOR {
	f := &decFakeOR{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		if r.Method != http.MethodPost {
			http.NotFound(w, r) // quota polling (/credits, /key) is not under test
			return
		}
		f.mu.Lock()
		f.paths = append(f.paths, r.URL.Path)
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/systemone" || r.URL.Path == "/alpha/decisions" {
			_, _ = io.WriteString(w, decE2EAnswer)
			return
		}
		_, _ = io.WriteString(w, `{"id":"c","usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *decFakeOR) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.paths...)
}

// decE2EConfig renders one openrouter account with a chat route and, when
// family is non-empty, a decision route "jev" of that family.
func decE2EConfig(url, family, decisionsBlock string) string {
	s := fmt.Sprintf(`listen: 127.0.0.1:0
data_dir: data
pricing_file: pricing.yaml
control: {require_auth: true}
clients:
  - {name: ide, class: interactive, key_file: a.key}
accounts:
  - id: or1
    provider: openrouter
    base_url: %s/v1
    api_key_env: HR_UPSTREAM_KEY
%sroutes:
  - name: chat
    models: [chat]
    interactive: [or1]
`, url, decisionsBlock)
	if family != "" {
		s += fmt.Sprintf(`  - name: jev
    models: [jev]
    interactive: [or1]
    upstreams:
      or1:
        upstream_model: typesafe/jev-1.13
        protocols: [%s]
        input_modalities: [text]
        question_types: [noul, choice, score]
`, family)
	}
	return s
}

const decE2EBody = `{"model":"jev","state":"I was charged twice.","questions":{"q":{"type":"noul","instructions":"Refund?"}}}`

// TestT1T15T16DecisionsEndToEnd: flags off is 404 on both paths with chat
// unchanged; a hot reload adds the route (served, ledger cost is OpenRouter's
// reported usage.cost); decisions bounds reload without a restart; removing
// the route returns both paths to 404.
func TestT1T15T16DecisionsEndToEnd(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HR_UPSTREAM_KEY", hrUpstreamKey)
	key := "hr-client-keyA-0123456789"
	hrWriteFile(t, filepath.Join(dir, "a.key"), key)
	hrWriteFile(t, filepath.Join(dir, "pricing.yaml"), "models: {}\n")
	f := newDecFakeOR(t)
	cfgPath := filepath.Join(dir, "config.yaml")
	// T1: a decisions: block without any decision route loads and changes nothing.
	hrWriteFile(t, cfgPath, decE2EConfig(f.srv.URL, "", "decisions: {max_questions: 8}\n"))
	e := hrBuild(t, dir, cfgPath, nil)

	notFound := e.do("POST", "/v1/unknown", key, decE2EBody)
	for _, p := range []string{"/v1/systemone", "/v1/decisions"} {
		r := e.do("POST", p, key, decE2EBody)
		if r.status != 404 || string(r.body) != string(notFound.body) {
			t.Fatalf("flags off %s = %d %s", p, r.status, r.body)
		}
	}
	if r := e.do("POST", "/v1/chat/completions", key, `{"model":"chat","messages":[]}`); r.status != 200 {
		t.Fatalf("chat = %d %s", r.status, r.body)
	}

	// T15: hot-add a systemone route.
	hrWriteFile(t, cfgPath, decE2EConfig(f.srv.URL, "systemone", ""))
	st, err := e.a.Reload(cfgPath)
	if err != nil || !st.OK || len(st.RestartOnly) != 0 {
		t.Fatalf("reload = %+v %v", st, err)
	}
	r := e.do("POST", "/v1/systemone", key, decE2EBody)
	if r.status != 200 || string(r.body) != decE2EAnswer {
		t.Fatalf("systemone = %d %s", r.status, r.body)
	}
	if r := e.do("POST", "/v1/decisions", key, decE2EBody); r.status != 400 || !strings.Contains(string(r.body), "capability_unsupported") {
		t.Fatalf("systemone route at /v1/decisions = %d %s", r.status, r.body)
	}
	if r := e.do("POST", "/v1/chat/completions", key, `{"model":"jev","messages":[]}`); r.status != 400 {
		t.Fatalf("decision route at chat = %d %s", r.status, r.body)
	}

	// T16: the row is costed at OpenRouter's reported usage.cost.
	rows, err := e.a.Ledger.Summary(context.Background(), time.Now().Add(-time.Hour), "route")
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, row := range rows {
		if row.Key == "jev" {
			found = true
			if row.Requests != 1 || row.InputTokens != 275 || row.OutputTokens != 20 || row.CostUSD == nil || *row.CostUSD != 0.00003 {
				t.Fatalf("jev ledger row = %+v cost=%v", row, row.CostUSD)
			}
		}
	}
	if !found {
		t.Fatalf("no jev ledger row in %+v", rows)
	}

	// Bounds are per generation: tightening them is a hot reload.
	hrWriteFile(t, cfgPath, decE2EConfig(f.srv.URL, "decisions", "decisions: {max_questions: 1}\n"))
	if st, err := e.a.Reload(cfgPath); err != nil || !st.OK || len(st.RestartOnly) != 0 {
		t.Fatalf("bounds reload = %+v %v", st, err)
	}
	two := `{"model":"jev","state":"s","questions":{"a":{"type":"noul"},"b":{"type":"noul"}}}`
	if r := e.do("POST", "/v1/decisions", key, two); r.status != 400 || !strings.Contains(string(r.body), "decision_bounds_exceeded") {
		t.Fatalf("bounded = %d %s", r.status, r.body)
	}
	if r := e.do("POST", "/v1/decisions", key, decE2EBody); r.status != 200 || string(r.body) != decE2EAnswer {
		t.Fatalf("decisions = %d %s", r.status, r.body)
	}

	// Removing the route unregisters both paths again.
	hrWriteFile(t, cfgPath, decE2EConfig(f.srv.URL, "", ""))
	if st, err := e.a.Reload(cfgPath); err != nil || !st.OK {
		t.Fatalf("remove reload = %+v %v", st, err)
	}
	for _, p := range []string{"/v1/systemone", "/v1/decisions"} {
		if r := e.do("POST", p, key, decE2EBody); r.status != 404 {
			t.Fatalf("after removal %s = %d", p, r.status)
		}
	}
	got := f.seen()
	want := []string{"/v1/chat/completions", "/v1/systemone", "/alpha/decisions"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("upstream POSTs = %v, want %v", got, want)
	}
}
