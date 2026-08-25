// Package auth provides the bootstrap-token and session machinery for
// the local web listener.
//
// The trust-boundary intuition:
//
//   - bootstrap-token: one-time, short-lived (60s), proves the holder
//     is the user who launched the daemon (because they invoked the
//     tray menu item, which spawned `aplexica web issue-token` against
//     the same UDS socket that already gates daemon control).
//   - session-cookie: HttpOnly cookie minted on first bootstrap
//     consumption. Survives page reloads. The local daemon persists its
//     session table to disk (NewPersistentSessionStore) with a long TTL
//     (LocalSessionTTL), so the cookie also survives a daemon restart — a
//     page refresh re-authenticates without a fresh tray token. A purely
//     in-memory store (NewSessionStore) still clears on restart.
//   - csrf-cookie: JS-readable companion to the session cookie, used
//     for double-submit CSRF protection on mutating requests.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/argon2"
)

// Argon2id parameters. These are conservative for a single-machine
// trust zone: argon2 here protects only against an attacker who has
// already obtained memory read of the daemon process — at which point
// the threat model has already failed. The tuning is intentionally
// modest (memory 32MB, time 1, parallelism 4, output 32B) so token
// verification stays sub-millisecond on commodity laptops.
const (
	argonTime      uint32 = 1
	argonMemoryKiB uint32 = 32 * 1024
	argonThreads   uint8  = 4
	argonKeyLen    uint32 = 32
)

const (
	tokenLookupBytes = 16
	tokenSecretBytes = 32
)

// argonSaltBytes is the per-token salt length. 128 bits is sufficient
// here because tokens live ≤ 60s and never persist across daemon
// restarts; the salt's role is to make per-token hashes distinct, not
// to guard against offline cracking.
const argonSaltBytes = 16

// DefaultTokenTTL is the V1 lifetime of a bootstrap token from issuance
// to consumption per the spec. Re-issuing is cheap; expanding the
// window enlarges the attack surface for replay if the URL leaks
// (e.g. shell history of a shared screen).
const DefaultTokenTTL = 60 * time.Second

// storedToken is the in-memory representation of an outstanding
// bootstrap token. The raw value disappears from process memory after
// Issue returns; only the argon2id hash + salt remain until Consume
// (or SweepExpired) deletes the entry.
type storedToken struct {
	hash      []byte
	salt      []byte
	expiresAt time.Time
	attempts  uint8
	cleanup   func()
}

// TokenStore issues and consumes one-time bootstrap tokens.
//
// Concurrency: all operations are safe under a single sync.Mutex. The
// scale is "single user, occasional click" — high-throughput
// optimisations aren't worth the design complexity.
type TokenStore struct {
	ttl time.Duration
	mu  sync.Mutex
	tbl map[string]storedToken // keyed by hex of the argon2id hash
	kdf chan struct{}
}

// NewTokenStore returns an empty store. ttl is the lifetime of each
// issued token; pass DefaultTokenTTL unless tests need otherwise.
func NewTokenStore(ttl time.Duration) *TokenStore {
	if ttl <= 0 {
		ttl = DefaultTokenTTL
	}
	return &TokenStore{ttl: ttl, tbl: map[string]storedToken{}, kdf: make(chan struct{}, 1)}
}

// TTL returns the configured token lifetime. Exposed for tests and
// for tooling that wants to display "expires in N seconds" hints.
func (s *TokenStore) TTL() time.Duration { return s.ttl }

// Issue mints a new bootstrap token and stages it for one-time
// consumption. The returned urlOut is the full URL to launch the
// browser at; the returned raw is the unencoded token (also embedded
// in the URL's `bootstrap=` query parameter). Callers that build their
// own URL can ignore urlOut and use raw directly.
//
// baseURL is the bound listener's origin, e.g. "http://127.0.0.1:51234".
func (s *TokenStore) Issue(baseURL string) (urlOut, raw string, err error) {
	lookup := make([]byte, tokenLookupBytes)
	secret := make([]byte, tokenSecretBytes)
	if _, err := rand.Read(lookup); err != nil {
		return "", "", fmt.Errorf("token: read entropy: %w", err)
	}
	if _, err := rand.Read(secret); err != nil {
		return "", "", fmt.Errorf("token: read entropy: %w", err)
	}
	lookupID := base64.RawURLEncoding.EncodeToString(lookup)
	secretText := base64.RawURLEncoding.EncodeToString(secret)
	raw = lookupID + "." + secretText

	salt := make([]byte, argonSaltBytes)
	if _, err := rand.Read(salt); err != nil {
		return "", "", fmt.Errorf("token: read salt: %w", err)
	}
	hash := argon2.IDKey([]byte(secretText), salt, argonTime, argonMemoryKiB, argonThreads, argonKeyLen)

	s.mu.Lock()
	old := make([]func(), 0, len(s.tbl))
	for _, entry := range s.tbl {
		if entry.cleanup != nil {
			old = append(old, entry.cleanup)
		}
	}
	clear(s.tbl) // only the newest issued token remains valid
	s.tbl[lookupID] = storedToken{
		hash:      hash,
		salt:      salt,
		expiresAt: time.Now().Add(s.ttl),
	}
	s.mu.Unlock()
	for _, cleanup := range old {
		cleanup()
	}

	return fmt.Sprintf("%s/?bootstrap=%s", baseURL, raw), raw, nil
}

