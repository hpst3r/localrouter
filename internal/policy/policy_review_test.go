package policy

// Adversarial review tests; skipped until fixed. Run with LOCALROUTER_REVIEW=1
// to see them fail.

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/hpst3r/localrouter/internal/core"
)

func reviewBug(t *testing.T, msg string) {
	t.Helper()
	if os.Getenv("LOCALROUTER_REVIEW") == "" {
		t.Skip("BUG: " + msg)
	}
}

// With a 0.999 reserve, 0% used and a 0.01 in-flight estimate the deny
// reason reads "background reserve 5h (used 0.00 > -0.01)": a negative
// "limit" that mixes reserve, safety margin, and in-flight load, shown as
// raw fractions. The Hermes plugin passes this string to the model verbatim.
// Proposed wording (percentages, no negative limit, the reserve named):
//
//	"5h: 0% used; background capped at 0% (reserve 99.9%, margin+in-flight 1%)"
func TestReviewDenyReasonReadable(t *testing.T) {
	reviewBug(t, "deny reason prints a negative limit as raw fractions ('used 0.00 > -0.01')")
	a := core.Account{ID: "cl", Provider: core.ProviderCodex, Reserve: map[string]float64{core.Window5h: 0.999}}
	h := newHarness([]core.Account{a}, Options{InflightEstimate: 0.01})
	h.q.Set(snap("cl", t0, w5h(0)))
	st := h.p.Status("cl")
	if st.BackgroundAdmissible {
		t.Fatal("background admitted under a 99.9% reserve")
	}
	t.Logf("reason: %q", st.Reason)
	if regexp.MustCompile(`-\d`).MatchString(st.Reason) {
		t.Errorf("reason shows a negative limit: %q", st.Reason)
	}
	if !strings.Contains(st.Reason, "99.9%") || !strings.Contains(st.Reason, "0% used") {
		t.Errorf("reason should state the reserve and usage as percentages: %q", st.Reason)
	}
}
