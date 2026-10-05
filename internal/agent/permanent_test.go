package agent

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hpst3r/localrouter/internal/core"
)

func TestHTTPErrorPermanent(t *testing.T) {
	for status, want := range map[int]bool{
		400: true, 413: true, 422: true,
		401: false, 403: false, 429: false, 500: false, 502: false, 503: false,
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, `{"error":"x"}`, status)
		}))
		l := NewRemoteLedger(NewClient(srv.URL, "vm1", testKey, srv.Client()))
		err := l.Record(context.Background(), core.RequestRecord{ID: "a"})
		srv.Close()
		var pe interface{ Permanent() bool }
		if !errors.As(err, &pe) || pe.Permanent() != want {
			t.Errorf("status %d: err %v, want permanent=%v", status, err, want)
		}
	}
}
