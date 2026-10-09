package control

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
)

// A control server built without the reload seam must omit the reload block
// entirely rather than report a fabricated generation. Diagnostics must never
// claim a reload state it cannot observe.
func TestDiagnosticsOmitsReloadWhenUnwired(t *testing.T) {
	f := newFixture(false)
	if f.srv.deps.ReloadStatus != nil {
		t.Fatal("fixture unexpectedly wires ReloadStatus")
	}
	rec := f.do(t, http.MethodGet, "/control/v1/diagnostics", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), `"reload"`) {
		t.Fatalf("unwired reload must be omitted: %s", rec.Body)
	}
}

// With the seam wired, diagnostics carries the sanitized last reload status,
// including the generation, timestamp and changed/restart-only key names.
func TestDiagnosticsIncludesReloadStatus(t *testing.T) {
	f := newFixture(false)
	at := t0.Add(2 * time.Minute)
	f.srv.deps.ReloadStatus = func() core.ReloadStatus {
		return core.ReloadStatus{Generation: 4, OK: true, At: at}
	}
	rec := f.do(t, http.MethodGet, "/control/v1/diagnostics", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	doc := decode(t, rec)
	rl, ok := doc["reload"].(map[string]any)
	if !ok {
		t.Fatalf("missing reload block: %s", rec.Body)
	}
	if rl["generation"] != float64(4) {
		t.Fatalf("generation = %v, want 4", rl["generation"])
	}
	if rl["ok"] != true {
		t.Fatalf("ok = %v, want true", rl["ok"])
	}
	if rl["at"] != at.UTC().Format(time.RFC3339) {
		t.Fatalf("at = %v, want %s", rl["at"], at.UTC().Format(time.RFC3339))
	}
	// Empty reason and restart_only are omitted, never emitted as empty
	// strings/arrays that a reader could mistake for real values.
	if _, ok := rl["reason"]; ok {
		t.Fatalf("empty reason must be omitted: %s", rec.Body)
	}
	if _, ok := rl["restart_only"]; ok {
		t.Fatalf("empty restart_only must be omitted: %s", rec.Body)
	}
}

// A rejected reload surfaces the sanitized reason and the restart-only keys
// that forced rejection. No raw error text, key material or secret path.
func TestDiagnosticsReloadFailureSanitized(t *testing.T) {
	f := newFixture(false)
	f.srv.deps.ReloadStatus = func() core.ReloadStatus {
		return core.ReloadStatus{
			Generation:  3,
			OK:          false,
			At:          t0,
			Reason:      "restart required",
			RestartOnly: []string{"listen", "data_dir"},
		}
	}
	rec := f.do(t, http.MethodGet, "/control/v1/diagnostics", "", "")
	doc := decode(t, rec)
	rl, ok := doc["reload"].(map[string]any)
	if !ok {
		t.Fatalf("missing reload block: %s", rec.Body)
	}
	if rl["ok"] != false || rl["reason"] != "restart required" {
		t.Fatalf("reload = %v", rl)
	}
	// The generation of a failed attempt must not advance or lag: it reports
	// exactly the still-serving generation.
	if rl["generation"] != float64(3) {
		t.Fatalf("generation = %v, want 3", rl["generation"])
	}
	ro, ok := rl["restart_only"].([]any)
	if !ok || len(ro) != 2 || ro[0] != "listen" || ro[1] != "data_dir" {
		t.Fatalf("restart_only = %v", rl["restart_only"])
	}
}

// ReloadStatus must be consulted on every diagnostics request (a live view),
// not sampled once at construction, so a later reload is immediately visible.
func TestDiagnosticsReloadStatusLive(t *testing.T) {
	f := newFixture(false)
	gen := uint64(1)
	f.srv.deps.ReloadStatus = func() core.ReloadStatus {
		return core.ReloadStatus{Generation: gen, OK: true, At: t0}
	}
	first := decode(t, f.do(t, http.MethodGet, "/control/v1/diagnostics", "", ""))
	gen = 7
	second := decode(t, f.do(t, http.MethodGet, "/control/v1/diagnostics", "", ""))
	if first["reload"].(map[string]any)["generation"] != float64(1) {
		t.Fatalf("first generation = %v", first["reload"])
	}
	if second["reload"].(map[string]any)["generation"] != float64(7) {
		t.Fatalf("second generation = %v", second["reload"])
	}
}
