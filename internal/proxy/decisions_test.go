package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/budget"
	"github.com/hpst3r/localrouter/internal/core"
	"github.com/hpst3r/localrouter/internal/routing"
)

// Typed-decision acceptance tests (docs/SPEC.md "Typed decisions", T1..T16).
// Fakes replay the saved OpenRouter examples: the Decisions API reference
// (design/or-decisions-ref.md lines 143-211) and the System One response
// (design/or-typesafe-sdk.md lines 127-136).

const (
	decPin = "typesafe/jev-1.13"
	// decClientBody is the Jev tutorial request with the client-facing model.
	decClientBody = `{"model":"jev","state":{"customer_tier":"enterprise","ticket":"My checkout page shows a blank screen."},` +
		`"questions":{"is_bug":{"type":"noul","instructions":"Is the customer reporting a software defect?",` +
		`"criteria":{"true":"broken behavior","false":"a question"}},` +
		`"team":{"type":"choice","instructions":"Which team?","criteria":{"payments":"p","frontend":"f","account":"a"}},` +
		`"urgency":{"type":"score","instructions":"How urgent?","criteria":["later","this week","now"]}}}`
	// decAnswer is the reference 200 response, byte for byte as relayed.
	decAnswer = `{"answers":{"is_bug":{"noul":0.96,"type":"noul"},"team":{"choice":"payments","confidence":0.75,` +
		`"probabilities":{"account":0,"frontend":0.16,"payments":0.84},"type":"choice"},"urgency":{"confidence":0.99,` +
		`"legend":{"0":"Can wait","1":"This week","2":"Now"},"probabilities":{"0":0,"1":0.01,"2":0.99},"score":1.99,"type":"score"}},` +
		`"id":"gen-dec-1789738314-X5e5eKGQdvR9rblyX250","model":"typesafe/jev-1.13-20260917","provider":"TypeSafe",` +
		`"usage":{"cost":0.000019992,"input_tokens":476,"output_tokens":70}}`
	noulOnly = `{"model":"jev","state":"s","questions":{"q":{"type":"noul","instructions":"i"}}}`
)

// decHit is one upstream request seen by a fake.
type decHit struct {
	path   string
	header http.Header
	body   []byte
}

// decUpstream is a fake OpenRouter account answering with fn.
type decUpstream struct {
	mu   sync.Mutex
	hits []decHit
}

func (u *decUpstream) all() []decHit {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]decHit(nil), u.hits...)
}

func (u *decUpstream) handler(fn http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		u.mu.Lock()
		u.hits = append(u.hits, decHit{path: r.URL.Path, header: r.Header.Clone(), body: b})
		u.mu.Unlock()
		fn(w, r)
	}
}

func answerOK(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, decAnswer)
}

func decSpec(proto string, qt ...string) core.UpstreamSpec {
	if len(qt) == 0 {
		qt = []string{routing.QuestionNoul, routing.QuestionChoice, routing.QuestionScore}
	}
	return core.UpstreamSpec{UpstreamModel: decPin, Protocols: []string{proto}, InputModalities: []string{"text"}, QuestionTypes: qt}
}

// decHarness wires one decision route "jev" over openrouter accounts ids,
// each served by fn, plus a legacy chat route "chat" on account "plain".
func decHarness(t *testing.T, proto string, fn http.HandlerFunc, ids ...string) (*harness, map[string]*decUpstream) {
	h := newHarness(t)
	ups := map[string]*decUpstream{}
	specs := map[string]core.UpstreamSpec{}
	for _, id := range ids {
		u := &decUpstream{}
		ups[id] = u
		h.upstream(id, core.ProviderOpenRouter, u.handler(fn))
		specs[id] = decSpec(proto)
	}
	plain := &decUpstream{}
	ups["plain"] = plain
	h.upstream("plain", core.ProviderOpenRouter, plain.handler(okJSON))
	h.routes = []core.Route{
		{Name: "jev", Models: []string{"jev"}, Interactive: ids, Background: ids, Upstreams: specs},
		{Name: "chat", Models: []string{"chat"}, Interactive: []string{"plain"}, Background: []string{"plain"}},
	}
	return h, ups
}

