package main

import (
	"context"
	"log/slog"
	"os"

	"github.com/hpst3r/localrouter/internal/app"
	"github.com/hpst3r/localrouter/internal/core"
)

// reloadTrigger is the minimal view of the app the SIGHUP loop needs: the
// validated, atomic, in-place reload. It is satisfied by *app.App and is
// written as an interface so cmdServe can be compiled and tested against a
// fake without standing up the whole app.
type reloadTrigger interface {
	// Reload re-reads path, validates it and atomically publishes the
	// reloadable subset. It returns the sanitized status and, on rejection,
	// an error while leaving the previous configuration serving.
	Reload(path string) (core.ReloadStatus, error)
}

// reasonUnknown is the fixed, sanitized reason logged when a reload is
// rejected but the app named no cause. The raw error is never logged: it can
// embed a secret filesystem path or key material.
const reasonUnknown = "reload failed"

// appReloader resolves the app's in-place reload seam. It resolves by type
// assertion so the CLI keeps building against the frozen signature
// Reload(path) (core.ReloadStatus, error) whether or not the reload manager is
// present in the same tree. A missing seam is reported once at startup rather
// than silently accepting a SIGHUP that would do nothing.
func appReloader(a *app.App, logger *slog.Logger) reloadTrigger {
	r, ok := any(a).(reloadTrigger)
	if !ok {
		logger.Warn("config reload unavailable: app exposes no Reload(path) (core.ReloadStatus, error) seam")
		return nil
	}
	return r
}

// reloadLoop performs one bounded Reload per signal received, until ctx is
// cancelled. The signal channel is buffered by the caller, so a burst of rapid
// SIGHUPs coalesces (last-wins) under normal Go channel semantics and never
// blocks the process; each reload is serialized inside the app.
//
// Only sanitized status is logged: the reason class and the restart-only
// config key names. The raw error is discarded so no secret path or key can
// reach the log. The loop returns promptly on ctx.Done, and the caller runs
// signal.Stop so the default signal disposition is restored on shutdown.
func reloadLoop(ctx context.Context, r reloadTrigger, logger *slog.Logger, path string, sigs <-chan os.Signal) {
	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-sigs:
			if !ok {
				return
			}
			out, err := r.Reload(path)
			if err != nil {
				reason := out.Reason
				if reason == "" {
					reason = reasonUnknown
				}
				logger.Warn("config reload rejected",
					"reason", reason,
					"restart_only", out.RestartOnly,
					"generation", out.Generation)
				continue
			}
			logger.Info("config reloaded", "generation", out.Generation)
		}
	}
}
