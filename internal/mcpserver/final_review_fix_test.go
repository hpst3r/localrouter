package mcpserver

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ---- F2: a malformed --url must never echo its embedded credentials ---------

// TestValidateBaseURLNeverEchoesCredentials pins the sanitized parse error: a
// URL that fails url.Parse AND carries userinfo must not come back in the error
// text. url.Error.Error() is `parse "<raw url>": <reason>`, so wrapping it with
// %w would print the secret verbatim into stderr (and any supervisor log).
func TestValidateBaseURLNeverEchoesCredentials(t *testing.T) {
	cases := []struct {
		raw    string
		secret string
	}{
		{"http://user:sup3rsecret@[::1", "sup3rsecret"},
		{"http://user:hunter2@:notaport", "hunter2"},
		{"http://user:tok3n@127.0.0.1:8787/%zz", "tok3n"},
		{"https://user:4n0th3r@router.internal:8443/x%zz", "4n0th3r"},
	}
	for _, c := range cases {
		got, err := ValidateBaseURL(c.raw)
		if err == nil {
			t.Fatalf("ValidateBaseURL(%q) = %q, want an error", c.raw, got)
		}
		msg := err.Error()
		if strings.Contains(msg, c.secret) {
			t.Errorf("ValidateBaseURL(%q) error leaks the credential: %q", c.raw, msg)
		}
		if strings.Contains(msg, c.raw) {
			t.Errorf("ValidateBaseURL(%q) error echoes the raw url: %q", c.raw, msg)
		}
		if strings.Contains(msg, "user") {
			t.Errorf("ValidateBaseURL(%q) error echoes the userinfo: %q", c.raw, msg)
		}
	}
}

// ---- F1: the control client must not consult the environment proxy --------

