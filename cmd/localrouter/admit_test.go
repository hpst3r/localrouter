package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeControl serves POST /control/v1/admit, recording the last request body.
func fakeControl(t *testing.T, code int, resp string) (*httptest.Server, *map[string]string) {
	t.Helper()
	var got map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/control/v1/admit" {
			http.NotFound(w, r)
			return
		}
		b, _ := io.ReadAll(r.Body)
		got = nil
		_ = json.Unmarshal(b, &got)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = w.Write([]byte(resp))
	}))
	t.Cleanup(srv.Close)
	return srv, &got
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee exitError
	if errors.As(err, &ee) {
		return ee.Code()
	}
	return -1
}

func TestAdmitAllow(t *testing.T) {
	srv, got := fakeControl(t, 200, `{"decision":"allow","account_id":"claude-max","reason":"admitted"}`)
	var out bytes.Buffer
	err := runAdmit([]string{"--account", "claude-max", "--url", srv.URL + "/"}, &out, io.Discard)
	if exitCode(err) != 0 {
		t.Fatalf("err = %v", err)
	}
	if out.String() != "allow claude-max: admitted\n" {
		t.Errorf("stdout = %q", out.String())
	}
	if (*got)["class"] != "background" || (*got)["account"] != "claude-max" {
		t.Errorf("request = %v", *got)
	}
	if _, ok := (*got)["model"]; ok {
		t.Errorf("model sent with account: %v", *got)
	}
}

func TestAdmitDenyJSON(t *testing.T) {
	srv, got := fakeControl(t, 200, `{"decision":"deny","account_id":"","reason":"weekly reserve"}`)
	var out bytes.Buffer
	err := runAdmit([]string{"--class", "interactive", "--model", "gpt-5", "--url", srv.URL, "--json"}, &out, io.Discard)
	if exitCode(err) != 1 {
		t.Fatalf("err = %v (code %d)", err, exitCode(err))
	}
	var res admitResult
	if err := json.Unmarshal(out.Bytes(), &res); err != nil || res.Decision != "deny" || res.Reason != "weekly reserve" {
		t.Errorf("json = %q (%v)", out.String(), err)
	}
	if (*got)["class"] != "interactive" || (*got)["model"] != "gpt-5" {
		t.Errorf("request = %v", *got)
	}

	out.Reset()
	err = runAdmit([]string{"--account", "a", "--url", srv.URL}, &out, io.Discard)
	if exitCode(err) != 1 || out.String() != "deny: weekly reserve\n" {
		t.Errorf("plain deny: code %d stdout %q", exitCode(err), out.String())
	}
}

func TestAdmitNotFound(t *testing.T) {
	srv, _ := fakeControl(t, 404, `{"error":{"message":"unknown account \"ghost\""}}`)
	var out bytes.Buffer
	err := runAdmit([]string{"--account", "ghost", "--url", srv.URL}, &out, io.Discard)
	if exitCode(err) != 2 || !strings.Contains(err.Error(), `HTTP 404: unknown account "ghost"`) {
		t.Fatalf("err = %v", err)
	}
	if out.Len() != 0 {
		t.Errorf("stdout = %q", out.String())
	}
}

func TestAdmitUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	err := runAdmit([]string{"--account", "a", "--url", url, "--timeout", "2s"}, io.Discard, io.Discard)
	if exitCode(err) != 2 || !strings.Contains(err.Error(), "unreachable") {
		t.Fatalf("err = %v", err)
	}
}

func TestAdmitUsageErrors(t *testing.T) {
	for _, args := range [][]string{
		{},
		{"--account", "a", "--model", "m"},
		{"--account", "a", "extra"},
		{"--bogus"},
	} {
		if err := runAdmit(args, io.Discard, io.Discard); exitCode(err) != 2 {
			t.Errorf("%v: err = %v", args, err)
		}
	}
	srv, _ := fakeControl(t, 200, `<html>`)
	if err := runAdmit([]string{"--account", "a", "--url", srv.URL}, io.Discard, io.Discard); exitCode(err) != 2 {
		t.Errorf("garbage response: err = %v", err)
	}
}

// fakeControlRaw serves POST /control/v1/admit, recording the raw request body
// plus its decoded form. Unlike fakeControl's map[string]string it keeps nested
// objects, so a capability "requirements" object survives; an empty raw string
// means no request was sent at all.
func fakeControlRaw(t *testing.T, code int, resp string) (*httptest.Server, *map[string]any, *string) {
	t.Helper()
	var got map[string]any
	var raw string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/control/v1/admit" {
			http.NotFound(w, r)
			return
		}
		b, _ := io.ReadAll(r.Body)
		raw = string(b)
		got = nil
		_ = json.Unmarshal(b, &got)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = w.Write([]byte(resp))
	}))
	t.Cleanup(srv.Close)
	return srv, &got, &raw
}

const allowBody = `{"decision":"allow","account_id":"a","reason":"admitted"}`

// TestAdmitLegacyArgsUnchanged pins the old CLI contract: without any
// capability flag the request body carries no "requirements" key at all, so an
// existing invocation sends exactly what it used to.
func TestAdmitLegacyArgsUnchanged(t *testing.T) {
	srv, got, raw := fakeControlRaw(t, 200, allowBody)
	var out bytes.Buffer
	if err := runAdmit([]string{"--account", "a", "--url", srv.URL}, &out, io.Discard); exitCode(err) != 0 {
		t.Fatalf("err = %v", err)
	}
	if *raw != `{"account":"a","class":"background"}` {
		t.Errorf("account body = %s", *raw)
	}
	if len(*got) != 2 {
		t.Errorf("account request = %v", *got)
	}

	*raw = ""
	if err := runAdmit([]string{"--model", "m", "--url", srv.URL}, &out, io.Discard); exitCode(err) != 0 {
		t.Fatalf("err = %v", err)
	}
	if *raw != `{"class":"background","model":"m"}` {
		t.Errorf("model body = %s", *raw)
	}
}

