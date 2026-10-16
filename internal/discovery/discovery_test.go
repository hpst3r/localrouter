package discovery

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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
)

const secretKey = "sk-upstream-SENTINEL-4242"

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) Add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type fakeCreds struct{ fail atomic.Bool }

func (c *fakeCreds) Credential(_ context.Context, id string) (core.Credential, error) {
	if c.fail.Load() {
		return core.Credential{}, errors.New("no key for " + secretKey)
	}
	h := http.Header{}
	h.Set("Authorization", "Bearer "+secretKey)
	return core.Credential{Headers: h}, nil
}
func (c *fakeCreds) Invalidate(string) {}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *syncBuf) Write(p []byte) (int, error) { b.mu.Lock(); defer b.mu.Unlock(); return b.b.Write(p) }
func (b *syncBuf) String() string              { b.mu.Lock(); defer b.mu.Unlock(); return b.b.String() }

// upstream serves GET /v1/models with a swappable handler and counts calls.
type upstream struct {
	srv   *httptest.Server
	calls atomic.Int64
	mu    sync.Mutex
	h     http.HandlerFunc
	auth  atomic.Value
}

func newUpstream(t *testing.T) *upstream {
	u := &upstream{}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		u.calls.Add(1)
		u.auth.Store(r.Header.Get("Authorization"))
		u.mu.Lock()
		h := u.h
		u.mu.Unlock()
		h(w, r)
	}))
	t.Cleanup(u.srv.Close)
	u.set(listingBody())
	return u
}

func (u *upstream) set(h http.HandlerFunc) { u.mu.Lock(); u.h = h; u.mu.Unlock() }

func listingBody(ids ...string) http.HandlerFunc {
	var parts []string
	for _, id := range ids {
		parts = append(parts, fmt.Sprintf(`{"id":%q,"object":"model","status":{"value":"loaded"}}`, id))
	}
	body := `{"object":"list","data":[` + strings.Join(parts, ",") + `]}`
	return func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }
}

type rig struct {
	t     *testing.T
	m     *Manager
	clock *fakeClock
	creds *fakeCreds
	logs  *syncBuf
	ups   map[string]*upstream
	accts map[string]core.Account
}

func newRig(t *testing.T, opts Options, ids ...string) *rig {
	r := &rig{t: t, clock: &fakeClock{t: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}, creds: &fakeCreds{},
		logs: &syncBuf{}, ups: map[string]*upstream{}, accts: map[string]core.Account{}}
	for _, id := range ids {
		u := newUpstream(t)
		r.ups[id] = u
		r.accts[id] = core.Account{ID: id, Provider: core.ProviderOpenAICompat, BaseURL: u.srv.URL + "/v1"}
	}
	opts.Clock = r.clock
	opts.Logger = slog.New(slog.NewTextHandler(r.logs, nil))
	r.m = New(r.creds, opts)
	t.Cleanup(r.m.Close)
	return r
}

func discRoute(name string, interval time.Duration, accts ...string) core.Route {
	return core.Route{Name: name, Interactive: accts, Background: accts, DiscoverModels: true, DiscoverInterval: interval}
}

func (r *rig) target(routes ...core.Route) { r.m.SetTargets(r.accts, routes) }
func (r *rig) pollDue()                    { r.m.PollDue(context.Background()) }

func (r *rig) listing(id string) []string {
	r.m.mu.RLock()
	defer r.m.mu.RUnlock()
	if e := r.m.targets[id]; e != nil {
		return e.sorted
	}
	return nil
}

func (r *rig) errClass(id string) string {
	r.m.mu.RLock()
	defer r.m.mu.RUnlock()
	if e := r.m.targets[id]; e != nil {
		return e.errClass
	}
	return "<none>"
}

