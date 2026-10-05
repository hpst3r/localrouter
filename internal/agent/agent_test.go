package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
)

const testKey = "lr_secretkey_0123456789"

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// --- config ---

func TestConfigDefaults(t *testing.T) {
	c, err := parseConfig([]byte("server: https://router.tail:8787/\nhost: vm1\nkey_file: agent.key\naccount: claude-max\n"),
		"/etc/lr", "/home/u", "linux")
	if err != nil {
		t.Fatal(err)
	}
	want := Config{
		Server: "https://router.tail:8787", Host: "vm1", KeyFile: "/etc/lr/agent.key", Account: "claude-max",
		ClaudeProjectsDir: "/home/u/.claude/projects",
		Credentials:       CredentialsConfig{Source: "auto", File: "/home/u/.claude/.credentials.json", KeychainService: "Claude Code-credentials"},
		PushInterval:      time.Minute, QuotaInterval: 5 * time.Minute,
		StateDir: "/home/u/.local/state/localrouter-agent",
	}
	if *c != want {
		t.Fatalf("got %+v\nwant %+v", *c, want)
	}
	c, err = parseConfig([]byte("server: http://10.0.0.1:8787\nhost: mac.local\nkey_file: ~/k\naccount: a\nquota_interval: 0s\npush_interval: 10s\n"),
		"/x", "/Users/u", "darwin")
	if err != nil {
		t.Fatal(err)
	}
	if c.StateDir != "/Users/u/Library/Application Support/localrouter-agent" || c.KeyFile != "/Users/u/k" ||
		c.QuotaInterval != 0 || c.PushInterval != 10*time.Second {
		t.Fatalf("darwin/explicit: %+v", *c)
	}
}

func TestConfigValidation(t *testing.T) {
	base := "key_file: k\naccount: a\n"
	cases := map[string]struct{ body, want string }{
		"no server":       {base + "host: vm1\n", "server is required"},
		"ftp server":      {base + "host: vm1\nserver: ftp://x\n", "http:// or https://"},
		"no scheme":       {base + "host: vm1\nserver: router:8787\n", "http:// or https://"},
		"userinfo":        {base + "host: vm1\nserver: http://u:p@x\n", "must not contain credentials"},
		"bad host":        {base + "host: vm/1\nserver: http://x\n", "host \"vm/1\" invalid"},
		"empty host":      {base + "server: http://x\n", "host \"\" invalid"},
		"long host":       {base + "host: " + strings.Repeat("a", 65) + "\nserver: http://x\n", "invalid"},
		"no key":          {"account: a\nhost: h\nserver: http://x\n", "key_file is required"},
		"no account":      {"key_file: k\nhost: h\nserver: http://x\n", "account is required"},
		"bad source":      {base + "host: h\nserver: http://x\ncredentials: {source: env}\n", "credentials.source"},
		"tiny push":       {base + "host: h\nserver: http://x\npush_interval: 1ms\n", "push_interval"},
		"tiny quota":      {base + "host: h\nserver: http://x\nquota_interval: 1s\n", "quota_interval"},
		"unknown field":   {base + "host: h\nserver: http://x\nbogus: 1\n", "bogus"},
		"bad duration":    {base + "host: h\nserver: http://x\npush_interval: soon\n", "invalid duration"},
		"negative quota":  {base + "host: h\nserver: http://x\nquota_interval: -1m\n", "quota_interval"},
		"server w/ query": {base + "host: h\nserver: http://x/?a=b\n", "must not contain"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := parseConfig([]byte(tc.body), "/b", "/h", "linux")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want containing %q", err, tc.want)
			}
		})
	}
}

func TestLoadConfigAndReadKey(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "agent.yaml")
	os.WriteFile(p, []byte("server: http://x\nhost: h\nkey_file: agent.key\naccount: a\n"), 0o600)
	c, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.KeyFile != filepath.Join(dir, "agent.key") {
		t.Fatalf("key file %q", c.KeyFile)
	}
	if _, err := ReadKey(c.KeyFile); err == nil || strings.Contains(err.Error(), dir) {
		t.Fatalf("missing key err = %v (must not include path)", err)
	}
	os.WriteFile(c.KeyFile, []byte("  "+testKey+"\n"), 0o600)
	if k, err := ReadKey(c.KeyFile); err != nil || k != testKey {
		t.Fatalf("ReadKey = %q, %v", k, err)
	}
	os.WriteFile(c.KeyFile, []byte("a b\n"), 0o600)
	if _, err := ReadKey(c.KeyFile); err == nil || strings.Contains(err.Error(), "a b") {
		t.Fatalf("two keys err = %v", err)
	}
}

