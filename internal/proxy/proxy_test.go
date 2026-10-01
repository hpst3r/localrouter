package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
)

const respBody = `{"model":"gpt-x","input":"hi <b>&</b>","stream":false}`

func okJSON(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"id":"r1","usage":{"input_tokens":10,"output_tokens":3,"input_tokens_details":{"cached_tokens":4},"output_tokens_details":{"reasoning_tokens":1}}}`)
}

func decodeErr(t *testing.T, resp *http.Response) errorBody {
	t.Helper()
	defer resp.Body.Close()
	var e errorBody
	if err := json.NewDecoder(resp.Body).Decode(&e); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	return e
}

func singleRoute(h *harness, accts ...string) {
	h.routes = []core.Route{{Name: "main", Models: []string{"gpt-x"}, Interactive: accts, Background: accts}}
}

func TestAuthRequired(t *testing.T) {
	h := newHarness(t)
	var hits atomic.Int32
	h.upstream("a", core.ProviderOpenAICompat, func(w http.ResponseWriter, r *http.Request) { hits.Add(1) })
	singleRoute(h, "a")
	h.start()

	for _, key := range []string{"", "wrong"} {
		resp := h.post("/v1/responses", key, respBody, nil)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("key %q: status %d", key, resp.StatusCode)
		}
		if e := decodeErr(t, resp); e.Error.Message == "" {
			t.Fatal("missing error message")
		}
	}
	req, _ := http.NewRequest(http.MethodGet, h.srv.URL+"/v1/models", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("models without key: %d", resp.StatusCode)
	}
	if hits.Load() != 0 || len(h.policy.classes()) != 0 {
		t.Fatal("unauthenticated request reached policy/upstream")
	}
}

func TestModelsSorted(t *testing.T) {
	h := newHarness(t)
	h.routes = []core.Route{
		{Name: "r1", Models: []string{"zeta", "alpha"}},
		{Name: "r2", Models: []string{"mid"}},
	}
	h.start()
	req, _ := http.NewRequest(http.MethodGet, h.srv.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+clientKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Object string `json:"object"`
		Data   []struct{ ID, Object, OwnedBy string }
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, d := range out.Data {
		ids = append(ids, d.ID)
	}
	if out.Object != "list" || strings.Join(ids, ",") != "alpha,mid,zeta" {
		t.Fatalf("got %+v", out)
	}
}

func TestUnknownModel(t *testing.T) {
	h := newHarness(t)
	h.upstream("a", core.ProviderOpenAICompat, okJSON)
	singleRoute(h, "a")
	h.start()
	resp := h.post("/v1/responses", clientKey, `{"model":"nope"}`, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status %d", resp.StatusCode)
	}
	e := decodeErr(t, resp)
	if strings.Contains(e.Error.Message, "gpt-x") || strings.Contains(e.Error.Message, "http") {
		t.Fatalf("error leaks config: %q", e.Error.Message)
	}
}

func TestCodexOnChatRejected(t *testing.T) {
	h := newHarness(t)
	var hits atomic.Int32
	h.upstream("cx", core.ProviderCodex, func(http.ResponseWriter, *http.Request) { hits.Add(1) })
	singleRoute(h, "cx")
	h.start()
	resp := h.post("/v1/chat/completions", clientKey, `{"model":"gpt-x","messages":[]}`, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if e := decodeErr(t, resp); e.Error.Message != "codex accounts only serve /v1/responses" {
		t.Fatalf("message %q", e.Error.Message)
	}
	if hits.Load() != 0 || len(h.policy.classes()) != 0 {
		t.Fatal("lease acquired or upstream hit")
	}
}

// Acceptance 1 (proxy level): policy deny -> 429, no upstream call.
func TestReserveDenyNoUpstream(t *testing.T) {
	h := newHarness(t)
	var hits atomic.Int32
	h.upstream("a", core.ProviderCodex, func(http.ResponseWriter, *http.Request) { hits.Add(1) })
	singleRoute(h, "a")
	h.policy.deny["a"] = true
	h.start()
	resp := h.post("/v1/responses", bgKey, respBody, nil)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if resp.Header.Get("Retry-After") != "60" {
		t.Fatalf("Retry-After %q", resp.Header.Get("Retry-After"))
	}
	e := decodeErr(t, resp)
	if e.Error.Type != "quota_reserve" || e.Error.Message != "localrouter: no admissible account: "+testReason {
		t.Fatalf("error %+v", e)
	}
	if hits.Load() != 0 {
		t.Fatal("upstream was called")
	}
	if len(h.ledger.all()) != 0 {
		t.Fatal("denied request should not write an attempt row")
	}
}

// Acceptance 4: 429 before body -> next account; two rows linked; cooldown hint.
func TestFailoverOn429(t *testing.T) {
	h := newHarness(t)
	h.upstream("a", core.ProviderCodex, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"rate limited"}}`)
	})
	h.upstream("b", core.ProviderCodex, okJSON)
	singleRoute(h, "a", "b")
	h.start()

	resp := h.post("/v1/responses", clientKey, respBody, nil)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"r1"`) {
		t.Fatalf("status %d body %s", resp.StatusCode, body)
	}
	rows := h.waitRows(2)
	if len(rows) != 2 {
		t.Fatalf("rows %d", len(rows))
	}
	first, second := rows[0], rows[1]
	if first.AccountID != "a" || first.Status != 429 || first.FailoverOf != "" {
		t.Fatalf("first row %+v", first)
	}
	if second.AccountID != "b" || second.Status != 200 || second.FailoverOf != first.ID {
		t.Fatalf("second row %+v (first id %s)", second, first.ID)
	}
	if !second.UsageKnown || second.Usage != (core.Usage{InputTokens: 10, CachedInputTokens: 4, OutputTokens: 3, ReasoningTokens: 1}) {
		t.Fatalf("usage %+v known=%v", second.Usage, second.UsageKnown)
	}
	if second.UpstreamIdentity != "ident-b" || second.Model != "gpt-x" || second.Client != "alice" {
		t.Fatalf("attribution %+v", second)
	}
	la := h.policy.leases[0]
	if la.id != "a" || la.released != 1 || la.outcome.Status != 429 || !la.outcome.ResetAt.Equal(testNow.Add(120*time.Second)) {
		t.Fatalf("lease a %+v released=%d", la.outcome, la.released)
	}
	if lb := h.policy.leases[1]; lb.released != 1 || lb.outcome.Status != 200 || !lb.outcome.UsageKnown {
		t.Fatalf("lease b %+v", lb.outcome)
	}
}

// When no further account is admissible, the buffered upstream 429 is relayed.
func TestFailoverExhaustedRelaysUpstream(t *testing.T) {
	h := newHarness(t)
	h.upstream("a", core.ProviderCodex, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("x-codex-primary-used-percent", "100")
		w.Header().Set("x-codex-primary-reset-after-seconds", "300")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"usage limit"}}`)
	})
	singleRoute(h, "a")
	h.start()
	resp := h.post("/v1/responses", clientKey, respBody, nil)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 429 || !strings.Contains(string(body), "usage limit") {
		t.Fatalf("status %d body %s", resp.StatusCode, body)
	}
	if got := h.policy.leases[0].outcome.ResetAt; !got.Equal(testNow.Add(300 * time.Second)) {
		t.Fatalf("ResetAt %v", got)
	}
}