func TestParseListingValidation(t *testing.T) {
	long := strings.Repeat("a", 257)
	ok256 := strings.Repeat("b", 256)
	body := `{"object":"list","data":[` +
		`{"id":"good/one","status":{"value":"loaded"},"architecture":{"x":1}},` +
		`{"id":"good/one"},` + // duplicate collapses (not dropped)
		`{"id":"has space"},{"id":"tab\tid"},{"id":"ctl\u0007id"},{"id":"nl\nid"},` +
		`{"id":"bad\ud800surrogate"},{"id":""},{"id":"` + long + `"},{"id":"` + ok256 + `"},` +
		`{"id":42},{"name":"no-id"},"not-an-object",{"id":"zz-last"}]}`
	l, err := parseListing([]byte(body), 1000)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{ok256, "good/one", "zz-last"}; !slices.Equal(l.ids, want) {
		t.Fatalf("ids = %q, want %q", l.ids, want)
	}
	if l.dropped != 10 {
		t.Fatalf("dropped = %d, want 10", l.dropped)
	}
	// Raw invalid UTF-8 inside an id is rejected (encoding/json would map it
	// to U+FFFD silently).
	raw := append([]byte(`{"data":[{"id":"x`), 0xff, 0xfe)
	raw = append(raw, []byte(`"},{"id":"fine"}]}`)...)
	l, err = parseListing(raw, 1000)
	if err != nil || !slices.Equal(l.ids, []string{"fine"}) || l.dropped != 1 {
		t.Fatalf("invalid utf-8: %q %d %v", l.ids, l.dropped, err)
	}
	for _, bad := range []string{`not json`, `[]`, `null`, `{}`, `{"data":null}`, `{"data":{}}`, `{"data":[]} trailing`, ``} {
		if _, err := parseListing([]byte(bad), 1000); err == nil {
			t.Fatalf("parseListing(%q) accepted a malformed body", bad)
		}
	}
	if l, err := parseListing([]byte(`{"data":[]}`), 1000); err != nil || len(l.ids) != 0 {
		t.Fatalf("empty listing: %v %v", l, err)
	}
}

func TestParseListingCapsIDs(t *testing.T) {
	var parts []string
	for i := 0; i < 1500; i++ {
		parts = append(parts, fmt.Sprintf(`{"id":"m-%04d"}`, i), fmt.Sprintf(`{"id":"m-%04d"}`, i))
	}
	l, err := parseListing([]byte(`{"data":[`+strings.Join(parts, ",")+`]}`), 1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(l.ids) != 1000 || l.dropped != 500 || l.ids[999] != "m-0999" {
		t.Fatalf("kept %d dropped %d last %q, want 1000/500/m-0999", len(l.ids), l.dropped, l.ids[len(l.ids)-1])
	}
}

// D8: hostile listings never crash, hang, exceed the bounds, or leak the body,
// URL or credential into logs.
func TestHostileListings(t *testing.T) {
	r := newRig(t, Options{Timeout: 200 * time.Millisecond}, "a")
	u := r.ups["a"]
	r.target(discRoute("r", time.Minute, "a"))
	u.set(listingBody("keep/me"))
	r.pollDue()
	if got := r.listing("a"); !slices.Equal(got, []string{"keep/me"}) {
		t.Fatalf("baseline listing %v", got)
	}
	if got := u.auth.Load(); got != "Bearer "+secretKey {
		t.Fatalf("poll Authorization = %v, want the account credential", got)
	}
	redirectHit := atomic.Bool{}
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		redirectHit.Store(true)
		listingBody("evil/redirected")(w, nil)
	}))
	defer target.Close()
	const marker = "BODY-MARKER-77"
	huge := `{"data":[{"id":"` + marker + `"}],"pad":"` + strings.Repeat("x", 1<<20) + `"}`
	cases := []struct {
		name  string
		h     http.HandlerFunc
		class string
	}{
		{"too large", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(huge)) }, ErrClassTooLarge},
		{"non-json", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("<html>" + marker)) }, ErrClassMalformed},
		{"redirect", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, target.URL+"/v1/models", http.StatusFound)
		}, ErrClassStatus},
		{"500", func(w http.ResponseWriter, _ *http.Request) { http.Error(w, marker, 500) }, ErrClassStatus},
		{"slow", func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}
		}, ErrClassTransport},
		{"slow body", func(w http.ResponseWriter, r *http.Request) {
			w.(http.Flusher).Flush()
			_, _ = w.Write([]byte(`{"data":[`))
			w.(http.Flusher).Flush()
			select {
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}
		}, ErrClassTransport},
	}
	for _, c := range cases {
		u.set(c.h)
		r.clock.Add(time.Minute)
		start := time.Now()
		r.pollDue()
		if el := time.Since(start); el > 2*time.Second {
			t.Fatalf("%s: poll took %v, want bounded by the timeout", c.name, el)
		}
		if got := r.errClass("a"); got != c.class {
			t.Fatalf("%s: error class %q, want %q", c.name, got, c.class)
		}
		if got := r.listing("a"); !slices.Equal(got, []string{"keep/me"}) {
			t.Fatalf("%s: listing %v, want last good kept", c.name, got)
		}
	}
	if redirectHit.Load() {
		t.Fatal("a redirect was followed")
	}
	r.creds.fail.Store(true)
	r.clock.Add(time.Minute)
	r.pollDue()
	if got := r.errClass("a"); got != ErrClassCredential {
		t.Fatalf("credential failure class %q", got)
	}
	logs := r.logs.String()
	for _, leak := range []string{u.srv.URL, "127.0.0.1", secretKey, marker, "evil"} {
		if strings.Contains(logs, leak) {
			t.Fatalf("logs leak %q:\n%s", leak, logs)
		}
	}
}

