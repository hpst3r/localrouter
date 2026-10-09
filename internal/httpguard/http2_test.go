package httpguard_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/httpguard"
)

// h2Server starts a real TLS server with HTTP/2 enabled and returns it together
// with a client whose transport explicitly sets ForceAttemptHTTP2, so the guard
// is exercised over a genuine HTTP/2 stream rather than an HTTP/1.1 loopback
// connection. The listener, h2 stack and client connection pool are all torn
// down via t.Cleanup, and every request carries a finite context deadline, so
// no case can hang the suite.
func h2Server(t *testing.T, h http.Handler) (*httptest.Server, *http.Client) {
	t.Helper()
	srv := httptest.NewUnstartedServer(h)
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)

	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	tr := &http.Transport{
		ForceAttemptHTTP2: true,
		TLSClientConfig:   &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
	}
	t.Cleanup(tr.CloseIdleConnections)
	return srv, &http.Client{Transport: tr}
}

// requireHTTP2 fails unless the response actually arrived over HTTP/2, so a
// silent downgrade to HTTP/1.1 can never let an h2 regression "pass".
func requireHTTP2(t *testing.T, resp *http.Response) {
	t.Helper()
	if resp.ProtoMajor != 2 {
		t.Fatalf("response proto = %q; want HTTP/2 (ProtoMajor 2)", resp.Proto)
	}
}

// slowBodyStream yields prefix and then blocks until stop is called, standing
// in for a client that stalls part-way through its request body.
func slowBodyStream(prefix string) (io.Reader, func()) {
	pr, pw := io.Pipe()
	go func() { _, _ = pw.Write([]byte(prefix)) }()
	return pr, func() { _ = pw.CloseWithError(errors.New("test cleanup")) }
}

// On HTTP/2 the guard must bound an incomplete slow body exactly as it does on
// HTTP/1.1: net/http's bundled h2 stack exposes SetReadDeadline on the
// per-stream response writer, and Wrap keeps the concrete writer type-assertable
// by passing it through untouched. The no-guard control subtest runs the same
// stalled body against the bare handler and requires that it is NOT answered
// within a window several times the guard budget, proving the bound comes from
// the guard rather than from some other layer. The control mutates no source:
// it simply omits the Wrap call for its own server.
func TestBodyTimeoutHTTP2BoundsSlowBody(t *testing.T) {
	const budget = 150 * time.Millisecond

	// guardedWindow is generous (13x budget) so a slow CI never flakes; the
	// controlWindow is 6x budget, comfortably clear of the armed deadline.
	const guardedWindow = 2 * time.Second
	const controlWindow = 900 * time.Millisecond

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.ReadAll(r.Body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	cases := []struct {
		name      string
		guard     bool
		wantBound bool
	}{
		{"guarded", true, true},
		{"control_no_guard", false, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var root http.Handler = handler
			if tc.guard {
				root = httpguard.NewBodyTimeout(budget).Wrap(handler)
			}
			srv, client := h2Server(t, root)

			body, stop := slowBodyStream("{")
			defer stop()

			ctxTimeout := guardedWindow
			if !tc.wantBound {
				ctxTimeout = controlWindow
			}
			ctx, cancel := context.WithTimeout(context.Background(), ctxTimeout)
			defer cancel()

			req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/x", body)
			if err != nil {
				t.Fatal(err)
			}
			req.ContentLength = 10 // announce more than we ever send

			start := time.Now()
			resp, err := client.Do(req)
			elapsed := time.Since(start)

			if tc.wantBound {
				if err != nil {
					t.Fatalf("guarded slow h2 body: %v (elapsed %v)", err, elapsed)
				}
				defer resp.Body.Close()
				requireHTTP2(t, resp)
				if resp.StatusCode != http.StatusBadRequest {
					t.Fatalf("guarded slow h2 body status %d, want 400", resp.StatusCode)
				}
				if elapsed > guardedWindow {
					t.Fatalf("slow h2 body bounded in %v; budget was %v", elapsed, budget)
				}
				return
			}

			// Control: without the guard nothing bounds the stalled read, so
			// the client must still be waiting when its own deadline fires.
			if err == nil {
				resp.Body.Close()
				t.Fatalf("no-guard control answered %q in %v; the h2 read deadline bound an unwrapped handler", resp.Status, elapsed)
			}
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("no-guard control failed with %v; want the client deadline to expire", err)
			}
			if elapsed < budget*3 {
				t.Fatalf("no-guard control gave up after %v, under 3x the %v budget", elapsed, budget)
			}
		})
	}
}

// A healthy h2 request whose body is read promptly and whose response then
// streams for several times the body budget must survive to completion: the
// guard arms only the connection read side and lifts it at clean body end, so
// it can never truncate a long-lived SSE response.
func TestBodyTimeoutHTTP2SSEOutlivesBodyBudget(t *testing.T) {
	const budget = 150 * time.Millisecond

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f, ok := w.(http.Flusher)
		if !ok {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if _, err := io.WriteString(w, "event: start\n\n"); err != nil {
			return
		}
		f.Flush()
		time.Sleep(4 * budget) // 600ms: four times the body budget
		if _, err := io.WriteString(w, "event: tick\ndata: "+string(body)+"\n\n"); err != nil {
			return
		}
		f.Flush()
	})
	srv, client := h2Server(t, httpguard.NewBodyTimeout(budget).Wrap(handler))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/stream", strings.NewReader(`{"k":"v"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	requireHTTP2(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("h2 stream status %d, want 200", resp.StatusCode)
	}

	start := time.Now()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading h2 stream: %v", err)
	}
	if !strings.Contains(string(data), "event: tick") {
		t.Fatalf("h2 stream truncated by the body budget: %q", data)
	}
	if elapsed := time.Since(start); elapsed < 3*budget {
		t.Fatalf("h2 stream ended in %v; it never outlived the %v body budget", elapsed, budget)
	}
}
