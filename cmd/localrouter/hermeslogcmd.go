package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/hpst3r/localrouter/internal/config"
	"github.com/hpst3r/localrouter/internal/core"
	"github.com/hpst3r/localrouter/internal/hermeslog"
	"github.com/hpst3r/localrouter/internal/ledger"
)

const hermeslogUsage = `usage: localrouter hermeslog backfill-self --model M [--model M ...] --account ID --after RFC3339 --before RFC3339 [--apply [--accept-partial-sessions]] [-config PATH]`

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
	afterS := fs.String("after", "", "inclusive start of the window, RFC3339 (required)")
	beforeS := fs.String("before", "", "exclusive end of the window, RFC3339 (required)")
	apply := fs.Bool("apply", false, "record rows (default: dry run)")
	partial := fs.Bool("accept-partial-sessions", false, "with --apply, also record review rows (no first_seen)")
	path := fs.String("config", defaultConfigPath(), "config file")
	if err := fs.Parse(args[1:]); err != nil {
		return fmt.Errorf("%w\n%s", err, hermeslogUsage)
	}
	if fs.NArg() != 0 || len(models) == 0 || *account == "" || *afterS == "" || *beforeS == "" {
		return errors.New(hermeslogUsage)
	}
	after, err := time.Parse(time.RFC3339, *afterS)
	if err != nil {
		return fmt.Errorf("--after: want RFC3339: %w", err)
	}
	before, err := time.Parse(time.RFC3339, *beforeS)
	if err != nil {
		return fmt.Errorf("--before: want RFC3339: %w", err)
	}
	if !after.Before(before) {
		return fmt.Errorf("--after must be before --before\n%s", hermeslogUsage)
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
		Models: models, After: after.UTC(), Before: before.UTC(), AccountID: *account, Provider: provider,
		Apply: *apply, AcceptPartialSessions: *partial,
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
	// A dry run never opens the ledger for writing (opening creates and
	// migrates it); it only reads which IDs are present, if the ledger exists.
	var rec core.Ledger = dryRunLedger{}
	if o.Apply {
		l, err := ledger.Open(ledgerPath, pricing, func(id string) string { return basis[id] })
		if err != nil {
			return err
		}
		defer l.Close()
		rec = l
	}
	if _, err := os.Stat(ledgerPath); err == nil {
		ids, err := sql.Open("sqlite", "file:"+(&url.URL{Path: ledgerPath}).EscapedPath()+"?mode=ro&_pragma=busy_timeout(5000)&_pragma=query_only(1)")
		if err != nil {
			return err
		}
		defer ids.Close()
		// The key is backfilled if its ID is present, or if the first backfill
		// (whose IDs hashed the counters) wrote a row with the same labels.
		o.Exists = func(ctx context.Context, r core.RequestRecord) (bool, error) {
			var ok bool
			err := ids.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM requests WHERE id = ?
  OR (substr(id, 1, ?) = ? AND substr(id, 1, ?) <> ?
      AND client = ? AND session = ? AND model = ? AND agent = ? AND task = ?))`,
				r.ID, len(hermeslog.BackfillIDPrefix), hermeslog.BackfillIDPrefix,
				len(hermeslog.BackfillKeyIDPrefix), hermeslog.BackfillKeyIDPrefix,
				r.Client, r.Session, r.Model, r.Agent, r.Task).Scan(&ok)
			return ok, err
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return errors.New("hermeslog backfill-self: cannot stat the ledger")
	}
	// Same wiring as the serve path, minus the state file: the backfill
	// never reads or writes the collector's state.
	c := hermeslog.New(rec, hermeslog.Options{
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

// dryRunLedger is the collector's ledger in a dry run, which never records.
type dryRunLedger struct{}

func (dryRunLedger) Record(context.Context, core.RequestRecord) error {
	return errors.New("hermeslog backfill-self: dry run does not record")
}

func (dryRunLedger) Summary(context.Context, time.Time, string) ([]core.UsageRow, error) {
	return nil, nil
}

func (dryRunLedger) Close() error { return nil }

func printBackfill(out io.Writer, o hermeslog.BackfillOptions, res hermeslog.BackfillResult, pricing *ledger.Pricing) {
	mode := "dry run"
	if o.Apply {
		mode = "apply"
	}
	fmt.Fprintf(out, "hermeslog backfill-self (%s): models=%s account=%s (%s) after=%s before=%s\n",
		mode, strings.Join(o.Models, ","), o.AccountID, o.Provider, o.After.Format(time.RFC3339), o.Before.Format(time.RFC3339))
	newSel := printBackfillRows(out, "selected", res.Selected, pricing)
	newRev := 0
	if len(res.Review) > 0 {
		fmt.Fprintf(out, "review: %d rows have no first_seen; WARNING: their counters include any calls in the same session before --after\n", len(res.Review))
		newRev = printBackfillRows(out, "review", res.Review, pricing)
		if !o.AcceptPartialSessions {
			fmt.Fprintln(out, "review rows are recorded only with --apply --accept-partial-sessions")
		}
	}
	fmt.Fprintf(out, "already in ledger: %d\n", res.AlreadyPresent)
	if res.SkippedInvalid > 0 {
		fmt.Fprintf(out, "skipped implausible: %d\n", res.SkippedInvalid)
	}
	fmt.Fprintf(out, "excluded: outside the window: %d\n", len(res.Excluded))
	if len(res.Excluded) > 0 {
		tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "  PROFILE\tSESSION\tMODEL\tTASK\tFIRST_SEEN\tLAST_SEEN\tREASON")
		for _, r := range res.Excluded {
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\t%s\t%s\n", r.Profile, r.Session, r.Model, r.Task, fmtTime(r.FirstSeen), fmtTime(r.LastSeen), r.Reason)
		}
		tw.Flush()
	}
	if !o.AcceptPartialSessions {
		newRev = 0
	}
	if o.Apply {
		fmt.Fprintf(out, "recorded: %d\n", res.Recorded)
	} else {
		fmt.Fprintf(out, "recorded: 0 (dry run; pass --apply to record %d new rows)\n", newSel+newRev)
	}
}

// printBackfillRows prints one table of backfill rows and its totals line,
// and returns how many rows are new (not already in the ledger).
func printBackfillRows(out io.Writer, name string, rows []hermeslog.BackfillRow, pricing *ledger.Pricing) int {
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(tw, "PROFILE\tSESSION\tMODEL\tTASK\tFIRST_SEEN\tLAST_SEEN\tCALLS\tINPUT\tCACHE_READ\tCACHE_WRITE\tOUTPUT\tREASONING\tEST_COST\tLEDGER\t")
	var calls, in, cr, cw, outT, rs int64
	var cost float64
	unpriced, fresh := 0, 0
	for _, r := range rows {
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
		} else {
			fresh++
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%d\t%d\t%d\t%d\t%d\t%d\t%s\t%s\t\n", r.Profile, r.Session, r.Model, r.Task,
			fmtTime(r.FirstSeen), fmtTime(r.LastSeen), r.Calls, u.InputTokens, u.CachedInputTokens, u.CacheCreationInputTokens,
			u.OutputTokens, u.ReasoningTokens, costS, state)
	}
	tw.Flush()
	fmt.Fprintf(out, "%s: %d rows, calls=%d input=%d (incl. cache) cache_read=%d cache_write=%d output=%d reasoning=%d est_cost=$%.2f",
		name, len(rows), calls, in, cr, cw, outT, rs, cost)
	if unpriced > 0 {
		fmt.Fprintf(out, " (%d rows unpriced)", unpriced)
	}
	fmt.Fprintln(out)
	return fresh
}

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Format(time.RFC3339)
}
