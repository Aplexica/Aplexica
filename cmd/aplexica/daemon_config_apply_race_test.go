package main

import (
	"sync"
	"testing"

	"github.com/spf13/cobra"
)

// TestReloadDaemonConfigPackage_ConcurrentNoRace fires
// reloadDaemonConfigPackage from many goroutines at once — the same
// concurrency the SIGHUP-TOML handler and the control-socket reloader
// create (ControlServer spawns `go s.handleConn` per connection with no
// lock around s.reloader(), and `kill -HUP` can fire in parallel).
//
// reloadDaemonConfigPackage snapshots daemonProjectScanRoots (a slice
// header) and then applyDaemonConfigPackage writes the package globals
// daemonProjectScanInterval/MaxDepth/Roots. Without tomlReloadMu those
// reads and writes overlap → `go test -race` flags a data race. The
// mutex serializes the full pre-snapshot/apply/post-snapshot sequence.
//
// Run under -race for this to be meaningful; without -race it just
// exercises the path and asserts no panic.
func TestReloadDaemonConfigPackage_ConcurrentNoRace(t *testing.T) {
	// Each goroutine gets its OWN *cobra.Command. cmd.Flags().Changed()
	// is the only per-cmd state reloadDaemonConfigPackage touches; the
	// race we are guarding against is on the package globals, not on the
	// command. A bare command (no project-scan-* flags defined) makes
	// Changed() return false, so the config-driven override path runs and
	// writes the globals — exactly the path under test.
	const goroutines = 16
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			cmd := &cobra.Command{Use: "reload-probe"}
			// Silence stderr config warnings during the storm.
			cmd.SetErr(&nopWriter{})
			for j := 0; j < 8; j++ {
				if _, err := reloadDaemonConfigPackage(cmd); err != nil {
					// A config-load error is acceptable in CI envs without
					// any config files; the point is the data race, not the
					// load result. Don't fail the test on it.
					return
				}
			}
		}()
	}
	wg.Wait()
}

// nopWriter discards writes (cmd.ErrOrStderr() target during the storm).
type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }
