package identity

import (
	"encoding/base32"
	"fmt"
	"io"
)

// idEncoding renders 16 random bytes as 26 lowercase base32 characters.
var idEncoding = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

const (
	idRandomBytes     = 16
	idChars           = 26 // idEncoding.EncodedLen(idRandomBytes)
	secretRandomBytes = 32
	secretChars       = 43 // base64.RawURLEncoding.EncodedLen(secretRandomBytes)
)

// random reads n bytes from the configured source; a short read fails.
func (s *Store) random(n int) ([]byte, error) {
	b := make([]byte, n)
	if _, err := io.ReadFull(s.opts.Rand, b); err != nil {
		return nil, fmt.Errorf("identity: random source: %w", err)
	}
	return b, nil
}

// newID returns prefix + 26 lowercase base32 characters of fresh randomness.
func (s *Store) newID(prefix string) (string, error) {
	b, err := s.random(idRandomBytes)
	if err != nil {
		return "", err
	}
	return prefix + idEncoding.EncodeToString(b), nil
}
