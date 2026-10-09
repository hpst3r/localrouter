package app_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestReadinessWiredToStorageAndShutdown(t *testing.T) {
	e := startWiring(t, wiringOpts{})
	probe := func() int {
		r := httptest.NewRecorder()
		e.app.Handler.ServeHTTP(r, httptest.NewRequest("GET", "http://localhost/readyz", nil))
		return r.Code
	}
	if got := probe(); got != http.StatusOK {
		t.Fatalf("initial readiness %d", got)
	}
	e.app.BeginShutdown()
	if got := probe(); got != http.StatusServiceUnavailable {
		t.Fatalf("shutdown readiness %d want 503", got)
	}
}

func TestReadinessFailsAfterStorageCloses(t *testing.T) {
	e := startWiring(t, wiringOpts{})
	if err := e.app.Ledger.Close(); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRecorder()
	e.app.Handler.ServeHTTP(r, httptest.NewRequest("GET", "http://localhost/readyz", nil))
	if r.Code != http.StatusServiceUnavailable {
		t.Fatalf("closed storage readiness %d want 503", r.Code)
	}
}
