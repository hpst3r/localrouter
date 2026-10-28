package hermeslog

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
)

const (
	// BackfillIDPrefix starts every backfilled row's ID, keeping it distinct
	// from the regular collector's "hermes:" rows.
	BackfillIDPrefix = "hermes-backfill:"
	// BackfillKeyIDPrefix starts the IDs of the current backfill, which are
	// derived from the row key alone (so each key is backfilled at most once).
	// A BackfillIDPrefix ID without it was written by the first backfill,
	// which hashed the cumulative counters too.
	BackfillKeyIDPrefix = BackfillIDPrefix + "k1:"
)

// Reasons a BackfillRow is excluded.
const (
	ExcludedNoLastSeen  = "no last_seen"
	ExcludedAfterBefore = "active at/after --before"
	ExcludedBeforeAfter = "active before --after"
)

// BackfillOptions selects self-routed rows for Collector.BackfillSelf.
type BackfillOptions struct {
	// Models restricts the backfill to these Hermes model names (required):
	// other self-routed traffic was metered by the proxy.
	Models []string
	// After and Before bound the window [After, Before) the row's whole
	// activity must lie in (both required, After < Before).
	After, Before time.Time
	// AccountID and Provider attribute every backfilled row (AccountID
	// required).
	AccountID string
	Provider  string
	// Apply records the selected rows; otherwise the backfill is a dry run.
	Apply bool
	// AcceptPartialSessions also records, on Apply, the Review rows (no
	// first_seen, so calls before After may be included).
	AcceptPartialSessions bool
	// Exists, if set, reports whether rec's key was already backfilled: a
	// ledger row with rec.ID, or an earlier BackfillIDPrefix row (not
	// BackfillKeyIDPrefix) with the same client, session, model, agent and
	// task. Such rows are counted in AlreadyPresent and never sent.
	Exists func(ctx context.Context, rec core.RequestRecord) (bool, error)
}

// BackfillRow is one self-routed Hermes row considered by a backfill.
type BackfillRow struct {
	Profile, Session, Model, Task string
	// FirstSeen and LastSeen are zero when Hermes recorded none.
	FirstSeen, LastSeen time.Time
	Calls               int64
	// Reason says why an Excluded row is excluded.
	Reason string
	// Record is the ledger row the backfill records (Selected and Review).
	Record core.RequestRecord
	// Present reports the key is already backfilled (needs Exists).
	Present bool
}

// BackfillResult summarises a backfill.
type BackfillResult struct {
	// Selected rows were active only inside [After, Before) and are (or, in
	// a dry run, would be) recorded for their full cumulative counters.
	Selected []BackfillRow
	// Review rows have no first_seen and a last_seen inside [After, Before):
	// their counters may include calls before After. They are recorded only
	// with AcceptPartialSessions.
	Review []BackfillRow
	// Excluded rows match a model but fall outside the window; they are
	// not recorded.
	Excluded []BackfillRow
	// SkippedInvalid counts matching rows with implausible counters or time.
	SkippedInvalid int
	AlreadyPresent int
	Recorded       int
}

// BackfillSelf records Hermes usage that was routed through this router but
// that the proxy failed to meter. It considers self-routed rows (the ones
// ScanOnce skips) whose model is in o.Models. A row is Selected when its
// first_seen is at or after o.After and its last_seen is before o.Before;
// a row without first_seen goes to Review when its last_seen is in
// [o.After, o.Before). Each is recorded as one ledger row for its full
// cumulative counters, attributed to o.AccountID/o.Provider and labeled like
// a regular row. The ID depends on the row key only, so a key is backfilled
// at most once even if its counters later grow. The collector's state is
// neither read nor written.
func (c *Collector) BackfillSelf(ctx context.Context, o BackfillOptions) (BackfillResult, error) {
	var res BackfillResult
	if len(o.Models) == 0 {
		return res, errors.New("hermeslog backfill: no models")
	}
	if o.Before.IsZero() || o.After.IsZero() {
		return res, errors.New("hermeslog backfill: no window")
	}
	if !o.After.Before(o.Before) {
		return res, errors.New("hermeslog backfill: after must be before before")
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
			row := BackfillRow{Profile: profile, Session: u.sess, Model: u.model, Task: u.task, Calls: u.cur.Calls,
				FirstSeen: unixTime(u.firstSeen), LastSeen: unixTime(u.lastSeen)}
			if row.FirstSeen.After(row.LastSeen) {
				row.FirstSeen = time.Time{} // inconsistent: treat as unknown
			}
			review := false
			switch {
			case row.LastSeen.IsZero():
				row.Reason = ExcludedNoLastSeen
			case !row.LastSeen.Before(o.Before):
				row.Reason = ExcludedAfterBefore
			case row.FirstSeen.IsZero() && row.LastSeen.Before(o.After),
				!row.FirstSeen.IsZero() && row.FirstSeen.Before(o.After):
				row.Reason = ExcludedBeforeAfter
			case row.FirstSeen.IsZero():
				review = true
			}
			if row.Reason != "" {
				res.Excluded = append(res.Excluded, row)
				return
			}
			key := u.key(profile)
			rec := c.record(key, profile, u.sess, u.model, u.prov, u.task, u.parent, u.repo, u.cwd, u.lastSeen, u.cur, u.cur)
			if why := c.implausible(u.cur, rec, u.lastSeen); why != "" {
				res.SkippedInvalid++
				c.opts.Logger.Debug("hermeslog backfill: skipping implausible row", "profile", profile, "reason", why)
				return
			}
			rec.ID = backfillID(key)
			rec.AccountID = o.AccountID
			rec.Provider = o.Provider
			row.Record = rec
			if review {
				res.Review = append(res.Review, row)
			} else {
				res.Selected = append(res.Selected, row)
			}
		})
		if err != nil {
			return res, fmt.Errorf("profile %s: %w", profile, err)
		}
	}
	sortRows(res.Selected)
	sortRows(res.Review)
	sortRows(res.Excluded)
	if err := c.backfill(ctx, o, res.Selected, o.Apply, &res); err != nil {
		return res, err
	}
	if err := c.backfill(ctx, o, res.Review, o.Apply && o.AcceptPartialSessions, &res); err != nil {
		return res, err
	}
	return res, nil
}

// backfill marks rows already present and, if apply, records the rest.
func (c *Collector) backfill(ctx context.Context, o BackfillOptions, rows []BackfillRow, apply bool, res *BackfillResult) error {
	for i := range rows {
		p := &rows[i]
		if o.Exists != nil {
			ok, err := o.Exists(ctx, p.Record)
			if err != nil {
				return err
			}
			if ok {
				p.Present = true
				res.AlreadyPresent++
				continue
			}
		}
		if !apply {
			continue
		}
		// The ledger ignores a duplicate ID, so a retry after a partial
		// failure is safe.
		if err := c.ledger.Record(ctx, p.Record); err != nil {
			return fmt.Errorf("record %s: %w", p.Record.ID, err)
		}
		res.Recorded++
	}
	return nil
}

// backfillID derives a backfill row's ID from its key alone, never from its
// counters or last_seen.
func backfillID(key string) string {
	sum := sha256.Sum256([]byte("backfill\x1f" + key))
	return BackfillKeyIDPrefix + hex.EncodeToString(sum[:])[:32]
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
