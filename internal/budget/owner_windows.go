//go:build windows

package budget

import (
	"errors"
	"fmt"
)

// ErrOwnershipUnsupported reports that exclusive budget ownership is not
// implemented on this platform. Windows is not a LocalRouter release platform;
// the build is kept honest rather than returning a no-op guard that would let a
// caller believe it had proved single-process ownership when it had not.
var ErrOwnershipUnsupported = errors.New("budget: exclusive ownership is unsupported on windows")

// Ownership is never produced on Windows. It exists so package budget builds.
type Ownership struct{}

// AcquireOwnership always fails on Windows, wrapping ErrOwnershipUnsupported.
func AcquireOwnership(path string) (*Ownership, error) {
	return nil, fmt.Errorf("%w: %q", ErrOwnershipUnsupported, path)
}

// Close is a no-op: AcquireOwnership never returns a usable *Ownership here.
func (o *Ownership) Close() error { return nil }
