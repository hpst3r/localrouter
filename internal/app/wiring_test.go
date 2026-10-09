package app_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/app"
	"github.com/hpst3r/localrouter/internal/config"
)

// wiringEnv is a real-TCP LocalRouter instance: an actual net/http.Server on an
// ephemeral loopback port, so header, body and idle deadlines are exercised by
// the OS network stack instead of httptest's in-process shortcut.
type wiringEnv struct {
	t      *testing.T
	app    *app.App
	addr   string
	ln     net.Listener
	cancel context.CancelFunc
	done   chan error
}

type wiringOpts struct {
	header, body, idle, shutdown time.Duration
	maxConcurrent                int
	perClient                    map[string]int
	upstream                     http.HandlerFunc
}

const wiringKey = "wiring-client-key-SECRET0123456789"

// startWiring builds a minimal LocalRouter over an httptest upstream and runs
// the real server loop on 127.0.0.1:0.
func startWiring(t *testing.T, o wiringOpts) *wiringEnv {
	t.Helper()
	if o.header == 0 {
		o.header = 2 * time.Second
	}
	if o.body == 0 {
		o.body = 600 * time.Millisecond
	}
	if o.idle == 0 {
		o.idle = 2 * time.Second
	}
	if o.shutdown == 0 {
		o.shutdown = 2 * time.Second
	}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if o.upstream != nil {
			o.upstream(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"r","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	t.Cleanup(up.Close)

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ide.key"), []byte(wiringKey+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WIRING_TEST_KEY", "sk-upstream")

	limits := ""
	if o.maxConcurrent > 0 || len(o.perClient) > 0 {
		limits = fmt.Sprintf("limits:\n  max_concurrent: %d\n", o.maxConcurrent)
		if len(o.perClient) > 0 {
			limits += "  max_concurrent_per_client:\n"
			for k, v := range o.perClient {
				limits += fmt.Sprintf("    %s: %d\n", k, v)
			}
		}
	}
	yaml := fmt.Sprintf(`data_dir: data
timeouts:
  header: %s
  body: %s
  idle: %s
  shutdown: %s
%sclients:
  - {name: ide, class: interactive, key_file: ide.key}
accounts:
  - id: up
    provider: openai_compat
    base_url: %s/v1
    api_key_env: WIRING_TEST_KEY
routes:
  - name: r
    models: [m]
    interactive: [up]
`, o.header, o.body, o.idle, o.shutdown, limits, up.URL)
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	a, err := app.Build(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), app.Overrides{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	env := &wiringEnv{t: t, app: a, addr: ln.Addr().String(), ln: ln, cancel: cancel, done: make(chan error, 1)}
	go func() { env.done <- a.Serve(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-env.done:
		case <-time.After(10 * time.Second):
			t.Error("Serve did not return after cancel")
		}
		_ = a.Close()
	})
	env.waitReady()
	return env
}

// waitReady polls /healthz until the listener answers, so tests never race the
// server's accept loop.
func (e *wiringEnv) waitReady() {
	e.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := http.Get("http://" + e.addr + "/healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("server never became ready: %v", err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (e *wiringEnv) get(path string) *http.Response {
	e.t.Helper()
	resp, err := http.Get("http://" + e.addr + path)
	if err != nil {
		e.t.Fatalf("GET %s: %v", path, err)
	}
	return resp
}

// rawConn dials the listener and returns a buffered reader so tests can speak
// HTTP by hand and stall deliberately.
func (e *wiringEnv) rawConn() (net.Conn, *bufio.Reader) {
	e.t.Helper()
	c, err := net.Dial("tcp", e.addr)
	if err != nil {
		e.t.Fatal(err)
	}
	return c, bufio.NewReader(c)
}

// A client that never finishes its request headers is dropped by the header
// deadline; the listener stays healthy for the next request.
func TestServeSlowHeaderTimeoutRecoversRealTCP(t *testing.T) {
	env := startWiring(t, wiringOpts{header: 150 * time.Millisecond})

	c, br := env.rawConn()
	defer c.Close()
	// Request line and one header, then stall before the blank line.
	if _, err := fmt.Fprintf(c, "POST /v1/responses HTTP/1.1\r\nHost: %s\r\n", env.addr); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)

	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	line, err := br.ReadString('\n')
	if err == nil {
		// net/http may answer 408 before closing; that is still a bounded reject.
		if !strings.Contains(line, "408") && !strings.Contains(line, "400") {
			t.Fatalf("slow header got %q, want 408/400 or close", line)
		}
	} else if !errors.Is(err, io.EOF) && !isNetTimeout(err) {
		t.Fatalf("slow header: %v", err)
	}

	resp := env.get("/healthz")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health after slow header: %d", resp.StatusCode)
	}
}

// A body that never arrives fails within the body deadline instead of pinning
// the connection, and the next request is served normally.
func TestServeIncompleteBodyTimeoutRecoversRealTCP(t *testing.T) {
	env := startWiring(t, wiringOpts{header: 2 * time.Second, body: 250 * time.Millisecond})

	c, br := env.rawConn()
	defer c.Close()
	// Announce a 1000-byte body, send one byte, then stall.
	if _, err := fmt.Fprintf(c, "POST /v1/responses HTTP/1.1\r\nHost: %s\r\nContent-Type: application/json\r\nContent-Length: 1000\r\n\r\n{", env.addr); err != nil {
		t.Fatal(err)
	}
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	status, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("no response to incomplete body: %v", err)
	}
	if !strings.Contains(status, "4") || strings.HasPrefix(status, "5") {
		t.Fatalf("incomplete body status %q, want 4xx", strings.TrimSpace(status))
	}

	resp := env.get("/healthz")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health after body timeout: %d", resp.StatusCode)
	}
}