func egress(ups map[string]*decUpstream) int {
	n := 0
	for _, u := range ups {
		n += len(u.all())
	}
	return n
}

func errType(t *testing.T, b string) string {
	t.Helper()
	var e errorBody
	if err := json.Unmarshal([]byte(b), &e); err != nil {
		t.Fatalf("decode error body %q: %v", b, err)
	}
	return e.Error.Type
}

// TestT1FlagsOffDecisionPaths404: with no decision route both paths answer
// exactly like any unknown path, and chat is unchanged.
func TestT1FlagsOffDecisionPaths404(t *testing.T) {
	h := newHarness(t)
	h.upstream("a", core.ProviderOpenRouter, okJSON)
	singleRoute(h, "a")
	h.start()
	unknown := h.post("/v1/nope", clientKey, noulOnly, nil)
	want := readAll(t, unknown)
	for _, p := range []string{pathSystemOne, pathDecisions} {
		resp := h.post(p, clientKey, noulOnly, nil)
		got := readAll(t, resp)
		if resp.StatusCode != http.StatusNotFound || string(got) != string(want) {
			t.Fatalf("%s: %d %s, want 404 %s", p, resp.StatusCode, got, want)
		}
	}
	if resp := h.post("/v1/responses", clientKey, respBody, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("responses status %d", resp.StatusCode)
	}
}

// TestT3SystemOneHappyPath: only model is rewritten, credentials replace the
// client key, non-allowlisted headers are dropped, the answer is relayed
// byte-identical, and the ledger row carries usage and the reported cost.
func TestT3SystemOneHappyPath(t *testing.T) {
	h, ups := decHarness(t, routing.ProtocolSystemOne, answerOK, "or1")
	h.start()
	resp := h.post(pathSystemOne, clientKey, decClientBody, map[string]string{
		"TypeSafe-Danger-Zone-Images": "true",
		"X-Session-Id":                "sess-1",
		"Cookie":                      "c=1",
		"X-LocalRouter-Task":          "triage",
	})
	got := readAll(t, resp)
	if resp.StatusCode != http.StatusOK || string(got) != decAnswer {
		t.Fatalf("relay = %d %s", resp.StatusCode, got)
	}
	hits := ups["or1"].all()
	if len(hits) != 1 {
		t.Fatalf("upstream hits = %d", len(hits))
	}
	hit := hits[0]
	if hit.path != "/v1/systemone" {
		t.Fatalf("upstream path = %q", hit.path)
	}
	if a := hit.header.Get("Authorization"); a != "Bearer "+tokenStem+"or1-0" {
		t.Fatalf("upstream Authorization = %q", a)
	}
	for _, hd := range []string{"TypeSafe-Danger-Zone-Images", "Cookie", "X-Localrouter-Task"} {
		if v := hit.header.Get(hd); v != "" {
			t.Fatalf("header %s forwarded: %q", hd, v)
		}
	}
	if hit.header.Get("X-Session-Id") != "sess-1" {
		t.Fatal("x-session-id not forwarded on a decision route")
	}
	var sent, orig map[string]any
	_ = json.Unmarshal(hit.body, &sent)
	_ = json.Unmarshal([]byte(decClientBody), &orig)
	if sent["model"] != decPin {
		t.Fatalf("upstream model = %v", sent["model"])
	}
	orig["model"] = decPin
	if !reflect.DeepEqual(sent, orig) {
		t.Fatalf("upstream body changed beyond model:\n%s", hit.body)
	}
	rec := h.waitRows(1)[0]
	if rec.Model != "jev" || rec.UpstreamModel != decPin || rec.PricingModel != decPin || rec.Route != "jev" ||
		rec.Provider != core.ProviderOpenRouter || rec.Status != 200 || rec.Task != "triage" {
		t.Fatalf("ledger row = %+v", rec)
	}
	if !rec.UsageKnown || rec.Usage.InputTokens != 476 || rec.Usage.OutputTokens != 70 {
		t.Fatalf("usage = %+v known=%v", rec.Usage, rec.UsageKnown)
	}
	if rec.ReportedCostUSD == nil || *rec.ReportedCostUSD != 0.000019992 {
		t.Fatalf("reported cost = %v", paCostString(rec.ReportedCostUSD))
	}
	if strings.Contains(h.logs.String(), "checkout") || strings.Contains(h.logs.String(), "payments") {
		t.Fatal("request or response content logged")
	}
}

