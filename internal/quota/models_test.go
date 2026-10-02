package quota

import (
	"context"
	"net/http"
	"testing"

	"github.com/hpst3r/localrouter/internal/core"
)

// Real-shape Ollama /api/usage: per-window models[] with request_count.
func TestOllamaModelRequests(t *testing.T) {
	h := newHarness(t, ollamaAcct(), func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"activity":{"cost":"0.00000","models":[]},
 "limits":{"session":{"usage":0.179,"models":[{"name":"glm-5.3-flash","request_count":331}]},
           "weekly":{"usage":0.416,"models":[{"name":"glm-5.3-flash","request_count":1555},
                                             {"name":"deepseek-v4.1-flash","request_count":4549},
                                             {"name":"nemotron-3-nano:30b","request_count":1},
                                             {"name":"","request_count":9}]}}}`))
	})
	h.m.refresh(context.Background(), "ol", true)
	s, ok := h.m.Latest("ol")
	if !ok {
		t.Fatal("no snapshot")
	}
	got5h := s.ModelRequests[core.Window5h]
	if len(got5h) != 1 || got5h[0] != (core.ModelCount{Model: "glm-5.3-flash", Requests: 331}) {
		t.Fatalf("5h = %+v", got5h)
	}
	want := []core.ModelCount{
		{Model: "deepseek-v4.1-flash", Requests: 4549},
		{Model: "glm-5.3-flash", Requests: 1555},
		{Model: "nemotron-3-nano:30b", Requests: 1},
	}
	gotW := s.ModelRequests[core.WindowWeekly]
	if len(gotW) != len(want) {
		t.Fatalf("weekly = %+v", gotW)
	}
	for i := range want {
		if gotW[i] != want[i] {
			t.Fatalf("weekly[%d] = %+v want %+v (sorted desc, empty name dropped)", i, gotW[i], want[i])
		}
	}
	// Latest must return a deep copy.
	gotW[0].Requests = -1
	s2, _ := h.m.Latest("ol")
	if s2.ModelRequests[core.WindowWeekly][0].Requests != 4549 {
		t.Fatal("Latest returned shared ModelRequests storage")
	}
}

func TestOllamaNoModelsLeavesNil(t *testing.T) {
	h := newHarness(t, ollamaAcct(), func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"limits":{"session":{"usage":0.1}}}`))
	})
	h.m.refresh(context.Background(), "ol", true)
	s, _ := h.m.Latest("ol")
	if s.ModelRequests != nil {
		t.Fatalf("want nil, got %+v", s.ModelRequests)
	}
}
