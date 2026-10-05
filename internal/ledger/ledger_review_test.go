package ledger

// Adversarial review tests; skipped until the bug is fixed. Run with
// LOCALROUTER_REVIEW=1 to see them fail.

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
)

func reviewBug(t *testing.T, msg string) {
	t.Helper()
	if os.Getenv("LOCALROUTER_REVIEW") == "" {
		t.Skip("BUG: " + msg)
	}
}

// Ingest only checks usage >= 0, so a single host can submit rows whose
// token counts sum past int64. SQLite's SUM() then fails with "integer
// overflow" and every Summary over that range errors — for all groups and
// all accounts, not just the poisoned one.
func TestReviewSummaryOverflowFromHugeUsage(t *testing.T) {
	l, err := Open(filepath.Join(t.TempDir(), "l.db"), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	now := time.Now()
	var rs []core.RequestRecord
	for _, id := range []string{"a", "b"} {
		rs = append(rs, core.RequestRecord{ID: id, StartedAt: now, AccountID: "claude-max",
			Provider: "claude", Host: "vm1", UsageKnown: true,
			Usage: core.Usage{InputTokens: math.MaxInt64}})
	}
	if err := l.RecordBatch(context.Background(), rs); err != nil {
		t.Fatal(err)
	}
	for _, g := range []string{"account", "host", "day"} {
		if _, err := l.Summary(context.Background(), now.Add(-time.Hour), g); err != nil {
			t.Errorf("group %s: %v", g, err)
		}
	}
}