// The body deadline governs reading the request, not writing the response: an
// SSE stream that far outlives it must complete intact.
func TestServeStreamOutlivesBodyTimeoutRealTCP(t *testing.T) {
	const first = "event: response.created\ndata: {\"type\":\"response.created\"}\n\n"
	const last = "event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n"
	env := startWiring(t, wiringOpts{
		body: 250 * time.Millisecond,
		upstream: func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, first)
			w.(http.Flusher).Flush()
			time.Sleep(700 * time.Millisecond) // three times the body deadline
			_, _ = io.WriteString(w, last)
			w.(http.Flusher).Flush()
		},
	})

	start := time.Now()
	req, _ := http.NewRequest(http.MethodPost, "http://"+env.addr+"/v1/responses", strings.NewReader(`{"model":"m","stream":true}`))
	req.Header.Set("Authorization", "Bearer "+wiringKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d body %q", resp.StatusCode, body)
	}
	if string(body) != first+last {
		t.Fatalf("stream truncated or altered: %q", body)
	}
	if elapsed := time.Since(start); elapsed < 600*time.Millisecond {
		t.Fatalf("stream returned in %v; it should have outlived the body deadline", elapsed)
	}
}

// Cancelling the context begins shutdown: readiness flips before the drain
// finishes, the in-flight request still completes, and Serve returns.
func TestServeShutdownFlipsReadyAndDrainsRealTCP(t *testing.T) {
	release := make(chan struct{})
	env := startWiring(t, wiringOpts{
		shutdown: 5 * time.Second,
		upstream: func(w http.ResponseWriter, r *http.Request) {
			<-release
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"r","usage":{"input_tokens":1,"output_tokens":1}}`)
		},
	})

	inflight := make(chan *http.Response, 1)
	go func() {
		req, _ := http.NewRequest(http.MethodPost, "http://"+env.addr+"/v1/responses", strings.NewReader(`{"model":"m"}`))
		req.Header.Set("Authorization", "Bearer "+wiringKey)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			inflight <- resp
		}
	}()
	// Let the request reach the upstream before cancelling.
	time.Sleep(150 * time.Millisecond)

	env.cancel()
	// Readiness must flip promptly, well before the drain finishes.
	deadline := time.Now().Add(2 * time.Second)
	for env.app.Serving() {
		if time.Now().After(deadline) {
			t.Fatal("readiness did not flip after shutdown began")
		}
		time.Sleep(5 * time.Millisecond)
	}
	// New connections are refused while the old request drains.
	if _, err := net.Dial("tcp", env.addr); err == nil {
		t.Fatal("listener accepted a new connection after shutdown began")
	}

	close(release)
	select {
	case resp := <-inflight:
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("drained request status %d", resp.StatusCode)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("in-flight request was not drained")
	}
}

// A rejected (429) request that trickles its body must not pin the connection:
// the armed body deadline bounds net/http's own drain of the unread body, and
// the guard spawns no goroutine of its own.
func TestServeRejectedPostSlowBodyClosesRealTCP(t *testing.T) {
	release := make(chan struct{})
	// Guard against a failed assertion leaving the upstream handler blocked,
	// which would otherwise hang httptest teardown for the whole test timeout.
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	env := startWiring(t, wiringOpts{
		body:          300 * time.Millisecond,
		maxConcurrent: 1,
		upstream: func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"r","usage":{"input_tokens":1,"output_tokens":1}}`)
		},
	})
	before := runtime.NumGoroutine()

	// Hold the single slot with a complete request.
	held, heldBr := env.rawConn()
	defer held.Close()
	fmt.Fprintf(held, "POST /v1/responses HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer %s\r\nContent-Type: application/json\r\nContent-Length: 13\r\n\r\n{\"model\":\"m\"}", env.addr, wiringKey)
	deadline := time.Now().Add(3 * time.Second)
	for env.app.Limiter.Stats().GlobalActive != 1 {
		if time.Now().After(deadline) {
			t.Fatal("held request never occupied the slot")
		}
		time.Sleep(5 * time.Millisecond)
	}
	// Confirm the held request reached the (blocked) upstream.
	_ = heldBr // the response body stays unread until the upstream releases

	// Rejected request announces a large body but sends only one byte.
	c, br := env.rawConn()
	defer c.Close()
	fmt.Fprintf(c, "POST /v1/responses HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer %s\r\nContent-Type: application/json\r\nContent-Length: 100000\r\n\r\n{", env.addr, wiringKey)
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("no response to rejected slow-body request: %v", err)
	}
	if resp.StatusCode != http.StatusTooManyRequests {
		resp.Body.Close()
		t.Fatalf("rejected status %d, want 429", resp.StatusCode)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	// The connection must be released (EOF) rather than pinned by a drain of
	// the 99,999 bytes the client never sent.
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := br.ReadByte(); err == nil {
		t.Fatal("connection stayed open, draining a body the client never sent")
	} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatal("rejected connection pinned past the body deadline")
	}

	close(release)
	// Give the released held request a moment to finish before counting.
	settle := time.Now().Add(3 * time.Second)
	for runtime.NumGoroutine() > before+8 && time.Now().Before(settle) {
		time.Sleep(20 * time.Millisecond)
	}
	if after := runtime.NumGoroutine(); after > before+8 {
		t.Fatalf("goroutines grew from %d to %d; suspected leak", before, after)
	}
}

func isNetTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}
