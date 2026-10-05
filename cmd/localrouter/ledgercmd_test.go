package main

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type fakeRelabeler struct {
	from, to string
	n        int64
	err      error
}

func (f *fakeRelabeler) RelabelHost(_ context.Context, from, to string) (int64, error) {
	f.from, f.to = from, to
	return f.n, f.err
}

func TestRelabelHost(t *testing.T) {
	f := &fakeRelabeler{n: 42}
	var out strings.Builder
	if err := relabelHost(context.Background(), f, "", "pf3", &out); err != nil {
		t.Fatal(err)
	}
	if f.from != "" || f.to != "pf3" || !strings.Contains(out.String(), "relabeled 42 rows") {
		t.Errorf("from=%q to=%q out=%q", f.from, f.to, out.String())
	}

	f.err = errors.New("boom")
	if err := relabelHost(context.Background(), f, "a", "b", &out); err == nil {
		t.Error("expected error")
	}
	if err := relabelHost(context.Background(), struct{}{}, "", "x", &out); err == nil {
		t.Error("expected unsupported error")
	}
}

func TestCmdLedgerUsage(t *testing.T) {
	for _, args := range [][]string{nil, {"bogus"}} {
		if err := cmdLedger(args); err == nil || !strings.Contains(err.Error(), "relabel-host") {
			t.Errorf("%v: %v", args, err)
		}
	}
}
