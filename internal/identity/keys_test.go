package identity

import (
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
)

func TestCreateKeyReturnsTokenOnceAndStoresOnlyDigest(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	u := f.user("sub-keys", core.RoleUser)

	k, err := f.s.CreateKey(ctx, u.ID, "laptop", 0)
	if err != nil {
		t.Fatalf("CreateKey = %v", err)
	}
	if !IsUserKeyToken(k.Token) {
		t.Fatalf("token %q does not match the user key grammar", k.Token)
	}
	if !strings.HasPrefix(k.ID, "k_") || !strings.HasPrefix(k.Token, "lrk_"+strings.TrimPrefix(k.ID, "k_")+"_") || len(k.Token) != 4+26+1+43 {
		t.Fatalf("token %q does not embed key id %q in lrk_<id>_<secret>", k.Token, k.ID)
	}
	now := f.clock.Now()
	if k.UserID != u.ID || k.Name != "laptop" || !k.CreatedAt.Equal(now) || !k.ExpiresAt.Equal(now.Add(MaxKeyTTL)) {
		t.Fatalf("key metadata = %+v", k.APIKey)
	}

	keys, err := f.s.ListKeys(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 || keys[0] != k.APIKey {
		t.Fatalf("ListKeys = %+v, want [%+v]", keys, k.APIKey)
	}

	secret := k.Token[len(k.Token)-43:]
	var hash []byte
	var name string
	if err := f.s.db.QueryRow(`SELECT token_hash, name FROM api_keys WHERE id = ?`, k.ID).Scan(&hash, &name); err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256([]byte(k.Token))
	if string(hash) != string(want[:]) {
		t.Fatal("stored digest is not SHA-256 of the full token")
	}
	assertNoValueInTables(t, f.s, k.Token, secret)
}

func TestCreateKeyTTL(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	u := f.user("sub-ttl", core.RoleUser)
	k, err := f.s.CreateKey(ctx, u.ID, "short", 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !k.ExpiresAt.Equal(f.clock.Now().Add(24 * time.Hour)) {
		t.Fatalf("ExpiresAt = %v", k.ExpiresAt)
	}
	for _, ttl := range []time.Duration{-time.Hour, MaxKeyTTL + time.Second, time.Second} {
		if _, err := f.s.CreateKey(ctx, u.ID, "bad", ttl); !errors.Is(err, ErrInvalid) {
			t.Errorf("CreateKey ttl %v = %v, want ErrInvalid", ttl, err)
		}
	}
}

func TestCreateKeyValidatesName(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	u := f.user("sub-names", core.RoleUser)
	const marker = "SECRETLABEL"
	for name, label := range map[string]string{
		"empty":        "",
		"blank":        "   ",
		"too long":     marker + strings.Repeat("x", MaxKeyNameBytes),
		"invalid utf8": marker + "\xff",
		"control":      marker + "\x1b[31m",
	} {
		_, err := f.s.CreateKey(ctx, u.ID, label, 0)
		if !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: CreateKey = %v, want ErrInvalid", name, err)
			continue
		}
		if strings.Contains(err.Error(), marker) {
			t.Errorf("%s: error %q echoes the key name", name, err)
		}
	}
	if _, err := f.s.CreateKey(ctx, u.ID, "ünïcode ok – 64 bytes max", 0); err != nil {
		t.Fatalf("valid unicode name rejected: %v", err)
	}
}

func TestCreateKeyLimitCountsLiveKeysOnly(t *testing.T) {
	f := newFixture(t)
	f.opts.MaxKeysPerUser = 2
	s := f.second()
	ctx := context.Background()
	u := f.user("sub-limit", core.RoleUser)
	if _, err := s.CreateKey(ctx, u.ID, "a", time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateKey(ctx, u.ID, "b", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateKey(ctx, u.ID, "c", 0); !errors.Is(err, ErrKeyLimit) {
		t.Fatalf("third key = %v, want ErrKeyLimit", err)
	}
	f.clock.Advance(time.Hour) // key "a" expires
	if _, err := s.CreateKey(ctx, u.ID, "c", 0); err != nil {
		t.Fatalf("key after expiry freed a slot = %v", err)
	}
}

func TestCreateKeyRequiresActiveRecentlyLoggedInUser(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.s.CreateKey(ctx, "u_doesnotexist", "x", 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown user = %v, want ErrNotFound", err)
	}
	u := f.user("sub-stale", core.RoleUser)
	f.clock.Advance(MaxLoginAge + time.Millisecond)
	if _, err := f.s.CreateKey(ctx, u.ID, "x", 0); !errors.Is(err, ErrStaleLogin) {
		t.Fatalf("CreateKey after 30d without login = %v, want ErrStaleLogin", err)
	}
}

func TestIsUserKeyTokenGrammar(t *testing.T) {
	f := newFixture(t)
	u := f.user("sub-grammar", core.RoleUser)
	k, err := f.s.CreateKey(context.Background(), u.ID, "g", 0)
	if err != nil {
		t.Fatal(err)
	}
	tok := k.Token
	bad := []string{
		"",
		"lr-" + strings.Repeat("A", 43),         // static client key format
		strings.Replace(tok, "lrk_", "LRK_", 1), // prefix case
		tok + "A",                               // too long
		tok[:len(tok)-1],                        // too short
		strings.Replace(tok, "_", "-", 2),       // separators
		"lrk_" + strings.ToUpper(tok[4:30]) + tok[30:], // id charset (uppercase)
		tok[:4] + "0" + tok[5:],                        // '0' outside base32 alphabet
		tok[:31] + "+" + tok[32:],                      // '+' outside base64url
		tok[:len(tok)-1] + "\xff",                      // invalid UTF-8
		" " + tok,                                      // whitespace
	}
	for _, b := range bad {
		if IsUserKeyToken(b) {
			t.Errorf("IsUserKeyToken(%q) = true", b)
		}
	}
}

// assertNoValueInTables fails if any TEXT or BLOB cell of any table contains
// one of the given values (raw or as hex).
func assertNoValueInTables(t *testing.T, s *Store, values ...string) {
	t.Helper()
	rows, err := s.db.Query(`SELECT name FROM sqlite_master WHERE type = 'table'`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, n)
	}
	rows.Close()
	for _, table := range tables {
		r, err := s.db.Query(`SELECT * FROM "` + table + `"`)
		if err != nil {
			t.Fatal(err)
		}
		cols, _ := r.Columns()
		for r.Next() {
			cells := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range cells {
				ptrs[i] = &cells[i]
			}
			if err := r.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			for i, c := range cells {
				var text string
				switch v := c.(type) {
				case string:
					text = v
				case []byte:
					text = string(v)
				default:
					continue
				}
				for _, val := range values {
					if val != "" && strings.Contains(text, val) {
						t.Errorf("table %s column %s contains a value that must not be stored", table, cols[i])
					}
				}
			}
		}
		r.Close()
	}
}
