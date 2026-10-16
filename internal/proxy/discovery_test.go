package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
)

// fakeDiscovery resolves from a fixed table; a refresh "publishes" pending.
type fakeDiscovery struct {
	mu       sync.Mutex
	routes   map[string]core.Route
	pending  map[string]core.Route
	models   []string
	resolves []string
	refresh  int
	block    bool // Refresh blocks until ctx is done
}

func (d *fakeDiscovery) Resolve(m string) (core.Route, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.resolves = append(d.resolves, m)
	r, ok := d.routes[m]
	return r, ok
}
func (d *fakeDiscovery) Models() []string { d.mu.Lock(); defer d.mu.Unlock(); return d.models }
func (d *fakeDiscovery) Refresh(ctx context.Context) {
	d.mu.Lock()
	d.refresh++
	for k, v := range d.pending {
		d.routes[k] = v
	}
	block := d.block
	d.mu.Unlock()
	if block {
		<-ctx.Done()
	}
}

func discHarness(t *testing.T, d *fakeDiscovery) (*harness, *recordingPolicy, func() (string, string)) {
	h := newHarness(t)
	pol := &recordingPolicy{}
	h.pol = pol
	var mu sync.Mutex
	var acct, model string
	for _, id := range []string{"static-acct", "lab", "lab2"} {
		id := id
		h.upstream(id, core.ProviderOpenAICompat, func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			var doc struct {
				Model string `json:"model"`
			}
			_ = json.Unmarshal(b, &doc)
			mu.Lock()
			acct, model = id, doc.Model
			mu.Unlock()
			okJSON(w, r)
		})
	}
	h.routes = []core.Route{{Name: "A", Models: []string{"both"}, UpstreamModel: "a-backend",
		Interactive: []string{"static-acct"}, Background: []string{"static-acct"}}}
	px := h.newProxy()
	px.deps.Discovery = d
	ph := px.Handler()
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer h.handled.Add(1)
		ph.ServeHTTP(w, r)
	}))
	t.Cleanup(h.srv.Close)
	return h, pol, func() (string, string) { mu.Lock(); defer mu.Unlock(); return acct, model }
}

// D6: a statically listed model is served by its static route with that
// route's rewrite; discovery is never consulted for it.
func TestDiscoveryStaticWins(t *testing.T) {
	d := &fakeDiscovery{routes: map[string]core.Route{
		"both": {Name: "B", DiscoverModels: true, Interactive: []string{"lab"}, Background: []string{"lab"}},
	}}
	h, _, last := discHarness(t, d)
	resp := h.post(pathChat, clientKey, `{"model":"both","messages":[]}`, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if a, m := last(); a != "static-acct" || m != "a-backend" {
		t.Fatalf("served by %s with model %s, want static-acct/a-backend", a, m)
	}
	if len(d.resolves) != 0 || d.refresh != 0 {
		t.Fatalf("discovery consulted for a static model: %v / %d", d.resolves, d.refresh)
	}
}

// D4/D5 at the proxy: the narrowed candidate list is exactly what policy sees,
// and the forwarded model is the client's.
func TestDiscoveryNarrowedCandidatesReachPolicy(t *testing.T) {
	d := &fakeDiscovery{routes: map[string]core.Route{
		"disc/m": {Name: "lab", DiscoverModels: true, Interactive: []string{"lab2"}, Background: nil},
	}}
	h, pol, last := discHarness(t, d)
	resp := h.post(pathChat, clientKey, `{"model":"disc/m","messages":[]}`, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if c := pol.take(); len(c) != 1 || !slices.Equal(c[0], []string{"lab2"}) {
		t.Fatalf("policy candidates %v, want [[lab2]]", c)
	}
	if a, m := last(); a != "lab2" || m != "disc/m" {
		t.Fatalf("served by %s model %s", a, m)
	}
	// Background has no listing account on this route: policy sees no
	// candidate, never one from outside the route.
	resp = h.post(pathChat, bgKey, `{"model":"disc/m","messages":[]}`, nil)
	resp.Body.Close()
	if c := pol.take(); len(c) != 1 || len(c[0]) != 0 {
		t.Fatalf("background candidates %v, want one empty list", c)
	}
	if d.refresh != 0 {
		t.Fatal("refresh on a hit")
	}
}

// D9 at the proxy: a miss refreshes once and retries once; a still-unknown
// model is today's 404.
func TestDiscoveryMissRefreshesAndRetriesOnce(t *testing.T) {
	d := &fakeDiscovery{routes: map[string]core.Route{}, pending: map[string]core.Route{
		"late": {Name: "lab", DiscoverModels: true, Interactive: []string{"lab"}, Background: []string{"lab"}},
	}}
	h, _, last := discHarness(t, d)
	resp := h.post(pathChat, clientKey, `{"model":"late","messages":[]}`, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || d.refresh != 1 || !slices.Equal(d.resolves, []string{"late", "late"}) {
		t.Fatalf("status %d refresh %d resolves %v", resp.StatusCode, d.refresh, d.resolves)
	}
	if a, m := last(); a != "lab" || m != "late" {
		t.Fatalf("served by %s model %s", a, m)
	}
	resp = h.post(pathChat, clientKey, `{"model":"never","messages":[]}`, nil)
	b := decodeErr(t, resp)
	if resp.StatusCode != http.StatusNotFound || b.Error.Type != "model_not_found" || d.refresh != 2 {
		t.Fatalf("unknown: %d %+v refresh %d", resp.StatusCode, b, d.refresh)
	}
}

// The request's cancellation ends its refresh wait.
func TestDiscoveryRefreshWaitEndsWithRequest(t *testing.T) {
	d := &fakeDiscovery{routes: map[string]core.Route{}, block: true}
	h, _, _ := discHarness(t, d)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := h.doCtx(ctx, pathChat, clientKey, `{"model":"x","messages":[]}`)
	if err == nil {
		t.Fatal("expected the cancelled request to fail")
	}
	h.waitHandled(1)
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("handler outlived the request by %v", el)
	}
}

func TestDiscoveryModelsMerged(t *testing.T) {
	d := &fakeDiscovery{routes: map[string]core.Route{}, models: []string{"zeta", "both", "alpha"}}
	h, _, _ := discHarness(t, d)
	req, _ := http.NewRequest(http.MethodGet, h.srv.URL+pathModels, nil)
	req.Header.Set("Authorization", "Bearer "+clientKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	want := `{"object":"list","data":[{"id":"alpha","object":"model","owned_by":"localrouter"},{"id":"both","object":"model","owned_by":"localrouter"},{"id":"zeta","object":"model","owned_by":"localrouter"}]}` + "\n"
	if string(b) != want {
		t.Fatalf("/v1/models = %s", b)
	}
}