// TestT4DecisionsHappyPath: /v1/decisions goes to <base>/alpha/decisions with
// the optional Decisions members passed through.
func TestT4DecisionsHappyPath(t *testing.T) {
	h, ups := decHarness(t, routing.ProtocolDecisions, answerOK, "or1")
	h.start()
	body := `{"model":"jev","state":"s","provider":{"sort":"price"},"session_id":"abc","user":"u1",` +
		`"questions":{"q":{"type":"noul","instructions":"i"}}}`
	resp := h.post(pathDecisions, clientKey, body, map[string]string{"X-Session-Id": "sess-2"})
	if got := readAll(t, resp); resp.StatusCode != http.StatusOK || string(got) != decAnswer {
		t.Fatalf("relay = %d %s", resp.StatusCode, got)
	}
	hit := ups["or1"].all()[0]
	if hit.path != "/alpha/decisions" {
		t.Fatalf("upstream path = %q", hit.path)
	}
	if hit.header.Get("X-Session-Id") != "sess-2" {
		t.Fatal("x-session-id not forwarded")
	}
	var sent map[string]any
	_ = json.Unmarshal(hit.body, &sent)
	if sent["model"] != decPin || sent["session_id"] != "abc" || sent["user"] != "u1" || sent["provider"] == nil {
		t.Fatalf("upstream body = %s", hit.body)
	}
	if got := decisionsURL("https://openrouter.ai/api/v1"); got != "https://openrouter.ai/api/alpha/decisions" {
		t.Fatalf("decisionsURL = %q", got)
	}
}

// TestT5UnknownUsage: missing, malformed or negative usage is unknown, never
// zero; a missing cost is nil.
func TestT5UnknownUsage(t *testing.T) {
	for name, payload := range map[string]string{
		"missing":  `{"id":"x","model":"m","answers":{}}`,
		"string":   `{"id":"x","model":"m","answers":{},"usage":{"input_tokens":"476","output_tokens":0}}`,
		"negative": `{"id":"x","model":"m","answers":{},"usage":{"input_tokens":-1,"output_tokens":0}}`,
	} {
		t.Run(name, func(t *testing.T) {
			h, _ := decHarness(t, routing.ProtocolSystemOne, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, payload)
			}, "or1")
			fb := newFakeBudget()
			h.startBudget(fb)
			resp := h.post(pathSystemOne, clientKey, noulOnly, nil)
			readAll(t, resp)
			rec := h.waitRows(1)[0]
			if rec.UsageKnown || rec.Usage != (core.Usage{}) || rec.ReportedCostUSD != nil {
				t.Fatalf("row = %+v", rec)
			}
			fb.waitSettles(t, 1)
		})
	}
}

