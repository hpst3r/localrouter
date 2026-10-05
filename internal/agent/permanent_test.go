package agent

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hpst3r/localrouter/internal/core"
)

// Only record-scoped rejections (and 413) may be isolated and dropped; a
// request-wide 400 (bad host, account missing from server config) must be
// retried, or bisecting would drop every valid record.
func TestHTTPErrorPermanent(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   bool
	}{
		{400, `{"error":{"message":"records[0]: bad","code":"invalid_record"}}`, true},
		{400, `{"error":{"message":"account_id not configured","code":"invalid_request"}}`, false},
		{400, `{"error":{"message":"no code"}}`, false},
		{400, `not json`, false},
		{422, `{"error":{"code":"invalid_record"}}`, true},
		{413, `{"error":{"message":"too big"}}`, true},
		{401, ``, false}, {403, ``, false}, {429, ``, false},
		{500, ``, false}, {502, ``, false}, {503, ``, false},
	}
	for _, tc := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(tc.body))
		}))
		l := NewRemoteLedger(NewClient(srv.URL, "vm1", testKey, srv.Client()))
		err := l.Record(context.Background(), core.RequestRecord{ID: "a"})
		srv.Close()
		var pe interface{ Permanent() bool }
		if !errors.As(err, &pe) || pe.Permanent() != tc.want {
			t.Errorf("status %d body %q: err %v, want permanent=%v", tc.status, tc.body, err, tc.want)
		}
	}
}