// Logging: at most one line per error-class change per account.
func TestPollFailureLogsOncePerClassChange(t *testing.T) {
	r := newRig(t, Options{}, "a")
	u := r.ups["a"]
	r.target(discRoute("r", time.Minute, "a"))
	u.set(func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "x", 503) })
	for i := 0; i < 3; i++ {
		r.pollDue()
		r.clock.Add(time.Minute)
	}
	u.set(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("nope")) })
	for i := 0; i < 3; i++ {
		r.pollDue()
		r.clock.Add(time.Minute)
	}
	u.set(listingBody("ok"))
	r.pollDue()
	logs := r.logs.String()
	if n := strings.Count(logs, "error_class=status"); n != 1 {
		t.Fatalf("status logged %d times:\n%s", n, logs)
	}
	if n := strings.Count(logs, "error_class=malformed"); n != 1 {
		t.Fatalf("malformed logged %d times:\n%s", n, logs)
	}
	if n := strings.Count(logs, "recovered"); n != 1 {
		t.Fatalf("recovery logged %d times:\n%s", n, logs)
	}
}

// D7: a poll failure keeps the last good listing; a later success replaces it.
func TestPollFailureKeepsLastGood(t *testing.T) {
	r := newRig(t, Options{}, "a")
	u := r.ups["a"]
	r.target(discRoute("r", time.Minute, "a"))
	u.set(listingBody("m1", "m2"))
	r.pollDue()
	u.set(func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "down", 502) })
	r.clock.Add(time.Minute)
	r.pollDue()
	if got := r.listing("a"); !slices.Equal(got, []string{"m1", "m2"}) {
		t.Fatalf("after failure listing %v, want [m1 m2]", got)
	}
	u.set(listingBody("m3"))
	r.clock.Add(time.Minute)
	r.pollDue()
	if got := r.listing("a"); !slices.Equal(got, []string{"m3"}) {
		t.Fatalf("after recovery listing %v, want [m3]", got)
	}
	if got := r.errClass("a"); got != "" {
		t.Fatalf("error class after recovery %q", got)
	}
}