// TestT6T7T8RejectedBeforeEgress: strict-JSON, bounds and unsupported-feature
// rejections are 400 with their type and never reach the upstream.
func TestT6T7T8RejectedBeforeEgress(t *testing.T) {
	big := make([]string, 65)
	for i := range big {
		big[i] = fmt.Sprintf(`"q%d":{"type":"noul","instructions":"i"}`, i)
	}
	cases := []struct {
		name, path, body string
		hdr              map[string]string
		typ              string
	}{
		{"T6 duplicate question", pathSystemOne, `{"model":"jev","state":"s","questions":{"a":{"type":"noul"},"a":{"type":"noul"}}}`, nil, "invalid_request_error"},
		{"T6 case-variant question", pathSystemOne, `{"model":"jev","state":"s","questions":{"A":{"type":"noul"},"a":{"type":"noul"}}}`, nil, "invalid_request_error"},
		{"T6 Questions variant", pathSystemOne, `{"model":"jev","state":"s","Questions":{"a":{"type":"noul"}}}`, nil, "invalid_request_error"},
		{"T6 duplicate top-level", pathDecisions, `{"model":"jev","state":"s","questions":{},"questions":{"a":{"type":"noul"}}}`, nil, "invalid_request_error"},
		{"T6 empty questions", pathSystemOne, `{"model":"jev","state":"s","questions":{}}`, nil, "invalid_request_error"},
		{"T7 65 questions", pathSystemOne, `{"model":"jev","state":"s","questions":{` + strings.Join(big, ",") + `}}`, nil, errTypeDecisionBounds},
		{"T7 11 score levels", pathDecisions, `{"model":"jev","state":"s","questions":{"s":{"type":"score","instructions":"i","criteria":[1,2,3,4,5,6,7,8,9,10,11]}}}`, nil, errTypeDecisionBounds},
		{"T7 nesting 65", pathSystemOne, `{"model":"jev","state":` + strings.Repeat("[", 65) + strings.Repeat("]", 65) + `,"questions":{"q":{"type":"noul"}}}`, nil, errTypeDecisionBounds},
		{"T8 stream true", pathSystemOne, `{"model":"jev","stream":true,"state":"s","questions":{"q":{"type":"noul"}}}`, nil, errTypeDecisionUnsupported},
		{"T8 box", pathSystemOne, `{"model":"jev","state":"s","questions":{"b":{"type":"box","instructions":"the cat"}}}`, nil, errTypeDecisionUnsupported},
		{"T8 assets", pathSystemOne, `{"model":"jev","state":"s","assets":{},"questions":{"q":{"type":"noul"}}}`, nil, errTypeDecisionUnsupported},
		{"T8 image state", pathDecisions, `{"model":"jev","state":[{"type":"image_url","image_url":{"url":"data:image/png;base64,AA"}}],"questions":{"q":{"type":"noul"}}}`, nil, errTypeDecisionUnsupported},
		{"T8 input not state", pathDecisions, `{"model":"jev","input":"hello","questions":{"q":{"type":"noul"}}}`, nil, errTypeDecisionUnsupported},
		{"T8a audio state", pathSystemOne, `{"model":"jev","state":[{"type":"input_audio","input_audio":{"data":"AA","format":"wav"}}],"questions":{"q":{"type":"noul"}}}`, nil, errTypeDecisionUnsupported},
		{"T8a image criteria", pathDecisions, `{"model":"jev","state":"s","questions":{"q":{"type":"choice","instructions":"i","criteria":{"a":[{"type":"image_url","image_url":{"url":"x"}}]}}}}`, nil, errTypeDecisionUnsupported},
		{"T8a file in unknown member", pathDecisions, `{"model":"jev","state":"s","extra":{"type":"file","file":{"file_id":"f"}},"questions":{"q":{"type":"noul"}}}`, nil, errTypeDecisionUnsupported},
		{"T8b input next to state", pathSystemOne, `{"model":"jev","state":"s","input":"hello","questions":{"q":{"type":"noul"}}}`, nil, errTypeDecisionUnsupported},
		{"T8 multipart", pathSystemOne, noulOnly, map[string]string{"Content-Type": "multipart/form-data; boundary=x"}, errTypeDecisionUnsupported},
		{"T8 gzip", pathSystemOne, noulOnly, map[string]string{"Content-Encoding": "gzip"}, errTypeDecisionUnsupported},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			proto := routing.ProtocolSystemOne
			if c.path == pathDecisions {
				proto = routing.ProtocolDecisions
			}
			h, ups := decHarness(t, proto, answerOK, "or1")
			h.start()
			resp := h.post(c.path, clientKey, c.body, c.hdr)
			b := readAll(t, resp)
			if resp.StatusCode != http.StatusBadRequest || errType(t, b) != c.typ {
				t.Fatalf("got %d %s, want 400 %s", resp.StatusCode, b, c.typ)
			}
			if n := egress(ups); n != 0 {
				t.Fatalf("egress = %d", n)
			}
			if (c.name == "T8 input not state" || c.name == "T8b input next to state") && !strings.Contains(string(b), "state") {
				t.Fatalf("message must name state: %s", b)
			}
		})
	}
}

