package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
)

// setupRunner runs a subcommand of THIS aplexica binary, streaming output to
// the wizard's streams. Abstracted so the setup bootstrap is unit-testable with
// a fake (the real one shells out to os.Executable()).
type setupRunner interface {
	run(args ...string) error
}

// execSetupRunner invokes the running aplexica binary with the given args.
type execSetupRunner struct {
	out, errOut io.Writer
}

func (r *execSetupRunner) run(args ...string) error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate executable: %w", err)
	}
	c := exec.Command(self, args...)
	c.Stdout = r.out
	c.Stderr = r.errOut
	return c.Run()
}

// newSetupRunner builds the production runner. Overridable in tests.
var newSetupRunner = func(out, errOut io.Writer) setupRunner {
	return &execSetupRunner{out: out, errOut: errOut}
}

// runSetupBootstrap installs and starts the full local stack by composing the
// existing, per-OS-tested subcommands — no new install logic, so it inherits
// every per-OS installer (launchd / systemd --user / Windows logon task) and
// their tests.
//
// Order matters: when a cloud plugin path is supplied it is installed BEFORE the
// daemon, so the daemon spawns the plugin as it starts. A single `daemon
// install` registers the per-OS service + tray autostart AND starts the daemon
// (launchd RunAtLoad / systemd enable --now / the Windows logon-task best-effort
// run), so there is deliberately no separate `daemon start` step — install
// already starts it.
//
// Each step is independently idempotent and re-runnable; a failure stops the
// sequence and is wrapped with which step failed so callers can surface the
// remediation.
func runSetupBootstrap(r setupRunner, dir string, tray bool, cloudPath string, out io.Writer) error {
	return runSetupBootstrapWithTrust(r, dir, tray, cloudPath, cloudPluginBootstrapOptions{}, out)
}

type cloudPluginBootstrapOptions struct {
	InitialSequence        uint64
	InitialRollbackFloor   uint64
	InitialInventorySHA256 string
	AllowLegacyOverlap     bool
}

func runSetupBootstrapWithTrust(r setupRunner, dir string, tray bool, cloudPath string, trust cloudPluginBootstrapOptions, out io.Writer) error {
	if cloudPath != "" {
		fmt.Fprintln(out, "→ Installing the Aplexica Cloud plugin…")
		args := []string{"remote", "install", cloudPath}
		if trust.AllowLegacyOverlap {
			args = append(args, "--allow-legacy-overlap")
		}
		if trust.InitialSequence != 0 {
			args = append(args, "--initial-sequence", fmt.Sprintf("%d", trust.InitialSequence))
		}
		if trust.InitialRollbackFloor != 0 {
			args = append(args, "--initial-rollback-floor", fmt.Sprintf("%d", trust.InitialRollbackFloor))
		}
		if trust.InitialInventorySHA256 != "" {
			args = append(args, "--initial-inventory-sha256", trust.InitialInventorySHA256)
		}
		if err := r.run(args...); err != nil {
			return fmt.Errorf("cloud plugin install failed: %w", err)
		}
	}
	fmt.Fprintln(out, "→ Installing and starting the daemon…")
	if err := r.run("daemon", "install", "--dir", dir, fmt.Sprintf("--tray=%t", tray)); err != nil {
		return fmt.Errorf("daemon install failed: %w", err)
	}
	return nil
}