// SetCleanup associates a best-effort cleanup with an outstanding token. It
// is used by the private browser handoff to remove the token-bearing file when
// the token is consumed, expires, or is superseded. The callback is never run
// while the token-table mutex is held.
func (s *TokenStore) SetCleanup(raw string, cleanup func()) error {
	parts := strings.Split(raw, ".")
	if len(parts) != 2 || cleanup == nil {
		return ErrTokenUnknown
	}
	s.mu.Lock()
	st, ok := s.tbl[parts[0]]
	if ok {
		st.cleanup = cleanup
		s.tbl[parts[0]] = st
	}
	s.mu.Unlock()
	if !ok {
		return ErrTokenUnknown
	}
	return nil
}

// ErrTokenUnknown signals that the supplied token does not match any
// outstanding entry. Distinguishable from ErrTokenExpired so callers
// (and tests) can branch on it; both render as 401 to the HTTP client.
var ErrTokenUnknown = errors.New("auth: token unknown")

// ErrTokenExpired signals that the supplied token matched a stored
// entry whose expiresAt has passed.
var ErrTokenExpired = errors.New("auth: token expired")
var ErrTokenBusy = errors.New("auth: token verifier busy")

// Consume verifies the supplied raw token against every outstanding
// entry in constant time and, on success, deletes the entry. Returns
// nil on success.
//
// On failure the entry is left intact unless the match was for the
// supplied token specifically (which is then deleted regardless of
// expiry status to preserve single-use semantics across replay).
func (s *TokenStore) Consume(raw string) error {
	parts := strings.Split(raw, ".")
	if len(parts) != 2 {
		return ErrTokenUnknown
	}
	lookup, secret := parts[0], parts[1]
	if b, err := base64.RawURLEncoding.DecodeString(lookup); err != nil || len(b) != tokenLookupBytes {
		return ErrTokenUnknown
	}
	if b, err := base64.RawURLEncoding.DecodeString(secret); err != nil || len(b) != tokenSecretBytes {
		return ErrTokenUnknown
	}
	s.mu.Lock()
	st, ok := s.tbl[lookup]
	s.mu.Unlock()
	if !ok {
		return ErrTokenUnknown
	}
	select {
	case s.kdf <- struct{}{}:
		defer func() { <-s.kdf }()
	default:
		return ErrTokenBusy
	}
	now := time.Now()
	got := argon2.IDKey([]byte(secret), st.salt, argonTime, argonMemoryKiB, argonThreads, argonKeyLen)
	matched := subtle.ConstantTimeCompare(got, st.hash) == 1
	s.mu.Lock()
	current, exists := s.tbl[lookup]
	if !exists || subtle.ConstantTimeCompare(current.hash, st.hash) != 1 {
		s.mu.Unlock()
		return ErrTokenUnknown
	}
	var cleanup func()
	if now.After(st.expiresAt) {
		delete(s.tbl, lookup)
		cleanup = current.cleanup
		s.mu.Unlock()
		if cleanup != nil {
			cleanup()
		}
		return ErrTokenExpired
	}
	if matched {
		delete(s.tbl, lookup)
		cleanup = current.cleanup
		s.mu.Unlock()
		if cleanup != nil {
			cleanup()
		}
		return nil
	}
	current.attempts++
	if current.attempts >= 3 {
		delete(s.tbl, lookup)
		cleanup = current.cleanup
	} else {
		s.tbl[lookup] = current
	}
	s.mu.Unlock()
	if cleanup != nil {
		cleanup()
	}
	return ErrTokenUnknown
}

// SweepExpired deletes any entries whose expiresAt < now. Intended for
// periodic housekeeping from the server's lifecycle goroutine. Returns
// the number of entries removed.
func (s *TokenStore) SweepExpired(now time.Time) int {
	s.mu.Lock()
	n := 0
	var cleanup []func()
	for k, st := range s.tbl {
		if now.After(st.expiresAt) {
			delete(s.tbl, k)
			if st.cleanup != nil {
				cleanup = append(cleanup, st.cleanup)
			}
			n++
		}
	}
	s.mu.Unlock()
	for _, fn := range cleanup {
		fn()
	}
	return n
}

// Outstanding returns the number of unconsumed tokens currently in
// the store. Used by tests and by the /api/daemon endpoint to surface
// a "pending bootstrap" hint to the UI.
func (s *TokenStore) Outstanding() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.tbl)
}
