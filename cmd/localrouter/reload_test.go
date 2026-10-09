package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
)

// syncBuffer is a race-safe log sink: the reload loop writes it from its own
// goroutine while the test reads it, so a plain bytes.Buffer would race.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// fakeSignal is a platform-independent os.Signal used to drive reloadLoop
// without delivering a real process signal (which could kill the test host).
type fakeSignal struct{}

func (fakeSignal) Signal()        {}
func (fakeSignal) String() string { return "fake" }

// fakeReloader records the path it was asked to reload and returns a canned
// status, standing in for the *app.App seam the CLI depends on.
type fakeReloader struct {
	calls   atomic.Int64
	path    atomic.Value
	out     core.ReloadStatus
	err     error
	blockOn chan struct{}
}

func (r *fakeReloader) Reload(path string) (core.ReloadStatus, error) {
	r.path.Store(path)
	if r.blockOn != nil {
		<-r.blockOn
	}
	r.calls.Add(1)
	return r.out, r.err
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition not met within deadline")
}

// A single signal triggers exactly one reload, with the configured path, and
// the success is logged by generation.
func TestReloadLoopAppliesSignal(t *testing.T) {
	var buf syncBuffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	r := &fakeReloader{out: core.ReloadStatus{Generation: 2, OK: true}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan os.Signal, 1)
	done := make(chan struct{})
	go func() { reloadLoop(ctx, r, logger, "/etc/localrouter/config.yaml", ch); close(done) }()

	ch <- fakeSignal{}
	waitFor(t, func() bool { return r.calls.Load() == 1 })
	if got := r.path.Load(); got != "/etc/localrouter/config.yaml" {
		t.Fatalf("reload path = %v", got)
	}
	if !strings.Contains(buf.String(), "generation=2") {
		t.Fatalf("log missing generation: %s", buf.String())
	}

	// Cancelling ctx must stop the loop (no goroutine leak, signal.Stop runs).
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("reloadLoop did not stop on ctx cancellation")
	}
}

// A rejected reload logs the sanitized status reason and never the raw error,
// which may embed a secret filesystem path.
func TestReloadLoopSanitizesFailure(t *testing.T) {
	var buf syncBuffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	r := &fakeReloader{
		out: core.ReloadStatus{
			Generation:  5,
			OK:          false,
			Reason:      "client keys: key file mode",
			RestartOnly: []string{"listen"},
		},
		err: errors.New("open /home/wporter/.secrets/me.key: permission denied"),
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan os.Signal, 1)
	go reloadLoop(ctx, r, logger, "/etc/lr.yaml", ch)

	ch <- fakeSignal{}
	waitFor(t, func() bool { return r.calls.Load() == 1 })
	waitFor(t, func() bool { return strings.Contains(buf.String(), "client keys: key file mode") })

	log := buf.String()
	if !strings.Contains(log, "reload rejected") {
		t.Fatalf("log missing rejection line: %s", log)
	}
	if strings.Contains(log, "permission denied") || strings.Contains(log, "/home/wporter/.secrets") {
		t.Fatalf("raw error leaked into log: %s", log)
	}
	if !strings.Contains(log, "listen") {
		t.Fatalf("restart_only keys not reported: %s", log)
	}
	cancel()
}

// When the producer cannot name a reason, the log still carries a fixed,
// sanitized placeholder instead of the raw error text.
func TestReloadLoopDefaultsReason(t *testing.T) {
	var buf syncBuffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	r := &fakeReloader{out: core.ReloadStatus{OK: false}, err: errors.New("boom secret")}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan os.Signal, 1)
	go reloadLoop(ctx, r, logger, "/etc/lr.yaml", ch)

	ch <- fakeSignal{}
	waitFor(t, func() bool { return r.calls.Load() == 1 })
	waitFor(t, func() bool { return strings.Contains(buf.String(), "reload rejected") })

	if strings.Contains(buf.String(), "boom secret") {
		t.Fatalf("raw error leaked: %s", buf.String())
	}
	cancel()
}

// A burst of rapid signals must never block the loop or wedge the process:
// the buffered channel coalesces and every delivered signal that is observed
// performs a bounded reload.
func TestReloadLoopSurvivesBurst(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(&syncBuffer{}, nil))
	r := &fakeReloader{out: core.ReloadStatus{Generation: 9, OK: true}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan os.Signal, 1)
	done := make(chan struct{})
	go func() { reloadLoop(ctx, r, logger, "/etc/lr.yaml", ch); close(done) }()

	for i := 0; i < 8; i++ {
		select {
		case ch <- fakeSignal{}:
		case <-time.After(3 * time.Second):
			t.Fatal("signal send blocked: loop not draining")
		}
	}
	waitFor(t, func() bool { return r.calls.Load() >= 1 })
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("loop did not stop")
	}
}
