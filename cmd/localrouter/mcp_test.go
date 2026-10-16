package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/mcpserver"
)

// devNull is a throwaway stderr for the CLI tests.
func devNull(t *testing.T) *os.File {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(t.TempDir(), "stderr.log"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

func TestMCPSubcommandRejectsBadFlags(t *testing.T) {
	errOut := devNull(t)
	cases := map[string][]string{
		"unknown flag":            {"--nope"},
		"positional argument":     {"--timeout", "5s", "extra"},
		"remote plaintext url":    {"--url", "http://router.example.com:8787"},
		"url with credentials":    {"--url", "http://user:pass@127.0.0.1:8787"},
		"url with query":          {"--url", "http://127.0.0.1:8787/?k=v"},
		"result budget too small": {"--max-result-chars", "10"},
		"timeout over the cap":    {"--timeout", "1h"},
		"missing key file":        {"--key-file", filepath.Join(t.TempDir(), "absent.key")},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			if err := runMCP(args, devNull(t), errOut); err == nil {
				t.Fatalf("runMCP(%v) = nil, want an error", args)
			}
		})
	}
}

// TestMCPSubcommandNeverEchoesMalformedURLCredentials pins the CLI-level
// sanitization: a --url that fails to parse AND carries userinfo must not print
// the secret, in the returned error (which main.go writes to stderr) or in any
// usage text, both of which a supervisor captures to its log.
func TestMCPSubcommandNeverEchoesMalformedURLCredentials(t *testing.T) {
	for _, secret := range []string{"sup3rsecret", "hunter2"} {
		raw := "http://user:" + secret + "@[::1"
		var buf bytes.Buffer
		errOut, stderrText := writerFile(t, &buf)
		err := runMCP([]string{"--url", raw}, devNull(t), errOut)
		if err == nil {
			t.Fatalf("runMCP(--url %q) = nil, want an error", raw)
		}
		msg := err.Error() + stderrText()
		if strings.Contains(msg, secret) {
			t.Errorf("stderr/error leaked the credential for %q: %q", raw, msg)
		}
		if strings.Contains(msg, raw) {
			t.Errorf("stderr/error echoed the raw url for %q: %q", raw, msg)
		}
	}
}

// writerFile adapts a bytes.Buffer to the *os.File runMCP writes usage to.
//
// The write end is a pipe drained into buf by a background goroutine, so buf
// must not be read until that goroutine has observed EOF and returned. The
// returned stderrText func closes the write end, joins the drain, and only then
// returns the captured output; call it before reading buf (or before the
// buffer's own cleanup), otherwise the reader races the drain under -race.
func writerFile(t *testing.T, buf *bytes.Buffer) (*os.File, func() string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		_, _ = io.Copy(buf, r)
	}()
	var once sync.Once
	stderrText := func() string {
		once.Do(func() {
			_ = w.Close()
			<-drained
			_ = r.Close()
		})
		return buf.String()
	}
	t.Cleanup(func() { _ = stderrText() })
	return w, stderrText
}

// TestMCPSubcommandNeverLoadsRuntimeConfig proves the subcommand cannot reach
// the router configuration or the ledger: --config is not one of its flags, so
// a stock invocation that would otherwise load ~/.config/localrouter/config.yaml
// is rejected outright, and HOME pointing at a broken config changes nothing.
func TestMCPSubcommandNeverLoadsRuntimeConfig(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".config", "localrouter"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".config", "localrouter", "config.yaml"),
		[]byte("this: : is not: valid yaml"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)

	if err := runMCP([]string{"--config", "/nonexistent"}, devNull(t), devNull(t)); err == nil {
		t.Fatal("--config must not be accepted by the mcp subcommand")
	}
	err := runMCP([]string{"--url", "http://router.example.com"}, devNull(t), devNull(t))
	if err == nil {
		t.Fatal("an insecure remote url must be rejected")
	}
	if !strings.HasPrefix(err.Error(), "mcp: ") {
		t.Errorf("error = %q, want it prefixed with %q", err, "mcp: ")
	}
}

func TestMCPSubcommandHelpIsAccepted(t *testing.T) {
	if err := runMCP([]string{"-h"}, devNull(t), devNull(t)); err != nil {
		t.Fatalf("runMCP(-h) = %v, want nil", err)
	}
}

// ---- F3: a deliberate shutdown must be a clean exit -----------------------

func TestServeMCPTreatsCancellationAsCleanExit(t *testing.T) {
	srv, err := mcpserver.New(mcpserver.Config{URL: "http://127.0.0.1:9"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := serveMCP(ctx, srv); err != nil {
		t.Fatalf("serveMCP(cancelled ctx) = %v, want nil: a deliberate shutdown is not a crash", err)
	}
}

// TestMCPSubcommandSignalShutdownIsClean is the end-to-end form of the same
// guarantee: SIGTERM on a live stdio session (the MCP host stopped it, or the
// operator hit Ctrl-C) must exit 0 with nothing on stderr, so a supervisor with
// Restart=on-failure does not read a deliberate stop as a crash.
//
// The child is this test binary re-executed: the helper branch runs the real
// subcommand on os.Stdin (which the parent keeps open, so the server blocks),
// and the parent signals it. Signals sent before the handler is installed would
// kill the process outright, so the attempt is retried with a longer settle.
func TestMCPSubcommandSignalShutdownIsClean(t *testing.T) {
	const helperEnv = "LOCALROUTER_MCP_SIGNAL_HELPER"
	if os.Getenv(helperEnv) == "1" {
		if err := runMCP([]string{"--url", "http://127.0.0.1:9"}, os.Stdout, os.Stderr); err != nil {
			fmt.Fprintln(os.Stderr, "localrouter:", err)
			os.Exit(1)
		}
		os.Exit(0)
	}

	var last string
	for attempt := 0; attempt < 3; attempt++ {
		cmd := exec.Command(os.Args[0], "-test.run=TestMCPSubcommandSignalShutdownIsClean")
		cmd.Env = append(os.Environ(), helperEnv+"=1")
		stdin, err := cmd.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		cmd.Stdout = io.Discard
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Duration(400*(attempt+1)) * time.Millisecond)
		_ = cmd.Process.Signal(syscall.SIGTERM)
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case err := <-done:
			_ = stdin.Close()
			if err == nil && stderr.Len() == 0 {
				return // clean exit: rc 0, nothing on stderr
			}
			last = fmt.Sprintf("attempt %d: wait=%v stderr=%q", attempt, err, stderr.String())
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Kill()
			_ = stdin.Close()
			t.Fatalf("mcp did not exit within 10s of SIGTERM (%s)", stderr.String())
		}
	}
	t.Fatalf("SIGTERM did not produce a clean exit: %s", last)
}
