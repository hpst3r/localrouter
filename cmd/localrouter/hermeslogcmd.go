package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/hpst3r/localrouter/internal/config"
	"github.com/hpst3r/localrouter/internal/hermeslog"
	"github.com/hpst3r/localrouter/internal/ledger"
)

const hermeslogUsage = `usage: localrouter hermeslog backfill-self --model M [--model M ...] --account ID --before RFC3339 [--apply] [-config PATH]`

// stringList is a repeatable string flag.
type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

func cmdHermeslog(args []string) error { return runHermeslog(args, os.Stdout) }

// runHermeslog implements `localrouter hermeslog backfill-self`: a one-time
// import of self-routed Hermes usage the proxy failed to meter (see
// hermeslog.Collector.BackfillSelf). It is a dry run unless --apply is given.
func runHermeslog(args []string, out io.Writer) error {
	if len(args) < 1 || args[0] != "backfill-self" {
		return errors.New(hermeslogUsage)
	}
	fs := flag.NewFlagSet("hermeslog backfill-self", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var models stringList
	fs.Var(&models, "model", "Hermes model name to backfill (repeatable, required)")
	account := fs.String("account", "", "account id to attribute rows to (required)")
	beforeS := fs.String("before", "", "exclusive last_seen cutoff, RFC3339 (required)")
	apply := fs.Bool("apply", false, "record rows (default: dry run)")
	path := fs.String("config", defaultConfigPath(), "config file")
	if err := fs.Parse(args[1:]); err != nil {
		return fmt.Errorf("%w\n%s", err, hermeslogUsage)
	}
	if fs.NArg() != 0 || len(models) == 0 || *account == "" || *beforeS == "" {
		return errors.New(hermeslogUsage)
	}
	before, err := time.Parse(time.RFC3339, *beforeS)
	if err != nil {
		return fmt.Errorf("--before: want RFC3339: %w", err)
	}
	cfg, err := config.Load(*path)
	if err != nil {
		return err
	}
	if !cfg.HermesLogs.Enabled {
		return errors.New("hermeslog backfill-self: hermes_logs is not enabled in the config")
	}
	provider := ""
	for _, a := range cfg.Accounts {
		if a.ID == *account {
			provider = a.Provider
		}
	}
	if provider == "" {
		return fmt.Errorf("hermeslog backfill-self: unknown account %q", *account)
	}
	return backfillSelf(context.Background(), cfg, hermeslog.BackfillOptions{
		Models: models, Before: before.UTC(), AccountID: *account, Provider: provider, Apply: *apply,
	}, out)
}

func backfillSelf(ctx context.Context, cfg *config.Config, o hermeslog.BackfillOptions, out io.Writer) error {
	pricing, err := ledger.LoadPricing(cfg.PricingFile)
	if err != nil {
		return fmt.Errorf("pricing: %w", err)
	}
	basis := map[string]string{}
	for _, a := range cfg.CoreAccounts() {
		basis[a.ID] = a.CostBasis
	}
	ledgerPath := filepath.Join(cfg.DataDir, "localrouter.db")
	l, err := ledger.Open(ledgerPath, pricing, func(id string) string { return basis[id] })
	if err != nil {
		return err
	}
	defer l.Close()
	ids, err := sql.Open("sqlite", "file:"+(&url.URL{Path: ledgerPath}).EscapedPath()+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return err
	}
	defer ids.Close()
	o.Exists = func(ctx context.Context, id string) (bool, error) {
		var ok bool
		err := ids.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM requests WHERE id = ?)`, id).Scan(&ok)
		return ok, err
	}
	// Same wiring as the serve path, minus the state file: the backfill
	// never reads or writes the collector's state.
	c := hermeslog.New(l, hermeslog.Options{
		Home:      cfg.HermesLogs.Home,
		Accounts:  cfg.HermesLogs.Accounts,
		Client:    cfg.HermesLogs.Client,
		Host:      cfg.HostName,
		SelfHosts: hermeslog.SelfHosts(cfg.Listen),
		Logger:    slog.New(slog.NewTextHandler(os.Stderr, nil)),
	})
	res, err := c.BackfillSelf(ctx, o)
	if err != nil {
		return err
	}
	printBackfill(out, o, res, pricing)
	return nil
}

func printBackfill(out io.Writer, o hermeslog.BackfillOptions, res hermeslog.BackfillResult, pricing *ledger.Pricing) {
	mode := "dry run"
	if o.Apply {
		mode = "apply"
	}
	fmt.Fprintf(out, "hermeslog backfill-self (%s): models=%s account=%s (%s) before=%s\n",
		mode, strings.Join(o.Models, ","), o.AccountID, o.Provider, o.Before.Format(time.RFC3339))
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(tw, "PROFILE\tSESSION\tMODEL\tTASK\tLAST_SEEN\tCALLS\tINPUT\tCACHE_READ\tCACHE_WRITE\tOUTPUT\tREASONING\tEST_COST\tLEDGER\t")
	var calls, in, cr, cw, outT, rs int64
	var cost float64
	unpriced := 0
	for _, r := range res.Selected {
		u := r.Record.Usage
		calls += r.Calls
		in += u.InputTokens
		cr += u.CachedInputTokens
		cw += u.CacheCreationInputTokens
		outT += u.OutputTokens
		rs += u.ReasoningTokens
		costS := "-"
		if c, _ := ledger.ResolveRecordCost(r.Record, pricing); c != nil {
			cost += *c
			costS = fmt.Sprintf("$%.4f", *c)
		} else {
			unpriced++
		}
		state := "new"
		if r.Present {
			state = "present"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%d\t%d\t%d\t%d\t%d\t%d\t%s\t%s\t\n", r.Profile, r.Session, r.Model, r.Task,
			r.LastSeen.Format(time.RFC3339), r.Calls, u.InputTokens, u.CachedInputTokens, u.CacheCreationInputTokens,
			u.OutputTokens, u.ReasoningTokens, costS, state)
	}
	tw.Flush()
	fmt.Fprintf(out, "selected: %d rows, calls=%d input=%d (incl. cache) cache_read=%d cache_write=%d output=%d reasoning=%d est_cost=$%.2f",
		len(res.Selected), calls, in, cr, cw, outT, rs, cost)
	if unpriced > 0 {
		fmt.Fprintf(out, " (%d rows unpriced)", unpriced)
	}
	fmt.Fprintln(out)
	fmt.Fprintf(out, "already in ledger: %d\n", res.AlreadyPresent)
	if res.SkippedInvalid > 0 {
		fmt.Fprintf(out, "skipped implausible: %d\n", res.SkippedInvalid)
	}
	fmt.Fprintf(out, "excluded: active after cutoff: %d\n", len(res.Excluded))
	if len(res.Excluded) > 0 {
		tw = tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "  PROFILE\tSESSION\tMODEL\tTASK\tLAST_SEEN")
		for _, r := range res.Excluded {
			ls := "-"
			if !r.LastSeen.IsZero() {
				ls = r.LastSeen.Format(time.RFC3339)
			}
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\n", r.Profile, r.Session, r.Model, r.Task, ls)
		}
		tw.Flush()
	}
	if o.Apply {
		fmt.Fprintf(out, "recorded: %d\n", res.Recorded)
	} else {
		fmt.Fprintf(out, "recorded: 0 (dry run; pass --apply to record %d new rows)\n", len(res.Selected)-res.AlreadyPresent)
	}
}
