package app

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/hpst3r/localrouter/internal/httpguard"
)

// Finite safety nets applied by Serve when a Timeout is zero. config.Load
// already fills these in, but a hand-built App must never silently get an
// unbounded (zero) net/http deadline, which would disable the protection.
const (
	defaultHeaderTimeout   = 10 * time.Second
	defaultIdleTimeout     = 120 * time.Second
	defaultShutdownTimeout = 30 * time.Second
)

// drainSettle is how long Serve waits after a failed graceful drain before it
// returns, giving handlers whose connections were force-closed a moment to
// record ledger rows before the caller closes the ledger.
const drainSettle = 2 * time.Second

// ErrNotServing is returned by Serve when the App's lifecycle has already been
// stopped: a pre-start Shutdown is terminal, so a later Serve must not start a
// half-ready server that still accepts connections.
var ErrNotServing = errors.New("app: not serving")

// ErrAlreadyStarted is returned by a duplicate Serve call. Serve is one-shot;
// the second caller is rejected rather than silently racing to overwrite the
// published server.
var ErrAlreadyStarted = errors.New("app: serve already started")

// Serve runs the HTTP server on ln until ctx is cancelled, then drains
// in-flight requests for at most the configured shutdown timeout. It is the
// single place inbound deadlines are applied, so cmd/localrouter and tests
// build the exact same server.
//
// Lifecycle: Serve is one-shot and owns the server it publishes. A second
// concurrent call is rejected with ErrAlreadyStarted (and closes its supplied
// listener) instead of racing the first. Shutdown may be called from any
// goroutine at any time: a call that arrives before Serve is terminal, so a
// later Serve returns ErrNotServing and closes ln rather than starting a
// half-ready server. Readiness is never reset to serving.
//
// The server deliberately sets no WriteTimeout and no ReadTimeout: an overall
// write deadline would truncate healthy long-lived SSE responses. Slow header
// reads are bounded by ReadHeaderTimeout, an idle connection by IdleTimeout,
// and the request body by the httpguard body deadline wrapped around Handler.
func (a *App) Serve(ctx context.Context, ln net.Listener) error {
	// Publish (or reject) synchronously, before any goroutine is spawned and
	// before the accept loop can run, so no half-ready active server is ever
	// visible to a concurrent Shutdown.
	a.srvMu.Lock()
	if a.stopped {
		a.srvMu.Unlock()
		_ = ln.Close()
		return ErrNotServing
	}
	if a.started {
		a.srvMu.Unlock()
		_ = ln.Close()
		return ErrAlreadyStarted
	}
	a.started = true
	to := a.Timeouts
	if to.Header <= 0 {
		to.Header = defaultHeaderTimeout
	}
	if to.Idle <= 0 {
		to.Idle = defaultIdleTimeout
	}
	if to.Shutdown <= 0 {
		to.Shutdown = defaultShutdownTimeout
	}
	h := a.Handler
	if a.BodyGuard != nil {
		h = a.BodyGuard.Wrap(h)
	}
	srv := &http.Server{
		Handler:           h,
		ReadHeaderTimeout: to.Header,
		IdleTimeout:       to.Idle,
	}
	a.srv = srv
	a.srvMu.Unlock()
	log := a.Logger
	if log == nil {
		log = slog.Default()
	}
	errc := make(chan error, 1)
	go func() {
		if a.TLSCertFile != "" {
			errc <- srv.ServeTLS(ln, a.TLSCertFile, a.TLSKeyFile)
			return
		}
		errc <- srv.Serve(ln)
	}()
	select {
	case <-ctx.Done():
		a.BeginShutdown()
		log.Info("shutting down; draining in-flight requests", "grace", to.Shutdown.String())
		shutdownCtx, cancel := context.WithTimeout(context.Background(), to.Shutdown)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Warn("drain timeout; closing remaining connections", "err", err)
			_ = srv.Close()
			<-time.After(drainSettle)
		}
		return nil
	case err := <-errc:
		a.BeginShutdown()
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// Shutdown begins a graceful drain, waiting up to ctx's deadline.
//
// It is safe to call from any goroutine and at any point in the lifecycle. If
// a server is already published (Serve is running) it drains that server and
// returns its error, exactly as cancelling the context passed to Serve would.
// If called before Serve, it records a terminal stop intent: no server is
// published, the drain is skipped, and a later Serve is rejected with
// ErrNotServing. Either way readiness is never re-armed. Shutdown does not wait
// for Serve to return; callers that need that must observe its return value.
func (a *App) Shutdown(ctx context.Context) error {
	a.BeginShutdown()
	a.srvMu.Lock()
	srv := a.srv
	a.stopped = true
	a.srvMu.Unlock()
	if srv == nil {
		return nil
	}
	return srv.Shutdown(ctx)
}

// Serving reports whether this instance is willing to serve: true while it is
// running normally and false once shutdown has begun. Readiness is exactly this
// value combined with local storage health; it says nothing about upstream
// provider health.
func (a *App) Serving() bool { return a.serving.Load() }

// BeginShutdown marks the router not-ready. It is idempotent and safe to call
// from any goroutine.
func (a *App) BeginShutdown() {
	a.srvMu.Lock()
	a.serving.Store(false)
	a.srvMu.Unlock()
}

// BodyGuardFor builds the body-read guard for a timeout. It exists so callers
// that construct an App by hand (tests) share the wiring defaults.
func BodyGuardFor(body time.Duration) *httpguard.BodyTimeout { return httpguard.NewBodyTimeout(body) }
