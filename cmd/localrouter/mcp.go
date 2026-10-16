package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/hpst3r/localrouter/internal/mcpserver"
)

const mcpUsage = `usage: localrouter mcp [--url URL] [--key-file FILE] [--timeout 10s] [--max-result-chars 24000]

Serve the Model Context Protocol on stdio, exposing three read-only tools over a
running LocalRouter control API: quota_status, usage_summary and check_admission.

  --url               control API base URL (default http://127.0.0.1:8787).
                      Plaintext http is accepted only for a loopback host; any
                      other host must use https. Credentials, query strings,
                      fragments and paths are rejected.
  --key-file          file holding the client bearer key, needed only when the
                      server sets control.require_auth
  --timeout           per-request timeout (default 10s, maximum 5m)
  --max-result-chars  byte budget for the whole serialized tool result object
                      (default 24000, minimum 1024); the flag name is historical,
                      the bound is a conservative byte count

No router configuration, database, ledger or provider credential is read; the
only file this process opens is the client key file named by --key-file.
Diagnostics go to stderr; stdout carries JSON-RPC only.
`

// cmdMCP implements `localrouter mcp`. It deliberately parses its own flags and
// never loads the router configuration or opens the ledger: this process is a
// thin protocol adapter in front of an already-running server.
func cmdMCP(args []string) error {
	return runMCP(args, os.Stdout, os.Stderr)
}

func runMCP(args []string, stdout, stderr *os.File) error {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	fs.SetOutput(stderr)
	base := fs.String("url", mcpserver.DefaultURL, "LocalRouter control API base URL")
	keyFile := fs.String("key-file", "", "file holding the client bearer key")
	timeout := fs.Duration("timeout", mcpserver.DefaultTimeout, "per-request timeout")
	maxChars := fs.Int("max-result-chars", mcpserver.DefaultMaxResultChars, "byte budget for the serialized tool result object")
	fs.Usage = func() { fmt.Fprint(stderr, mcpUsage) }
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return errors.New("mcp: " + err.Error())
	}
	if fs.NArg() > 0 {
		return errors.New("mcp: unexpected arguments: " + strings.Join(fs.Args(), " "))
	}
	srv, err := mcpserver.New(mcpserver.Config{
		URL:            *base,
		KeyFile:        *keyFile,
		Timeout:        *timeout,
		MaxResultChars: *maxChars,
	})
	if err != nil {
		return errors.New("mcp: " + err.Error())
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return serveMCP(ctx, srv)
}

// serveMCP serves the MCP server until ctx is cancelled. A deliberate shutdown
// (SIGINT/SIGTERM here, or a host closing the stdio transport) is a clean exit,
// not a failure: context.Canceled is reported as a nil error so a supervisor
// (systemd Restart=on-failure, an MCP host's restart logic) does not read it as
// a crash. Any other error is passed through unchanged.
func serveMCP(ctx context.Context, srv *mcpserver.Server) error {
	if err := srv.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}
