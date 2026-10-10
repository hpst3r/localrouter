package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hpst3r/localrouter/internal/config"
	"github.com/hpst3r/localrouter/internal/control"
	"github.com/hpst3r/localrouter/internal/core"
	"github.com/hpst3r/localrouter/internal/identity"
	"github.com/hpst3r/localrouter/internal/weblogin"
)

// identityStartupTimeout bounds opening, migrating and pinning identity.db
// while Build holds identity.lock, before anything is served.
const identityStartupTimeout = 10 * time.Second

// identityClock returns the clock for the identity store, login service and
// bearer checks: Overrides.IdentityClock or the system clock. It is
// deliberately not Overrides.Clock, which fakes upstream/quota time.
func identityClock(ov Overrides) core.Clock {
	if ov.IdentityClock != nil {
		return ov.IdentityClock
	}
	return core.SystemClock{}
}

// registerOwnedKeys binds every key of every configured role "user" static
// client to its owner in the identity store, before the generation that
// holds them is served, whether or not a key has ever been used. Bindings and
// revocations are durable: a key revoked by its owner's disable, delete or
// policy denial stays revoked when it is configured again (reload, restart, a
// restored file), and only a new key can replace it.
//
// The key files are read here a second time, with auth's checks. A file that
// changed since the static key table loaded it leaves the loaded key
// unregistered, which denies it (fail closed). An unreadable key file, a key
// bound to a different owner or a store failure fails the generation, with
// its reload reason. Revoked and pending keys do not: they are logged by
// client name and denied per request.
func registerOwnedKeys(st *identity.Store, clients []config.ClientConfig, logger *slog.Logger) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), identityStartupTimeout)
	defer cancel()
	for _, cl := range clients {
		if core.Role(cl.Role) != core.RoleUser {
			continue
		}
		for _, path := range cl.KeyPaths() {
			token, err := readOwnedKey(cl.Name, path)
			if err != nil {
				return "client keys invalid", err
			}
			state, err := st.RegisterStaticKey(ctx, cl.Owner, token)
			if errors.Is(err, identity.ErrStaticKeyConflict) {
				return "client keys invalid", fmt.Errorf("client %s: a key is already bound to a different identity user; generate a new key", cl.Name)
			}
			if err != nil {
				return "identity unavailable", fmt.Errorf("client %s: register owned key: %w", cl.Name, err)
			}
			switch state {
			case identity.StaticKeyRevoked:
				logger.Warn("owned static key is revoked; replace it with a new key", "client", cl.Name)
			case identity.StaticKeyPending:
				logger.Warn("owned static key owner is not provisioned", "client", cl.Name)
			}
		}
	}
	return "", nil
}

// readOwnedKey reads one client key file as auth.LoadClientKeyFiles does:
// private file, whitespace trimmed.
func readOwnedKey(client, path string) (string, error) {
	if err := core.CheckPrivateFile("client "+client+": key file", path); err != nil {
		return "", err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("client %s: key file: %w", client, err)
	}
	return strings.TrimSpace(string(b)), nil
}

// openIdentity takes server ownership of identity.db and opens it with
// the block's store options (the store rejects anything above its ceilings,
// so configuration can only tighten). Ownership is acquired BEFORE the store
// is opened; any failure releases exactly what was acquired, store first, so
// a failed Build leaves the lock free for the next attempt.
func openIdentity(cfg *config.Config, clock core.Clock) (*identityLock, *identity.Store, error) {
	lock, err := acquireIdentityLock(filepath.Join(cfg.DataDir, "identity.lock"))
	if err != nil {
		return nil, nil, err
	}
	opts := cfg.Identity.StoreOptions()
	opts.Clock = clock
	ctx, cancel := context.WithTimeout(context.Background(), identityStartupTimeout)
	defer cancel()
	st, err := identity.Open(ctx, filepath.Join(cfg.DataDir, "identity.db"), opts)
	if err != nil {
		_ = lock.Close()
		return nil, nil, fmt.Errorf("identity store: %w", err)
	}
	return lock, st, nil
}

// newWebLogin builds the process-lifetime OIDC login service. It performs no
// network I/O: discovery is lazy, on the first login, so the provider is
// never a startup or readiness dependency.
func newWebLogin(ic *config.IdentityConfig, st *identity.Store, clock core.Clock, logger *slog.Logger, ov Overrides) (*weblogin.Service, error) {
	svc, err := weblogin.New(webLoginConfig(ic, st, clock, logger, ov))
	if err != nil {
		return nil, fmt.Errorf("identity login: %w", err)
	}
	return svc, nil
}

// webLoginConfig is the block as config.WebLoginConfig maps it — the access
// policy as configured (weblogin requires an allowed claim value for every
// login and lets admin_subjects only promote one) — with the client secret
// re-read by config's private-file reader at every token exchange, the
// identity store hooks, and the process-level seams.
func webLoginConfig(ic *config.IdentityConfig, st *identity.Store, clock core.Clock, logger *slog.Logger, ov Overrides) weblogin.Config {
	wc := ic.WebLoginConfig(ic.OIDC.ReadClientSecret, loginHooks{store: st})
	wc.Outbound.RootCAs = ov.IdentityRootCAs
	wc.Outbound.TestDialContext = ov.IdentityTestDialContext
	wc.Clock = clock
	wc.Logger = logger
	return wc
}

// closeIdentity closes the identity store and then releases identity.lock, in
// that order: Store.Close waits for in-flight operations and is terminal, so
// none of our writes are still on the file when another process may become
// its owner. The public Identity pointer is kept (a closed store fails every
// call closed with ErrClosed), so handlers still holding it never race a nil
// write. Idempotent; called from Build's failure cleanup (pre-publication)
// and from Close under reloadMu.
func (a *App) closeIdentity() {
	if a.Identity != nil {
		_ = a.Identity.Close()
	}
	if a.identityLock != nil {
		_ = a.identityLock.Close()
		a.identityLock = nil
	}
}

// storagePingers is the readiness probe of a multi-user generation: local
// storage health is the ledger AND the identity store (a closed or broken
// identity.db cannot authenticate anyone). The first failure wins. It never
// contacts the OIDC provider, so provider availability is not readiness.
type storagePingers []control.StoragePinger

func (p storagePingers) Ping(ctx context.Context) error {
	for _, s := range p {
		if err := s.Ping(ctx); err != nil {
			return err
		}
	}
	return nil
}
