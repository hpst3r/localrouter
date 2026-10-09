package app_test

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/app"
	"github.com/hpst3r/localrouter/internal/config"
)

type reloadBarrierClock struct {
	armed   atomic.Bool
	entered chan struct{}
	resume  chan struct{}
}

func (c *reloadBarrierClock) Now() time.Time {
	if c.armed.CompareAndSwap(true, false) {
		close(c.entered)
		<-c.resume
	}
	return time.Now()
}

func TestReloadCannotPublishAfterShutdown(t *testing.T) {
	dir := t.TempDir()
	key := filepath.Join(dir, "client.key")
	if err := os.WriteFile(key, []byte("synthetic-client-key-for-reload-shutdown"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RELOAD_SHUTDOWN_KEY", "synthetic-provider-key")
	path := filepath.Join(dir, "config.yaml")
	text := fmt.Sprintf(`listen: 127.0.0.1:0
data_dir: %s/data
clients:
 - {name: client, class: interactive, key_file: %s}
accounts:
 - {id: upstream, provider: openai_compat, base_url: http://127.0.0.1:1/v1, api_key_env: RELOAD_SHUTDOWN_KEY}
routes:
 - {name: test, models: [test], interactive: [upstream], background: [upstream]}
`, dir, key)
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	clock := &reloadBarrierClock{entered: make(chan struct{}), resume: make(chan struct{})}
	a, err := app.Build(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), app.Overrides{Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	clock.armed.Store(true)
	done := make(chan error, 1)
	go func() { _, err := a.ReloadConfig(cfg); done <- err }()
	select {
	case <-clock.entered:
	case <-time.After(3 * time.Second):
		close(clock.resume)
		t.Fatal("reload did not reach generation build barrier")
	}
	a.BeginShutdown()
	close(clock.resume)
	select {
	case err := <-done:
		if !errors.Is(err, app.ErrNotServing) {
			t.Fatalf("reload published after shutdown: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("reload blocked")
	}
	if a.ReloadStatus().Generation != 1 {
		t.Fatal("generation advanced after shutdown")
	}
}
