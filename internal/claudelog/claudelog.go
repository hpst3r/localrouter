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
	Skipped  int // lines yielding no usage (other entry types, malformed, oversized, synthetic, zero usage)
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
			c.opts.Logger.Warn("claudelog: walk", "path", path, "err", err)
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
			c.opts.Logger.Warn("claudelog: scan file", "file", rel, "err", err)
			if firstErr == nil {
				firstErr = err
			}
		}
		return nil
	})
	if walkErr != nil && firstErr == nil {
		firstErr = walkErr
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

// record writes finals to the ledger, batched when supported. It returns the
// first error; the caller then keeps the file's old state.
func (c *Collector) record(ctx context.Context, finals []core.RequestRecord, st *Stats) error {
	if bl, ok := c.ledger.(core.BatchLedger); ok {
		for len(finals) > 0 {
			n := min(len(finals), MaxBatch)
			if err := bl.RecordBatch(ctx, finals[:n]); err != nil {
				return fmt.Errorf("record batch: %w", err)
			}
			st.Recorded += n
			finals = finals[n:]
		}
		return nil
	}
	for _, r := range finals {
		if err := c.ledger.Record(ctx, r); err != nil {
			return fmt.Errorf("record: %w", err)
		}
		st.Recorded++
	}
	return nil
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
	ts, err := time.Parse(time.RFC3339Nano, e.Timestamp)
	if err != nil {
		ts = c.opts.Clock.Now()
	}
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
			InputTokens:              u.InputTokens + u.CacheCreationTokens + u.CacheReadTokens,
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
			c.opts.Logger.Warn("claudelog: load state", "err", err)
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
