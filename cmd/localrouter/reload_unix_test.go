//go:build unix

package main

import (
	"syscall"
	"testing"
)

// On every unix-like platform the reload trigger is SIGHUP (linux, darwin,
// the BSDs). The windows counterpart returns nil and is compile-only here.
func TestReloadSignalIsSIGHUP(t *testing.T) {
	if got := reloadSignal(); got != syscall.SIGHUP {
		t.Fatalf("reloadSignal() = %v, want SIGHUP", got)
	}
}
