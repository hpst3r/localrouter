package claudelog

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
)

// maxLineBytes is the largest transcript line (excluding the newline) that is
// parsed; longer lines are skipped.
const maxLineBytes = 8 << 20

// MaxBatch is the largest slice passed to one core.BatchLedger.RecordBatch
// call.
const MaxBatch = 500

// maxTokens bounds every usage field of a parsed record; larger values are
// implausible and would be rejected by the server's ingest validation.
const maxTokens = 1_000_000_000_000

// maxIDLen bounds RequestRecord.ID (ours are always 39 bytes).
const maxIDLen = 128

// PermanentError is implemented by ledger errors that classify themselves
// (e.g. *agent.HTTPError). Permanent() == true means the records were
// rejected as invalid and resending them unchanged cannot succeed.
type PermanentError interface {
	Permanent() bool
}

// Defaults for Options.
const (
	DefaultScanInterval = time.Minute
	DefaultQuietPeriod  = 30 * time.Second
)

// Options configures a Collector.
type Options struct {
	// Dir is the Claude Code projects directory (e.g. ~/.claude/projects).
	Dir string
	// AccountID is the ledger account (claude_logs.account).
	AccountID string
	// Host is set on every produced RequestRecord.Host. The per-host agent
	// sets it; the server-local collector leaves it "".
	Host string
	// StatePath is the JSON file persisting per-file offsets; empty disables
	// persistence (every restart rescans, which is safe but slower).
	StatePath string
	// ScanInterval between scans in Start (default 1m).
	ScanInterval time.Duration
	// QuietPeriod after which an unmodified file's last message is final
	// (default 30s).
	QuietPeriod time.Duration
	Clock       core.Clock
	Logger      *slog.Logger
}

// Stats summarizes one scan.
type Stats struct {
	Files    int // transcript files examined
	Lines    int // complete lines read
	Recorded int // final messages passed to Ledger.Record/RecordBatch without error
	Pending  int // messages awaiting finality after the scan
	Skipped  int // lines yielding no usage (other entry types, malformed, oversized, synthetic, zero or invalid usage)
	Dropped  int // final messages the ledger permanently rejected; dropped so the file can advance
}

// Collector incrementally ingests Claude Code transcripts into a ledger.
type Collector struct {
	ledger core.Ledger
	opts   Options

	mu    sync.Mutex // serializes scans; guards files
	files map[string]*fileState
}

// fileState tracks one transcript. Offset is how far complete lines have been
// read; the persisted offset is the start of the pending message's first line
// (or Offset when nothing is pending) so a restart re-reads unfinished keys.
type fileState struct {
	Size    int64
	Offset  int64
	pending *pendingMsg
}

func (s *fileState) commitOffset() int64 {
	if s.pending != nil {
		return s.pending.start
	}
	return s.Offset
}

type pendingMsg struct {
	key   string
	start int64 // byte offset of the first line seen for key
	rec   core.RequestRecord
}

