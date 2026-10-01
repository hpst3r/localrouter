package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Token is the persisted OAuth state of one Codex account.
type Token struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	IDToken      string    `json:"id_token"`
	AccountID    string    `json:"account_id"` // chatgpt_account_id
	ExpiresAt    time.Time `json:"expires_at"` // access token expiry; zero if unknown
	LastRefresh  time.Time `json:"last_refresh"`
}

// String redacts secrets so a Token can never be printed by accident.
func (t Token) String() string {
	return fmt.Sprintf("Token{account_id=%q expires_at=%s}", t.AccountID, t.ExpiresAt.Format(time.RFC3339))
}

// GoString redacts secrets for %#v.
func (t Token) GoString() string { return t.String() }

// LogValue redacts secrets in slog output.
func (t Token) LogValue() slog.Value {
	return slog.GroupValue(slog.String("account_id", t.AccountID), slog.Time("expires_at", t.ExpiresAt))
}

var accountIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// Store persists Codex tokens as <dir>/<account>.json.
type Store struct {
	dir string
}

// NewStore creates dir (mode 0700) if needed and returns a Store rooted there.
// An existing dir is tightened to 0700.
func NewStore(dir string) (*Store, error) {
	if dir == "" {
		return nil, errors.New("auth: token store dir is empty")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("auth: create token dir: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, fmt.Errorf("auth: chmod token dir: %w", err)
	}
	return &Store{dir: dir}, nil
}

// Dir returns the store directory.
func (s *Store) Dir() string { return s.dir }

func (s *Store) path(account string) (string, error) {
	if !accountIDRe.MatchString(account) {
		return "", fmt.Errorf("auth: invalid account id %q", account)
	}
	return filepath.Join(s.dir, account+".json"), nil
}

// Load reads the token for account. The error wraps os.ErrNotExist if no
// token has been stored.
func (s *Store) Load(account string) (Token, error) {
	p, err := s.path(account)
	if err != nil {
		return Token{}, err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return Token{}, fmt.Errorf("auth: read token for %s: %w", account, err)
	}
	var t Token
	if err := json.Unmarshal(b, &t); err != nil {
		// Do not wrap: json errors may quote file content.
		return Token{}, fmt.Errorf("auth: token file for %s is not valid JSON", account)
	}
	return t, nil
}

// fileStamp identifies one version of a token file on disk.
type fileStamp struct {
	exists bool
	mtime  time.Time
	size   int64
}

// stamp returns the current fileStamp of account's token file. A stat error
// other than not-exist yields a zero stamp.
func (s *Store) stamp(account string) fileStamp {
	p, err := s.path(account)
	if err != nil {
		return fileStamp{}
	}
	fi, err := os.Stat(p)
	if err != nil {
		return fileStamp{}
	}
	return fileStamp{exists: true, mtime: fi.ModTime(), size: fi.Size()}
}

// Save atomically writes the token for account: temp file in the same dir
// (0600), fsync, rename, fsync dir.
func (s *Store) Save(account string, t Token) error {
	p, err := s.path(account)
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return fmt.Errorf("auth: encode token: %w", err)
	}
	f, err := os.CreateTemp(s.dir, "."+account+".*.tmp")
	if err != nil {
		return fmt.Errorf("auth: create temp token file: %w", err)
	}
	tmp := f.Name()
	ok := false
	defer func() {
		if !ok {
			_ = f.Close()
			_ = os.Remove(tmp)
		}
	}()
	if err := f.Chmod(0o600); err != nil {
		return fmt.Errorf("auth: chmod temp token file: %w", err)
	}
	if _, err := f.Write(b); err != nil {
		return fmt.Errorf("auth: write temp token file: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("auth: fsync temp token file: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("auth: close temp token file: %w", err)
	}
	if err := os.Rename(tmp, p); err != nil {
		return fmt.Errorf("auth: rename token file: %w", err)
	}
	ok = true
	d, err := os.Open(s.dir)
	if err != nil {
		return fmt.Errorf("auth: open token dir: %w", err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("auth: fsync token dir: %w", err)
	}
	return nil
}

// Accounts lists the account ids that have a stored token.
func (s *Store) Accounts() ([]string, error) {
	ents, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("auth: list token dir: %w", err)
	}
	var ids []string
	for _, e := range ents {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		id := strings.TrimSuffix(name, ".json")
		if accountIDRe.MatchString(id) {
			ids = append(ids, id)
		}
	}
	return ids, nil
}
