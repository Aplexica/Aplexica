package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"sync"
	"time"

	"github.com/aplexica/aplexica/internal/atomicfile"
)

// DefaultSessionTTL is the per-session lifetime per the spec. 24
// hours is long enough that a user who leaves a tab open across a
// lunch break doesn't get bounced; short enough that a forgotten
// laptop session expires within a calendar day.
const DefaultSessionTTL = 24 * time.Hour

// LocalSessionTTL is the session lifetime used by the local, loopback-only,
// single-user web UI. It is deliberately long (~10 years) so a daemon
// restart never forces the user to re-bootstrap a fresh token from the
// tray: the session table is persisted to disk (see
// NewPersistentSessionStore) and reloaded on startup, and the cookie
// outlives any restart. The listener binds 127.0.0.1 and serves a single
// OS user, so a long-lived HttpOnly session is an acceptable trade for not
// reauthenticating on every restart. (Browsers cap the cookie's own
// Max-Age to ~400 days, so the practical worst case is one re-bootstrap a
// year — and the server-side session still survives restarts within it.)
const LocalSessionTTL = 10 * 365 * 24 * time.Hour

// sessionIDBytes is the entropy width of a session ID. 256 bits in
// base64url is 43 characters; the cookie value comfortably fits the
// browser's 4 KiB per-cookie ceiling.
const sessionIDBytes = 32

// csrfTokenBytes is the entropy width of the CSRF token. 128 bits is
// adequate for double-submit because the attacker must also predict
// the session ID; we don't need full 256-bit unguessability here.
const csrfTokenBytes = 16

// Session holds the per-session state stored server-side.
//
// V1 has a single OS user, so User is always "local"; the field
// exists to keep the shape stable for cloud-enabled variants which
// will carry the Cognito subject.
//
// ID is the SHA-256 hash (base64url) of the raw session ID, NOT the raw
// value the browser carries in its cookie. The raw ID is emitted once by
// Create and never stored: the in-memory map is keyed by the hash and the
// persisted record carries the hash, so an at-rest read of the session
// file does not directly yield replayable bearer cookies. This mirrors how
// bootstrap tokens persist only their argon2id hash (see token.go).
type Session struct {
	ID         string
	CSRF       string
	User       string
	InstanceID string
	CreatedAt  time.Time
	ExpiresAt  time.Time
}

