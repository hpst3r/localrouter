package weblogin

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"sync"
	"time"
)

// Pending authorization flows are stateless. GET /auth/login seals the
// flow's state, nonce, PKCE verifier, creation and expiry time into the
// browser's __Host-lr_login cookie with AES-256-GCM under a key drawn at
// process start; the callback opens it. The server keeps nothing for a flow
// nobody redeemed, so anonymous logins cannot exhaust anything, from one
// address or many. The cookie binds the flow to the browser: a callback whose
// state differs from the sealed state is refused as login CSRF. Flows sealed
// by an earlier process no longer open after a restart; the user logs in
// again.
//
// Sealed layout (fixed size, so nothing is parsed before authentication):
//
//	cookie = base64url-nopad(version || GCM nonce (12) || ciphertext || tag (16))
//	plain  = state (32) || nonce (32) || verifier (32) || created (8) || expires (8)
//
// The AAD is the version byte, a fixed label and PublicBaseURL. Times are
// big-endian Unix nanoseconds. GCM nonces are random (NewGCMWithRandomNonce):
// a key is never used anywhere near the 2^32 seals that would make a
// collision plausible, and it dies with the process.
const (
	flowVersion   = 1
	flowRandLen   = 32
	flowPlainLen  = 3*flowRandLen + 2*8
	flowSealedLen = 1 + 12 + flowPlainLen + 16
)

// flowCookieLen is the exact length of a login cookie value.
var flowCookieLen = base64.RawURLEncoding.EncodedLen(flowSealedLen)

const flowLabel = "localrouter weblogin login flow\x00"

// flow is one authorization request. state, nonce and verifier are 43
// base64url characters (32 random bytes) each.
type flow struct {
	state    string
	nonce    string
	verifier string // PKCE code_verifier
	created  time.Time
	expires  time.Time
}

// flowSealer seals and opens flows under one process-random key.
type flowSealer struct {
	aead cipher.AEAD
	aad  []byte
}

func newFlowSealer(publicBaseURL string) *flowSealer {
	key := make([]byte, 32)
	rand.Read(key) // never returns an error; crashes the program on failure
	return newFlowSealerKey(key, publicBaseURL)
}

func newFlowSealerKey(key []byte, publicBaseURL string) *flowSealer {
	block, err := aes.NewCipher(key)
	if err != nil {
		panic("weblogin: aes: " + err.Error()) // a 32-byte key is always valid
	}
	aead, err := cipher.NewGCMWithRandomNonce(block)
	if err != nil {
		panic("weblogin: gcm: " + err.Error())
	}
	aad := append([]byte{flowVersion}, flowLabel...)
	return &flowSealer{aead: aead, aad: append(aad, publicBaseURL...)}
}

// newFlow starts a flow at now that expires after ttl and returns it with
// its sealed cookie value.
func (s *flowSealer) newFlow(now time.Time, ttl time.Duration) (flow, string) {
	plain := make([]byte, flowPlainLen)
	rand.Read(plain[:3*flowRandLen])
	f := flow{
		state:    b64(plain[0:32]),
		nonce:    b64(plain[32:64]),
		verifier: b64(plain[64:96]),
		created:  now,
		expires:  now.Add(ttl),
	}
	binary.BigEndian.PutUint64(plain[96:104], uint64(f.created.UnixNano()))
	binary.BigEndian.PutUint64(plain[104:112], uint64(f.expires.UnixNano()))
	sealed := s.aead.Seal([]byte{flowVersion}, nil, plain, s.aad)
	return f, base64.RawURLEncoding.EncodeToString(sealed)
}

// open authenticates and decodes a cookie value. Anything but an exact,
// canonical, authentic sealed flow of this version fails.
func (s *flowSealer) open(value string) (flow, bool) {
	if len(value) != flowCookieLen {
		return flow{}, false
	}
	sealed, err := base64.RawURLEncoding.Strict().DecodeString(value)
	if err != nil || len(sealed) != flowSealedLen || sealed[0] != flowVersion {
		return flow{}, false
	}
	plain, err := s.aead.Open(nil, nil, sealed[1:], s.aad)
	if err != nil || len(plain) != flowPlainLen {
		return flow{}, false
	}
	return flow{
		state:    b64(plain[0:32]),
		nonce:    b64(plain[32:64]),
		verifier: b64(plain[64:96]),
		created:  time.Unix(0, int64(binary.BigEndian.Uint64(plain[96:104]))),
		expires:  time.Unix(0, int64(binary.BigEndian.Uint64(plain[104:112]))),
	}, true
}

// redeemTable remembers callback states (by SHA-256) so that each state is
// exchanged, and reaches a hook, at most once. A callback enters its state
// before the token exchange while holding an exchange slot, so in-flight
// entries never exceed MaxConcurrentExchanges. A failed exchange or ID token
// verification removes the entry: anonymous callbacks with bogus codes leave
// nothing behind. Once the IdP returned an ID token signed for the flow's
// nonce the entry is redeemed and kept until the flow expires; only
// IdP-authenticated logins, admitted or denied, can fill the table. A full
// table refuses new callbacks (503) instead of forgetting a live state.
type redeemTable struct {
	mu  sync.Mutex
	max int
	m   map[[32]byte]redeemEntry
}

type redeemEntry struct {
	expires  time.Time
	redeemed bool
}

type beginResult int

const (
	redeemBegun beginResult = iota
	redeemReplay
	redeemFull
)

func newRedeemTable(max int) *redeemTable {
	return &redeemTable{max: max, m: make(map[[32]byte]redeemEntry)}
}

// begin enters state as in flight unless it is already present (a replay or
// a concurrent callback) or the table is full of live entries.
func (t *redeemTable) begin(state string, expires, now time.Time) beginResult {
	k := hashToken(state)
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.m[k]; ok {
		return redeemReplay
	}
	if len(t.m) >= t.max {
		// Sweep only when full: the scan is O(max), and only verified
		// logins can make the table full.
		for k, e := range t.m {
			if e.redeemed && now.After(e.expires) {
				delete(t.m, k)
			}
		}
		if len(t.m) >= t.max {
			return redeemFull
		}
	}
	t.m[k] = redeemEntry{expires: expires}
	return redeemBegun
}

// finish ends an in-flight entry: redeemed keeps it until it expires,
// otherwise it is removed.
func (t *redeemTable) finish(state string, redeemed bool) {
	k := hashToken(state)
	t.mu.Lock()
	defer t.mu.Unlock()
	if redeemed {
		e := t.m[k]
		e.redeemed = true
		t.m[k] = e
	} else {
		delete(t.m, k)
	}
}

func (t *redeemTable) len() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.m)
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func hashToken(v string) [32]byte { return sha256.Sum256([]byte(v)) }
