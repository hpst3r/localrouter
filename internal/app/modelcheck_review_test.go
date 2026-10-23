package app

import (
	"os"
	"testing"

	"github.com/hpst3r/localrouter/internal/config"
)

// TestReviewMCR2DisabledModelCheckFormsNotRestartOnly: absent, null and
// `interval: 0` all mean "disabled" (docs/SPEC.md), yet a reload from an
// absent block to `model_check: {interval: 0}` (or `model_check: {}`) is
// refused with restart_only ["model_check"] because restartFields compares the
// raw pointer (nil vs &{0}) instead of the effective interval.
func TestReviewMCR2DisabledModelCheckFormsNotRestartOnly(t *testing.T) {
	if os.Getenv("LR_REVIEW") == "" {
		t.Skip("BUG MC-2: reload absent -> model_check {interval: 0} (both disabled) is refused as restart-only")
	}
	base := &config.Config{}
	cand := &config.Config{ModelCheck: &config.ModelCheckConfig{}}
	if base.ModelCheckInterval() != cand.ModelCheckInterval() {
		t.Fatal("precondition: both forms must be disabled")
	}
	if f := restartFields(base, cand); len(f) != 0 {
		t.Fatalf("restartFields = %v; equivalent disabled forms must not require a restart", f)
	}
}