// --- remote ledger ---

type ingestCall struct {
	auth string
	req  core.IngestRequest
}

func TestRemoteLedgerRecordBatch(t *testing.T) {
	var mu sync.Mutex
	var calls []ingestCall
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != IngestPath || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("bad request %s %s", r.Method, r.URL.Path)
		}
		var raw map[string]json.RawMessage
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &raw)
		if _, ok := raw["snapshots"]; ok {
			t.Errorf("records push must omit snapshots: %s", b)
		}
		var req core.IngestRequest
		json.Unmarshal(b, &req)
		mu.Lock()
		calls = append(calls, ingestCall{r.Header.Get("Authorization"), req})
		mu.Unlock()
		json.NewEncoder(w).Encode(core.IngestResponse{SchemaVersion: 1, RecordsAccepted: len(req.Records)})
	}))
	defer srv.Close()

	l := NewRemoteLedger(NewClient(srv.URL+"/", "vm1", testKey, srv.Client()))
	rs := make([]core.RequestRecord, 1201)
	for i := range rs {
		rs[i] = core.RequestRecord{ID: fmt.Sprintf("claude:%d", i), AccountID: "claude-max", UsageKnown: true}
	}
	if err := l.RecordBatch(context.Background(), rs); err != nil {
		t.Fatal(err)
	}
	if rs[0].Host != "" {
		t.Fatal("RecordBatch mutated caller's slice")
	}
	if err := l.Record(context.Background(), core.RequestRecord{ID: "one"}); err != nil {
		t.Fatal(err)
	}
	sizes := []int{}
	for _, c := range calls {
		sizes = append(sizes, len(c.req.Records))
		if c.auth != "Bearer "+testKey || c.req.SchemaVersion != 1 || c.req.Host != "vm1" {
			t.Fatalf("bad call: auth=%q schema=%d host=%q", c.auth, c.req.SchemaVersion, c.req.Host)
		}
		for _, r := range c.req.Records {
			if r.Host != "vm1" {
				t.Fatalf("record host %q", r.Host)
			}
		}
	}
	if fmt.Sprint(sizes) != "[500 500 201 1]" {
		t.Fatalf("batch sizes %v", sizes)
	}
	if calls[2].req.Records[200].ID != "claude:1200" {
		t.Fatal("records out of order")
	}
	if _, err := l.Summary(context.Background(), time.Time{}, "model"); !errors.Is(err, ErrSummaryUnsupported) {
		t.Fatalf("Summary err = %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRemoteLedgerErrors(t *testing.T) {
	status, body := 0, ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		io.WriteString(w, body)
	}))
	defer srv.Close()
	l := NewRemoteLedger(NewClient(srv.URL, "vm1", testKey, srv.Client()))
	ctx := context.Background()

	status, body = 503, "down\x1b[31m for "+testKey+" "+strings.Repeat("x", 500)
	err := l.Record(ctx, core.RequestRecord{ID: "a"})
	var he *HTTPError
	if !errors.As(err, &he) || he.Status != 503 {
		t.Fatalf("err = %v", err)
	}
	msg := err.Error()
	if strings.Contains(msg, testKey) || strings.Contains(msg, "\x1b") || len([]rune(he.Body)) > maxErrBody+1 {
		t.Fatalf("unsanitized error: %q", msg)
	}
	if !strings.Contains(msg, "http 503") || !strings.Contains(msg, "[redacted]") {
		t.Fatalf("error %q", msg)
	}

	status, body = 403, ""
	if err := l.Record(ctx, core.RequestRecord{ID: "a"}); err == nil || err.Error() != "ingest: http 403" {
		t.Fatalf("403 err = %v", err)
	}
	status, body = 200, "not json"
	if err := l.Record(ctx, core.RequestRecord{ID: "a"}); err == nil || !strings.Contains(err.Error(), "malformed") {
		t.Fatalf("malformed err = %v", err)
	}

	// Transport error.
	dead := NewRemoteLedger(NewClient("http://127.0.0.1:1", "vm1", testKey, nil))
	if err := dead.Record(ctx, core.RequestRecord{ID: "a"}); err == nil || strings.Contains(err.Error(), testKey) {
		t.Fatalf("transport err = %v", err)
	}
}

