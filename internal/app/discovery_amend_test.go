package app_test

// Model-discovery Amendment 1 acceptance tests (docs/SPEC.md "Amendment 1
// (post-review decisions)", D13-D15 and D19) through the real App surface.

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/app"
	"github.com/hpst3r/localrouter/internal/config"
)

// amendConfigYAML: the discConfigYAML accounts with the given routes block.
func amendConfigYAML(upstreamURL, routes string) string {
	head := strings.Split(discConfigYAML(upstreamURL), "routes:\n")[0]
	return head + "routes:\n" + routes
}

// D13: a discovered model that only the other class's accounts list is a 404
// model_not_found for this class, and check_admission reports no route; no
// upstream is contacted.
func TestDiscoveryD13ClassEmptyNarrowingIsMiss(t *testing.T) {
	up := newDiscUpstream(t)
	up.setListing("lab", "m-bg-only")
	up.setListing("two")
	e := newDiscEnv(t, up, amendConfigYAML(up.srv.URL, `  - name: split
    discover_models: true
    interactive: [llama-two]
    background: [llama-lab]
`), true)
	e.waitModels("m-bg-only")
	st, body := e.do("POST", "/v1/chat/completions", chatBody("m-bg-only"))
	if st != http.StatusNotFound || !strings.Contains(string(body), `"model_not_found"`) ||
		!strings.Contains(string(body), "localrouter: unknown model") {
		t.Fatalf("interactive request for a background-only model: %d %s, want 404 model_not_found", st, body)
	}
	if h := up.takeHits(); len(h) != 0 {
		t.Fatalf("class-empty request reached an upstream: %+v", h)
	}
	st, body = e.do("POST", "/control/v1/admit", `{"class":"interactive","model":"m-bg-only"}`)
	if st != http.StatusNotFound || !strings.Contains(string(body), "no route for model") {
		t.Fatalf("check_admission: %d %s, want 404 no route", st, body)
	}
	_, body = e.do("POST", "/control/v1/admit", `{"class":"background","model":"m-bg-only"}`)
	var d admitDoc
	if err := json.Unmarshal(body, &d); err != nil || d.AccountID != "llama-lab" {
		t.Fatalf("background check_admission = %s, want llama-lab", body)
	}
}