func TestFailoverOn5xxAndCredentialError(t *testing.T) {
	h := newHarness(t)
	h.upstream("a", core.ProviderOpenAICompat, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	})
	h.upstream("b", core.ProviderOpenAICompat, okJSON)
	h.upstream("c", core.ProviderOpenAICompat, okJSON)
	h.creds.fail["b"] = true
	singleRoute(h, "a", "b", "c")
	h.start()
	resp := h.post("/v1/responses", clientKey, respBody, nil)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	rows := h.waitRows(3)
	if rows[0].AccountID != "a" || rows[1].AccountID != "b" || rows[2].AccountID != "c" {
		t.Fatalf("chain %s,%s,%s", rows[0].AccountID, rows[1].AccountID, rows[2].AccountID)
	}
	if rows[1].FailoverOf != rows[0].ID || rows[2].FailoverOf != rows[1].ID {
		t.Fatal("failover chain not linked")
	}
	for _, l := range h.policy.leases {
		if l.released != 1 {
			t.Fatalf("lease %s released %d times", l.id, l.released)
		}
	}
}

func TestMaxFailovers(t *testing.T) {
	h := newHarness(t)
	var hits atomic.Int32
	fail := func(w http.ResponseWriter, r *http.Request) { hits.Add(1); w.WriteHeader(503) }
	h.upstream("a", core.ProviderOpenAICompat, fail)
	h.upstream("b", core.ProviderOpenAICompat, fail)
	h.upstream("c", core.ProviderOpenAICompat, fail)
	h.upstream("d", core.ProviderOpenAICompat, fail)
	singleRoute(h, "a", "b", "c", "d")
	h.start()
	resp := h.post("/v1/responses", clientKey, respBody, nil)
	resp.Body.Close()
	if resp.StatusCode != 503 || hits.Load() != 3 {
		t.Fatalf("status %d hits %d (want 503, 3)", resp.StatusCode, hits.Load())
	}
}