func TestPushSnapshotShape(t *testing.T) {
	var got core.IngestRequest
	var raw string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		raw = string(b)
		json.Unmarshal(b, &got)
		io.WriteString(w, `{"schema_version":1,"snapshots_accepted":1}`)
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "vm1", testKey, srv.Client())
	at := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	resp, err := c.PushSnapshot(context.Background(), core.Snapshot{AccountID: "claude-max", FetchedAt: at,
		Windows: []core.Window{{Kind: core.Window5h, UsedFrac: 0.25}}})
	if err != nil || resp.SnapshotsAccepted != 1 {
		t.Fatalf("resp %+v err %v", resp, err)
	}
	if strings.Contains(raw, `"records"`) || got.Host != "vm1" || got.SchemaVersion != 1 ||
		len(got.Snapshots) != 1 || got.Snapshots[0].AccountID != "claude-max" || !got.Snapshots[0].FetchedAt.Equal(at) {
		t.Fatalf("body %s", raw)
	}
}

// --- credentials ---

func TestFileCredentials(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "creds.json")
	r := FileCredentials{Path: p}
	if _, err := r.ReadCredentials(context.Background()); !errors.Is(err, ErrCredentialsUnavailable) {
		t.Fatalf("missing: %v", err)
	}
	os.WriteFile(p, []byte(`{"claudeAiOauth":{"accessToken":"tok"}}`), 0o400)
	before, _ := os.Stat(p)
	b, err := r.ReadCredentials(context.Background())
	if err != nil || !strings.Contains(string(b), "tok") {
		t.Fatalf("read %q %v", b, err)
	}
	after, _ := os.Stat(p)
	if !after.ModTime().Equal(before.ModTime()) || after.Mode() != before.Mode() {
		t.Fatal("credentials file modified")
	}
	if _, err := (FileCredentials{Path: dir}).ReadCredentials(context.Background()); err == nil || strings.Contains(err.Error(), dir) {
		t.Fatalf("dir: %v", err)
	}
}

type fakeRunner struct {
	out   string
	err   error
	calls [][]string
	dl    bool
}

func (f *fakeRunner) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	_, f.dl = ctx.Deadline()
	f.calls = append(f.calls, append([]string{name}, args...))
	return []byte(f.out), f.err
}

func TestKeychainCredentials(t *testing.T) {
	f := &fakeRunner{out: "{\"claudeAiOauth\":{\"accessToken\":\"tok\"}}\n"}
	k := KeychainCredentials{Service: "Claude Code-credentials", Run: f.run}
	b, err := k.ReadCredentials(context.Background())
	if err != nil || string(b) != `{"claudeAiOauth":{"accessToken":"tok"}}` {
		t.Fatalf("got %q %v", b, err)
	}
	if fmt.Sprint(f.calls[0]) != "[security find-generic-password -a "+DefaultKeychainAccount()+" -s Claude Code-credentials -w]" || !f.dl {
		t.Fatalf("calls %q deadline=%v", f.calls, f.dl)
	}
	f.err, f.out = errors.New("exit status 44: secret stderr tok"), "partial tok"
	if _, err := k.ReadCredentials(context.Background()); !errors.Is(err, ErrKeychainUnavailable) || strings.Contains(err.Error(), "tok") {
		t.Fatalf("err = %v", err)
	}
}