// TestT7ConfiguredBoundsApply: Options.DecisionBounds replaces the defaults.
func TestT7ConfiguredBoundsApply(t *testing.T) {
	h, ups := decHarness(t, routing.ProtocolSystemOne, answerOK, "or1")
	h.opts.DecisionBounds = routing.DecisionBounds{MaxQuestions: 1}
	h.start()
	two := `{"model":"jev","state":"s","questions":{"a":{"type":"noul"},"b":{"type":"noul"}}}`
	resp := h.post(pathSystemOne, clientKey, two, nil)
	if b := readAll(t, resp); resp.StatusCode != 400 || errType(t, b) != errTypeDecisionBounds || egress(ups) != 0 {
		t.Fatalf("got %d %s", resp.StatusCode, b)
	}
}

// TestT9QuestionTypeFilter: a candidate lacking an asked type is skipped and
// order is preserved; no capable candidate is capability_unsupported.
func TestT9QuestionTypeFilter(t *testing.T) {
	h, ups := decHarness(t, routing.ProtocolSystemOne, answerOK, "or1", "or2")
	h.routes[0].Upstreams["or1"] = decSpec(routing.ProtocolSystemOne, routing.QuestionNoul)
	h.start()
	score := `{"model":"jev","state":"s","questions":{"u":{"type":"score","instructions":"i","criteria":["a","b"]}}}`
	resp := h.post(pathSystemOne, clientKey, score, nil)
	if readAll(t, resp); resp.StatusCode != 200 || len(ups["or1"].all()) != 0 || len(ups["or2"].all()) != 1 {
		t.Fatalf("status %d or1=%d or2=%d", resp.StatusCode, len(ups["or1"].all()), len(ups["or2"].all()))
	}
	resp = h.post(pathSystemOne, clientKey, noulOnly, nil)
	if readAll(t, resp); resp.StatusCode != 200 || len(ups["or1"].all()) != 1 {
		t.Fatal("noul request must go to the first candidate")
	}

	h2, ups2 := decHarness(t, routing.ProtocolSystemOne, answerOK, "or1")
	h2.routes[0].Upstreams["or1"] = decSpec(routing.ProtocolSystemOne, routing.QuestionNoul)
	h2.start()
	resp = h2.post(pathSystemOne, clientKey, score, nil)
	if b := readAll(t, resp); resp.StatusCode != 400 || errType(t, b) != errTypeCapability || egress(ups2) != 0 {
		t.Fatalf("got %d %s", resp.StatusCode, b)
	}
}

