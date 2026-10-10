package identity

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
)

// AccessPolicy is the login access policy the server enforces, as recorded
// in the store so a configuration change that narrows it is noticed at the
// next open (see ApplyAccessPolicy). It carries no secrets.
type AccessPolicy struct {
	Claim         string   `json:"claim"`
	UserValues    []string `json:"user_values"`
	AdminValues   []string `json:"admin_values"`
	AdminSubjects []string `json:"admin_subjects"`
}

// Access policy meta keys.
const (
	metaAccessPolicy = "access_policy"
	// metaPolicyEpoch is the unix ms of the last narrowing: a user's
	// credentials authenticate only after an allowed login at or after it.
	metaPolicyEpoch = "policy_epoch"
)

// policyEpochSQL reads the policy epoch (0 if none) inside an
// authentication query, so every *Store on the file, in any process, sees
// a narrowing on its next read.
const policyEpochSQL = `COALESCE((SELECT CAST(value AS INTEGER) FROM meta WHERE key = '` + metaPolicyEpoch + `'), 0)`

// canonical returns p with every list sorted, deduplicated and non-nil.
func (p AccessPolicy) canonical() AccessPolicy {
	norm := func(v []string) []string {
		out := slices.Clone(v)
		slices.Sort(out)
		return append([]string{}, slices.Compact(out)...)
	}
	return AccessPolicy{
		Claim:         p.Claim,
		UserValues:    norm(p.UserValues),
		AdminValues:   norm(p.AdminValues),
		AdminSubjects: norm(p.AdminSubjects),
	}
}

// subset reports whether every element of a is in one of bs.
func subset(a []string, bs ...[]string) bool {
	for _, v := range a {
		if !slices.ContainsFunc(bs, func(b []string) bool { return slices.Contains(b, v) }) {
			return false
		}
	}
	return true
}

// narrows reports whether moving from old to p could deny a login, or the
// admin role, that old allowed.
func (p AccessPolicy) narrows(old AccessPolicy) bool {
	return p.Claim != old.Claim ||
		!subset(old.AdminValues, p.AdminValues) ||
		!subset(old.AdminSubjects, p.AdminSubjects) ||
		!subset(old.UserValues, p.UserValues, p.AdminValues)
}

// ApplyAccessPolicy records p as the enforced access policy, in one write
// transaction, and reports whether it narrowed the recorded one. Call it at
// every open for serving, before anything authenticates.
//
// A policy that only widens the recorded one (or equals it), or is the
// first recorded in a store without users, is recorded and nothing else
// changes. Otherwise — the claim changed, an admin value or subject was
// removed, a user value is no longer allowed, or users exist but no policy
// was recorded — every session is deleted, every admin is demoted to user
// until a fresh login re-grants the role, and the policy epoch is set to now:
// no key, owned static key or session of a user authenticates, and none can
// be created, until that user's next allowed login. Keys are not revoked;
// they resume after the owner's next allowed login. The narrowing is
// audited.
func (s *Store) ApplyAccessPolicy(ctx context.Context, p AccessPolicy) (bool, error) {
	if !validText(p.Claim, maxBindingBytes) {
		return false, fmt.Errorf("%w: access policy claim must be non-empty printable UTF-8", ErrInvalid)
	}
	p = p.canonical()
	enc, err := json.Marshal(p)
	if err != nil {
		return false, fmt.Errorf("identity: access policy: %w", err)
	}
	now := s.now()
	var narrowed bool
	err = s.write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var stored string
		err := tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, metaAccessPolicy).Scan(&stored)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			var users int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&users); err != nil {
				return fmt.Errorf("identity: access policy: %w", err)
			}
			narrowed = users > 0
		case err != nil:
			return fmt.Errorf("identity: access policy: %w", err)
		default:
			var old AccessPolicy
			// An unreadable record is treated as narrowed (fail closed).
			narrowed = json.Unmarshal([]byte(stored), &old) != nil || p.narrows(old.canonical())
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO meta (key, value) VALUES (?, ?) ON CONFLICT (key) DO UPDATE SET value = excluded.value`,
			metaAccessPolicy, string(enc)); err != nil {
			return fmt.Errorf("identity: access policy: %w", err)
		}
		if !narrowed {
			return nil
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM sessions`); err != nil {
			return fmt.Errorf("identity: access policy: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE users SET role = 'user', updated_at = ? WHERE role = 'admin'`, toMS(now)); err != nil {
			return fmt.Errorf("identity: access policy: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO meta (key, value) VALUES (?, ?) ON CONFLICT (key) DO UPDATE SET value = excluded.value`,
			metaPolicyEpoch, strconv.FormatInt(toMS(now), 10)); err != nil {
			return fmt.Errorf("identity: access policy: %w", err)
		}
		return audit(ctx, tx, now, AuditAccessPolicyNarrowed, Actor{Kind: ActorSystem}, "", "", OutcomeOK, "")
	})
	if err != nil {
		return false, err
	}
	return narrowed, nil
}

// policyEpoch reads the policy epoch (unix ms, 0 if none) inside tx.
func policyEpoch(ctx context.Context, tx *sql.Tx) (int64, error) {
	var epoch int64
	if err := tx.QueryRowContext(ctx, `SELECT `+policyEpochSQL).Scan(&epoch); err != nil {
		return 0, fmt.Errorf("identity: policy epoch: %w", err)
	}
	return epoch, nil
}
