package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"unicode"

	"github.com/hpst3r/localrouter/internal/app"
	"github.com/hpst3r/localrouter/internal/config"
	"github.com/hpst3r/localrouter/internal/core"
	"github.com/hpst3r/localrouter/internal/modelcheck"
)

// probeModels runs `check -probe`: it loads credentials as serve does, fetches
// each eligible account's /models once and prints one line per non-ok verdict
// plus a summary (or, with asJSON, the structured result). It exits 1 when a
// verdict is missing, or with strict when one is unchecked. It never prints
// credentials, URLs or response bodies.
func probeModels(cfg *config.Config, out io.Writer, strict, asJSON bool) error {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	creds, err := app.NewAuth(cfg, logger, app.Overrides{})
	if err != nil {
		return err
	}
	accounts := map[string]core.Account{}
	for _, a := range cfg.CoreAccounts() {
		accounts[a.ID] = a
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	res := modelcheck.Probe(ctx, creds, accounts, cfg.CoreRoutes(), modelcheck.Options{})
	if asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(res); err != nil {
			return err
		}
	} else {
		fmt.Fprintf(out, "config OK: %d clients, %d accounts, %d routes; data_dir=%s\n",
			len(cfg.Clients), len(cfg.Accounts), len(cfg.Routes), cfg.DataDir)
		writeVerdicts(out, res)
	}
	s := res.Summary
	switch {
	case s.Missing > 0:
		return exitError{code: 1, msg: fmt.Sprintf("model check: %d missing", s.Missing)}
	case strict && s.Unchecked > 0:
		return exitError{code: 1, msg: fmt.Sprintf("model check: %d unchecked", s.Unchecked)}
	}
	return nil
}

// writeVerdicts prints one line per non-ok verdict and a summary line.
func writeVerdicts(out io.Writer, res modelcheck.Result) {
	for _, v := range res.Verdicts {
		if v.Status == modelcheck.StatusOK {
			continue
		}
		line := fmt.Sprintf("%-10s route=%s account=%s expected=%s", v.Status, field(v.Route), field(v.Account), field(v.Expected))
		switch v.Status {
		case modelcheck.StatusMissing:
			if len(v.Suggestions) > 0 {
				line += " suggestions=" + field(strings.Join(v.Suggestions, ","))
			}
			if v.SingleModel {
				line += " single_model=true loaded=" + field(v.Loaded)
			}
		case modelcheck.StatusUnchecked:
			line += " error_class=" + v.ErrorClass
		case modelcheck.StatusIneligible:
			line += " provider=" + field(v.Provider)
		}
		fmt.Fprintln(out, line)
	}
	s := res.Summary
	fmt.Fprintf(out, "model check: %d ok, %d missing (%d single_model), %d unchecked, %d ineligible; %d accounts probed\n",
		s.OK, s.Missing, s.SingleModel, s.Unchecked, s.Ineligible, len(res.Accounts))
}

// field quotes a value that is empty or contains a space, quote, '=' or a
// non-graphic rune, so every line stays parseable as key=value pairs.
func field(s string) string {
	if s == "" || strings.ContainsFunc(s, func(r rune) bool {
		return r == '"' || r == '=' || unicode.IsSpace(r) || !unicode.IsGraphic(r)
	}) {
		return strconv.Quote(s)
	}
	return s
}
