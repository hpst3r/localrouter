package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"regexp"
	"time"
)

// maxCredBytes bounds a credentials read.
const maxCredBytes = 1 << 20

// keychainTimeout bounds one `security` invocation.
const keychainTimeout = 5 * time.Second

// Sanitized credential errors (never contain the token, paths, or command
// output).
var (
	ErrCredentialsUnavailable = errors.New("claude credentials: unavailable")
	ErrKeychainUnavailable    = errors.New("claude credentials: keychain item unavailable")
)

// CredentialReader returns the raw Claude Code credentials JSON (the same
// document as ~/.claude/.credentials.json). It never writes.
type CredentialReader interface {
	ReadCredentials(ctx context.Context) ([]byte, error)
}

// CommandRunner runs a command and returns its stdout.
type CommandRunner func(ctx context.Context, name string, args ...string) ([]byte, error)

// ExecRunner runs commands with os/exec, discarding stderr.
func ExecRunner(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var out bytes.Buffer
	cmd.Stdout = &limitedWriter{w: &out, n: maxCredBytes}
	err := cmd.Run()
	return out.Bytes(), err
}

type limitedWriter struct {
	w io.Writer
	n int
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if len(p) > l.n {
		return 0, errors.New("output too large")
	}
	l.n -= len(p)
	return l.w.Write(p)
}

// FileCredentials reads the credentials file read-only.
type FileCredentials struct{ Path string }

// ReadCredentials implements CredentialReader.
func (f FileCredentials) ReadCredentials(context.Context) ([]byte, error) {
	fh, err := os.Open(f.Path)
	if err != nil {
		return nil, ErrCredentialsUnavailable
	}
	defer fh.Close()
	if fi, err := fh.Stat(); err != nil || !fi.Mode().IsRegular() {
		return nil, ErrCredentialsUnavailable
	}
	b, err := io.ReadAll(io.LimitReader(fh, maxCredBytes))
	if err != nil || len(bytes.TrimSpace(b)) == 0 {
		return nil, ErrCredentialsUnavailable
	}
	return b, nil
}

// KeychainCredentials reads a macOS keychain generic password with
// `security find-generic-password -a <Account> -s <Service> -w`, matching how
// Claude Code itself reads its login (account = $USER).
type KeychainCredentials struct {
	Service string
	Account string        // "" = DefaultKeychainAccount()
	Run     CommandRunner // nil = ExecRunner
}

var keychainAccountRe = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

// DefaultKeychainAccount mirrors Claude Code: $USER (or the OS username),
// falling back to "claude-code-user" when unset or not [a-zA-Z0-9._-]+.
func DefaultKeychainAccount() string {
	name := os.Getenv("USER")
	if name == "" {
		if u, err := user.Current(); err == nil {
			name = u.Username
		}
	}
	if !keychainAccountRe.MatchString(name) {
		return "claude-code-user"
	}
	return name
}

// ReadCredentials implements CredentialReader.
func (k KeychainCredentials) ReadCredentials(ctx context.Context) ([]byte, error) {
	run := k.Run
	if run == nil {
		run = ExecRunner
	}
	ctx, cancel := context.WithTimeout(ctx, keychainTimeout)
	defer cancel()
	acct := k.Account
	if acct == "" {
		acct = DefaultKeychainAccount()
	}
	out, err := run(ctx, "security", "find-generic-password", "-a", acct, "-s", k.Service, "-w")
	out = bytes.TrimSpace(out)
	if err != nil || len(out) == 0 {
		return nil, ErrKeychainUnavailable
	}
	return out, nil
}

// firstOf tries readers in order and returns the first success.
type firstOf []CredentialReader

func (rs firstOf) ReadCredentials(ctx context.Context) ([]byte, error) {
	var last error = ErrCredentialsUnavailable
	for _, r := range rs {
		b, err := r.ReadCredentials(ctx)
		if err == nil {
			return b, nil
		}
		last = err
	}
	return nil, last
}

// NewCredentialReader builds the reader for cc on goos. Source "auto" means
// keychain then file on darwin, file elsewhere. run nil uses ExecRunner.
func NewCredentialReader(cc CredentialsConfig, goos string, run CommandRunner) (CredentialReader, error) {
	file := FileCredentials{Path: cc.File}
	kc := KeychainCredentials{Service: cc.KeychainService, Account: cc.KeychainAccount, Run: run}
	switch cc.Source {
	case SourceFile:
		return file, nil
	case SourceKeychain:
		return kc, nil
	case SourceAuto, "":
		if goos == "darwin" {
			return firstOf{kc, file}, nil
		}
		return file, nil
	default:
		return nil, fmt.Errorf("credentials.source %q invalid", cc.Source)
	}
}