// D14: an id two discovering routes claim is refused with the ambiguity
// message, omitted from /v1/models, and listed per route under "ambiguous".
func TestDiscoveryD14AmbiguousIDRefused(t *testing.T) {
	up := newDiscUpstream(t)
	up.setListing("lab", "shared/m", "shared/lab-only")
	up.setListing("two", "shared/m", "two-only")
	e := newDiscEnv(t, up, amendConfigYAML(up.srv.URL, `  - name: local
    discover_models: true
    discover_match: ["shared/*"]
    interactive: [llama-lab]
  - name: other
    discover_models: true
    interactive: [llama-two]
`), true)
	e.waitModels("shared/lab-only", "two-only")
	if got, want := e.models(), []string{"shared/lab-only", "two-only"}; !slices.Equal(got, want) {
		t.Fatalf("/v1/models = %v, want %v (ambiguous id omitted)", got, want)
	}
	st, body := e.do("POST", "/v1/chat/completions", chatBody("shared/m"))
	if st != http.StatusNotFound || !strings.Contains(string(body), `"model_not_found"`) ||
		!strings.Contains(string(body), "localrouter: model is claimed by more than one discovering route") {
		t.Fatalf("ambiguous id: %d %s, want 404 with the ambiguity message", st, body)
	}
	if h := up.takeHits(); len(h) != 0 {
		t.Fatalf("ambiguous id reached an upstream: %+v", h)
	}
	if st, _ := e.do("POST", "/control/v1/admit", `{"class":"interactive","model":"shared/m"}`); st != http.StatusNotFound {
		t.Fatalf("check_admission for an ambiguous id: %d, want 404", st)
	}
	_, b := e.do("GET", "/control/v1/diagnostics", "")
	var doc struct {
		Discovery struct {
			Routes []struct {
				Route     string   `json:"route"`
				Models    []string `json:"models"`
				Ambiguous []string `json:"ambiguous"`
			} `json:"routes"`
		} `json:"discovery"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	rs := doc.Discovery.Routes
	if len(rs) != 2 || !slices.Equal(rs[0].Models, []string{"shared/lab-only"}) || !slices.Equal(rs[0].Ambiguous, []string{"shared/m"}) ||
		!slices.Equal(rs[1].Models, []string{"two-only"}) || !slices.Equal(rs[1].Ambiguous, []string{"shared/m"}) {
		t.Fatalf("diagnostics routes = %+v", rs)
	}
	if strings.Contains(string(b), "shadowed") {
		t.Fatalf("diagnostics still carries the removed shadowed field: %s", b)
	}
}

// D15: a model no discovering route's discover_match admits is an immediate
// 404 with no on-demand /models call.
func TestDiscoveryD15NoRefreshForUnadmittedModel(t *testing.T) {
	up := newDiscUpstream(t)
	up.setListing("lab", "m-early")
	up.setListing("two")
	e := newDiscEnv(t, up, discConfigYAML(up.srv.URL), false) // no background poller
	st, body := e.do("POST", "/v1/chat/completions", chatBody("nomatch/model"))
	if st != http.StatusNotFound || !strings.Contains(string(body), "model_not_found") {
		t.Fatalf("unadmitted model: %d %s, want 404", st, body)
	}
	if lab, two := up.calls("lab"), up.calls("two"); lab != 0 || two != 0 {
		t.Fatalf("unadmitted model triggered /models calls (lab %d, two %d), want none", lab, two)
	}
	// An admitted miss still refreshes.
	if st, _ := e.do("POST", "/v1/chat/completions", chatBody("m-early")); st != http.StatusOK {
		t.Fatalf("admitted model after refresh: %d, want 200", st)
	}
}

// D19: with no discovering route, App.Start starts no discovery goroutine.
func TestDiscoveryD19FlagsOffStartsNoGoroutine(t *testing.T) {
	up := newDiscUpstream(t)
	flagsOff := strings.Split(discConfigYAML(up.srv.URL), "  - name: lab\n")[0]
	dir := t.TempDir()
	t.Setenv("HR_UPSTREAM_KEY", hrUpstreamKey)
	hrWriteFile(t, filepath.Join(dir, "me.key"), goldenKey)
	cfgPath := filepath.Join(dir, "config.yaml")
	hrWriteFile(t, cfgPath, flagsOff)
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	a, err := app.Build(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), app.Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	a.Start(ctx)
	time.Sleep(20 * time.Millisecond) // let any started goroutine get scheduled
	buf := make([]byte, 1<<20)
	if s := string(buf[:runtime.Stack(buf, true)]); strings.Contains(s, "internal/discovery.") {
		t.Fatalf("flags-off App runs a discovery goroutine after Start:\n%s", discoveryStacks(s))
	}

	// A reload that adds a discovering route starts polling; removing it again
	// stops the goroutine.
	hrWriteFile(t, cfgPath, discConfigYAML(up.srv.URL))
	up.setListing("lab", "m-one")
	if st, err := a.Reload(cfgPath); err != nil || !st.OK {
		t.Fatalf("reload: %+v %v", st, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for up.calls("lab") == 0 {
		if time.Now().After(deadline) {
			t.Fatal("discovering route added by reload was never polled")
		}
		time.Sleep(10 * time.Millisecond)
	}
	hrWriteFile(t, cfgPath, flagsOff)
	if st, err := a.Reload(cfgPath); err != nil || !st.OK {
		t.Fatalf("reload: %+v %v", st, err)
	}
	for {
		s := string(buf[:runtime.Stack(buf, true)])
		if !strings.Contains(s, "internal/discovery.") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("discovery goroutine alive with no discovering route:\n%s", discoveryStacks(s))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// discoveryStacks keeps the goroutine dumps that mention internal/discovery.
func discoveryStacks(all string) string {
	var out []string
	for _, g := range strings.Split(all, "\n\n") {
		if strings.Contains(g, "internal/discovery.") {
			out = append(out, g)
		}
	}
	return strings.Join(out, "\n\n")
}
