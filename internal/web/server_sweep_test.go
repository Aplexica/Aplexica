package web

import (
	"context"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/web/auth"
)

// TestServerSweepsExpiredBootstrapTokens asserts that the server's
// lifecycle starts periodic housekeeping that evicts expired bootstrap
// tokens from the in-memory TokenStore. Without the wired sweep, expired
// tokens accumulate for the daemon's entire lifetime (each one then
// re-paying argon2 on every Consume), so Outstanding() stays > 0.
//
// The test is deterministic by construction: a TokenStore never sweeps
// itself (expiry alone leaves entries in place; only Consume of the
// matching token, or an explicit SweepExpired, removes one). So before
// Start no goroutine can clear the table, and we can issue N short-TTL
// tokens, let them expire, and assert all N are still present. Starting
// the server then wires the sweep, which must drive Outstanding() to 0.
func TestServerSweepsExpiredBootstrapTokens(t *testing.T) {
	dir := t.TempDir()

	srv, err := NewServer(Options{
		Bind:        "127.0.0.1",
		Port:        0,
		PortInfoDir: dir,
		Version:     "v0.0.0-test",
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	// Tiny TTL so the tokens are already expired by the time the sweep
	// runs; a fast sweep cadence so the test does not wait the
	// production interval. Set BEFORE Start so no sweep has run yet.
	srv.tokens = auth.NewTokenStore(time.Millisecond)
	srv.sweepInterval = 5 * time.Millisecond

	// Issue several tokens while the server is NOT started. Nothing
	// sweeps them, so even after they expire they remain in the table.
	const n = 5
	base := "http://127.0.0.1:0"
	for i := 0; i < n; i++ {
		if _, _, err := srv.tokens.Issue(base); err != nil {
			t.Fatalf("Issue: %v", err)
		}
	}
	time.Sleep(5 * time.Millisecond) // let the 1ms TTL lapse
	if got := srv.tokens.Outstanding(); got != 1 {
		t.Fatalf("Outstanding before Start = %d, want 1 (only newest token remains valid)", got)
	}

	// Start the server: this is the unit under test — its lifecycle must
	// run the housekeeping sweep that reaps the expired tokens.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.Start(ctx) }()
	waitForPort(t, srv)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if srv.tokens.Outstanding() == 0 {
			return // swept by the wired lifecycle — pass
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("expired bootstrap tokens were never swept by the server lifecycle: Outstanding = %d, want 0", srv.tokens.Outstanding())
}