// TestT10EndpointRouteMismatch: decision routes never serve chat/responses,
// chat routes never serve decision paths, and the two decision families never
// serve each other.
func TestT10EndpointRouteMismatch(t *testing.T) {
	h, ups := decHarness(t, routing.ProtocolSystemOne, answerOK, "or1")
	h.start()
	cases := []struct{ path, body string }{
		{"/v1/chat/completions", `{"model":"jev","messages":[{"role":"user","content":"hi"}]}`},
		{"/v1/responses", `{"model":"jev","input":"hi"}`},
		{pathSystemOne, `{"model":"chat","state":"s","questions":{"q":{"type":"noul"}}}`},
		{pathDecisions, noulOnly},
	}
	for _, c := range cases {
		resp := h.post(c.path, clientKey, c.body, nil)
		if b := readAll(t, resp); resp.StatusCode != 400 || errType(t, b) != errTypeCapability {
			t.Fatalf("%s %s: %d %s", c.path, c.body, resp.StatusCode, b)
		}
	}
	if n := egress(ups); n != 0 {
		t.Fatalf("egress = %d", n)
	}
}

// statusFake answers each request with the next status in seq (200 answers
// after the sequence runs out).
func statusFake(seq map[string][]int, hdr http.Header) func(id string) http.HandlerFunc {
	var mu sync.Mutex
	return func(id string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			st := 200
			if s := seq[id]; len(s) > 0 {
				st, seq[id] = s[0], s[1:]
			}
			mu.Unlock()
			if st == 200 {
				answerOK(w, r)
				return
			}
			for k, v := range hdr {
				w.Header()[k] = v
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(st)
			fmt.Fprintf(w, `{"error":{"code":%d,"message":"upstream %s"}}`, st, id)
		}
	}
}

// decFailover builds a two-account decision route whose accounts answer
// per seq.
func decFailover(t *testing.T, seq map[string][]int, hdr http.Header, ids ...string) (*harness, map[string]*decUpstream) {
	h := newHarness(t)
	fake := statusFake(seq, hdr)
	ups := map[string]*decUpstream{}
	specs := map[string]core.UpstreamSpec{}
	for _, id := range ids {
		u := &decUpstream{}
		ups[id] = u
		h.upstream(id, core.ProviderOpenRouter, u.handler(fake(id)))
		specs[id] = decSpec(routing.ProtocolSystemOne)
	}
	h.routes = []core.Route{{Name: "jev", Models: []string{"jev"}, Interactive: ids, Background: ids, Upstreams: specs}}
	return h, ups
}