// hashSID returns the lookup key for a raw session ID: the base64url
// SHA-256 of the raw value. The session ID already carries 256 bits of
// entropy from the OS CSPRNG, so a plain (un-salted, un-stretched) hash is
// sufficient here — there is nothing to brute-force — and it keeps Get on
// the hot request path cheap. Both the in-memory map key and the persisted
// Session.ID use this so the raw cookie value is never written to disk.
func hashSID(sid string) string {
	sum := sha256.Sum256([]byte(sid))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// SessionStore is a session table that is optionally persisted to disk.
// With an empty path it is purely in-memory (lost on daemon restart, the
// historical behaviour). With a path set (see NewPersistentSessionStore)
// the table is flushed atomically on every mutation and reloaded on
// startup, so the browser's session cookie keeps authenticating across a
// daemon restart instead of forcing a fresh tray bootstrap.
//
// Concurrency: a single sync.RWMutex protects the underlying map.
// The Get path is hot (every authenticated request goes through it)
// so we use RLock for reads and Lock only for mutation.
type SessionStore struct {
	ttl        time.Duration
	path       string // "" = in-memory only
	mu         sync.RWMutex
	tbl        map[string]Session
	instanceID string
}

// NewSessionStore returns an empty in-memory store. ttl is the per-session
// lifetime; pass DefaultSessionTTL unless tests need otherwise.
func NewSessionStore(ttl time.Duration) *SessionStore {
	return NewSessionStoreForInstance(ttl, "default")
}
func NewSessionStoreForInstance(ttl time.Duration, instanceID string) *SessionStore {
	if ttl <= 0 {
		ttl = DefaultSessionTTL
	}
	return &SessionStore{ttl: ttl, tbl: map[string]Session{}, instanceID: instanceID}
}

// NewPersistentSessionStore returns a store backed by a JSON file at path.
// Any existing, non-expired sessions are loaded so they survive a daemon
// restart; subsequent mutations are flushed to disk atomically (0600). The
// file holds bearer-equivalent session IDs, so callers must place it in a
// user-private directory (the daemon uses its 0700 state-dir).
func NewPersistentSessionStore(ttl time.Duration, path string) *SessionStore {
	if ttl <= 0 {
		ttl = DefaultSessionTTL
	}
	s := &SessionStore{ttl: ttl, path: path, tbl: map[string]Session{}}
	s.load()
	return s
}

// load reads the persisted table (if any), dropping already-expired
// sessions. A missing or unreadable/corrupt file is treated as an empty
// store — persistence is a convenience, never a hard dependency.
func (s *SessionStore) load() {
	if s.path == "" {
		return
	}
	b, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	var sessions []Session
	if json.Unmarshal(b, &sessions) != nil {
		return
	}
	now := time.Now()
	s.mu.Lock()
	for _, sess := range sessions {
		if now.After(sess.ExpiresAt) {
			continue
		}
		s.tbl[sess.ID] = sess
	}
	s.mu.Unlock()
}

// persistLocked flushes the table to disk atomically. The caller MUST hold
// s.mu (write lock). No-op for an in-memory store. A write failure is
// swallowed: the in-memory table stays authoritative for this process, and
// the only consequence is that sessions may not survive the next restart.
func (s *SessionStore) persistLocked() {
	if s.path == "" {
		return
	}
	sessions := make([]Session, 0, len(s.tbl))
	for _, sess := range s.tbl {
		sessions = append(sessions, sess)
	}
	b, err := json.Marshal(sessions)
	if err != nil {
		return
	}
	_ = atomicfile.WriteFile(s.path, b, 0o600)
}

// TTL returns the configured session lifetime.
func (s *SessionStore) TTL() time.Duration { return s.ttl }

// ErrEntropy signals failure to read random bytes from the OS pool.
// Surfaces only when /dev/urandom (or its equivalent) is unavailable;
// in practice never observed at runtime.
var ErrEntropy = errors.New("auth: read entropy")

// Create mints a new session, persists it in the table, and returns
// the session ID + matching CSRF token. Both values are base64url-
// encoded random bytes.
func (s *SessionStore) Create(user string) (sid, csrf string, err error) {
	sidBytes := make([]byte, sessionIDBytes)
	csrfBytes := make([]byte, csrfTokenBytes)
	if _, err := rand.Read(sidBytes); err != nil {
		return "", "", ErrEntropy
	}
	if _, err := rand.Read(csrfBytes); err != nil {
		return "", "", ErrEntropy
	}
	sid = base64.RawURLEncoding.EncodeToString(sidBytes)
	csrf = base64.RawURLEncoding.EncodeToString(csrfBytes)

	// Key the table (and the persisted record) on the hash, never the raw
	// sid: the raw value is returned to the caller for the cookie and then
	// dropped. Lookups (Get/Revoke) re-hash the incoming cookie value.
	key := hashSID(sid)
	now := time.Now()
	sess := Session{
		ID:         key,
		CSRF:       csrf,
		User:       user,
		InstanceID: s.instanceID,
		CreatedAt:  now,
		ExpiresAt:  now.Add(s.ttl),
	}
	s.mu.Lock()
	s.tbl[key] = sess
	s.persistLocked()
	s.mu.Unlock()
	return sid, csrf, nil
}

// Get returns the session for the raw session ID sid (the cookie value),
// or false if the session is unknown or expired. The raw value is hashed
// before the map lookup, matching how Create stored it. Expired sessions
// are GC'd on Get; the caller sees a clean "not found" without needing to
// call Revoke explicitly.
func (s *SessionStore) Get(sid string) (Session, bool) {
	key := hashSID(sid)
	s.mu.RLock()
	sess, ok := s.tbl[key]
	s.mu.RUnlock()
	if !ok || sess.InstanceID != s.instanceID {
		return Session{}, false
	}
	if time.Now().After(sess.ExpiresAt) {
		s.Revoke(sid)
		return Session{}, false
	}
	return sess, true
}

// Revoke deletes the session identified by the raw session ID sid (the
// cookie value) from the table. The raw value is hashed before deletion,
// matching the key Create used. No-op if sid is unknown. Always returns
// nil; the signature reserves an error for a future persistent backend.
func (s *SessionStore) Revoke(sid string) {
	key := hashSID(sid)
	s.mu.Lock()
	delete(s.tbl, key)
	s.persistLocked()
	s.mu.Unlock()
}

// RevokeAll deletes every session in the table and returns the count.
// Used by `aplexica web revoke-sessions` to invalidate every active
// session in one shot (e.g. after suspecting compromise).
func (s *SessionStore) RevokeAll() int {
	s.mu.Lock()
	n := len(s.tbl)
	s.tbl = map[string]Session{}
	s.persistLocked()
	s.mu.Unlock()
	return n
}

// Count returns the current number of sessions in the table. Used by
// the /api/daemon endpoint's "active sessions" hint and by tests.
func (s *SessionStore) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.tbl)
}
