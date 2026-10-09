package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"os"
	"sort"
	"strings"
)

// MinClientKeyLen is the minimum accepted client key length.
const MinClientKeyLen = 16

// ClientKeys authenticates downstream clients by bearer key.
type ClientKeys struct {
	entries []clientKey
}

type clientKey struct {
	name   string
	digest [sha256.Size]byte
}

// LoadClientKeys reads one key per client from files (client name -> key
// file path). Each file must not be group/world accessible and must contain a
// key of at least MinClientKeyLen characters after trimming whitespace.
// Duplicate keys across clients are rejected.
//
// LoadClientKeys is the legacy single-key form; LoadClientKeyFiles is the
// rotation-aware form that accepts a list of active keys per client and
// delegates here to preserve identical validation and duplicate semantics.
func LoadClientKeys(files map[string]string) (*ClientKeys, error) {
	single := make(map[string][]string, len(files))
	for name, path := range files {
		single[name] = []string{path}
	}
	return LoadClientKeyFiles(single)
}

// LoadClientKeyFiles reads one or more keys per client (client name -> list
// of key file paths). A client may hold several active keys at once so a key
// can be rotated without downtime: every listed key authenticates as that
// client. Each file must not be group/world accessible and must contain a key
// of at least MinClientKeyLen characters after trimming whitespace, and each
// file must hold exactly one raw key.
//
// A client with no key files is rejected, as is any duplicate key digest:
// both across clients (ambiguous) and within a single client (a repeated key
// adds no authentication value).
func LoadClientKeyFiles(files map[string][]string) (*ClientKeys, error) {
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	ck := &ClientKeys{}
	seen := map[[sha256.Size]byte]string{}
	for _, name := range names {
		paths := files[name]
		if len(paths) == 0 {
			return nil, fmt.Errorf("auth: client %s: at least one key file is required", name)
		}
		for _, path := range paths {
			fi, err := os.Stat(path)
			if err != nil {
				return nil, fmt.Errorf("auth: client %s: key file: %w", name, err)
			}
			if fi.Mode().Perm()&0o077 != 0 {
				return nil, fmt.Errorf("auth: client %s: key file %s has mode %#o; must not be group/world accessible (chmod 600)",
					name, path, fi.Mode().Perm())
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return nil, fmt.Errorf("auth: client %s: key file: %w", name, err)
			}
			key := strings.TrimSpace(string(b))
			if len(key) < MinClientKeyLen {
				return nil, fmt.Errorf("auth: client %s: key in %s is empty or shorter than %d characters", name, path, MinClientKeyLen)
			}
			d := sha256.Sum256([]byte(key))
			if prev, dup := seen[d]; dup {
				if prev == name {
					return nil, fmt.Errorf("auth: client %s lists the same key more than once", name)
				}
				return nil, fmt.Errorf("auth: clients %s and %s share the same key", prev, name)
			}
			seen[d] = name
			ck.entries = append(ck.entries, clientKey{name: name, digest: d})
		}
	}
	return ck, nil
}

// Lookup returns the client name owning bearer. Every configured key is
// compared (over fixed-length SHA-256 digests, with crypto/subtle) so timing
// does not reveal which or how much of a key matched.
func (c *ClientKeys) Lookup(bearer string) (name string, ok bool) {
	if c == nil || bearer == "" {
		return "", false
	}
	d := sha256.Sum256([]byte(bearer))
	match := -1
	for i := range c.entries {
		if subtle.ConstantTimeCompare(d[:], c.entries[i].digest[:]) == 1 {
			match = i
		}
	}
	if match < 0 {
		return "", false
	}
	return c.entries[match].name, true
}

// GenerateKey returns a new client key: "lr-" + base64url(32 random bytes).
func GenerateKey() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("auth: generate key: %w", err)
	}
	return "lr-" + base64.RawURLEncoding.EncodeToString(b[:]), nil
}
