package discovery

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
)

// D16 (Amendment 1 item 4). One hung discovering account makes every on-demand refresh wait the full
// RefreshWait, even when the healthy account that lists the new model answered
// in milliseconds: the waiters are released only when pollAll finishes every
// account, although each account's listing is published as soon as its own
// poll ends.
func TestReviewRefreshWaitsForSlowestAccount(t *testing.T) {
	r := newRig(t, Options{}, "fast", "hung")
	rt := discRoute("r", time.Hour, "fast", "hung")
	r.target(rt)
	r.pollDue()
	res := r.m.Resolver([]core.Route{rt})
	r.ups["fast"].set(listingBody("fresh/model"))
	r.ups["hung"].set(func(w http.ResponseWriter, req *http.Request) { <-req.Context().Done() })

	start := time.Now()
	res.Refresh(context.Background(), "fresh/model")
	el := time.Since(start)
	if _, ok, _ := res.Resolve("fresh/model"); !ok {
		t.Fatal("fresh model did not resolve after refresh")
	}
	if el > time.Second {
		t.Fatalf("request blocked %v for a model the healthy account listed immediately (want well under 1s)", el)
	}
}

// The body is capped at 1 MiB, but response headers are not bounded below
// net/http's 10 MiB default, so a hostile /models endpoint can make every poll
// buffer ~10x the body cap and the poll still succeeds.
func TestReviewHugeResponseHeadersAccepted(t *testing.T) {
	r := newRig(t, Options{}, "a")
	r.target(discRoute("r", time.Hour, "a"))
	pad := strings.Repeat("x", 64<<10)
	r.ups["a"].set(func(w http.ResponseWriter, _ *http.Request) {
		for i := 0; i < 64; i++ { // 64 x 64 KiB = 4 MiB of header values
			w.Header().Add("X-Pad", pad)
		}
		listingBody("hdr/model")(w, nil)
	})
	r.pollDue()
	if got, class := r.listing("a"), r.errClass("a"); class == "" && slices.Equal(got, []string{"hdr/model"}) {
		t.Fatalf("listing with 4 MiB of response headers accepted (class %q, ids %v); want a bounded failure", class, got)
	}
}

// ids made of bidi overrides, zero-width or BOM characters pass validation
// (they are unicode.Cf, neither IsSpace nor IsControl) and are published in
// /v1/models and diagnostics, letting an upstream list look-alike ids such as
// "qwen3\u200b:8b" next to "qwen3:8b" or reverse-rendered names.
func TestReviewInvisibleFormatCharsInIDs(t *testing.T) {
	for _, id := range []string{"qwen3\u200b:8b", "gpt\u202eoss", "\ufeffllama", "a\u2060b"} {
		body := []byte(`{"data":[{"id":"` + id + `"}]}`)
		l, err := parseListing(body, 10)
		if err != nil {
			t.Fatal(err)
		}
		if slices.Contains(l.ids, id) {
			t.Errorf("id %+q with an invisible format character was accepted", id)
		}
	}
}
