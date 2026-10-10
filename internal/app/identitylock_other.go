//go:build !linux && !darwin

package app

import "errors"

// errIdentityLockHeld exists so the package builds everywhere.
var errIdentityLockHeld = errors.New("identity: identity.db is in use by another LocalRouter server")

// errIdentityLockUnsupported: exclusive ownership of identity.db is only
// implemented on the release platforms (linux, darwin). Multi-user mode fails
// closed elsewhere instead of running without single-process ownership.
var errIdentityLockUnsupported = errors.New("identity: exclusive database ownership is unsupported on this platform")

type identityLock struct{}

func acquireIdentityLock(string) (*identityLock, error) { return nil, errIdentityLockUnsupported }

func (l *identityLock) Close() error { return nil }