// TestUpstreamTransportIgnoresEnvironmentProxy pins the transport policy: the
// adapter refuses redirects so the client key is never re-sent to a host the
// operator did not choose, and it must be equally explicit about proxies. A nil
// Transport would inherit http.DefaultTransport, whose Proxy is
// http.ProxyFromEnvironment, silently routing the bearer key through
// HTTP(S)_PROXY. The global default transport must not be mutated.
func TestUpstreamTransportIgnoresEnvironmentProxy(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://proxy.invalid:3128")
	t.Setenv("HTTPS_PROXY", "http://proxy.invalid:3128")
	t.Setenv("ALL_PROXY", "http://proxy.invalid:3128")

	srv, err := New(Config{URL: "http://127.0.0.1:8787"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	tr, ok := srv.up.client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("client transport is %T, want an explicit *http.Transport", srv.up.client.Transport)
	}
	if tr.Proxy != nil {
		t.Fatalf("client transport consults the environment proxy: Proxy is non-nil (default transport would send the bearer key to %q)",
			"$HTTPS_PROXY")
	}
	// The package must not have neutralized the global for everyone else.
	def, ok := http.DefaultTransport.(*http.Transport)
	if !ok || def.Proxy == nil {
		t.Fatal("the global http.DefaultTransport was mutated; it must still honour the environment")
	}
	// The explicit transport must keep the sane dial/TLS knobs of the default.
	if tr.TLSHandshakeTimeout == 0 || tr.IdleConnTimeout == 0 || tr.DialContext == nil {
		t.Errorf("explicit transport dropped default knobs: %+v", tr)
	}
}

// ---- F4/F5: duplicate keys, explicit null, blank/whitespace, literal ids --

func TestDecodeArgsRejectsDuplicateKeys(t *testing.T) {
	for _, raw := range []string{
		`{"a":1,"a":2}`,
		`{"limit":1,"limit":99}`,
		`{"group":"model","group":"bogus"}`,
		`{"a":"x","b":true,"a":"y"}`,
	} {
		if _, err := decodeArgs([]byte(raw), "a", "b", "limit", "group"); err == nil {
			t.Errorf("decodeArgs(%s) accepted a duplicate key, want invalid_arguments", raw)
		} else if err.Code != CodeInvalidArguments {
			t.Errorf("decodeArgs(%s) code = %q, want invalid_arguments", raw, err.Code)
		}
	}
	// A nested duplicate inside an opaque value is not a top-level key clash and
	// is out of scope for the argument gate.
	if _, err := decodeArgs([]byte(`{"a":{"x":1,"x":2}}`), "a"); err != nil {
		t.Errorf("a nested duplicate key is not a top-level argument clash: %v", err)
	}
}

func TestDecodeArgsRejectsExplicitNullButAllowsAbsentAndEmpty(t *testing.T) {
	// Absent arguments (nil or empty) are the empty bag.
	for _, raw := range []string{"", "   "} {
		if _, err := decodeArgs([]byte(raw), "a"); err != nil {
			t.Errorf("decodeArgs(%q) = %v, want the empty bag", raw, err)
		}
	}
	bag, err := decodeArgs(nil, "a")
	if err != nil {
		t.Fatalf("decodeArgs(nil) = %v, want the empty bag", err)
	}
	if bag.has("a") || bag.present("a") {
		t.Error("absent arguments must decode to an empty bag")
	}
	// An empty object is explicitly allowed.
	bag, err = decodeArgs([]byte(`{}`), "a")
	if err != nil {
		t.Fatalf("decodeArgs({}) = %v, want the empty bag", err)
	}
	if bag.has("a") {
		t.Error("an empty object must not report any argument as supplied")
	}
	// An explicitly supplied null argument object is rejected.
	if _, err := decodeArgs([]byte(`null`), "a"); err == nil {
		t.Error("decodeArgs(null) accepted an explicit null argument object, want invalid_arguments")
	}
	// A supplied null field is invalid rather than silently defaulted.
	bag, err = decodeArgs([]byte(`{"a":null}`), "a")
	if err != nil {
		t.Fatalf("a null field still decodes as an object field: %v", err)
	}
	if !bag.has("a") {
		t.Error("has() must report a supplied (null) field")
	}
	if bag.present("a") {
		t.Error("present() must report a null field as not present")
	}
	if _, err := bag.str("a", false); err == nil {
		t.Error("str() accepted an explicit null value, want invalid_arguments")
	}
	if _, _, err := bag.integer("a"); err == nil {
		t.Error("integer() accepted an explicit null value, want invalid_arguments")
	}
}

func TestArgStringsAreLiteralAndBlankIsRejected(t *testing.T) {
	// Blank and whitespace-only strings are invalid, whatever the field.
	for _, raw := range []string{`{"a":""}`, `{"a":"   "}`, `{"a":"\t\n"}`, `{"a":"\u00a0"}`} {
		bag, err := decodeArgs([]byte(raw), "a")
		if err != nil {
			t.Fatalf("decodeArgs(%s): %v", raw, err)
		}
		if _, err := bag.str("a", false); err == nil {
			t.Errorf("str() accepted the blank value %s, want invalid_arguments", raw)
		}
	}
	// Surrounding whitespace is rejected, never silently normalized away: an
	// account/model identifier is matched literally.
	bag, _ := decodeArgs([]byte(`{"a":" primary "}`), "a")
	if _, err := bag.str("a", false); err == nil {
		t.Error("str() trimmed a value with surrounding whitespace, want invalid_arguments")
	}
	// A valid identifier is preserved byte for byte.
	bag, _ = decodeArgs([]byte(`{"a":"primary-us-east"}`), "a")
	got, err := bag.str("a", false)
	if err != nil {
		t.Fatalf("str() rejected a valid identifier: %v", err)
	}
	if got != "primary-us-east" {
		t.Errorf("str() = %q, want the literal identifier", got)
	}
	// An absent optional field is still the zero value, not an error.
	bag, _ = decodeArgs([]byte(`{}`), "a")
	if s, err := bag.str("a", false); err != nil || s != "" {
		t.Errorf("absent optional string = (%q, %v), want (\"\", nil)", s, err)
	}
	if _, ok, err := bag.enum("a", []string{"x"}); err != nil || ok {
		t.Errorf("absent enum = (ok=%v, %v), want (false, nil)", ok, err)
	}
}

// ---- F5 integration: the tools themselves reject these ---------------------

func TestUsageSummaryRejectsNullBlankAndDuplicateArguments(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		jsonResp(w, `{"schema_version":1,"rows":[]}`)
	}, Config{})
	cases := map[string]string{
		"duplicate limit":  `{"limit":1,"limit":99}`,
		"duplicate group":  `{"group":"model","group":"account"}`,
		"since null":       `{"since":null}`,
		"since blank":      `{"since":"   "}`,
		"since whitespace": `{"since":" 24h "}`,
		"group null":       `{"group":null}`,
		"group blank":      `{"group":"  "}`,
		"limit null":       `{"limit":null}`,
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			res, doc := h.rawCall("usage_summary", args)
			if !res.IsError || h.errorCode(doc) != string(CodeInvalidArguments) {
				t.Fatalf("isError=%v code=%q, want invalid_arguments for %s", res.IsError, h.errorCode(doc), args)
			}
		})
	}
	if n := len(h.requests()); n != 0 {
		t.Fatalf("rejected calls must not reach the control API; saw %d requests", n)
	}
	// The defaults still apply when the fields are genuinely absent.
	h2 := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		jsonResp(w, `{"schema_version":1,"rows":[]}`)
	}, Config{})
	if res, doc := h2.call("usage_summary", map[string]any{}); res.IsError {
		t.Fatalf("an empty argument object must still default: %v", doc)
	}
}

