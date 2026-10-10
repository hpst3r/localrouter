package identity

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/hpst3r/localrouter/internal/core"
)

// After DisableUser returns on one *Store, no authentication that starts
// afterwards on another *Store may succeed (no positive cache anywhere).
func TestConcurrentLookupsVersusDisableOnSecondStore(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	c := f.userWithCreds("sub-concurrent", core.RoleUser)
	cli := f.second()

	var disabled atomic.Bool
	var lateSuccess, ok atomic.Int64
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				after := disabled.Load()
				var err error
				if i%2 == 0 {
					_, err = f.s.AuthenticateKey(ctx, c.key.Token)
				} else {
					_, err = f.s.AuthenticateSession(ctx, c.session.Token)
				}
				switch {
				case err == nil && after:
					lateSuccess.Add(1)
				case err == nil:
					ok.Add(1)
				case !errors.Is(err, core.ErrUnauthenticated):
					t.Errorf("lookup error: %v", err)
				}
			}
		}(i)
	}
	for ok.Load() < 20 {
		// let lookups succeed before the disable
	}
	if err := cli.DisableUser(ctx, Actor{Kind: ActorCLI}, c.user.ID); err != nil {
		t.Fatal(err)
	}
	disabled.Store(true)
	for i := 0; i < 50; i++ {
		f.assertCredsDenied(f.s, c)
	}
	close(stop)
	wg.Wait()
	if n := lateSuccess.Load(); n != 0 {
		t.Fatalf("%d lookups that started after DisableUser returned succeeded", n)
	}
}

// Two stores racing the first login of one subject produce exactly one user.
func TestConcurrentFirstLoginProvisionsOnce(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	stores := []*Store{f.s, f.second(), f.second()}
	ids := make([]string, 12)
	var wg sync.WaitGroup
	for i := range ids {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			u, err := stores[i%len(stores)].ResolveLogin(ctx, f.login("sub-race", core.RoleUser))
			if err != nil {
				t.Errorf("ResolveLogin = %v", err)
				return
			}
			ids[i] = u.ID
		}(i)
	}
	wg.Wait()
	for _, id := range ids {
		if id != ids[0] {
			t.Fatalf("racing logins produced different users: %v", ids)
		}
	}
	users, _ := f.s.ListUsers(ctx, "", MaxListLimit)
	if len(users) != 1 {
		t.Fatalf("users = %d, want 1", len(users))
	}
}
