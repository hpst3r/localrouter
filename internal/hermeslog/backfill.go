package hermeslog

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
)

// backfillIDPrefix keeps backfilled rows distinct from the regular
// collector's "hermes:" rows for the same key and counters.
const backfillIDPrefix = "hermes-backfill:"

// BackfillOptions selects self-routed rows for Collector.BackfillSelf.
type BackfillOptions struct {
	// Models restricts the backfill to these Hermes model names (required):
	// other self-routed traffic was metered by the proxy.
	Models []string
	// Before is the exclusive cutoff on last_seen (required).
	Before time.Time
	// AccountID and Provider attribute every backfilled row (AccountID
	// required).
	AccountID string
	Provider  string
	// Apply records the selected rows; otherwise the backfill is a dry run.
	Apply bool
	// Exists, if set, reports whether a ledger row with the given ID is
	// already present; such rows are counted in AlreadyPresent and not sent.
	Exists func(ctx context.Context, id string) (bool, error)
}

// BackfillRow is one self-routed Hermes row considered by a backfill.
type BackfillRow struct {
	Profile, Session, Model, Task string
	// LastSeen is zero when Hermes recorded no last_seen.
	LastSeen time.Time
	Calls    int64
	// Record is the ledger row the backfill records (Selected rows only).
	Record core.RequestRecord
	// Present reports the row is already in the ledger (needs Exists).
	Present bool
}

// BackfillResult summarises a backfill.
type BackfillResult struct {
	// Selected rows were last seen before the cutoff and are (or, in a dry
	// run, would be) recorded for their full cumulative counters.
	Selected []BackfillRow
	// Excluded rows match a model but were active at or after the cutoff
	// (or have no last_seen); they are not recorded.
	Excluded []BackfillRow
	// SkippedInvalid counts matching rows with implausible counters or time.
	SkippedInvalid int
	AlreadyPresent int
	Recorded       int
}

// BackfillSelf records Hermes usage that was routed through this router but
// that the proxy failed to meter. It selects self-routed rows (the ones
// ScanOnce skips) whose model is in o.Models and whose last_seen is strictly
// before o.Before, and records each as one ledger row for its full
// cumulative counters, attributed to o.AccountID/o.Provider and labeled
// like a regular row. IDs are deterministic with their own prefix, so a
// repeated backfill records nothing new. The collector's state is neither
// read nor written.
func (c *Collector) BackfillSelf(ctx context.Context, o BackfillOptions) (BackfillResult, error) {
	var res BackfillResult
	if len(o.Models) == 0 {
		return res, errors.New("hermeslog backfill: no models")
	}
	if o.Before.IsZero() {
		return res, errors.New("hermeslog backfill: no cutoff")
	}
	if o.AccountID == "" {
		return res, errors.New("hermeslog backfill: no account")
	}
	models := map[string]bool{}
	for _, m := range o.Models {
		models[m] = true
	}
	for _, db := range c.databases() {
		profile := db[0]
		err := readUsage(ctx, db[1], func(u usageRow) {
			if !c.isSelf(u.baseURL) || !models[u.model] || u.cur.zero() {
				return
			}
			row := BackfillRow{Profile: profile, Session: u.sess, Model: u.model, Task: u.task, Calls: u.cur.Calls}
			if u.lastSeen > 0 && u.lastSeen <= maxUnixSeconds {
				sec, frac := math.Modf(u.lastSeen)
				row.LastSeen = time.Unix(int64(sec), int64(frac*1e9)).UTC()
			}
			if row.LastSeen.IsZero() || !row.LastSeen.Before(o.Before) {
				res.Excluded = append(res.Excluded, row)
				return
			}
			rec := c.record(u.key(profile), profile, u.sess, u.model, u.prov, u.task, u.parent, u.repo, u.cwd, u.lastSeen, u.cur, u.cur)
			if why := c.implausible(u.cur, rec, u.lastSeen); why != "" {
				res.SkippedInvalid++
				c.opts.Logger.Debug("hermeslog backfill: skipping implausible row", "profile", profile, "reason", why)
				return
			}
			rec.ID = backfillIDPrefix + strings.TrimPrefix(rec.ID, "hermes:")
			rec.AccountID = o.AccountID
			rec.Provider = o.Provider
			row.Record = rec
			res.Selected = append(res.Selected, row)
		})
		if err != nil {
			return res, fmt.Errorf("profile %s: %w", profile, err)
		}
	}
	sortRows(res.Selected)
	sortRows(res.Excluded)
	for i := range res.Selected {
		p := &res.Selected[i]
		if o.Exists != nil {
			ok, err := o.Exists(ctx, p.Record.ID)
			if err != nil {
				return res, err
			}
			if ok {
				p.Present = true
				res.AlreadyPresent++
				continue
			}
		}
		if !o.Apply {
			continue
		}
		// The ledger ignores a duplicate ID, so a retry after a partial
		// failure is safe.
		if err := c.ledger.Record(ctx, p.Record); err != nil {
			return res, fmt.Errorf("record %s: %w", p.Record.ID, err)
		}
		res.Recorded++
	}
	return res, nil
}

func sortRows(rs []BackfillRow) {
	sort.SliceStable(rs, func(i, j int) bool {
		a, b := rs[i], rs[j]
		if a.Profile != b.Profile {
			return a.Profile < b.Profile
		}
		if !a.LastSeen.Equal(b.LastSeen) {
			return a.LastSeen.Before(b.LastSeen)
		}
		return a.Session < b.Session
	})
}
