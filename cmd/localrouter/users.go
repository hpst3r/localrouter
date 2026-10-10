package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/hpst3r/localrouter/internal/config"
	"github.com/hpst3r/localrouter/internal/identity"
)

const usersUsage = `usage: localrouter users list    [-config PATH] [-after USER_ID] [-limit N]
       localrouter users disable [-config PATH] USER_ID
       localrouter users enable  [-config PATH] USER_ID
       localrouter users delete  [-config PATH] --yes USER_ID
       localrouter users keys    [-config PATH] USER_ID
       localrouter users revoke  [-config PATH] USER_ID KEY_ID`

// usersArgs is the number of positional arguments each subcommand takes.
var usersArgs = map[string]int{
	"list": 0, "disable": 1, "enable": 1, "delete": 1, "keys": 1, "revoke": 2,
}

func cmdUsers(args []string) error { return runUsers(args, os.Stdout) }

// runUsers is the local break-glass administration of the identity database.
// It works on the database file directly (no server or IdP involved), may run
// while the server is up (every identity write is its own transaction and
// the server reads fresh state on each request) and acts as identity.ActorCLI.
// Operations name one explicit user; there are no bulk forms.
func runUsers(args []string, out io.Writer) error {
	if len(args) < 1 {
		return errors.New(usersUsage)
	}
	sub := args[0]
	if sub == "-h" || sub == "--help" || sub == "help" {
		fmt.Fprintln(out, usersUsage)
		return nil
	}
	nargs, ok := usersArgs[sub]
	if !ok {
		return errors.New(usersUsage)
	}
	fs := flag.NewFlagSet("users "+sub, flag.ContinueOnError)
	var (
		after string
		limit int
		yes   bool
	)
	switch sub {
	case "list":
		fs.StringVar(&after, "after", "", "list users after this user ID (next page)")
		fs.IntVar(&limit, "limit", identity.MaxListLimit, "maximum users to list (1-200)")
	case "delete":
		fs.BoolVar(&yes, "yes", false, "confirm the irreversible deletion")
	}
	cfg, err := loadConfig(fs, args[1:])
	if errors.Is(err, flag.ErrHelp) {
		return nil // flag already printed the subcommand's flags
	}
	if err != nil {
		return err
	}
	if fs.NArg() != nargs {
		return errors.New(usersUsage)
	}
	if sub == "delete" && !yes {
		return errors.New("users delete is irreversible (profile scrubbed, keys revoked, sign-in blocked for good); rerun with --yes before the user ID to confirm")
	}
	if cfg.Identity == nil {
		return errors.New("users: this command requires the identity block (multi-user mode); the config is single-user")
	}
	if err := checkFiles(fs, cfg); err != nil {
		return err
	}
	if sub == "list" && (limit < 1 || limit > identity.MaxListLimit) {
		return fmt.Errorf("users list: -limit must be between 1 and %d", identity.MaxListLimit)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	st, err := openExistingIdentity(ctx, identityDBPath(cfg), identityOptions(cfg))
	if err != nil {
		return err
	}
	defer st.Close()
	switch sub {
	case "list":
		return usersList(ctx, st, after, limit, out)
	case "disable":
		return usersDisable(ctx, st, fs.Arg(0), out)
	case "enable":
		return usersEnable(ctx, st, fs.Arg(0), out)
	case "delete":
		return usersDelete(ctx, st, fs.Arg(0), out)
	case "keys":
		return usersKeys(ctx, st, fs.Arg(0), time.Now(), out)
	default: // revoke
		return usersRevoke(ctx, st, fs.Arg(0), fs.Arg(1), out)
	}
}

// identityDBPath is where the server keeps the identity database.
func identityDBPath(cfg *config.Config) string {
	return filepath.Join(cfg.DataDir, "identity.db")
}

// identityOptions maps the identity block onto identity.Options. Issuer and
// client ID must be exactly the server's: they are pinned in the database and
// a mismatch is refused. The lifetimes only bound key and session creation,
// which the CLI never does, but they are validated by identity.Open.
func identityOptions(cfg *config.Config) identity.Options {
	id := cfg.Identity
	return identity.Options{
		Issuer:             id.OIDC.Issuer,
		ClientID:           id.OIDC.ClientID,
		KeyMaxTTL:          id.Keys.MaxTTL.D(),
		LoginMaxAge:        id.Keys.RequireLoginWithin.D(),
		MaxKeysPerUser:     id.Keys.MaxPerUser,
		SessionIdleTTL:     id.Session.IdleTTL.D(),
		SessionAbsoluteTTL: id.Session.AbsoluteTTL.D(),
	}
}

// openExistingIdentity opens the identity database the server initialized.
// identity.Open creates and pins a missing database, so the CLI checks first:
// administering an empty database (a data_dir typo, or before the first
// server start) would silently succeed against nothing.
func openExistingIdentity(ctx context.Context, path string, opts identity.Options) (*identity.Store, error) {
	fi, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("identity database %s does not exist; start the server with the identity block configured first", path)
	}
	if err != nil {
		return nil, fmt.Errorf("identity database: %w", err)
	}
	if fi.Mode().IsRegular() && fi.Size() == 0 {
		return nil, fmt.Errorf("identity database %s is not initialized; start the server with the identity block configured first", path)
	}
	// Symlinks and other non-regular files are refused by identity.Open.
	return identity.Open(ctx, path, opts)
}

