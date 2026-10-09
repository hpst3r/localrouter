//go:build windows

package main

import "testing"

// Windows has no SIGHUP, so cmdServe must not register a reload signal. This
// is a compile-and-run guard for the windows/amd64 target: if reloadSignal
// ever returned a real signal, the reload loop would try to register a signal
// the platform cannot deliver.
func TestReloadSignalNilOnWindows(t *testing.T) {
	if got := reloadSignal(); got != nil {
		t.Fatalf("reloadSignal() = %v on windows, want nil", got)
	}
}