// TestAdmitCapabilityFlags covers the happy path: every capability flag lands
// in one explicit profile, spelled the way the control API expects (notably
// json_schema), and nothing extra is invented.
func TestAdmitCapabilityFlags(t *testing.T) {
	srv, got, raw := fakeControlRaw(t, 200, allowBody)
	args := []string{
		"--model", "m", "--url", srv.URL,
		"--protocol", "responses",
		"--input-modalities", "text,image",
		"--requires-tools", "--requires-json-schema", "--stream",
	}
	if err := runAdmit(args, io.Discard, io.Discard); exitCode(err) != 0 {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(*raw, "jsonSchema") || strings.Contains(*raw, "jsonschema") {
		t.Errorf("wire spelling drifted from json_schema: %s", *raw)
	}
	if !strings.Contains(*raw, `"json_schema":true`) {
		t.Fatalf("body = %s", *raw)
	}
	reqs, ok := (*got)["requirements"].(map[string]any)
	if !ok {
		t.Fatalf("no requirements object: %s", *raw)
	}
	if reqs["protocol"] != "responses" || reqs["tools"] != true || reqs["json_schema"] != true || reqs["stream"] != true {
		t.Errorf("requirements = %v", reqs)
	}
	mods, ok := reqs["modalities"].([]any)
	if !ok || len(mods) != 2 || mods[0] != "text" || mods[1] != "image" {
		t.Errorf("modalities = %v", reqs["modalities"])
	}

	// A single feature flag generates a profile that omits the fields the caller
	// did not give: the CLI never fills in a default protocol or modality.
	*raw = ""
	if err := runAdmit([]string{"--model", "m", "--url", srv.URL, "--requires-tools"}, io.Discard, io.Discard); exitCode(err) != 0 {
		t.Fatalf("err = %v", err)
	}
	for _, forbidden := range []string{`"protocol"`, `"modalities"`} {
		if strings.Contains(*raw, forbidden) {
			t.Errorf("default %s invented in %s", forbidden, *raw)
		}
	}
	if !strings.Contains(*raw, `"tools":true`) {
		t.Errorf("body = %s", *raw)
	}

	// Blank entries in the modality list are skipped, not treated as unknown.
	*raw = ""
	if err := runAdmit([]string{"--model", "m", "--url", srv.URL, "--input-modalities", "text,,image,"}, io.Discard, io.Discard); exitCode(err) != 0 {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(*raw, `"modalities":["text","image"]`) {
		t.Errorf("body = %s", *raw)
	}
}

// TestAdmitCapabilityFlagValidation pins that a flag value outside the modelled
// vocabulary is a usage error the CLI catches locally, before any request goes
// out.
func TestAdmitCapabilityFlagValidation(t *testing.T) {
	srv, _, raw := fakeControlRaw(t, 200, allowBody)
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"--model", "m", "--url", srv.URL, "--protocol", "completion"}, "must be chat or responses"},
		{[]string{"--model", "m", "--url", srv.URL, "--input-modalities", "text,video"}, "must be text, image or audio"},
		{[]string{"--model", "m", "--url", srv.URL, "--input-modalities", "image,audio,weird"}, "must be text, image or audio"},
	}
	for _, c := range cases {
		*raw = ""
		err := runAdmit(c.args, io.Discard, io.Discard)
		if exitCode(err) != 2 || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%v: err = %v (code %d)", c.args, err, exitCode(err))
		}
		if *raw != "" {
			t.Errorf("%v: request sent despite a usage error: %s", c.args, *raw)
		}
	}
}

// TestAdmitTypedErrorSurfaced covers how a capability refusal from the server is
// reported: the stable error type is kept alongside the message so a script can
// branch on it, while an untyped legacy error keeps its old, unprefixed message.
// capability_unsupported is a definite "no account can serve this" answer, so it
// exits 1 like any other deny; an incomplete profile
// (capability_requirements_required) is a usage error and exits 2.
func TestAdmitTypedErrorSurfaced(t *testing.T) {
	srv, _, _ := fakeControlRaw(t, 400, `{"error":{"type":"capability_unsupported","message":"localrouter: no account supports the capabilities this request needs"}}`)
	err := runAdmit([]string{"--model", "m", "--url", srv.URL, "--protocol", "chat", "--input-modalities", "text"}, io.Discard, io.Discard)
	if exitCode(err) != 1 || !strings.Contains(err.Error(), "capability_unsupported") || !strings.Contains(err.Error(), "no account supports") {
		t.Fatalf("err = %v", err)
	}
	if !strings.HasPrefix(err.Error(), "admit: HTTP 400: capability_unsupported: ") {
		t.Errorf("err = %q", err.Error())
	}

	srvReq, _, _ := fakeControlRaw(t, 400, `{"error":{"type":"capability_requirements_required","message":"localrouter: capability requirements are required for this route"}}`)
	err = runAdmit([]string{"--model", "m", "--url", srvReq.URL}, io.Discard, io.Discard)
	if exitCode(err) != 2 || !strings.Contains(err.Error(), "capability_requirements_required") {
		t.Fatalf("requirements_required: err = %v (code %d)", err, exitCode(err))
	}

	srv2, _, _ := fakeControlRaw(t, 400, `{"error":{"message":"exactly one of model or account is required"}}`)
	err = runAdmit([]string{"--model", "m", "--url", srv2.URL}, io.Discard, io.Discard)
	if exitCode(err) != 2 || !strings.Contains(err.Error(), "admit: HTTP 400: exactly one of model or account is required") {
		t.Fatalf("legacy error = %v", err)
	}
}
