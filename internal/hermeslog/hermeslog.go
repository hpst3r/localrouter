// Package hermeslog imports Hermes Agent's own per-session, per-model token
// accounting into the ledger, so traffic Hermes sends directly to providers
// (not through LocalRouter) is counted.
//
// Source: <home>/state.db and <home>/profiles/*/state.db, table
// session_model_usage (cumulative counters per session × model × billing
// route × auxiliary task). The databases are opened read-only and never
// written. Each scan compares the cumulative counters with the last values
// seen (persisted in StatePath) and records the increase as one ledger row.
// Row IDs are derived from the key plus the new cumulative totals, so
// re-recording after a crash (before state was saved) is a no-op.
//
// Rows whose billing_base_url points at this router are skipped: that
// traffic is already in the ledger from the proxy.
package hermeslog

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
	_ "modernc.org/sqlite"
)

// Options configures a Collector.
type Options struct {
	Home string // Hermes home (~/.hermes)
	// Accounts maps Hermes billing_provider -> LocalRouter account id.
	Accounts map[string]string
	Client   string
	Host     string
	// SelfHosts are host[:port] values that identify this router in a
	// billing_base_url (e.g. "127.0.0.1:8787", "localhost:8787").
	SelfHosts    []string
	StatePath    string
	ScanInterval time.Duration
	Logger       *slog.Logger
}

// Stats summarises one scan.
type Stats struct {
	Databases   int
	Rows        int
	Recorded    int
	SkippedSelf int
}

type counters struct {
	Calls      int64 `json:"c"`
	Input      int64 `json:"i"`
	Output     int64 `json:"o"`
	CacheRead  int64 `json:"r"`
	CacheWrite int64 `json:"w"`
	Reasoning  int64 `json:"x"`
}

func (a counters) less(b counters) bool {
	return b.Calls < a.Calls || b.Input < a.Input || b.Output < a.Output ||
		b.CacheRead < a.CacheRead || b.CacheWrite < a.CacheWrite || b.Reasoning < a.Reasoning
}

func (a counters) sub(b counters) counters {
	return counters{a.Calls - b.Calls, a.Input - b.Input, a.Output - b.Output,
		a.CacheRead - b.CacheRead, a.CacheWrite - b.CacheWrite, a.Reasoning - b.Reasoning}
}

func (a counters) zero() bool { return a == counters{} }

// Collector polls Hermes state databases.
type Collector struct {
	ledger core.Ledger
	opts   Options
	seen   map[string]counters // key -> last cumulative counters
}

