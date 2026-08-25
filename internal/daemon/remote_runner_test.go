package daemon

import (
	"context"
	"os/exec"
	"runtime"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestRemoteRunnerDeviceIDSupportsInitializationAndRuntimeUpdate(t *testing.T) {
	r := &RemoteRunner{DeviceID: "startup-device"}
	if got := r.CurrentDeviceID(); got != "startup-device" {
		t.Fatalf("CurrentDeviceID() = %q, want startup-device", got)
	}

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 1_000; j++ {
				r.SetDeviceID("paired-device")
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 1_000; j++ {
				_ = r.CurrentDeviceID()
			}
		}()
	}
	wg.Wait()

	if got := r.CurrentDeviceID(); got != "paired-device" {
		t.Fatalf("CurrentDeviceID() = %q, want paired-device", got)
	}
}

// startSleeper launches a long-lived child process used to stand in for
// a running remote plugin. Skips on platforms without a usable sleep.
func startSleeper(t *testing.T) *exec.Cmd {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("sleeper helper not wired for windows")
	}
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleeper: %v", err)
	}
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}
	})
	return cmd
}

// TestRemoteRunner_RestartKillsCurrentProcess verifies Restart delivers a
// kill to the currently-tracked child so the supervisor's cmd.Wait() in
// runOnce unblocks (the respawn signal). We stand in for runOnce by
// manually registering the cmd via setCmd.
func TestRemoteRunner_RestartKillsCurrentProcess(t *testing.T) {
	r := &RemoteRunner{Executable: "/bin/true"}
	cmd := startSleeper(t)
	r.setCmd(cmd)

	if err := r.Restart(context.Background()); err != nil {
		t.Fatalf("Restart returned error: %v", err)
	}

	// The kill should make Wait return promptly.
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
		// Killed process: Wait returns (non-nil error). Success — the
		// supervisor would observe this exit and respawn.
	case <-time.After(5 * time.Second):
		t.Fatal("process not killed by Restart within 5s")
	}
}

// TestRemoteRunner_RestartNoopAfterStop verifies Restart never kills a
// process once Stop has run — Restart must not interfere with the
// terminal shutdown path.
func TestRemoteRunner_RestartNoopAfterStop(t *testing.T) {
	r := &RemoteRunner{Executable: "/bin/true"}
	cmd := startSleeper(t)
	r.setCmd(cmd)

	// Simulate that Stop has been invoked.
	r.stopped.Store(true)

	if err := r.Restart(context.Background()); err != nil {
		t.Fatalf("Restart returned error: %v", err)
	}

	// Process must still be alive (Restart was a no-op). Signal 0 probes
	// liveness without killing.
	if cmd.Process == nil {
		t.Fatal("no process handle")
	}
	if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("process should still be alive after no-op Restart, signal err: %v", err)
	}
}

// TestRemoteRunner_RestartNilCmdNoError verifies Restart on a runner that
// has never spawned (no cmd) is a benign no-op rather than a panic/error.
func TestRemoteRunner_RestartNilCmdNoError(t *testing.T) {
	r := &RemoteRunner{Executable: "/bin/true"}
	if err := r.Restart(context.Background()); err != nil {
		t.Fatalf("Restart on nil cmd returned error: %v", err)
	}
}