func TestCheckAdmissionRejectsNullBlankAndWhitespaceTargets(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		jsonResp(w, `{"decision":"allow","account_id":"primary","reason":"admitted"}`)
	}, Config{})
	cases := map[string]string{
		"null account with model":   `{"account":null,"model":"gpt-5"}`,
		"blank account with model":  `{"account":"   ","model":"gpt-5"}`,
		"padded account with model": `{"account":" primary ","model":"gpt-5"}`,
		"null model with account":   `{"account":"primary","model":null}`,
		"blank model with account":  `{"account":"primary","model":" "}`,
		"null class":                `{"account":"primary","class":null}`,
		"blank class":               `{"account":"primary","class":"   "}`,
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			res, doc := h.rawCall("check_admission", args)
			if !res.IsError || h.errorCode(doc) != string(CodeInvalidArguments) {
				t.Fatalf("isError=%v code=%q, want invalid_arguments for %s", res.IsError, h.errorCode(doc), args)
			}
		})
	}
	if n := len(h.requests()); n != 0 {
		t.Fatalf("rejected calls must not reach the control API; saw %d requests", n)
	}
	// A literal (unpadded) identifier still works and is forwarded verbatim.
	if res, doc := h.call("check_admission", map[string]any{"account": "primary"}); res.IsError {
		t.Fatalf("a literal account id must be accepted: %v", doc)
	}
}

func TestQuotaStatusRejectsNullAndNullArgumentObject(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) { jsonResp(w, statusDoc(oneAccount)) }, Config{})
	if res, doc := h.rawCall("quota_status", `{"account":null}`); !res.IsError || h.errorCode(doc) != string(CodeInvalidArguments) {
		t.Fatalf("a null account = isError=%v code=%q, want invalid_arguments", res.IsError, h.errorCode(doc))
	}
	if res, doc := h.rawCall("quota_status", `{"account":"  "}`); !res.IsError || h.errorCode(doc) != string(CodeInvalidArguments) {
		t.Fatalf("a blank account = isError=%v code=%q, want invalid_arguments", res.IsError, h.errorCode(doc))
	}
	if n := len(h.requests()); n != 0 {
		t.Fatalf("rejected calls must not reach the control API; saw %d requests", n)
	}
}

// ---- errResult: multibyte truncation must terminate ------------------------

// TestErrResultTruncatesMultibyteMessagesWithoutHanging pins the rune-boundary
// search. The previous byte-index search rounded mid down to a UTF-8 boundary
// and could land it back on lo while hi = lo+1, so lo never advanced and the
// loop spun forever. The guard turns a regression into a bounded failure.
func TestErrResultTruncatesMultibyteMessagesWithoutHanging(t *testing.T) {
	const budget = MinMaxResultChars
	s := &Server{cfg: Config{MaxResultChars: budget}}
	markers := []string{
		strings.Repeat("界", 4000), // 3 bytes each
		strings.Repeat("😀", 3000), // 4 bytes each
		strings.Repeat("é", 5000), // 2 bytes each
	}
	for i, pad := range markers {
		msg := "unknown argument " + pad + " is not a tool argument"
		done := make(chan *mcp.CallToolResult, 1)
		go func() { done <- s.errResult(&toolError{Code: CodeInvalidArguments, Message: msg}) }()
		select {
		case res := <-done:
			if got := resultBytes(res); got > budget {
				t.Fatalf("marker %d: error result is %d bytes, over the %d budget", i, got, budget)
			}
			text := textOf(t, res)
			if !utf8.ValidString(text) {
				t.Fatalf("marker %d: truncated error is not valid UTF-8", i)
			}
			var v map[string]any
			if err := json.Unmarshal([]byte(text), &v); err != nil {
				t.Fatalf("marker %d: truncated error is not valid JSON: %v (%q)", i, err, text)
			}
			if !strings.Contains(text, "truncated") {
				t.Errorf("marker %d: truncation must be marked, got %q", i, text)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("marker %d: errResult did not terminate within 5s on a multibyte over-budget message", i)
		}
	}
}
