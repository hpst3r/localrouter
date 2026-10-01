package quota

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
)

// ObserveHeaders merges Codex x-codex-{primary,secondary}-* rate-limit
// headers into the account's snapshot. It is a no-op for non-codex accounts
// or when no used-percent header is present.
//
// Merge rules (conservative):
//   - Only observed window kinds are updated (primary → 5h, secondary →
//     weekly); other windows are kept. ResetAt / WindowSeconds are updated
//     only when the corresponding header is present.
//   - Source becomes "headers" and FetchedAt becomes now. Plan and Err are kept.
//   - Allowed: set to false if any observed window is at ≥100%. A previous
//     false is flipped to true only if every observed window is below 100%.
//     Unknown (nil) Allowed stays nil unless a window is exhausted.
func (m *Manager) ObserveHeaders(accountID string, h http.Header) {
	type obs struct {
		kind       string
		used       float64
		resetAfter *float64
		minutes    *float64
		defSeconds int64
	}
	var observed []obs
	for _, p := range []struct {
		prefix, kind string
		defSeconds   int64
	}{
		{"x-codex-primary-", core.Window5h, 18000},
		{"x-codex-secondary-", core.WindowWeekly, 604800},
	} {
		used, ok := headerFloat(h, p.prefix+"used-percent")
		if !ok {
			continue
		}
		o := obs{kind: p.kind, used: clampFrac(used / 100), defSeconds: p.defSeconds}
		if v, ok := headerFloat(h, p.prefix+"reset-after-seconds"); ok && v >= 0 {
			o.resetAfter = &v
		}
		if v, ok := headerFloat(h, p.prefix+"window-minutes"); ok && v > 0 {
			o.minutes = &v
		}
		observed = append(observed, o)
	}
	if len(observed) == 0 {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.state[accountID]
	if st == nil || st.acct.Provider != core.ProviderCodex {
		return
	}
	now := m.opts.Clock.Now()
	var snap core.Snapshot
	if st.snap != nil {
		snap = copySnapshot(*st.snap)
	} else {
		snap = core.Snapshot{AccountID: accountID}
	}
	anyExhausted := false
	for _, o := range observed {
		idx := -1
		for i := range snap.Windows {
			if snap.Windows[i].Kind == o.kind {
				idx = i
				break
			}
		}
		if idx < 0 {
			snap.Windows = append(snap.Windows, core.Window{Kind: o.kind, WindowSeconds: o.defSeconds})
			idx = len(snap.Windows) - 1
		}
		w := &snap.Windows[idx]
		w.UsedFrac = o.used
		if o.resetAfter != nil {
			w.ResetAt = now.Add(time.Duration(*o.resetAfter * float64(time.Second)))
		}
		if o.minutes != nil {
			w.WindowSeconds = int64(*o.minutes * 60)
		}
		if o.used >= 1 {
			anyExhausted = true
		}
	}
	switch {
	case anyExhausted:
		f := false
		snap.Allowed = &f
	case snap.Allowed != nil && !*snap.Allowed:
		t := true
		snap.Allowed = &t
	}
	snap.Source = SourceHeaders
	snap.FetchedAt = now
	st.snap = &snap
}

func headerFloat(h http.Header, key string) (float64, bool) {
	v := strings.TrimSpace(h.Get(key))
	if v == "" {
		return 0, false
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f != f {
		return 0, false
	}
	return f, true
}