// Scheduling follows the injected clock: nothing is polled before the
// interval elapses, and the smallest interval among routes wins.
func TestPollScheduleAndRetarget(t *testing.T) {
	r := newRig(t, Options{}, "a", "b")
	r.target(discRoute("slow", 10*time.Minute, "a"), discRoute("fast", time.Minute, "a", "b"))
	r.pollDue()
	if r.ups["a"].calls.Load() != 1 || r.ups["b"].calls.Load() != 1 {
		t.Fatal("first pass did not poll every target")
	}
	r.clock.Add(59 * time.Second)
	r.pollDue()
	if r.ups["a"].calls.Load() != 1 {
		t.Fatal("polled before the interval elapsed")
	}
	r.clock.Add(time.Second)
	r.pollDue()
	if r.ups["a"].calls.Load() != 2 {
		t.Fatal("a not polled at the smallest interval (1m)")
	}
	r.ups["a"].set(listingBody("kept"))
	r.clock.Add(time.Minute)
	r.pollDue()

	// Re-target: drop "fast" (b unreferenced; a back to 10m).
	r.target(discRoute("slow", 10*time.Minute, "a"))
	if got := r.listing("a"); !slices.Equal(got, []string{"kept"}) {
		t.Fatalf("listing of a still-referenced account lost on re-target: %v", got)
	}
	if r.listing("b") != nil || r.errClass("b") != "<none>" {
		t.Fatal("unreferenced account kept")
	}
	before := r.ups["a"].calls.Load()
	r.clock.Add(9 * time.Minute)
	r.pollDue()
	if r.ups["a"].calls.Load() != before || r.ups["b"].calls.Load() != 3 {
		t.Fatal("re-interval or removal not honoured")
	}
	r.clock.Add(time.Minute)
	r.pollDue()
	if r.ups["a"].calls.Load() != before+1 {
		t.Fatal("a not polled at its new interval")
	}
}

