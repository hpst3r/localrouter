package connlim

// Per-user concurrency (multi-user mode). A user's limit is counted across all
// of their API keys and owned static clients, in counters the Controller owns
// and every generation View shares. Additive API:
// Controller.ViewWithUsers(global, clientLimits, perUser) (*View, error),
// View.AcquirePrincipal(core.Principal), Controller.AcquirePrincipal,
// Controller.UserActive(userID), View.UserLimit().

import (
	"errors"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/hpst3r/localrouter/internal/core"
)

const (
	userA = "u_aaaaaaaaaaaaaaaaaaaaaaaaaa"
	userB = "u_bbbbbbbbbbbbbbbbbbbbbbbbbb"
	key1  = "k_1111111111111111111111111a"
	key2  = "k_2222222222222222222222222a"
	key3  = "k_3333333333333333333333333a"
)

func userKey(user, key string) core.Principal {
	return core.Principal{Kind: core.PrincipalUserKey, Role: core.RoleUser, UserID: user, KeyID: key,
		Client: core.Client{Name: key, Class: core.ClassInteractive}}
}

func staticClient(name, owner string, role core.Role) core.Principal {
	return core.Principal{Kind: core.PrincipalStaticClient, Role: role, UserID: owner,
		Client: core.Client{Name: name, Class: core.ClassBackground}}
}

