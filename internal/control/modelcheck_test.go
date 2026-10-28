package control

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakeModelCheck struct{}

func (fakeModelCheck) Diagnostics() any {
	return map[string]any{"interval": "5m0s", "verdicts": []any{}}
}

// MC1: the model_check block appears only when ModelCheck is wired.
func TestMC1DiagnosticsModelCheckBlockOnlyWhenWired(t *testing.T) {
	f := newFixture(false)
	get := func() string {
		rec := httptest.NewRecorder()
		f.h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/control/v1/diagnostics", nil))
		return rec.Body.String()
	}
	if s := get(); strings.Contains(s, "model_check") {
		t.Fatalf("unwired diagnostics has a model_check block: %s", s)
	}
	f.srv.deps.ModelCheck = fakeModelCheck{}
	if s := get(); !strings.Contains(s, `"model_check":{"interval":"5m0s","verdicts":[]}`) {
		t.Fatalf("wired diagnostics lacks the model_check block: %s", s)
	}
}