// D9 at the manager level: single-flight, spaced by the injected clock, wait
// bounded, and a caller's cancellation never cancels the shared refresh.
func TestRefreshSingleFlightSpacingAndWait(t *testing.T) {
	r := newRig(t, Options{RefreshWait: 100 * time.Millisecond}, "a")
	r.target(discRoute("r", time.Hour, "a"))
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); r.m.Refresh(context.Background()) }()
	}
	wg.Wait()
	if n := r.ups["a"].calls.Load(); n != 1 {
		t.Fatalf("100 concurrent refreshes made %d calls, want 1", n)
	}
	r.clock.Add(29 * time.Second)
	r.m.Refresh(context.Background())
	if n := r.ups["a"].calls.Load(); n != 1 {
		t.Fatalf("refresh inside the 30s window made a call (%d)", n)
	}
	r.clock.Add(time.Second)

	// A slow refresh: the caller's wait is bounded by RefreshWait, and a
	// cancelled caller returns at once while the refresh still completes.
	gate := make(chan struct{})
	r.ups["a"].set(func(w http.ResponseWriter, _ *http.Request) { <-gate; listingBody("late")(w, nil) })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	r.m.Refresh(ctx)
	if el := time.Since(start); el > 50*time.Millisecond {
		t.Fatalf("cancelled caller waited %v", el)
	}
	start = time.Now()
	r.m.Refresh(context.Background()) // joins the in-flight refresh
	if el := time.Since(start); el < 90*time.Millisecond || el > time.Second {
		t.Fatalf("joined wait %v, want ~RefreshWait", el)
	}
	close(gate)
	deadline := time.Now().Add(2 * time.Second)
	for !slices.Equal(r.listing("a"), []string{"late"}) {
		if time.Now().After(deadline) {
			t.Fatal("shared refresh was cancelled with its first caller")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if n := r.ups["a"].calls.Load(); n != 2 {
		t.Fatalf("calls = %d, want 2 (one per window)", n)
	}
}

func TestResolverNarrowsMatchesAndStaticWins(t *testing.T) {
	r := newRig(t, Options{}, "a", "b", "c")
	routes := []core.Route{
		{Name: "static", Models: []string{"shared/static"}, Interactive: []string{"c"}, Background: []string{"c"}},
		{Name: "first", Interactive: []string{"a", "b"}, Background: []string{"b"}, DiscoverModels: true, DiscoverInterval: time.Minute, DiscoverMatch: []string{"x/*"}},
		{Name: "second", Interactive: []string{"b", "a"}, Background: []string{"a"}, DiscoverModels: true, DiscoverInterval: time.Minute},
	}
	r.ups["a"].set(listingBody("x/a", "x/both", "y/a", "shared/static"))
	r.ups["b"].set(listingBody("x/both", "y/b"))
	r.ups["c"].set(listingBody("c-only"))
	r.target(routes...)
	r.pollDue()
	res := r.m.Resolver(routes)
	if res == nil {
		t.Fatal("nil resolver with discovering routes")
	}
	check := func(model, route string, in, bg []string) {
		t.Helper()
		rt, ok := res.Resolve(model)
		if route == "" {
			if ok {
				t.Fatalf("%s resolved to %s, want none", model, rt.Name)
			}
			return
		}
		if !ok || rt.Name != route || !slices.Equal(rt.Interactive, in) || !slices.Equal(rt.Background, bg) {
			t.Fatalf("%s -> %v %+v, want %s %v %v", model, ok, rt, route, in, bg)
		}
	}
	check("x/a", "first", []string{"a"}, nil)
	check("x/both", "first", []string{"a", "b"}, []string{"b"})
	check("y/a", "second", []string{"a"}, []string{"a"}) // x/* filters it out of "first"
	check("y/b", "second", []string{"b"}, nil)
	check("shared/static", "", nil, nil) // static wins (D6): the caller's map serves it
	check("c-only", "", nil, nil)        // c is not a discovering candidate
	check("nope", "", nil, nil)
	if got, want := res.Models(), []string{"x/a", "x/both", "y/a", "y/b"}; !slices.Equal(got, want) {
		t.Fatalf("Models = %v, want %v", got, want)
	}
	d := res.Snapshot()
	if len(d.Routes) != 2 || !slices.Equal(d.Routes[0].Models, []string{"x/a", "x/both"}) ||
		!slices.Equal(d.Routes[1].Models, []string{"y/a", "y/b"}) ||
		!slices.Equal(d.Routes[1].Shadowed, []string{"shared/static", "x/a", "x/both"}) {
		t.Fatalf("route diagnostics %+v", d.Routes)
	}
	if len(d.Accounts) != 2 || d.Accounts[0].Account != "a" || d.Accounts[0].ModelCount != 4 ||
		!slices.Equal(d.Accounts[0].Routes, []string{"first", "second"}) || d.Accounts[0].LastSuccessAt == nil {
		t.Fatalf("account diagnostics %+v", d.Accounts)
	}
	if r.m.Resolver(routes[:1]) != nil {
		t.Fatal("resolver for static-only routes must be nil")
	}
}

// Race: resolves, listings and diagnostics run concurrently with polls and
// re-targets (run under -race).
func TestConcurrentResolveDuringPollAndRetarget(t *testing.T) {
	r := newRig(t, Options{}, "a", "b")
	r.ups["a"].set(listingBody("m1", "m2"))
	r.ups["b"].set(listingBody("m2", "m3"))
	full := []core.Route{discRoute("r1", time.Minute, "a"), discRoute("r2", time.Minute, "a", "b")}
	r.target(full...)
	r.pollDue()
	res := r.m.Resolver(full)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				res.Resolve("m2")
				res.Models()
				res.Snapshot()
			}
		}()
	}
	for i := 0; i < 50; i++ {
		r.clock.Add(time.Minute)
		if i%2 == 0 {
			r.target(full[:1]...)
		} else {
			r.target(full...)
		}
		r.pollDue()
		r.m.Refresh(context.Background())
	}
	close(stop)
	wg.Wait()
}

func TestStartCloseNoLeakAndRefreshAfterClose(t *testing.T) {
	base := runtime.NumGoroutine()
	r := newRig(t, Options{Tick: 5 * time.Millisecond}, "a")
	r.ups["a"].set(listingBody("m1"))
	r.target(discRoute("r", time.Minute, "a"))
	r.m.Start(context.Background())
	deadline := time.Now().Add(2 * time.Second)
	for r.listing("a") == nil {
		if time.Now().After(deadline) {
			t.Fatal("background loop never polled")
		}
		time.Sleep(5 * time.Millisecond)
	}
	r.m.Close()
	r.m.Close() // idempotent
	before := r.ups["a"].calls.Load()
	r.clock.Add(time.Hour)
	r.m.Refresh(context.Background())
	r.m.Start(context.Background())
	if r.ups["a"].calls.Load() != before {
		t.Fatal("manager polled after Close")
	}
	r.ups["a"].srv.Close()
	deadline = time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > base {
		if time.Now().After(deadline) {
			buf := make([]byte, 1<<16)
			t.Fatalf("goroutine leak: %d > %d\n%s", runtime.NumGoroutine(), base, buf[:runtime.Stack(buf, true)])
		}
		time.Sleep(5 * time.Millisecond)
	}
}