// TestT11ErrorTable covers the decision error mapping.
func TestT11ErrorTable(t *testing.T) {
	// Final statuses: one attempt, verbatim relay, request-scoped (no cooldown).
	for _, st := range []int{400, 404, 413, 422, 529} {
		t.Run(fmt.Sprint(st), func(t *testing.T) {
			h, ups := decFailover(t, map[string][]int{"a": {st}}, http.Header{"Retry-After": {"9"}}, "a", "b")
			h.start()
			resp := h.post(pathSystemOne, clientKey, noulOnly, nil)
			b := readAll(t, resp)
			if resp.StatusCode != st || string(b) != fmt.Sprintf(`{"error":{"code":%d,"message":"upstream a"}}`, st) {
				t.Fatalf("relay = %d %s", resp.StatusCode, b)
			}
			if resp.Header.Get("Retry-After") != "9" {
				t.Fatal("Retry-After not relayed")
			}
			if len(ups["a"].all()) != 1 || len(ups["b"].all()) != 0 {
				t.Fatalf("attempts a=%d b=%d", len(ups["a"].all()), len(ups["b"].all()))
			}
			l := paAwaitLease(t, "lease", h.policy.releasedCh)
			if !l.outcome.RequestScoped || l.outcome.Status != st {
				t.Fatalf("outcome = %+v", l.outcome)
			}
			if rows := h.waitRows(1); rows[0].Status != st || rows[0].UsageKnown {
				t.Fatalf("row = %+v", rows[0])
			}
		})
	}
	// 401: one credential refresh on the same account, then failover.
	t.Run("401", func(t *testing.T) {
		h, ups := decFailover(t, map[string][]int{"a": {401, 401}}, nil, "a", "b")
		h.start()
		resp := h.post(pathSystemOne, clientKey, noulOnly, nil)
		if readAll(t, resp); resp.StatusCode != 200 {
			t.Fatalf("status %d", resp.StatusCode)
		}
		if len(ups["a"].all()) != 2 || len(ups["b"].all()) != 1 || h.invalidations("a") != 1 {
			t.Fatalf("a=%d b=%d inval=%d", len(ups["a"].all()), len(ups["b"].all()), h.invalidations("a"))
		}
		rows := h.waitRows(3)
		if rows[1].FailoverOf != rows[0].ID || rows[2].FailoverOf != rows[1].ID {
			t.Fatal("attempt rows not chained by failover_of")
		}
	})
	// 429 with Retry-After: ResetAt hint and failover; with no account left the
	// client gets the upstream 429 and its Retry-After.
	t.Run("429", func(t *testing.T) {
		h, ups := decFailover(t, map[string][]int{"a": {429}}, http.Header{"Retry-After": {"7"}}, "a", "b")
		h.start()
		resp := h.post(pathSystemOne, clientKey, noulOnly, nil)
		if readAll(t, resp); resp.StatusCode != 200 || len(ups["b"].all()) != 1 {
			t.Fatalf("status %d b=%d", resp.StatusCode, len(ups["b"].all()))
		}
		l := paAwaitLease(t, "lease a", h.policy.releasedCh)
		if l.id != "a" || !l.outcome.ResetAt.Equal(testNow.Add(7*time.Second)) {
			t.Fatalf("lease %s outcome %+v", l.id, l.outcome)
		}

		h2, _ := decFailover(t, map[string][]int{"a": {429}, "b": {429}}, http.Header{"Retry-After": {"7"}}, "a", "b")
		h2.start()
		resp = h2.post(pathSystemOne, clientKey, noulOnly, nil)
		if b := readAll(t, resp); resp.StatusCode != 429 || resp.Header.Get("Retry-After") != "7" || !strings.Contains(string(b), "upstream b") {
			t.Fatalf("last 429 = %d %q %s", resp.StatusCode, resp.Header.Get("Retry-After"), b)
		}
	})
	// 500/502/503/524 fail over.
	for _, st := range []int{500, 502, 503, 524} {
		t.Run(fmt.Sprint(st), func(t *testing.T) {
			h, ups := decFailover(t, map[string][]int{"a": {st}}, nil, "a", "b")
			h.start()
			resp := h.post(pathSystemOne, clientKey, noulOnly, nil)
			if readAll(t, resp); resp.StatusCode != 200 || len(ups["a"].all()) != 1 || len(ups["b"].all()) != 1 {
				t.Fatalf("status %d", resp.StatusCode)
			}
		})
	}
	// A transport error fails over too.
	t.Run("transport", func(t *testing.T) {
		h, ups := decFailover(t, nil, nil, "b")
		h.accounts["a"] = core.Account{ID: "a", Provider: core.ProviderOpenRouter, BaseURL: "http://127.0.0.1:1/v1"}
		h.routes[0].Interactive = []string{"a", "b"}
		h.routes[0].Upstreams["a"] = decSpec(routing.ProtocolSystemOne)
		h.start()
		resp := h.post(pathSystemOne, clientKey, noulOnly, nil)
		if readAll(t, resp); resp.StatusCode != 200 || len(ups["b"].all()) != 1 {
			t.Fatalf("status %d", resp.StatusCode)
		}
	})
}

