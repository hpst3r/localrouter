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
