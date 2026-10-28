package app

import (
	"slices"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/config"
)

// MC-2 (Amendment R1.2): absent, null/{} and `interval: 0` are the same
// disabled model_check value, so reloading between them needs no restart;
// switching to or from an enabled interval (or between intervals) stays
// restart-only.
func TestMCFixMC2DisabledModelCheckFormsNotRestartOnly(t *testing.T) {
	interval := func(d time.Duration) *config.Config {
		return &config.Config{ModelCheck: &config.ModelCheckConfig{Interval: config.Duration(d)}}
	}
	disabled := []*config.Config{{}, {ModelCheck: &config.ModelCheckConfig{}}, interval(0)}
	for i, base := range disabled {
		for j, cand := range disabled {
			if f := restartFields(base, cand); len(f) != 0 {
				t.Fatalf("disabled form %d -> %d: restartFields = %v, want none", i, j, f)
			}
		}
		for _, pair := range [][2]*config.Config{{base, interval(time.Minute)}, {interval(time.Minute), base}} {
			if f := restartFields(pair[0], pair[1]); !slices.Equal(f, []string{"model_check"}) {
				t.Fatalf("disabled form %d <-> enabled: restartFields = %v, want [model_check]", i, f)
			}
		}
	}
	if f := restartFields(interval(time.Minute), interval(2*time.Minute)); !slices.Equal(f, []string{"model_check"}) {
		t.Fatalf("interval change: restartFields = %v, want [model_check]", f)
	}
}
