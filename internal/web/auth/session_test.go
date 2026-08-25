package auth

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestSessionPersistedFileHasNoRawSessionID pins the at-rest hardening:
// the persisted sessions.json must NOT contain the raw session ID (the
// bearer cookie value). A leaked file must not directly yield replayable
// cookies. The store keys both the map and the persisted record on a hash
// of the session ID, mirroring how bootstrap tokens persist only the hash
// (token.go). Lookup by the presented raw cookie value must still work
// after a restart, because Get hashes the incoming value before lookup.
func TestSessionPersistedFileHasNoRawSessionID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")

	s1 := NewPersistentSessionStore(DefaultSessionTTL, path)
	sid, _, err := s1.Create("local")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	if strings.Contains(string(b), sid) {
		t.Fatalf("persisted session file leaks the raw session ID %q:\n%s", sid, b)
	}
	// The hash of the sid is what must be stored instead.
	if want := hashSID(sid); !strings.Contains(string(b), want) {
		t.Errorf("persisted file does not contain the hashed session ID %q:\n%s", want, b)
	}

	// Lookup by the raw cookie value must still resolve after a restart.
	s2 := NewPersistentSessionStore(DefaultSessionTTL, path)
	if _, ok := s2.Get(sid); !ok {
		t.Error("Get(raw sid) returned !ok after restart; lookup must hash before matching")
	}
}

// TestSessionPersistsAcrossRestart pins the fix for the "must re-bootstrap
// from the tray after every daemon restart" pain: a persistent store flushes
// sessions to disk, and a fresh store at the same path reloads them — so the
// browser's existing cookie still authenticates after the daemon restarts.
func TestSessionPersistsAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")

	s1 := NewPersistentSessionStore(DefaultSessionTTL, path)
	sid, csrf, err := s1.Create("local")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Simulate a daemon restart: a brand-new store reads the same file.
	s2 := NewPersistentSessionStore(DefaultSessionTTL, path)
	sess, ok := s2.Get(sid)
	if !ok {
		t.Fatal("session did not survive restart: Get returned !ok")
	}
	if sess.CSRF != csrf || sess.User != "local" {
		t.Errorf("reloaded session mismatch: csrf=%q user=%q", sess.CSRF, sess.User)
	}
}

// TestSessionPersistRevoke verifies a revoke is flushed: after revoking, a
// fresh store at the same path must NOT see the session.
func TestSessionPersistRevoke(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	s1 := NewPersistentSessionStore(DefaultSessionTTL, path)
	sid, _, _ := s1.Create("local")
	s1.Revoke(sid)

	s2 := NewPersistentSessionStore(DefaultSessionTTL, path)
	if _, ok := s2.Get(sid); ok {
		t.Error("revoked session reappeared after restart")
	}
}

// TestSessionPersistDropsExpired verifies expired sessions are not reloaded.
func TestSessionPersistDropsExpired(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	s1 := NewPersistentSessionStore(5*time.Millisecond, path)
	sid, _, _ := s1.Create("local")
	time.Sleep(20 * time.Millisecond)

	s2 := NewPersistentSessionStore(5*time.Millisecond, path)
	if _, ok := s2.Get(sid); ok {
		t.Error("expired session was reloaded across restart")
	}
}

func TestSessionCreateAndGet(t *testing.T) {
	s := NewSessionStore(DefaultSessionTTL)
	sid, csrf, err := s.Create("local")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if sid == "" || csrf == "" {
		t.Fatalf("Create returned empty: sid=%q csrf=%q", sid, csrf)
	}
	sess, ok := s.Get(sid)
	if !ok {
		t.Fatal("Get returned !ok for freshly-created session")
	}
	if sess.User != "local" {
		t.Errorf("User = %q, want local", sess.User)
	}
	if sess.CSRF != csrf {
		t.Errorf("CSRF roundtrip mismatch: %q != %q", sess.CSRF, csrf)
	}
	// The stored ID is the hash of the raw sid, never the raw value itself
	// (so an at-rest read does not yield a replayable cookie).
	if sess.ID == sid {
		t.Errorf("stored Session.ID must be the hash, not the raw sid %q", sid)
	}
	if sess.ID != hashSID(sid) {
		t.Errorf("stored Session.ID = %q, want hashSID(sid) = %q", sess.ID, hashSID(sid))
	}
}

func TestSessionExpiry(t *testing.T) {
	s := NewSessionStore(5 * time.Millisecond)
	sid, _, _ := s.Create("local")
	time.Sleep(20 * time.Millisecond)
	if _, ok := s.Get(sid); ok {
		t.Error("Get returned ok for expired session")
	}
	if got := s.Count(); got != 0 {
		t.Errorf("expired session should be GC'd on Get; Count = %d", got)
	}
}

func TestSessionRevoke(t *testing.T) {
	s := NewSessionStore(DefaultSessionTTL)
	sid, _, _ := s.Create("local")
	s.Revoke(sid)
	if _, ok := s.Get(sid); ok {
		t.Error("Get returned ok after Revoke")
	}
}

func TestSessionRevokeAll(t *testing.T) {
	s := NewSessionStore(DefaultSessionTTL)
	for i := 0; i < 5; i++ {
		_, _, _ = s.Create("local")
	}
	if got := s.Count(); got != 5 {
		t.Fatalf("Count before RevokeAll = %d, want 5", got)
	}
	if n := s.RevokeAll(); n != 5 {
		t.Errorf("RevokeAll returned %d, want 5", n)
	}
	if got := s.Count(); got != 0 {
		t.Errorf("Count after RevokeAll = %d, want 0", got)
	}
}

func TestSessionDefaultTTLAppliedWhenZero(t *testing.T) {
	s := NewSessionStore(0)
	if got := s.TTL(); got != DefaultSessionTTL {
		t.Errorf("TTL = %v, want DefaultSessionTTL=%v", got, DefaultSessionTTL)
	}
}

func TestSessionIDsAreUnique(t *testing.T) {
	s := NewSessionStore(DefaultSessionTTL)
	seen := map[string]struct{}{}
	for i := 0; i < 100; i++ {
		sid, _, _ := s.Create("local")
		if _, dup := seen[sid]; dup {
			t.Fatalf("duplicate session ID: %q", sid)
		}
		seen[sid] = struct{}{}
	}
}
