package agent

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
)

// MaxBackoff caps the retry delay after errors.
const MaxBackoff = 5 * time.Minute

// ErrTokenExpired is returned by a QuotaFetcher when the Claude token has
// expired; the agent then pushes nothing (the server's snapshot goes stale)
// and retries at the normal interval.
var ErrTokenExpired = errors.New("claude token expired; run claude to refresh")

// QuotaFetcher turns raw credentials JSON into a quota snapshot for the
// account (e.g. via quota.ParseClaudeCredentials + quota.FetchClaudeSnapshot).
type QuotaFetcher func(ctx context.Context, rawCredentials []byte, now time.Time) (core.Snapshot, error)

// Options configures an Agent.
type Options struct {
	Client    *Client
	AccountID string
	// Scan runs one transcript scan that records through a RemoteLedger
	// (e.g. claudelog.Collector.ScanOnce). nil disables usage pushes.
	Scan func(ctx context.Context) error
	// Credentials and FetchQuota produce snapshots; either nil disables
	// quota pushes, as does QuotaInterval == 0.
	Credentials   CredentialReader
	FetchQuota    QuotaFetcher
	PushInterval  time.Duration // default DefaultPushInterval
	QuotaInterval time.Duration
	Clock         core.Clock
	Logger        *slog.Logger
}

// Agent runs the usage and quota push loops.
type Agent struct {
	opts Options
}

// New returns an Agent.
func New(opts Options) *Agent {
	if opts.PushInterval <= 0 {
		opts.PushInterval = DefaultPushInterval
	}
	if opts.Clock == nil {
		opts.Clock = core.SystemClock{}
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Agent{opts: opts}
}

func (a *Agent) quotaEnabled() bool {
	return a.opts.Credentials != nil && a.opts.FetchQuota != nil && a.opts.QuotaInterval > 0
}

// RunOnce performs one scan and (if enabled) one quota push, returning their
// errors joined. An expired token is not an error.
func (a *Agent) RunOnce(ctx context.Context) error {
	var errs []error
	if a.opts.Scan != nil {
		errs = append(errs, a.scan(ctx))
	}
	if a.quotaEnabled() {
		errs = append(errs, a.pushQuota(ctx))
	}
	return errors.Join(errs...)
}

// Run runs both loops until ctx is cancelled, retrying failures with
// exponential backoff (never faster than the loop's interval, capped at
// MaxBackoff or the interval if larger). It returns nil on cancellation.
func (a *Agent) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	if a.opts.Scan != nil {
		wg.Go(func() { loop(ctx, a.opts.PushInterval, a.scan) })
	}
	if a.quotaEnabled() {
		wg.Go(func() { loop(ctx, a.opts.QuotaInterval, a.pushQuota) })
	}
	wg.Wait()
	return nil
}

func (a *Agent) scan(ctx context.Context) error {
	if err := a.opts.Scan(ctx); err != nil {
		a.opts.Logger.Warn("agent: usage push failed", "err", err)
		return err
	}
	return nil
}

func (a *Agent) pushQuota(ctx context.Context) error {
	raw, err := a.opts.Credentials.ReadCredentials(ctx)
	if err != nil {
		a.opts.Logger.Warn("agent: read credentials", "err", err)
		return err
	}
	snap, err := a.opts.FetchQuota(ctx, raw, a.opts.Clock.Now())
	clear(raw)
	if errors.Is(err, ErrTokenExpired) {
		a.opts.Logger.Info("agent: claude token expired; skipping quota push")
		return nil
	}
	if err != nil {
		a.opts.Logger.Warn("agent: quota fetch failed", "err", err)
		return err
	}
	snap.AccountID = a.opts.AccountID
	resp, err := a.opts.Client.PushSnapshot(ctx, snap)
	if err != nil {
		a.opts.Logger.Warn("agent: quota push failed", "err", err)
		return err
	}
	if resp.SnapshotsIgnored > 0 {
		a.opts.Logger.Debug("agent: server kept a newer snapshot")
	}
	return nil
}

// loop calls step immediately and then after every interval, backing off
// after consecutive failures, until ctx is done.
func loop(ctx context.Context, interval time.Duration, step func(context.Context) error) {
	fails := 0
	for {
		if ctx.Err() != nil {
			return
		}
		if err := step(ctx); err != nil && ctx.Err() == nil {
			fails++
		} else {
			fails = 0
		}
		t := time.NewTimer(backoff(interval, fails))
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

// backoff is the delay before the next attempt after fails consecutive
// failures: interval when healthy, then doubling, capped at
// max(MaxBackoff, interval).
func backoff(interval time.Duration, fails int) time.Duration {
	limit := max(MaxBackoff, interval)
	d := interval
	for range fails {
		d *= 2
		if d >= limit {
			return limit
		}
	}
	return d
}
