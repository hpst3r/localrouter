package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/hpst3r/localrouter/internal/ledger"
)

const ledgerUsage = `usage: localrouter ledger relabel-host [--from ""] --to NAME [-config PATH]`

// relabeler is implemented by *ledger.Ledger: it rewrites host from -> to
// in one transaction and returns the number of rows changed.
type relabeler interface {
	RelabelHost(ctx context.Context, from, to string) (int64, error)
}

func cmdLedger(args []string) error {
	if len(args) < 1 || args[0] != "relabel-host" {
		return errors.New(ledgerUsage)
	}
	fs := flag.NewFlagSet("ledger relabel-host", flag.ExitOnError)
	from := fs.String("from", "", "host value to replace (default: empty, i.e. unattributed rows)")
	to := fs.String("to", "", "new host name (required)")
	cfg, err := loadConfig(fs, args[1:])
	if err != nil {
		return err
	}
	if fs.NArg() != 0 || *to == "" {
		return errors.New(ledgerUsage)
	}
	if *from == *to {
		return errors.New("ledger relabel-host: --from and --to are the same")
	}
	l, err := ledger.Open(filepath.Join(cfg.DataDir, "localrouter.db"), nil, nil)
	if err != nil {
		return err
	}
	defer l.Close()
	return relabelHost(context.Background(), l, *from, *to, os.Stdout)
}

func relabelHost(ctx context.Context, l any, from, to string, out io.Writer) error {
	r, ok := l.(relabeler)
	if !ok {
		return errors.New("ledger relabel-host: this ledger does not support relabeling")
	}
	n, err := r.RelabelHost(ctx, from, to)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "relabeled %d rows: host %q -> %q\n", n, from, to)
	return nil
}
