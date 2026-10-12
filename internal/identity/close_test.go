package identity

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
)

func TestCloseIsTerminalAndIdempotent(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	u := f.user("sub-close", core.RoleUser)
	if err := f.s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.s.Close(); err != nil {
		t.Fatalf("second Close = %v", err)
	}
	checks := map[string]error{
		"Ping":          f.s.Ping(ctx),
		"DisableUser":   f.s.DisableUser(ctx, Actor{Kind: ActorCLI}, u.ID),
		"RevokeSession": f.s.RevokeSession(ctx, "lrs_"+strings.Repeat("A", 43)),
	}
	_, checks["User"] = f.s.User(ctx, u.ID)
	_, checks["CreateKey"] = f.s.CreateKey(ctx, u.ID, "k", 0)
	_, checks["ResolveLogin"] = f.s.ResolveLogin(ctx, f.login("sub-close", core.RoleUser))
	_, checks["ListKeys"] = f.s.ListKeys(ctx, u.ID)
	_, checks["AuditEvents"] = f.s.AuditEvents(ctx, 0, 1)
	_, checks["PurgeExpiredSessions"] = f.s.PurgeExpiredSessions(ctx)
	for name, err := range checks {
		if !errors.Is(err, ErrClosed) {
			t.Errorf("%s after Close = %v, want ErrClosed", name, err)
		}
	}
}

func TestCloseRacesInFlightOperations(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	u := f.user("sub-race-close", core.RoleUser)
	k, err := f.s.CreateKey(ctx, u.ID, "k", 0)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	start := make(chan struct{})
	closed := make(chan struct{})
	var mu sync.Mutex
	var afterCloseOK int
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			for j := 0; j < 25; j++ {
				var err error
				if i%2 == 0 {
					_, err = f.s.AuthenticateKey(ctx, k.Token)
				} else {
					_, err = f.s.CreateSession(ctx, u.ID)
				}
				select {
				case <-closed:
					if err == nil {
						mu.Lock()
						afterCloseOK++
						mu.Unlock()
					}
				default:
				}
				if err != nil && !errors.Is(err, ErrClosed) && !errors.Is(err, core.ErrAuthUnavailable) && !errors.Is(err, ErrStaleLogin) {
					t.Errorf("unexpected error during close race: %v", err)
				}
			}
		}(i)
	}
	close(start)
	time.Sleep(5 * time.Millisecond)
	if err := f.s.Close(); err != nil {
		t.Fatalf("Close = %v", err)
	}
	close(closed)
	wg.Wait()
	if afterCloseOK != 0 {
		t.Fatalf("%d operations started after Close returned succeeded", afterCloseOK)
	}
}

func TestOperationsAreBoundedByOpTimeout(t *testing.T) {
	f := newFixture(t)
	f.opts.OpTimeout = 300 * time.Millisecond
	s := f.second()
	ctx := context.Background()
	u := f.user("sub-timeout", core.RoleUser)

	// Another process holds the write lock.
	holder, err := sql.Open("sqlite", "file:"+f.path+"?_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	tx, err := holder.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()

	begin := time.Now()
	err = s.DisableUser(ctx, Actor{Kind: ActorCLI}, u.ID)
	elapsed := time.Since(begin)
	if err == nil {
		t.Fatal("write succeeded while another connection held the write lock")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("blocked write took %v, want bounded by OpTimeout", elapsed)
	}
	tx.Rollback()
	if got, _ := f.s.User(ctx, u.ID); got.Status != StatusActive {
		t.Fatal("failed write had an effect")
	}
}
