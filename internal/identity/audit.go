package identity

import (
	"context"
	"fmt"
)

// AuditEvents returns up to limit audit events with Seq > afterSeq in Seq
// order (limit clamped to [1, MaxListLimit]); pass the last Seq of a page to
// get the next. Events carry opaque ids and fixed codes only.
func (s *Store) AuditEvents(ctx context.Context, afterSeq int64, limit int) ([]AuditEvent, error) {
	limit = clampLimit(limit)
	var out []AuditEvent
	err := s.read(ctx, func(ctx context.Context) error {
		rows, err := s.db.QueryContext(ctx,
			`SELECT seq, at, action, actor_kind, actor_user_id, target_user_id, target_key_id, outcome, reason
			 FROM audit_events WHERE seq > ? ORDER BY seq LIMIT ?`, afterSeq, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var (
				e     AuditEvent
				at    int64
				actor string
			)
			if err := rows.Scan(&e.Seq, &at, &e.Action, &actor, &e.ActorUserID, &e.TargetUserID, &e.TargetKeyID, &e.Outcome, &e.Reason); err != nil {
				return err
			}
			e.At, e.ActorKind = fromMS(at), ActorKind(actor)
			out = append(out, e)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("identity: audit events: %w", err)
	}
	return out, nil
}
