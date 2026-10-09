package httpguard_test

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/httpguard"
)

// server runs the wrapped handler on a real loopback TCP listener so read
// deadlines act on a genuine net.Conn, not an in-process shortcut.
func server(t *testing.T, budget time.Duration, h http.HandlerFunc) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: httpguard.NewBodyTimeout(budget).Wrap(h)}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return ln.Addr().String()
}

// A body that arrives too slowly fails the read within the budget rather than
// blocking the handler forever.
func TestBodyTimeoutBoundsSlowBody(t *testing.T) {
	addr := server(t, 200*time.Millisecond, func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.ReadAll(r.Body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	c, _ := net.Dial("tcp", addr)
	defer c.Close()
	// Announce 10 bytes but send only one, then stall past the budget.
	fmt.Fprintf(c, "POST /x HTTP/1.1\r\nHost: %s\r\nContent-Length: 10\r\n\r\n{", addr)
	start := time.Now()
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	status, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		t.Fatalf("no response: %v", err)
	}
	if !strings.Contains(status, "400") {
		t.Fatalf("slow body status %q, want 400", strings.TrimSpace(status))
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("slow body bounded in %v; budget was 200ms", elapsed)
	}
}

// The deadline is cleared the moment the body read ends: a handler that reads
// its whole body and then writes a response far beyond the budget must not be
// cut off. This is the SSE property — the budget governs the request, never
// the response.
func TestBodyTimeoutClearedAtBodyEndNotStreamEnd(t *testing.T) {
	addr := server(t, 150*time.Millisecond, func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.ReadAll(r.Body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		time.Sleep(600 * time.Millisecond) // 4x the body budget, still fine
		_, _ = io.WriteString(w, "streamed")
	})

	start := time.Now()
	resp, err := http.Post("http://"+addr+"/x", "application/json", strings.NewReader(`{"k":"v"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(body) != "streamed" {
		t.Fatalf("status %d body %q", resp.StatusCode, body)
	}
	if elapsed := time.Since(start); elapsed < 500*time.Millisecond {
		t.Fatalf("response returned in %v; the body budget leaked into the write path", elapsed)
	}
}

// Wrap must not hide the ResponseWriter behind an adapter, or a streaming
// handler's Flusher assertion would fail.
func TestBodyTimeoutPreservesFlusher(t *testing.T) {
	addr := server(t, time.Second, func(w http.ResponseWriter, r *http.Request) {
		f, ok := w.(http.Flusher)
		if !ok {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, "no flusher")
			return
		}
		_, _ = io.WriteString(w, "event: a\n\n")
		f.Flush()
	})

	resp, err := http.Post("http://"+addr+"/x", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(body) != "event: a\n\n" {
		t.Fatalf("flusher not preserved: status %d body %q", resp.StatusCode, body)
	}
}

// A request with no body is passed straight through, so it never inherits a
// read deadline it does not need.
func TestBodyTimeoutBodylessPassthrough(t *testing.T) {
	var sawBody bool
	addr := server(t, 50*time.Millisecond, func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			var err error
			_, err = io.ReadAll(r.Body)
			sawBody = err == nil
		}
		_, _ = io.WriteString(w, "ok")
	})

	resp, err := http.Get("http://" + addr + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("bodyless GET status %d", resp.StatusCode)
	}
	if !sawBody {
		t.Fatal("handler saw a body read error on a bodyless request")
	}
}

// A body-carrying request followed by a bodyless one on the same keep-alive
// connection must both succeed: the body budget must never leak into the
// second request.
func TestBodyTimeoutKeepAliveNoStaleDeadline(t *testing.T) {
	addr := server(t, 150*time.Millisecond, func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		time.Sleep(300 * time.Millisecond) // outlive the body budget, write freely
		_, _ = io.WriteString(w, "ok:"+string(body))
	})

	c, _ := net.Dial("tcp", addr)
	defer c.Close()
	br := bufio.NewReader(c)

	// Request 1: body-carrying, reads to EOF so the budget is lifted.
	fmt.Fprintf(c, "POST /x HTTP/1.1\r\nHost: %s\r\nContent-Length: 2\r\n\r\n{}", addr)
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	resp1, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("request 1: %v", err)
	}
	b1, _ := io.ReadAll(resp1.Body)
	resp1.Body.Close()
	if resp1.StatusCode != http.StatusOK || string(b1) != "ok:{}" {
		t.Fatalf("request 1 status %d body %q", resp1.StatusCode, b1)
	}

	// Request 2 on the SAME connection: bodyless, must not inherit the budget.
	fmt.Fprintf(c, "GET /healthz HTTP/1.1\r\nHost: %s\r\n\r\n", addr)
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	resp2, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("request 2 on keep-alive conn: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp2.Body)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("request 2 status %d, want 200", resp2.StatusCode)
	}
}