// New returns a Collector; state is loaded from StatePath if present.
func New(ledger core.Ledger, opts Options) *Collector {
	if opts.ScanInterval <= 0 {
		opts.ScanInterval = time.Minute
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Client == "" {
		opts.Client = "hermes"
	}
	c := &Collector{ledger: ledger, opts: opts, seen: map[string]counters{}}
	c.loadState()
	return c
}

// Start scans immediately and then every ScanInterval until ctx ends.
func (c *Collector) Start(ctx context.Context) {
	go func() {
		t := time.NewTicker(c.opts.ScanInterval)
		defer t.Stop()
		for {
			if st, err := c.ScanOnce(ctx); err != nil {
				c.opts.Logger.Warn("hermeslog: scan failed", "err", err)
			} else if st.Recorded > 0 {
				c.opts.Logger.Info("hermeslog: imported", "records", st.Recorded, "databases", st.Databases)
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
}

// databases returns (profile, path) pairs that exist.
func (c *Collector) databases() [][2]string {
	var out [][2]string
	if fi, err := os.Stat(filepath.Join(c.opts.Home, "state.db")); err == nil && fi.Mode().IsRegular() {
		out = append(out, [2]string{"default", filepath.Join(c.opts.Home, "state.db")})
	}
	matches, _ := filepath.Glob(filepath.Join(c.opts.Home, "profiles", "*", "state.db"))
	for _, m := range matches {
		out = append(out, [2]string{filepath.Base(filepath.Dir(m)), m})
	}
	return out
}

// ScanOnce imports new usage from every Hermes database.
func (c *Collector) ScanOnce(ctx context.Context) (Stats, error) {
	var st Stats
	var errs []error
	for _, db := range c.databases() {
		st.Databases++
		if err := c.scanDB(ctx, db[0], db[1], &st); err != nil {
			errs = append(errs, fmt.Errorf("profile %s: %w", db[0], err))
		}
	}
	if st.Recorded > 0 {
		if err := c.saveState(); err != nil {
			errs = append(errs, fmt.Errorf("save state: %w", err))
		}
	}
	return st, errors.Join(errs...)
}

const usageQuery = `
SELECT u.session_id, u.model, u.billing_provider, u.billing_base_url, u.billing_mode, u.task,
       u.api_call_count, u.input_tokens, u.output_tokens, u.cache_read_tokens,
       u.cache_write_tokens, u.reasoning_tokens, COALESCE(u.last_seen, 0),
       COALESCE(s.parent_session_id, ''), COALESCE(s.git_repo_root, ''), COALESCE(s.cwd, ''),
       COALESCE(s.source, '')
FROM session_model_usage u LEFT JOIN sessions s ON s.id = u.session_id`

func (c *Collector) scanDB(ctx context.Context, profile, path string, st *Stats) error {
	// Read-only; never create or modify anything under the Hermes home.
	dsn := "file:" + (&url.URL{Path: path}).EscapedPath() + "?mode=ro&_pragma=busy_timeout(5000)&_pragma=query_only(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name='session_model_usage'`).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return nil // older Hermes schema: nothing to import
	}
	rows, err := db.QueryContext(ctx, usageQuery)
	if err != nil {
		return err
	}
	defer rows.Close()
	type pending struct {
		key string
		cur counters
		rec core.RequestRecord
	}
	var todo []pending
	for rows.Next() {
		var (
			sess, model, prov, baseURL, mode, task, parent, repo, cwd, source string
			cur                                                               counters
			lastSeen                                                          float64
		)
		if err := rows.Scan(&sess, &model, &prov, &baseURL, &mode, &task,
			&cur.Calls, &cur.Input, &cur.Output, &cur.CacheRead, &cur.CacheWrite, &cur.Reasoning,
			&lastSeen, &parent, &repo, &cwd, &source); err != nil {
			return err
		}
		st.Rows++
		if c.isSelf(baseURL) {
			st.SkippedSelf++
			continue
		}
		key := strings.Join([]string{profile, sess, model, prov, baseURL, mode, task}, "\x1f")
		prev := c.seen[key]
		if prev.less(cur) {
			// Counters went backwards (session rewritten/reset): rebase.
			c.seen[key] = cur
			continue
		}
		d := cur.sub(prev)
		if d.zero() {
			continue
		}
		todo = append(todo, pending{key: key, cur: cur, rec: c.record(key, profile, sess, model, prov, task, parent, repo, cwd, lastSeen, cur, d)})
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, p := range todo {
		if err := c.ledger.Record(ctx, p.rec); err != nil {
			return err // state for later rows not advanced; retried next scan
		}
		c.seen[p.key] = p.cur
		st.Recorded++
	}
	return nil
}

func (c *Collector) record(key, profile, sess, model, prov, task, parent, repo, cwd string, lastSeen float64, cur, d counters) core.RequestRecord {
	idSrc := fmt.Sprintf("%s\x1f%d/%d/%d/%d/%d/%d", key, cur.Calls, cur.Input, cur.Output, cur.CacheRead, cur.CacheWrite, cur.Reasoning)
	sum := sha256.Sum256([]byte(idSrc))
	ts := time.Now().UTC()
	if lastSeen > 0 && !math.IsNaN(lastSeen) {
		sec, frac := math.Modf(lastSeen)
		ts = time.Unix(int64(sec), int64(frac*1e9)).UTC()
	}
	agent := "main"
	if parent != "" {
		agent = "subagent"
	}
	if task != "" {
		agent = "aux:" + task // e.g. title_generation, compression, approval
	}
	project := ""
	if repo != "" {
		project = filepath.Base(repo)
	} else if cwd != "" {
		project = filepath.Base(cwd)
	}
	client := c.opts.Client
	if profile != "default" {
		client += "/" + profile
	}
	return core.RequestRecord{
		ID:         "hermes:" + hex.EncodeToString(sum[:])[:32],
		StartedAt:  ts,
		FinishedAt: ts,
		Client:     client,
		Class:      core.ClassInteractive,
		Route:      "hermes",
		Model:      model,
		Provider:   prov,
		AccountID:  c.account(prov, model),
		Status:     200,
		Usage: core.Usage{
			// Hermes input_tokens excludes cache reads/writes; the ledger's
			// InputTokens is the total prompt (OpenAI convention).
			InputTokens:              d.Input + d.CacheRead + d.CacheWrite,
			CachedInputTokens:        d.CacheRead,
			CacheCreationInputTokens: d.CacheWrite,
			OutputTokens:             d.Output,
			ReasoningTokens:          d.Reasoning,
		},
		UsageKnown: true,
		Session:    sess,
		Task:       project,
		Agent:      agent,
		Host:       c.opts.Host,
	}
}

func (c *Collector) account(prov, model string) string {
	if prov == "" && strings.HasPrefix(model, "claude-") {
		prov = "anthropic"
	}
	return c.opts.Accounts[prov]
}

func (c *Collector) isSelf(baseURL string) bool {
	if baseURL == "" {
		return false
	}
	u, err := url.Parse(baseURL)
	if err != nil {
		return false
	}
	for _, h := range c.opts.SelfHosts {
		if strings.EqualFold(u.Host, h) {
			return true
		}
	}
	return false
}

type persisted struct {
	Seen map[string]counters `json:"seen"`
}

func (c *Collector) loadState() {
	if c.opts.StatePath == "" {
		return
	}
	b, err := os.ReadFile(c.opts.StatePath)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			c.opts.Logger.Warn("hermeslog: load state", "err", err)
		}
		return
	}
	var p persisted
	if err := json.Unmarshal(b, &p); err != nil {
		c.opts.Logger.Warn("hermeslog: parse state; reimporting (deduplicated by id)", "err", err)
		return
	}
	for k, v := range p.Seen {
		c.seen[k] = v
	}
}

func (c *Collector) saveState() error {
	if c.opts.StatePath == "" {
		return nil
	}
	b, err := json.Marshal(persisted{Seen: c.seen})
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(c.opts.StatePath), ".hermeslog-state-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, c.opts.StatePath)
}