// Acceptance 5: Responses SSE usage capture incl. cached + reasoning,
// multi-line data fields and CRLF line endings.
func TestResponsesSSEUsage(t *testing.T) {
	stream := "event: response.created\r\n" +
		"data: {\"type\":\"response.created\",\"response\":{\"usage\":null}}\r\n\r\n" +
		": keepalive\r\n\r\n" +
		"event: response.output_text.delta\r\n" +
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"usage\"}\r\n\r\n" +
		"event: response.completed\r\n" +
		"data: {\"type\":\"response.completed\",\r\n" +
		"data: \"response\":{\"usage\":{\"input_tokens\":120,\"input_tokens_details\":{\"cached_tokens\":100},\r\n" +
		"data: \"output_tokens\":50,\"output_tokens_details\":{\"reasoning_tokens\":30}}}}\r\n\r\n"
	h := newHarness(t)
	h.upstream("a", core.ProviderCodex, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		for i := 0; i < len(stream); i += 7 {
			end := min(i+7, len(stream))
			_, _ = io.WriteString(w, stream[i:end])
			fl.Flush()
		}
	})
	singleRoute(h, "a")
	h.start()
	resp := h.post("/v1/responses", clientKey, `{"model":"gpt-x","stream":true}`, nil)
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(got) != stream {
		t.Fatalf("stream altered:\n%q", got)
	}
	rows := h.waitRows(1)
	want := core.Usage{InputTokens: 120, CachedInputTokens: 100, OutputTokens: 50, ReasoningTokens: 30}
	if !rows[0].UsageKnown || rows[0].Usage != want {
		t.Fatalf("usage %+v known=%v", rows[0].Usage, rows[0].UsageKnown)
	}
	if rows[0].BytesOut != int64(len(stream)) {
		t.Fatalf("bytes %d", rows[0].BytesOut)
	}
}

