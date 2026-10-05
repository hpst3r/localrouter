package quota

import (
	"errors"
	"fmt"

	"github.com/hpst3r/localrouter/internal/core"
)

// QuotaSourceAgent is the Account.QuotaSource value for claude accounts whose
// snapshots are pushed by per-host agents instead of polled.
const QuotaSourceAgent = "agent"

// ErrSnapshotStale is returned by IngestSnapshot when the pushed snapshot's
// FetchedAt is not newer than the stored one; the snapshot is ignored.
var ErrSnapshotStale = errors.New("quota: snapshot not newer than stored one")

var _ core.SnapshotIngester = (*Manager)(nil)

// agentSourced reports whether the account's snapshots come only from
// IngestSnapshot (never polled: no credential read, no HTTP).
func agentSourced(a core.Account) bool {
	return a.Provider == core.ProviderClaude && a.QuotaSource == QuotaSourceAgent
}

// IngestSnapshot stores a snapshot pushed by an agent for a claude account
// with quota_source "agent". It returns an error for unknown accounts or
// accounts that are not claude+agent, and ErrSnapshotStale (nothing stored)
// if s.FetchedAt is not after the stored snapshot's FetchedAt. The stored
// value is a deep copy of s, including any Err the agent set.
func (m *Manager) IngestSnapshot(s core.Snapshot) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.state[s.AccountID]
	if st == nil {
		return fmt.Errorf("quota: unknown account %q", s.AccountID)
	}
	if !agentSourced(st.acct) {
		return fmt.Errorf("quota: account %q is not an agent-sourced claude account", s.AccountID)
	}
	if st.snap != nil && !s.FetchedAt.After(st.snap.FetchedAt) {
		return ErrSnapshotStale
	}
	c := copySnapshot(s)
	st.snap = &c
	return nil
}