// TestT12RetryBound: three 500s with max_failovers 2 is exactly three
// attempts; a 529 is exactly one.
func TestT12RetryBound(t *testing.T) {
	seq := map[string][]int{"a": {500}, "b": {500}, "c": {500}, "d": {500}}
	h, ups := decFailover(t, seq, nil, "a", "b", "c", "d")
	h.start()
	resp := h.post(pathSystemOne, clientKey, noulOnly, nil)
	if readAll(t, resp); resp.StatusCode != 500 || egress(ups) != 3 || len(ups["d"].all()) != 0 {
		t.Fatalf("status %d egress %d", resp.StatusCode, egress(ups))
	}
	h2, ups2 := decFailover(t, map[string][]int{"a": {529}, "b": {529}, "c": {529}}, nil, "a", "b", "c")
	h2.start()
	resp = h2.post(pathSystemOne, clientKey, noulOnly, nil)
	if readAll(t, resp); resp.StatusCode != 529 || egress(ups2) != 1 {
		t.Fatalf("529 status %d egress %d", resp.StatusCode, egress(ups2))
	}
}

// TestT13Budget: an over-budget client gets 429 budget_exceeded with zero
// egress; a completed decision settles once (not incomplete).
func TestT13Budget(t *testing.T) {
	h, ups := decHarness(t, routing.ProtocolSystemOne, answerOK, "or1", "or2")
	fb := newFakeBudget()
	fb.reserveErr = budget.ErrExceeded
	h.startBudget(fb)
	resp := h.post(pathSystemOne, clientKey, noulOnly, nil)
	if b := readAll(t, resp); resp.StatusCode != 429 || errType(t, b) != "budget_exceeded" || egress(ups) != 0 || h.acquires() != 1 {
		t.Fatalf("got %d %s egress %d", resp.StatusCode, b, egress(ups))
	}

	h2, _ := decHarness(t, routing.ProtocolSystemOne, answerOK, "or1")
	fb2 := newFakeBudget()
	h2.startBudget(fb2)
	readAll(t, h2.post(pathSystemOne, clientKey, noulOnly, nil))
	s := fb2.waitSettles(t, 1)
	fb2.mu.Lock()
	inc := fb2.incomplete[0]
	fb2.mu.Unlock()
	if inc || s[0].ReportedCostUSD == nil || !s[0].UsageKnown {
		t.Fatalf("settle = %+v incomplete=%v", s[0], inc)
	}
}

// TestT14UpstreamSSEIsIncomplete: an event-stream answer on a decision
// endpoint is never complete: usage unknown, SettleIncomplete.
func TestT14UpstreamSSEIsIncomplete(t *testing.T) {
	h, _ := decHarness(t, routing.ProtocolSystemOne, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: "+decAnswer+"\n\ndata: [DONE]\n\n")
	}, "or1")
	fb := newFakeBudget()
	h.startBudget(fb)
	readAll(t, h.post(pathSystemOne, clientKey, noulOnly, nil))
	rec := h.waitRows(1)[0]
	if rec.UsageKnown || rec.Error != errUnterminatedStream {
		t.Fatalf("row = %+v", rec)
	}
	fb.waitSettles(t, 1)
	fb.mu.Lock()
	defer fb.mu.Unlock()
	if !fb.incomplete[0] {
		t.Fatal("SSE decision settled as complete")
	}
}

// TestDecisionConcurrencyLimitApplies: the limiter guards decision paths.
func TestDecisionConcurrencyLimitApplies(t *testing.T) {
	var hits atomic.Int32
	h, _ := decHarness(t, routing.ProtocolSystemOne, func(w http.ResponseWriter, r *http.Request) { hits.Add(1); answerOK(w, r) }, "or1")
	h.setLimiter(0, map[string]int{"alice": 0}) // alice has no free slot
	h.start()
	resp := h.post(pathSystemOne, clientKey, noulOnly, nil)
	if b := readAll(t, resp); resp.StatusCode != 429 || errType(t, b) != "concurrency_limit_exceeded" || hits.Load() != 0 {
		t.Fatalf("got %d %s", resp.StatusCode, b)
	}
	resp = h.post(pathSystemOne, "wrong", noulOnly, nil)
	if readAll(t, resp); resp.StatusCode != 401 {
		t.Fatalf("unauthenticated status %d", resp.StatusCode)
	}
}
