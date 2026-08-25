package syncd

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRecursionGuard_FreshPath_NotSuppressed(t *testing.T) {
	g := NewRecursionGuard(500 * time.Millisecond)
	require.False(t, g.Suppressed("/some/path"), "untouched path must not be suppressed")
}

func TestRecursionGuard_MarkedPath_Suppressed(t *testing.T) {
	g := NewRecursionGuard(500 * time.Millisecond)
	g.Mark("/some/path")
	require.True(t, g.Suppressed("/some/path"), "freshly marked path must be suppressed within window")
}

func TestRecursionGuard_AfterWindow_NotSuppressed(t *testing.T) {
	g := NewRecursionGuard(50 * time.Millisecond)
	g.Mark("/some/path")
	require.True(t, g.Suppressed("/some/path"))
	time.Sleep(80 * time.Millisecond)
	require.False(t, g.Suppressed("/some/path"),
		"after window expires, path must no longer be suppressed")
}

func TestRecursionGuard_IndependentPaths(t *testing.T) {
	g := NewRecursionGuard(500 * time.Millisecond)
	g.Mark("/a")
	require.True(t, g.Suppressed("/a"))
	require.False(t, g.Suppressed("/b"))
}

func TestRecursionGuard_ConcurrentMarkAndCheck(t *testing.T) {
	g := NewRecursionGuard(500 * time.Millisecond)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); g.Mark("/x") }()
		go func() { defer wg.Done(); _ = g.Suppressed("/x") }()
	}
	wg.Wait()
	require.True(t, g.Suppressed("/x"), "after concurrent Mark+check, path must still register as suppressed")
}

// TestRecursionGuard_SetWindow verifies the SIGHUP live-setter path: the new
// suppression window is reflected via the Window getter. (Entries already in
// the guard keep their original eviction deadline by design — see SetWindow
// docs for rationale.)
func TestRecursionGuard_SetWindow(t *testing.T) {
	g := NewRecursionGuard(5 * time.Second)
	require.Equal(t, 5*time.Second, g.Window())
	g.SetWindow(10 * time.Second)
	require.Equal(t, 10*time.Second, g.Window())
}

func TestRecursionGuard_ReMarkExtendsWindow(t *testing.T) {
	// Generous window/sleep margins: with 60ms/40ms the second sleep
	// overshooting by just 20ms on a loaded CI runner (observed on
	// windows-latest under -race) let the suppression legitimately
	// expire before the assertion.
	g := NewRecursionGuard(2 * time.Second)
	g.Mark("/p")
	time.Sleep(1200 * time.Millisecond)
	g.Mark("/p")                        // re-mark resets the timer
	time.Sleep(1200 * time.Millisecond) // 2.4s past first mark, 1.2s past re-mark
	require.True(t, g.Suppressed("/p"),
		"re-marking a path must extend the suppression window")
}
