//go:build unix

package main

import (
	"os"
	"syscall"
)

// reloadSignal is the signal that triggers a configuration reload on unix-like
// platforms (linux, darwin, the BSDs). SIGHUP terminates a process by default
// but is the conventional "reload your configuration" signal; once the process
// calls signal.Notify for it, the default terminate action is replaced.
//
// On Windows reloadSignal returns nil (SIGHUP does not exist there) and the
// reload loop is simply not started; see reload_windows.go.
func reloadSignal() os.Signal { return syscall.SIGHUP }
