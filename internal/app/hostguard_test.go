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

func TestHostGuardAllowed(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) })
	h := HostGuard([]string{"Router.Tail", "100.64.0.10", "[fd00::1]", "mesh.example:8787"}, ok)
	for host, want := range map[string]int{
		"router.tail:8787": 204, "ROUTER.TAIL": 204, "100.64.0.10:8787": 204, "[fd00:0::1]:8787": 204,
		"mesh.example": 204, "127.0.0.1": 204, "localhost:1": 204,
		"evil.example": 403, "100.64.0.11": 403, "router.tail.evil": 403, "": 403,
	} {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Host = host
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Errorf("host %q: got %d want %d", host, rec.Code, want)
		}
	}
}