func mustViewUsers(t *testing.T, c *Controller, global int, clients map[string]int, perUser int) *View {
	t.Helper()
	v, err := c.ViewWithUsers(global, clients, perUser)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// Two API keys of one user share that user's cap; another user is unaffected.
func TestUserCapSharedAcrossKeys(t *testing.T) {
	c, err := New(0, nil)
	if err != nil {
		t.Fatal(err)
	}
	v := mustViewUsers(t, c, 0, nil, 1)
	rel, ok := v.AcquirePrincipal(userKey(userA, key1))
	if !ok {
		t.Fatal("first key refused")
	}
	if r2, ok := v.AcquirePrincipal(userKey(userA, key2)); ok {
		r2()
		t.Fatal("second key of the same user admitted past the per-user cap")
	}
	rb, ok := v.AcquirePrincipal(userKey(userB, key3))
	if !ok {
		t.Fatal("another user refused by user A's cap")
	}
	rb()
	rel()
	r2, ok := v.AcquirePrincipal(userKey(userA, key2))
	if !ok {
		t.Fatal("user slot not returned on release")
	}
	r2()
}

// A reload view sees the previous generation's per-user leases: a fresh view
// never starts with empty user counters, so a reload cannot bypass the cap.
func TestUserCapSharedAcrossViews(t *testing.T) {
	c, err := New(0, nil)
	if err != nil {
		t.Fatal(err)
	}
	old := mustViewUsers(t, c, 0, nil, 2)
	r1, ok := old.AcquirePrincipal(userKey(userA, key1))
	if !ok {
		t.Fatal("old view refused")
	}
	r2, ok := old.AcquirePrincipal(userKey(userA, key2))
	if !ok {
		t.Fatal("old view refused second")
	}
	fresh := mustViewUsers(t, c, 0, nil, 2)
	if rel, ok := fresh.AcquirePrincipal(userKey(userA, key3)); ok {
		rel()
		t.Fatal("fresh view admitted past the old generation's user leases")
	}
	looser := mustViewUsers(t, c, 0, nil, 3)
	r3, ok := looser.AcquirePrincipal(userKey(userA, key3))
	if !ok {
		t.Fatal("looser generation refused within its own limit")
	}
	if got := c.UserActive(userA); got != 3 {
		t.Fatalf("UserActive = %d, want 3", got)
	}
	r1()
	r2()
	r3()
	if got := c.UserActive(userA); got != 0 {
		t.Fatalf("after release UserActive = %d, want 0", got)
	}
	if fresh.UserLimit() != 2 || looser.UserLimit() != 3 {
		t.Fatalf("UserLimit = %d/%d", fresh.UserLimit(), looser.UserLimit())
	}
}

// An owned static client counts against the global, client and user limits at
// once; a rejection by any one of them increments nothing.
func TestPrincipalAdmissionIsAtomic(t *testing.T) {
	c, err := New(0, nil)
	if err != nil {
		t.Fatal(err)
	}
	v := mustViewUsers(t, c, 3, map[string]int{"vm1": 1}, 2)
	rs, ok := v.AcquirePrincipal(staticClient("vm1", userA, core.RoleUser))
	if !ok {
		t.Fatal("owned static client refused")
	}
	// Client limit saturated: refused, and the user counter is untouched.
	if rel, ok := v.AcquirePrincipal(staticClient("vm1", userA, core.RoleUser)); ok {
		rel()
		t.Fatal("client limit not enforced for a principal")
	}
	if got := c.UserActive(userA); got != 1 {
		t.Fatalf("client-limit rejection changed user count: %d", got)
	}
	// User limit: the static client and a key share it.
	rk, ok := v.AcquirePrincipal(userKey(userA, key1))
	if !ok {
		t.Fatal("key refused within user limit")
	}
	if rel, ok := v.AcquirePrincipal(userKey(userA, key2)); ok {
		rel()
		t.Fatal("user limit not shared by static client and key")
	}
	st := v.Stats()
	if st.GlobalActive != 2 {
		t.Fatalf("user-limit rejection changed global count: %+v", st)
	}
	// Global limit: a third request from another user fits, a fourth does not.
	rb, ok := v.AcquirePrincipal(userKey(userB, key3))
	if !ok {
		t.Fatal("other user refused below global limit")
	}
	if rel, ok := v.AcquirePrincipal(staticClient("svc", "", core.RoleService)); ok {
		rel()
		t.Fatal("global limit not enforced for a principal")
	}
	if got := c.UserActive(userB); got != 1 {
		t.Fatalf("global rejection changed counts: userB=%d", got)
	}
	// User keys never appear as per-client entries.
	for _, cl := range v.Stats().Clients {
		if cl.Name != "vm1" {
			t.Fatalf("unexpected per-client entry %+v", cl)
		}
	}
	rs()
	rk()
	rb()
	if st := v.Stats(); st.GlobalActive != 0 || len(st.Clients) != 1 || st.Clients[0].Active != 0 {
		t.Fatalf("after release stats %+v", st)
	}
}

// Release is idempotent for the user counter too.
func TestPrincipalReleaseOnce(t *testing.T) {
	c, err := New(0, nil)
	if err != nil {
		t.Fatal(err)
	}
	v := mustViewUsers(t, c, 0, nil, 2)
	r1, _ := v.AcquirePrincipal(userKey(userA, key1))
	r2, _ := v.AcquirePrincipal(userKey(userA, key2))
	r1()
	r1()
	if got := c.UserActive(userA); got != 1 {
		t.Fatalf("double release: UserActive = %d, want 1", got)
	}
	r2()
}

// Principals that may not run inference, or are malformed, are never admitted.
func TestAcquirePrincipalRejectsInvalid(t *testing.T) {
	c, err := New(0, nil)
	if err != nil {
		t.Fatal(err)
	}
	v := mustViewUsers(t, c, 0, nil, 0)
	long := "u_" + strings.Repeat("a", 127)
	cases := map[string]core.Principal{
		"session":          {Kind: core.PrincipalSession, Role: core.RoleUser, UserID: userA},
		"session with key": {Kind: core.PrincipalSession, Role: core.RoleUser, UserID: userA, KeyID: key1, Client: core.Client{Name: key1}},
		"unknown kind":     {Kind: "bogus", Role: core.RoleUser, UserID: userA, KeyID: key1, Client: core.Client{Name: key1}},
		"zero":             {},
		"key no user":      userKey("", key1),
		"key no key id":    userKey(userA, ""),
		"key admin role":   {Kind: core.PrincipalUserKey, Role: core.RoleAdmin, UserID: userA, KeyID: key1},
		"key bad user":     userKey("u_a b", key1),
		"key long user":    userKey(long, key1),
		"key control char": userKey(userA, "k_\n"),
		"static no name":   staticClient("", userA, core.RoleUser),
		"static bad owner": staticClient("vm1", "u_ä", core.RoleUser),
	}
	for name, p := range cases {
		if rel, ok := v.AcquirePrincipal(p); ok {
			rel()
			t.Errorf("%s: admitted", name)
		}
		if rel, ok := c.AcquirePrincipal(p); ok {
			rel()
			t.Errorf("%s: admitted by controller", name)
		}
	}
	if st := v.Stats(); st.GlobalActive != 0 || st.GlobalPeak != 0 {
		t.Fatalf("rejections changed counters: %+v", st)
	}
}

func TestViewWithUsersRejectsNegative(t *testing.T) {
	c, err := New(0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.ViewWithUsers(0, nil, -1); !errors.Is(err, ErrNegativeLimit) {
		t.Fatalf("perUser -1: %v", err)
	}
	if _, err := c.ViewWithUsers(-1, nil, 1); !errors.Is(err, ErrNegativeLimit) {
		t.Fatalf("global -1: %v", err)
	}
}

// The Controller's own AcquirePrincipal counts the user so views see it, and
// the legacy Acquire never touches user counters.
func TestControllerAcquirePrincipalSharesUserCounter(t *testing.T) {
	c, err := New(0, nil)
	if err != nil {
		t.Fatal(err)
	}
	rc, ok := c.AcquirePrincipal(userKey(userA, key1))
	if !ok {
		t.Fatal("controller refused")
	}
	v := mustViewUsers(t, c, 0, nil, 1)
	if rel, ok := v.AcquirePrincipal(userKey(userA, key2)); ok {
		rel()
		t.Fatal("view did not see controller-admitted user lease")
	}
	rl, ok := v.Acquire(key2)
	if !ok {
		t.Fatal("legacy acquire refused")
	}
	if got := c.UserActive(userA); got != 1 {
		t.Fatalf("legacy Acquire touched user counter: %d", got)
	}
	rl()
	rc()
}

// Concurrent admissions across two views never exceed the per-user cap.
func TestUserCapConcurrentAcrossViews(t *testing.T) {
	c, err := New(0, nil)
	if err != nil {
		t.Fatal(err)
	}
	views := []*View{mustViewUsers(t, c, 0, nil, 3), mustViewUsers(t, c, 0, nil, 3)}
	var (
		mu       sync.Mutex
		inflight int
		maxSeen  int
		wg       sync.WaitGroup
	)
	keys := []string{key1, key2, key3}
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				rel, ok := views[(i+j)%2].AcquirePrincipal(userKey(userA, keys[i%3]))
				if !ok {
					continue
				}
				mu.Lock()
				inflight++
				if inflight > maxSeen {
					maxSeen = inflight
				}
				mu.Unlock()
				runtime.Gosched()
				mu.Lock()
				inflight--
				mu.Unlock()
				rel()
			}
		}(i)
	}
	wg.Wait()
	if maxSeen > 3 {
		t.Fatalf("observed %d concurrent admissions for one user, cap 3", maxSeen)
	}
	if got := c.UserActive(userA); got != 0 {
		t.Fatalf("leaked user leases: %d", got)
	}
}
