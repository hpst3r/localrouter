//go:build windows

package main

import "os"

// reloadSignal reports that Windows has no configuration-reload signal
// (SIGHUP does not exist there). Returning nil makes cmdServe skip the reload
// goroutine entirely; the in-process Reload seam remains callable. This file
// exists so cmdServe stays platform-agnostic and windows/amd64 keeps building.
func reloadSignal() os.Signal { return nil }