// Acceptance 5: client abort mid-stream -> usage_known=false, lease released.
func TestClientAbortMidStream(t *testing.T) {
	h := newHarness(t)
	upstreamDone := make(chan struct{})
	h.upstream("a", core.ProviderCodex, func(w http.ResponseWriter, r *http.Request) {
		defer close(upstreamDone)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\"}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	})
	singleRoute(h, "a")
	h.start()

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, h.srv.URL+"/v1/responses", strings.NewReader(`{"model":"gpt-x","stream":true}`))
	req.Header.Set("Authorization", "Bearer "+clientKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	if err != nil || !strings.HasPrefix(line, "event: response.created") {
		t.Fatalf("first line %q err %v", line, err)
	}
	cancel()
	resp.Body.Close()

	select {
	case l := <-h.policy.releasedCh:
		if l.outcome.UsageKnown {
			t.Fatal("lease outcome claims usage known")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("lease not released after client abort")
	}
	rows := h.waitRows(1)
	if rows[0].UsageKnown || rows[0].Error != "client disconnected" {
		t.Fatalf("row %+v", rows[0])
	}
	select {
	case <-upstreamDone:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream request not canceled")
	}
}

func TestChatStreamIncludeUsage(t *testing.T) {
	h := newHarness(t)
	bodyCh := make(chan []byte, 1)
	h.upstream("o", core.ProviderOllama, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path %s", r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		bodyCh <- b
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}],\"usage\":null}\n\n"+
			"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":9,\"completion_tokens\":4,\"prompt_tokens_details\":{\"cached_tokens\":2},\"completion_tokens_details\":{\"reasoning_tokens\":1}}}\n\n"+
			"data: [DONE]\n\n")
	})
	singleRoute(h, "o")
	h.start()
	in := `{"model":"gpt-x","stream":true,"stream_options":{"continuous_usage_stats":false},"messages":[{"role":"user","content":"<hi>"}]}`
	resp := h.post("/v1/chat/completions", clientKey, in, nil)
	_, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	gotBody := <-bodyCh

	var sent struct {
		StreamOptions map[string]any `json:"stream_options"`
		Messages      []map[string]string
	}
	if err := json.Unmarshal(gotBody, &sent); err != nil {
		t.Fatal(err)
	}
	if sent.StreamOptions["include_usage"] != true || sent.StreamOptions["continuous_usage_stats"] != false {
		t.Fatalf("stream_options %v", sent.StreamOptions)
	}
	if sent.Messages[0]["content"] != "<hi>" || strings.Contains(string(gotBody), `\u003c`) {
		t.Fatalf("messages altered: %s", gotBody)
	}
	rows := h.waitRows(1)
	if want := (core.Usage{InputTokens: 9, CachedInputTokens: 2, OutputTokens: 4, ReasoningTokens: 1}); !rows[0].UsageKnown || rows[0].Usage != want {
		t.Fatalf("usage %+v", rows[0].Usage)
	}
}

