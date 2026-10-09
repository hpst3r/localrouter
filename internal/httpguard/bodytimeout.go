// Package httpguard applies bounded inbound read deadlines to an HTTP server
// without ever bounding the response. LocalRouter streams long-lived SSE
// responses, so an overall write or request timeout is unacceptable: a body
// deadline must expire only while the request body is being read, and must be
// lifted the moment the body read ends so the stream that follows is free to
// run as long as the upstream keeps producing.
package httpguard

import (
	"io"
	"net/http"
	"time"
)

// BodyTimeout bounds how long reading a request body may take. The deadline is
// armed when a request that carries a body begins, and cleared as soon as the
// body is fully read. Because the deadline is attached to the connection's
// read side only, it cannot truncate a response written afterwards.
type BodyTimeout struct {
	d time.Duration
}

// NewBodyTimeout returns a guard with the given body-read budget. A
// non-positive budget disables the guard.
func NewBodyTimeout(d time.Duration) *BodyTimeout { return &BodyTimeout{d: d} }

// Wrap returns next with the body-read deadline applied. It passes the original
// http.ResponseWriter through untouched, so every optional interface a handler
// may assert (Flusher, Hijacker, io.ReaderFrom, CloseNotifier, ...) stays
// available; no Unwrap shim is needed.
//
// Read deadlines are a property of the connection, not the writer, and the
// stdlib honours them here on both transports: HTTP/1.1 via the net.Conn, and
// HTTP/2 via the bundled h2 stack, whose per-stream response writer implements
// SetReadDeadline (arming that stream's read deadline) and clears it on a zero
// time. Should a transport ever refuse the deadline, SetReadDeadline returns an
// error and the guard degrades to no deadline rather than failing the request.
func (b *BodyTimeout) Wrap(next http.Handler) http.Handler {
	if b == nil || b.d <= 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rc := http.NewResponseController(w)
		if r.Body == nil || r.ContentLength == 0 {
			// Clear any deadline left armed by an earlier body-carrying request
			// on this keep-alive connection, so a bodyless request is never
			// bounded by a stale budget.
			_ = rc.SetReadDeadline(time.Time{})
			next.ServeHTTP(w, r)
			return
		}
		// Arm the read deadline. If the connection cannot honour it we simply
		// proceed unbounded rather than reject a valid request.
		if err := rc.SetReadDeadline(time.Now().Add(b.d)); err != nil {
			next.ServeHTTP(w, r)
			return
		}
		r.Body = &deadlineBody{ReadCloser: r.Body, rc: rc}
		next.ServeHTTP(w, r)
	})
}

// deadlineBody clears the connection read deadline when the body read reaches a
// clean end, so the response that follows is never bounded by the body budget.
// A rejected request (for example a 429 from the concurrency limiter) never
// reads its body, so the deadline stays armed. net/http then drains the unread
// body in (*response).finishRequest before reusing the connection, but it drains
// (*response).reqBody — the original request body captured when the request was
// read, before this handler ran — never this wrapper. That original body's reads
// still carry the connection read deadline armed here, so the drain fails fast
// once the budget expires and a slow body cannot pin a rejected request's
// connection open. (The guard spawns no reader goroutine of its own; the drain
// belongs to net/http.)
type deadlineBody struct {
	io.ReadCloser
	rc *http.ResponseController
}

func (b *deadlineBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	// Clear the deadline ONLY on a clean body end (io.EOF). Clearing it on a
	// timeout would be a real bug: net/http drains an unread body in
	// (*response).finishRequest via io.Copy(io.Discard, ...), and although that
	// drain re-reads the ORIGINAL body rather than this wrapper, those reads
	// still carry the connection read deadline armed here. Lifting it would let
	// the drain block forever on bytes the client never sent, so the error
	// response would never be flushed; leaving the (now expired) deadline armed
	// makes the drain fail immediately instead.
	if err == io.EOF {
		b.clear()
	}
	return n, err
}

// Close lifts the deadline as a fallback for handlers that close the body
// without reading it to EOF. On the stdlib path this is largely redundant —
// net/http closes the original reqBody it captured, not this wrapper — but it is
// harmless and keeps a deadline from leaking into the next keep-alive request if
// a handler closes the wrapped body explicitly. The post-handler drain runs while
// the connection deadline is still armed, which is what bounds it.
func (b *deadlineBody) Close() error {
	err := b.ReadCloser.Close()
	b.clear()
	return err
}

// clear lifts the read deadline. A zero time means "no deadline"; errors are
// ignored because clearing a deadline on a closing connection is best-effort.
func (b *deadlineBody) clear() { _ = b.rc.SetReadDeadline(time.Time{}) }