func TestCredentialReaderAutoOrdering(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "creds.json")
	os.WriteFile(file, []byte(`{"from":"file"}`), 0o600)
	cc := CredentialsConfig{Source: SourceAuto, File: file, KeychainService: "svc"}
	ctx := context.Background()

	// darwin: keychain first.
	f := &fakeRunner{out: `{"from":"keychain"}`}
	r, _ := NewCredentialReader(cc, "darwin", f.run)
	if b, _ := r.ReadCredentials(ctx); string(b) != `{"from":"keychain"}` {
		t.Fatalf("darwin auto got %s", b)
	}
	// darwin: keychain fails -> file.
	f = &fakeRunner{err: errors.New("not found")}
	r, _ = NewCredentialReader(cc, "darwin", f.run)
	if b, _ := r.ReadCredentials(ctx); string(b) != `{"from":"file"}` || len(f.calls) != 1 {
		t.Fatalf("darwin fallback got %s calls %d", b, len(f.calls))
	}
	// linux: file only, keychain never invoked.
	f = &fakeRunner{out: `{"from":"keychain"}`}
	r, _ = NewCredentialReader(cc, "linux", f.run)
	if b, _ := r.ReadCredentials(ctx); string(b) != `{"from":"file"}` || len(f.calls) != 0 {
		t.Fatalf("linux auto got %s calls %d", b, len(f.calls))
	}
	// explicit keychain on linux is honored.
	cc.Source = SourceKeychain
	r, _ = NewCredentialReader(cc, "linux", f.run)
	if b, _ := r.ReadCredentials(ctx); string(b) != `{"from":"keychain"}` {
		t.Fatalf("explicit keychain got %s", b)
	}
	cc.Source = SourceFile
	r, _ = NewCredentialReader(cc, "darwin", f.run)
	if b, _ := r.ReadCredentials(ctx); string(b) != `{"from":"file"}` {
		t.Fatalf("explicit file got %s", b)
	}
	cc.Source = "bogus"
	if _, err := NewCredentialReader(cc, "linux", nil); err == nil {
		t.Fatal("bogus source accepted")
	}
}

// --- agent loop ---

func TestBackoff(t *testing.T) {
	cases := []struct {
		iv    time.Duration
		fails int
		want  time.Duration
	}{
		{time.Minute, 0, time.Minute},
		{time.Minute, 1, 2 * time.Minute},
		{time.Minute, 2, 4 * time.Minute},
		{time.Minute, 3, 5 * time.Minute},
		{time.Minute, 100, 5 * time.Minute},
		{10 * time.Minute, 2, 10 * time.Minute},
	}
	for _, c := range cases {
		if got := backoff(c.iv, c.fails); got != c.want {
			t.Errorf("backoff(%v,%d) = %v want %v", c.iv, c.fails, got, c.want)
		}
	}
}

type staticCreds []byte

func (s staticCreds) ReadCredentials(context.Context) ([]byte, error) {
	return append([]byte(nil), s...), nil
}

func TestRunOnceQuota(t *testing.T) {
	var snaps []core.Snapshot
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req core.IngestRequest
		json.NewDecoder(r.Body).Decode(&req)
		snaps = append(snaps, req.Snapshots...)
		io.WriteString(w, `{"schema_version":1}`)
	}))
	defer srv.Close()
	now := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	var expired bool
	var gotRaw string
	a := New(Options{
		Client: NewClient(srv.URL, "vm1", testKey, srv.Client()), AccountID: "claude-max",
		Credentials: staticCreds(`{"tok":1}`), QuotaInterval: time.Minute,
		FetchQuota: func(_ context.Context, raw []byte, at time.Time) (core.Snapshot, error) {
			gotRaw = string(raw)
			if expired {
				return core.Snapshot{}, ErrTokenExpired
			}
			return core.Snapshot{FetchedAt: at, Windows: []core.Window{{Kind: core.Window5h, UsedFrac: .5}}}, nil
		},
		Clock: fixedClock{now}, Logger: quiet(),
	})
	if err := a.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if gotRaw != `{"tok":1}` || len(snaps) != 1 || snaps[0].AccountID != "claude-max" || !snaps[0].FetchedAt.Equal(now) {
		t.Fatalf("snaps %+v raw %q", snaps, gotRaw)
	}
	expired = true
	if err := a.RunOnce(context.Background()); err != nil {
		t.Fatalf("expired token must not be an error: %v", err)
	}
	if len(snaps) != 1 {
		t.Fatal("pushed a snapshot with an expired token")
	}
	// quota_interval 0 disables quota even in --once.
	a.opts.QuotaInterval, expired = 0, false
	a.RunOnce(context.Background())
	if len(snaps) != 1 {
		t.Fatal("quota pushed while disabled")
	}
}

func TestRunRetriesAndStops(t *testing.T) {
	var mu sync.Mutex
	n := 0
	done := make(chan struct{})
	a := New(Options{
		PushInterval: 5 * time.Millisecond, Logger: quiet(),
		Scan: func(context.Context) error {
			mu.Lock()
			defer mu.Unlock()
			n++
			if n == 3 {
				close(done)
			}
			if n < 3 {
				return errors.New("server down")
			}
			return nil
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- a.Run(ctx) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("scan not retried")
	}
	cancel()
	select {
	case err := <-errc:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop")
	}
}

type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time { return c.t }