func TestChatNonStreamBodyUnchanged(t *testing.T) {
	h := newHarness(t)
	bodyCh := make(chan string, 1)
	h.upstream("o", core.ProviderOllama, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodyCh <- string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"usage":{"prompt_tokens":5,"completion_tokens":2}}`)
	})
	singleRoute(h, "o")
	h.start()
	in := `{"model":"gpt-x",  "messages":[]}`
	resp := h.post("/v1/chat/completions", clientKey, in, nil)
	resp.Body.Close()
	if gotBody := <-bodyCh; gotBody != in {
		t.Fatalf("body changed: %q", gotBody)
	}
	if rows := h.waitRows(1); rows[0].Usage.InputTokens != 5 || rows[0].Usage.OutputTokens != 2 {
		t.Fatalf("usage %+v", rows[0].Usage)
	}
}

func TestAuth401InvalidatesAndRetriesSameAccount(t *testing.T) {
	h := newHarness(t)
	var mu sync.Mutex
	var auths []string
	h.upstream("a", core.ProviderCodex, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auths = append(auths, r.Header.Get("Authorization"))
		first := len(auths) == 1
		mu.Unlock()
		if first {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		okJSON(w, r)
	})
	var bHits atomic.Int32
	h.upstream("b", core.ProviderCodex, func(w http.ResponseWriter, r *http.Request) { bHits.Add(1); okJSON(w, r) })
	singleRoute(h, "a", "b")
	h.start()
	resp := h.post("/v1/responses", clientKey, respBody, nil)
	resp.Body.Close()
	if resp.StatusCode != 200 || bHits.Load() != 0 {
		t.Fatalf("status %d bHits %d", resp.StatusCode, bHits.Load())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(auths) != 2 || auths[0] == auths[1] {
		t.Fatalf("auths %v", auths)
	}
	if h.creds.invalidated["a"] != 1 {
		t.Fatalf("invalidated %d", h.creds.invalidated["a"])
	}
	rows := h.waitRows(2)
	if rows[0].Status != 401 || rows[1].Status != 200 || rows[1].FailoverOf != rows[0].ID || rows[1].AccountID != "a" {
		t.Fatalf("rows %+v", rows)
	}
	if len(h.policy.leases) != 1 || h.policy.leases[0].released != 1 {
		t.Fatal("expected one lease released once")
	}
}

func TestHeadersForwarding(t *testing.T) {
	h := newHarness(t)
	hdrCh := make(chan http.Header, 1)
	h.upstream("a", core.ProviderCodex, func(w http.ResponseWriter, r *http.Request) {
		hdrCh <- r.Header.Clone()
		w.Header().Set("Connection", "X-Hop")
		w.Header().Set("X-Hop", "1")
		w.Header().Set("Set-Cookie", "s=1")
		w.Header().Set("X-Upstream", "yes")
		okJSON(w, r)
	})
	singleRoute(h, "a")
	h.start()
	resp := h.post("/v1/responses", clientKey, respBody, map[string]string{
		"OpenAI-Beta": "responses=v1", "session_id": "s-1", "X-LocalRouter-Session": "sess",
		"X-LocalRouter-Task": strings.Repeat("t", 300), "Cookie": "c=1", "X-Other": "x",
	})
	resp.Body.Close()
	got := <-hdrCh
	if a := got.Get("Authorization"); strings.Contains(a, clientKey) || !strings.HasPrefix(a, "Bearer "+tokenStem) {
		t.Fatalf("Authorization forwarded wrongly: %q", a)
	}
	if got.Get("ChatGPT-Account-Id") != "ident-a" || got.Get("OpenAI-Beta") != "responses=v1" || got.Get("session_id") != "s-1" {
		t.Fatalf("missing headers: %v", got)
	}
	for _, k := range []string{"X-Localrouter-Session", "X-Localrouter-Task", "Cookie", "X-Other"} {
		if got.Get(k) != "" {
			t.Fatalf("header %s forwarded", k)
		}
	}
	if resp.Header.Get("X-Upstream") != "yes" || resp.Header.Get("X-Hop") != "" || resp.Header.Get("Set-Cookie") != "" {
		t.Fatalf("response headers %v", resp.Header)
	}
	rows := h.waitRows(1)
	if rows[0].Session != "sess" || len(rows[0].Task) != 128 {
		t.Fatalf("attribution session=%q task len=%d", rows[0].Session, len(rows[0].Task))
	}
}

func TestUpstreamModelRewrite(t *testing.T) {
	h := newHarness(t)
	bodyCh := make(chan []byte, 1)
	h.upstream("a", core.ProviderOllama, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodyCh <- b
		okJSON(w, r)
	})
	h.routes = []core.Route{{Name: "main", Models: []string{"gpt-x"}, UpstreamModel: "real-model", Interactive: []string{"a"}, Background: []string{"a"}}}
	h.start()
	resp := h.post("/v1/responses", clientKey, `{"model":"gpt-x","input":[{"x":1.50}],"temperature":0.2}`, nil)
	resp.Body.Close()
	got := <-bodyCh
	var m map[string]json.RawMessage
	if err := json.Unmarshal(got, &m); err != nil {
		t.Fatal(err)
	}
	if string(m["model"]) != `"real-model"` || string(m["input"]) != `[{"x":1.50}]` || string(m["temperature"]) != `0.2` {
		t.Fatalf("body %s", got)
	}
	if rows := h.waitRows(1); rows[0].Model != "gpt-x" || rows[0].Route != "main" {
		t.Fatalf("row model %q", rows[0].Model)
	}
}

func TestClassDowngradeOnly(t *testing.T) {
	h := newHarness(t)
	h.upstream("a", core.ProviderOpenAICompat, okJSON)
	singleRoute(h, "a")
	h.start()
	cases := []struct {
		key, hdr string
		want     core.Class
	}{
		{clientKey, "", core.ClassInteractive},
		{clientKey, "background", core.ClassBackground},
		{bgKey, "interactive", core.ClassBackground},
	}
	for _, c := range cases {
		resp := h.post("/v1/responses", c.key, respBody, map[string]string{"X-LocalRouter-Class": c.hdr})
		resp.Body.Close()
	}
	got := h.policy.classes()
	for i, c := range cases {
		if got[i] != c.want {
			t.Fatalf("case %d: class %s want %s", i, got[i], c.want)
		}
	}
	rows := h.waitRows(3)
	if rows[1].Class != core.ClassBackground {
		t.Fatalf("row class %s", rows[1].Class)
	}
}

func TestBadBodies(t *testing.T) {
	h := newHarness(t)
	h.upstream("a", core.ProviderOpenAICompat, okJSON)
	singleRoute(h, "a")
	h.opts.MaxBodyBytes = 64
	h.start()
	if resp := h.post("/v1/responses", clientKey, `not json`, nil); resp.StatusCode != 400 {
		t.Fatalf("invalid json: %d", resp.StatusCode)
	}
	big := `{"model":"gpt-x","input":"` + strings.Repeat("a", 100) + `"}`
	if resp := h.post("/v1/responses", clientKey, big, nil); resp.StatusCode != 413 {
		t.Fatalf("big body: %d", resp.StatusCode)
	}
}

func TestLedgerErrorDoesNotFailRequest(t *testing.T) {
	h := newHarness(t)
	h.upstream("a", core.ProviderOpenAICompat, okJSON)
	singleRoute(h, "a")
	h.ledger.err = io.ErrUnexpectedEOF
	h.start()
	resp := h.post("/v1/responses", clientKey, respBody, nil)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	h.waitRows(1)
}

// Acceptance 9: no log line or response contains a client key or upstream token.
func TestNoSecretsInLogsOrResponses(t *testing.T) {
	h := newHarness(t)
	var n atomic.Int32
	h.upstream("a", core.ProviderCodex, func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusTooManyRequests)
	})
	h.upstream("b", core.ProviderCodex, okJSON)
	singleRoute(h, "a", "b")
	h.start()

	var bodies strings.Builder
	for _, key := range []string{clientKey, "bad-" + clientKey, bgKey} {
		resp := h.post("/v1/responses", key, respBody, nil)
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		bodies.Write(b)
		for k, v := range resp.Header {
			bodies.WriteString(k + strings.Join(v, ","))
		}
	}
	h.policy.deny["a"], h.policy.deny["b"] = true, true
	resp := h.post("/v1/responses", bgKey, respBody, nil)
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	bodies.Write(b)

	logs := h.logs.String()
	if logs == "" {
		t.Fatal("expected some logs")
	}
	for _, secret := range []string{"SENTINEL", "hi <b>"} {
		if strings.Contains(logs, secret) {
			t.Fatalf("logs contain %q:\n%s", secret, logs)
		}
		if strings.Contains(bodies.String(), secret) {
			t.Fatalf("responses contain %q", secret)
		}
	}
	for _, row := range h.ledger.all() {
		if strings.Contains(row.Error, "SENTINEL") || strings.Contains(row.UpstreamIdentity, "SENTINEL") {
			t.Fatalf("ledger row leaks secret: %+v", row)
		}
	}
}

func TestResetHint(t *testing.T) {
	h := http.Header{}
	if !resetHint(h, testNow).IsZero() {
		t.Fatal("expected zero")
	}
	h.Set("Retry-After", "30")
	h.Set("x-codex-primary-used-percent", "42")
	h.Set("x-codex-primary-reset-after-seconds", "9000")
	h.Set("x-codex-secondary-used-percent", "100")
	h.Set("x-codex-secondary-reset-after-seconds", "600")
	if got := resetHint(h, testNow); !got.Equal(testNow.Add(600 * time.Second)) {
		t.Fatalf("got %v", got)
	}
	h2 := http.Header{}
	h2.Set("Retry-After", testNow.Add(time.Hour).Format(http.TimeFormat))
	if got := resetHint(h2, testNow); !got.Equal(testNow.Add(time.Hour)) {
		t.Fatalf("date form got %v", got)
	}
}