// usersList prints one page of users. It shows profile data (email, display
// name) to the local operator who asked for it, and nothing secret: no
// subjects, issuer, key material or session data exist on identity.User.
func usersList(ctx context.Context, st *identity.Store, after string, limit int, out io.Writer) error {
	if limit <= 0 || limit > identity.MaxListLimit {
		limit = identity.MaxListLimit
	}
	users, err := st.ListUsers(ctx, after, limit)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tSTATUS\tROLE\tLAST LOGIN\tCREATED\tEMAIL\tNAME")
	for _, u := range users {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", u.ID, u.Status, u.Role,
			fmtTime(u.LastLoginAt), fmtTime(u.CreatedAt), orDash(u.Email), orDash(u.DisplayName))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if len(users) == limit {
		fmt.Fprintf(out, "more users may follow: localrouter users list -after %s\n", users[len(users)-1].ID)
	}
	return nil
}

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.UTC().Format(time.RFC3339)
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// cliActor is the break-glass actor: it may act on any user, including the
// only admin, which the browser admin API refuses for self-service.
var cliActor = identity.Actor{Kind: identity.ActorCLI}

func usersDisable(ctx context.Context, st *identity.Store, id string, out io.Writer) error {
	if err := checkUserID(id); err != nil {
		return err
	}
	if err := st.DisableUser(ctx, cliActor, id); err != nil {
		return userErr(id, err)
	}
	fmt.Fprintf(out, "user %s disabled; its API keys and sessions are revoked\n", id)
	return nil
}

func usersEnable(ctx context.Context, st *identity.Store, id string, out io.Writer) error {
	if err := checkUserID(id); err != nil {
		return err
	}
	if err := st.EnableUser(ctx, cliActor, id); err != nil {
		return userErr(id, err)
	}
	fmt.Fprintf(out, "user %s enabled; revoked keys and sessions stay revoked until the user signs in again\n", id)
	return nil
}

// usersDelete is the destructive, irreversible operation: profile data and
// key names are scrubbed and the subject is tombstoned so it can never be
// provisioned again. runUsers requires --yes before calling it.
func usersDelete(ctx context.Context, st *identity.Store, id string, out io.Writer) error {
	if err := checkUserID(id); err != nil {
		return err
	}
	if err := st.DeleteUser(ctx, cliActor, id); err != nil {
		return userErr(id, err)
	}
	fmt.Fprintf(out, "user %s deleted; its keys and sessions are revoked and it cannot sign in again\n", id)
	return nil
}

// usersKeys prints a user's API key metadata (newest first, at most
// identity.MaxListLimit). Key tokens and digests are never readable.
func usersKeys(ctx context.Context, st *identity.Store, uid string, now time.Time, out io.Writer) error {
	if err := checkUserID(uid); err != nil {
		return err
	}
	// ListKeys of an unknown user is simply empty; look the user up first so
	// a typo is reported as such.
	if _, err := st.User(ctx, uid); err != nil {
		return userErr(uid, err)
	}
	keys, err := st.ListKeys(ctx, uid)
	if err != nil {
		return err
	}
	if len(keys) == 0 {
		fmt.Fprintf(out, "user %s has no API keys\n", uid)
		return nil
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tSTATUS\tCREATED\tEXPIRES\tNAME")
	for _, k := range keys {
		status := "active"
		switch {
		case !k.RevokedAt.IsZero():
			status = "revoked (" + k.RevokeReason + ")"
		case !now.Before(k.ExpiresAt):
			status = "expired"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", k.ID, status,
			fmtTime(k.CreatedAt), fmtTime(k.ExpiresAt), orDash(k.Name))
	}
	return tw.Flush()
}

// usersRevoke revokes one key owned by uid. Revoking an already revoked key
// is a no-op.
func usersRevoke(ctx context.Context, st *identity.Store, uid, kid string, out io.Writer) error {
	if err := checkUserID(uid); err != nil {
		return err
	}
	if !validID("k_", kid) {
		return errors.New("invalid key id: expected k_ followed by 26 lowercase base32 characters (see `localrouter users keys`)")
	}
	if _, err := st.User(ctx, uid); err != nil {
		return userErr(uid, err)
	}
	err := st.RevokeKey(ctx, cliActor, uid, kid)
	if errors.Is(err, identity.ErrNotFound) {
		return fmt.Errorf("key %s not found for user %s", kid, uid)
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "key %s of user %s revoked\n", kid, uid)
	return nil
}

// userErr turns the identity store's lifecycle errors into operator messages.
// id has already been validated, so echoing it is safe.
func userErr(id string, err error) error {
	switch {
	case errors.Is(err, identity.ErrNotFound):
		return fmt.Errorf("user %s not found", id)
	case errors.Is(err, identity.ErrUserDeleted):
		return fmt.Errorf("user %s is deleted", id)
	default:
		return err
	}
}

// Identity ids are a fixed prefix plus 26 lowercase base32 characters.
// Malformed input is rejected before any query and never echoed, since it
// may carry terminal control sequences.
const idChars = 26

func validID(prefix, s string) bool {
	if len(s) != len(prefix)+idChars || !strings.HasPrefix(s, prefix) {
		return false
	}
	for _, c := range s[len(prefix):] {
		if (c < 'a' || c > 'z') && (c < '2' || c > '7') {
			return false
		}
	}
	return true
}

func checkUserID(id string) error {
	if !validID("u_", id) {
		return errors.New("invalid user id: expected u_ followed by 26 lowercase base32 characters (see `localrouter users list`)")
	}
	return nil
}
