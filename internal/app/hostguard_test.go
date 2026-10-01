package app

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestLocalHostGuard(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) })
	h := LocalHostGuard(ok)
	for host, want := range map[string]int{
		"127.0.0.1:8787": 204, "localhost:8787": 204, "LOCALHOST": 204, "[::1]:8787": 204,
		"127.5.5.5": 204, "evil.example:8787": 403, "192.168.1.5:8787": 403, "": 403,
	} {
		req := httptest.NewRequest(http.MethodGet, "/control/v1/status", nil)
		req.Host = host
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Errorf("host %q: got %d want %d", host, rec.Code, want)
		}
	}
}
