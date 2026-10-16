package control

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/hpst3r/localrouter/internal/core"
)

type fakeDiscovery struct {
	routes   map[string]core.Route
	amb      map[string]bool
	resolves []string
}

func (d *fakeDiscovery) Resolve(m string) (core.Route, bool, bool) {
	d.resolves = append(d.resolves, m)
	if d.amb[m] {
		return core.Route{}, false, true
	}
	r, ok := d.routes[m]
	return r, ok, false
}
func (d *fakeDiscovery) Diagnostics() any {
	return map[string]any{"accounts": []any{}, "routes": []any{map[string]any{"route": "lab", "models": []string{"disc/m"}}}}
}

func post(h http.Handler, path, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)))
	return rec
}

// D11: check_admission resolves a discovered id to the narrowed candidates
// (no refresh), and static models never reach discovery.
func TestAdmitResolvesDiscoveredModel(t *testing.T) {
	f := newFixture(false)
	d := &fakeDiscovery{routes: map[string]core.Route{
		"disc/m": {Name: "lab", DiscoverModels: true, Interactive: []string{"lab-b"}, Background: []string{"lab-a"}},
	}}
	f.srv.deps.Discovery = d
	for _, c := range []struct {
		class string
		want  []string
	}{{"interactive", []string{"lab-b"}}, {"background", []string{"lab-a"}}} {
		rec := post(f.h, "/control/v1/admit", `{"class":"`+c.class+`","model":"disc/m"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", c.class, rec.Code, rec.Body)
		}
		got := f.policy.dryRuns[len(f.policy.dryRuns)-1].candidates
		if !slices.Equal(got, c.want) {
			t.Fatalf("%s: dry-run candidates %v, want %v", c.class, got, c.want)
		}
	}
	if rec := post(f.h, "/control/v1/admit", `{"class":"interactive","model":"gpt-5"}`); rec.Code != http.StatusOK {
		t.Fatalf("static: %d", rec.Code)
	}
	if rec := post(f.h, "/control/v1/admit", `{"class":"interactive","model":"nope"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown: %d", rec.Code)
	}
	if !slices.Equal(d.resolves, []string{"disc/m", "disc/m", "nope"}) {
		t.Fatalf("resolves %v: static model must not reach discovery", d.resolves)
	}
}

// D13 (Amendment 1 item 1): check_admission reports "no route" for a class
// whose narrowed candidate list is empty, without a policy dry run; D14: an
// ambiguous id is refused the same way.
func TestAdmitDiscoveredClassEmptyAndAmbiguous(t *testing.T) {
	f := newFixture(false)
	f.srv.deps.Discovery = &fakeDiscovery{
		routes: map[string]core.Route{
			"disc/m": {Name: "lab", DiscoverModels: true, Interactive: []string{"lab-b"}},
		},
		amb: map[string]bool{"dup/m": true},
	}
	before := len(f.policy.dryRuns)
	rec := post(f.h, "/control/v1/admit", `{"class":"background","model":"disc/m"}`)
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), `no route for model \"disc/m\"`) {
		t.Fatalf("class-empty: %d %s, want 404 no route", rec.Code, rec.Body)
	}
	rec = post(f.h, "/control/v1/admit", `{"class":"interactive","model":"dup/m"}`)
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "more than one discovering route") {
		t.Fatalf("ambiguous: %d %s, want 404", rec.Code, rec.Body)
	}
	if n := len(f.policy.dryRuns) - before; n != 0 {
		t.Fatalf("%d policy dry runs for unresolvable models", n)
	}
	if rec := post(f.h, "/control/v1/admit", `{"class":"interactive","model":"disc/m"}`); rec.Code != http.StatusOK {
		t.Fatalf("interactive: %d %s", rec.Code, rec.Body)
	}
}

// D12: the discovery block appears only when Discovery is wired.
func TestDiagnosticsDiscoveryBlockOnlyWhenWired(t *testing.T) {
	f := newFixture(false)
	get := func() string {
		rec := httptest.NewRecorder()
		f.h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/control/v1/diagnostics", nil))
		return rec.Body.String()
	}
	if s := get(); strings.Contains(s, "discovery") {
		t.Fatalf("unwired diagnostics has a discovery block: %s", s)
	}
	f.srv.deps.Discovery = &fakeDiscovery{}
	if s := get(); !strings.Contains(s, `"discovery":{"accounts":[],"routes":[{"models":["disc/m"],"route":"lab"}]}`) {
		t.Fatalf("wired diagnostics lacks the discovery block: %s", s)
	}
}