// New returns a Collector writing to ledger. Persisted state at
// opts.StatePath, if any, is loaded immediately.
func New(ledger core.Ledger, opts Options) *Collector {
	if opts.ScanInterval <= 0 {
		opts.ScanInterval = DefaultScanInterval
	}
	if opts.QuietPeriod <= 0 {
		opts.QuietPeriod = DefaultQuietPeriod
	}
	if opts.Clock == nil {
		opts.Clock = core.SystemClock{}
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	c := &Collector{ledger: ledger, opts: opts, files: make(map[string]*fileState)}
	c.loadState()
	return c
}

// Start scans once immediately and then every ScanInterval in a background
// goroutine until ctx is cancelled. It does not block.
func (c *Collector) Start(ctx context.Context) {
	go func() {
		t := time.NewTicker(c.opts.ScanInterval)
		defer t.Stop()
		for {
			if st, err := c.ScanOnce(ctx); err != nil {
				c.opts.Logger.Warn("claudelog: scan failed", "err", err)
			} else if st.Recorded > 0 {
				c.opts.Logger.Debug("claudelog: scan", "files", st.Files, "recorded", st.Recorded, "pending", st.Pending)
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
}

// ScanOnce walks Dir for *.jsonl files, reads newly appended complete lines,
// records messages whose usage is final, and persists offsets. If the ledger
// implements core.BatchLedger, each file's final messages are sent with
// RecordBatch in chunks of at most MaxBatch; otherwise Record is called per
// message. A missing Dir is not an error. On a ledger error the affected
// file's state is left unchanged (it is re-read next scan and IDs dedupe) and
// the first such error is returned.
func (c *Collector) ScanOnce(ctx context.Context) (Stats, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	var st Stats
	var firstErr error
	seen := make(map[string]bool)
	dirty := false

	walkErr := filepath.WalkDir(c.opts.Dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if path == c.opts.Dir && errors.Is(err, fs.ErrNotExist) {
				return fs.SkipAll
			}
			c.opts.Logger.Warn("claudelog: walk", "project", c.walkProject(path, d), "err", stripPath(err))
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".jsonl") || !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(c.opts.Dir, path)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		seen[rel] = true
		st.Files++
		changed, err := c.scanFile(ctx, path, rel, &st)
		if changed {
			dirty = true
		}
		if err != nil {
			// Log only the project dir slug, never the session file path.
			err = stripPath(err)
			c.opts.Logger.Warn("claudelog: scan file", "project", projectName(rel), "err", err)
			if firstErr == nil {
				firstErr = err
			}
		}
		return nil
	})
	if walkErr != nil && firstErr == nil {
		firstErr = stripPath(walkErr)
	}
	if walkErr == nil {
		for rel := range c.files {
			if !seen[rel] {
				delete(c.files, rel)
				dirty = true
			}
		}
	}
	for _, fsx := range c.files {
		if fsx.pending != nil {
			st.Pending++
		}
	}
	if dirty {
		if err := c.saveState(); err != nil {
			err = stripPath(err)
			c.opts.Logger.Warn("claudelog: save state", "err", err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return st, firstErr
}

// scanFile processes one transcript. It computes the new state and the
// records made final, records them, and only then commits the new state.
// changed reports whether persisted state changed.
func (c *Collector) scanFile(ctx context.Context, path, rel string, st *Stats) (changed bool, err error) {
	old := c.files[rel]
	if old == nil {
		old = &fileState{}
	}
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return false, err
	}

	next := *old
	if info.Size() < next.Offset { // truncated or replaced: start over
		next = fileState{}
	}
	next.Size = info.Size()

	var finals []core.RequestRecord
	if info.Size() > next.Offset {
		task := projectName(rel)
		end, err := readLines(f, next.Offset, func(line []byte, start int64, oversized bool) {
			st.Lines++
			if oversized {
				st.Skipped++
				return
			}
			key, rec, ok := c.parse(line, task)
			if !ok {
				st.Skipped++
				return
			}
			if why := invalid(rec); why != "" {
				st.Skipped++
				c.opts.Logger.Warn("claudelog: skipping invalid usage", "project", task, "reason", why)
				return
			}
			p := next.pending
			switch {
			case p == nil:
				next.pending = &pendingMsg{key: key, start: start, rec: rec}
			case p.key != key:
				finals = append(finals, p.rec)
				next.pending = &pendingMsg{key: key, start: start, rec: rec}
			case rec.Usage.OutputTokens >= p.rec.Usage.OutputTokens:
				next.pending = &pendingMsg{key: key, start: p.start, rec: rec}
			}
		})
		if err != nil {
			return false, err
		}
		next.Offset = end
	}

	// Re-stat after reading so a write during the read keeps the file "recent".
	if next.pending != nil {
		if info2, err := f.Stat(); err == nil {
			info = info2
		}
		if c.opts.Clock.Now().Sub(info.ModTime()) >= c.opts.QuietPeriod {
			finals = append(finals, next.pending.rec)
			next.pending = nil
		}
	}

	if err := c.record(ctx, finals, st); err != nil {
		return false, err
	}
	changed = old.Size != next.Size || old.commitOffset() != next.commitOffset() || c.files[rel] == nil
	c.files[rel] = &next
	return changed, nil
}

// record writes finals to the ledger, batched when supported. A permanent
// rejection (see PermanentError) is narrowed down by bisection and only the
// rejected records are dropped, so one bad record cannot stall its file
// forever. Any other error is returned; the caller then keeps the file's old
// state and the records are resent next scan (IDs dedupe).
func (c *Collector) record(ctx context.Context, finals []core.RequestRecord, st *Stats) error {
	if bl, ok := c.ledger.(core.BatchLedger); ok {
		for len(finals) > 0 {
			n := min(len(finals), MaxBatch)
			if err := c.sendBatch(ctx, bl, finals[:n], st); err != nil {
				return fmt.Errorf("record batch: %w", err)
			}
			finals = finals[n:]
		}
		return nil
	}
	for _, r := range finals {
		if err := c.ledger.Record(ctx, r); err != nil {
			if !isPermanent(err) {
				return fmt.Errorf("record: %w", err)
			}
			c.drop(r, err, st)
			continue
		}
		st.Recorded++
	}
	return nil
}

// sendBatch records rs, bisecting on permanent errors to isolate and drop
// only the rejected records.
func (c *Collector) sendBatch(ctx context.Context, bl core.BatchLedger, rs []core.RequestRecord, st *Stats) error {
	err := bl.RecordBatch(ctx, rs)
	switch {
	case err == nil:
		st.Recorded += len(rs)
		return nil
	case !isPermanent(err):
		return err
	case len(rs) == 1:
		c.drop(rs[0], err, st)
		return nil
	}
	mid := len(rs) / 2
	if err := c.sendBatch(ctx, bl, rs[:mid], st); err != nil {
		return err
	}
	return c.sendBatch(ctx, bl, rs[mid:], st)
}

// drop counts and logs (project slug and record ID only) a rejected record.
func (c *Collector) drop(r core.RequestRecord, err error, st *Stats) {
	st.Dropped++
	c.opts.Logger.Warn("claudelog: ledger rejected record; dropping", "project", r.Task, "id", r.ID, "err", err)
}

func isPermanent(err error) bool {
	var pe PermanentError
	return errors.As(err, &pe) && pe.Permanent()
}

// invalid returns why rec would fail ingest validation, or "" if it is valid.
func invalid(rec core.RequestRecord) string {
	u := rec.Usage
	for _, v := range []int64{u.InputTokens, u.CachedInputTokens, u.CacheCreationInputTokens, u.OutputTokens, u.ReasoningTokens} {
		if v < 0 {
			return "negative usage"
		}
		if v > maxTokens {
			return "implausible usage"
		}
	}
	if rec.ID == "" || len(rec.ID) > maxIDLen {
		return "bad id"
	}
	if rec.StartedAt.IsZero() {
		return "missing timestamp"
	}
	return ""
}

// sumTokens adds token counts, returning -1 (rejected by invalid) if any term
// is out of range, which also rules out overflow.
func sumTokens(vs ...int64) int64 {
	var s int64
	for _, v := range vs {
		if v < 0 || v > maxTokens {
			return -1
		}
		s += v
	}
	return s
}

// readLines reads complete newline-terminated lines from f starting at off,
// calling fn for each with its starting offset. Lines longer than
// maxLineBytes are reported with oversized=true and no content. A trailing
// partial line is not consumed. It returns the offset after the last complete
// line.
func readLines(f *os.File, off int64, fn func(line []byte, start int64, oversized bool)) (int64, error) {
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return off, err
	}
	r := bufio.NewReaderSize(f, 64<<10)
	pos := off
	for {
		start := pos
		var buf []byte
		oversized := false
		for {
			chunk, err := r.ReadSlice('\n')
			pos += int64(len(chunk))
			if !oversized {
				if len(buf)+len(chunk) > maxLineBytes+1 {
					oversized, buf = true, nil
				} else {
					buf = append(buf, chunk...)
				}
			}
			if err == nil {
				break
			}
			if errors.Is(err, bufio.ErrBufferFull) {
				continue
			}
			if errors.Is(err, io.EOF) {
				return start, nil
			}
			return start, err
		}
		fn(bytes.TrimSuffix(buf, []byte("\n")), start, oversized)
	}
}

// entry is the subset of a transcript line that is read. Content fields are
// deliberately absent so they are never retained.
type entry struct {
	RequestID   string `json:"requestId"`
	SessionID   string `json:"sessionId"`
	Timestamp   string `json:"timestamp"`
	IsSidechain bool   `json:"isSidechain"`
	Message     *struct {
		ID    string `json:"id"`
		Model string `json:"model"`
		Usage *struct {
			InputTokens         int64 `json:"input_tokens"`
			CacheCreationTokens int64 `json:"cache_creation_input_tokens"`
			CacheReadTokens     int64 `json:"cache_read_input_tokens"`
			OutputTokens        int64 `json:"output_tokens"`
			OutputTokensDetails *struct {
				ThinkingTokens int64 `json:"thinking_tokens"`
			} `json:"output_tokens_details"`
		} `json:"usage"`
	} `json:"message"`
}

// parse extracts the dedupe key and ledger record from one line; ok=false if
// the line carries no recordable usage.
func (c *Collector) parse(line []byte, task string) (key string, rec core.RequestRecord, ok bool) {
	var e entry
	if err := json.Unmarshal(line, &e); err != nil || e.Message == nil || e.Message.Usage == nil {
		return "", rec, false
	}
	m, u := e.Message, e.Message.Usage
	if m.Model == "<synthetic>" {
		return "", rec, false
	}
	if u.InputTokens == 0 && u.CacheCreationTokens == 0 && u.CacheReadTokens == 0 && u.OutputTokens == 0 {
		return "", rec, false
	}
	switch {
	case m.ID != "" && e.RequestID != "":
		key = m.ID + ":" + e.RequestID
	case m.ID != "":
		key = m.ID
	case e.RequestID != "":
		key = e.RequestID
	default:
		return "", rec, false
	}
	// An unparseable timestamp leaves ts zero; invalid then rejects the line.
	ts, _ := time.Parse(time.RFC3339Nano, e.Timestamp)
	var thinking int64
	if u.OutputTokensDetails != nil {
		thinking = u.OutputTokensDetails.ThinkingTokens
	}
	agent := "main"
	if e.IsSidechain {
		agent = "subagent"
	}
	sum := sha256.Sum256([]byte(key))
	rec = core.RequestRecord{
		ID:         "claude:" + hex.EncodeToString(sum[:])[:32],
		StartedAt:  ts,
		FinishedAt: ts,
		Client:     "claude-code",
		Class:      core.ClassInteractive,
		Route:      "claude",
		Model:      m.Model,
		Provider:   core.ProviderClaude,
		AccountID:  c.opts.AccountID,
		Status:     200,
		Usage: core.Usage{
			InputTokens:              sumTokens(u.InputTokens, u.CacheCreationTokens, u.CacheReadTokens),
			CachedInputTokens:        u.CacheReadTokens,
			CacheCreationInputTokens: u.CacheCreationTokens,
			OutputTokens:             u.OutputTokens,
			ReasoningTokens:          thinking,
		},
		UsageKnown: true,
		Session:    e.SessionID,
		Task:       task,
		Agent:      agent,
		Host:       c.opts.Host,
	}
	return key, rec, true
}

// projectName returns the project directory (first path element) of a
// slash-separated path relative to Dir, or "" for files directly in Dir.
func projectName(rel string) string {
	if i := strings.IndexByte(rel, '/'); i > 0 {
		return rel[:i]
	}
	return ""
}

// walkProject returns the project slug for a walk entry: the first path
// element under Dir, or "" for Dir itself and files directly in it.
func (c *Collector) walkProject(path string, d fs.DirEntry) string {
	rel, err := filepath.Rel(c.opts.Dir, path)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return ""
	}
	rel = filepath.ToSlash(rel)
	if p := projectName(rel); p != "" {
		return p
	}
	if d != nil && d.IsDir() {
		return rel
	}
	return ""
}

// stripPath drops file paths from *fs.PathError and *os.LinkError, keeping
// the operation and cause, so logs and returned errors stay path-free.
func stripPath(err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return fmt.Errorf("%s: %w", pe.Op, pe.Err)
	}
	var le *os.LinkError
	if errors.As(err, &le) {
		return fmt.Errorf("%s: %w", le.Op, le.Err)
	}
	return err
}

// persisted is the on-disk state format.
type persisted struct {
	Files map[string]persistedFile `json:"files"`
}

type persistedFile struct {
	Size   int64 `json:"size"`
	Offset int64 `json:"offset"`
}

func (c *Collector) loadState() {
	if c.opts.StatePath == "" {
		return
	}
	b, err := os.ReadFile(c.opts.StatePath)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			c.opts.Logger.Warn("claudelog: load state", "err", stripPath(err))
		}
		return
	}
	var p persisted
	if err := json.Unmarshal(b, &p); err != nil {
		c.opts.Logger.Warn("claudelog: parse state; rescanning", "err", err)
		return
	}
	for rel, pf := range p.Files {
		if pf.Offset < 0 || pf.Size < pf.Offset {
			continue
		}
		c.files[rel] = &fileState{Size: pf.Size, Offset: pf.Offset}
	}
}

// saveState atomically writes the state file with mode 0600.
func (c *Collector) saveState() error {
	if c.opts.StatePath == "" {
		return nil
	}
	p := persisted{Files: make(map[string]persistedFile, len(c.files))}
	for rel, s := range c.files {
		p.Files[rel] = persistedFile{Size: s.Size, Offset: s.commitOffset()}
	}
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	dir := filepath.Dir(c.opts.StatePath)
	tmp, err := os.CreateTemp(dir, ".claudelog-state-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename
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
	return os.Rename(tmpName, c.opts.StatePath)
}
