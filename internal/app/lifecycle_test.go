package app

import (
	"context"
	"errors"
	"net"
	"net/http"
	"testing"
	"time"
)

// testLifecycleApp builds a hand-wired App with readiness armed, mirroring what
// Build does, without touching the ledger/quota graph.
func testLifecycleApp() *App {
	a := &App{
		Handler:  http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}),
		Timeouts: Timeouts{Header: time.Second, Idle: time.Second, Shutdown: time.Second},
	}
	a.serving.Store(true)
	return a
}

// TestServeShutdownConcurrentLifecycleRace drives Serve and Shutdown from
// separate goroutines (the classic signal-handler shape) and asserts the
// lifecycle is race-free. Regression test for review F1/F7: the unsynchronised
// publish of a.srv in Serve raced the load in Shutdown under -race.
func TestServeShutdownConcurrentLifecycleRace(t *testing.T) {
	for i := 0; i < 200; i++ {
		a := testLifecycleApp()
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		sd := make(chan error, 1)
		go func() { sd <- a.Serve(ctx, ln) }()
		hd := make(chan error, 1)
		go func() { hd <- a.Shutdown(context.Background()) }()

		if err := <-hd; err != nil {
			t.Fatalf("iteration %d: Shutdown: %v", i, err)
		}
		cancel()
		// Serve either ran the server (nil) or lost the race to a pre-start
		// Shutdown (ErrNotServing). Both are correct and race-free.
		if err := <-sd; err != nil && !errors.Is(err, ErrNotServing) {
			t.Fatalf("iteration %d: Serve: %v", i, err)
		}
		if a.Serving() {
			t.Fatalf("iteration %d: readiness re-armed after shutdown", i)
		}
	}
}

// waitDial blocks until the listener accepts a TCP connection, so tests never
// race the accept loop.
func waitDial(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			_ = c.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("listener %s never accepted: %v", addr, err)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// TestServeRejectedAfterShutdownBeforeServe covers review F2: Shutdown before
// Serve must not leave a half-ready server that still accepts connections.
func TestServeRejectedAfterShutdownBeforeServe(t *testing.T) {
	a := testLifecycleApp()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()

	if err := a.Shutdown(context.Background()); err != nil {
		t.Fatalf("pre-Serve Shutdown: %v", err)
	}
	if a.Serving() {
		t.Fatal("Shutdown reset readiness to serving")
	}
	err = a.Serve(context.Background(), ln)
	if !errors.Is(err, ErrNotServing) {
		t.Fatalf("Serve after pre-start Shutdown = %v, want ErrNotServing", err)
	}
	if a.Serving() {
		t.Fatal("rejected Serve re-armed readiness")
	}
	if c, derr := net.DialTimeout("tcp", addr, 300*time.Millisecond); derr == nil {
		_ = c.Close()
		t.Fatal("supplied listener still accepting after rejected Serve")
	}
}

// TestServeOneShotRejectsDuplicate covers the one-shot contract: a second
// Serve must be rejected, and its supplied listener closed, instead of racing
// to overwrite the published server.
func TestServeOneShotRejectsDuplicate(t *testing.T) {
	a := testLifecycleApp()
	ln1, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- a.Serve(ctx, ln1) }()
	waitDial(t, ln1.Addr().String())

	ln2, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen2: %v", err)
	}
	addr2 := ln2.Addr().String()
	if err := a.Serve(context.Background(), ln2); !errors.Is(err, ErrAlreadyStarted) {
		t.Fatalf("duplicate Serve = %v, want ErrAlreadyStarted", err)
	}
	if c, derr := net.DialTimeout("tcp", addr2, 300*time.Millisecond); derr == nil {
		_ = c.Close()
		t.Fatal("duplicate Serve's listener still accepting")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("first Serve: %v", err)
	}
}
