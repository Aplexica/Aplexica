//go:build windows

package daemon

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"strings"
)

// newPlatformInstaller returns the Windows installer: a per-user, logon-
// triggered Scheduled Task (NOT an SCM service).
//
// History: v0.32.0 registered the daemon as a LocalSystem SCM service. That
// was wrong for this per-user product — a LocalSystem daemon's os.UserHomeDir()
// resolves to ...\config\systemprofile, so every adapter (which locates its
// agent under the user home) discovered nothing. The macOS (launchd
// LaunchAgent) and Linux (systemd --user) installers register the daemon as
// the logged-in user; this installer restores that parity on Windows via a
// logon-triggered task with an InteractiveToken principal — runs as the user,
// no Administrator rights, no stored password. (The SCM serve path in
// svc_windows.go remains for anyone who manually hosts the binary under SCM,
// but `daemon install` no longer registers one.)
func newPlatformInstaller(opts InstallOptions) Installer {
	return &schedTaskInstaller{opts: opts, userID: currentTaskUser()}
}

type schedTaskInstaller struct {
	opts   InstallOptions
	userID string
	// runOverride, when non-nil, replaces the schtasks shell-out: Install
	// passes ("create", tempXMLPath); Uninstall passes ("delete", ""). Tests
	// use it to capture the invocation without touching the host scheduler.
	runOverride func(action, xmlPath string) error
}

func (s *schedTaskInstaller) PlatformLabel() string { return "Windows logon Scheduled Task" }

// currentTaskUser resolves the principal account for the task in DOMAIN\User
// form, falling back to %USERDOMAIN%\%USERNAME% if the os/user lookup fails.
func currentTaskUser() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	dom := os.Getenv("USERDOMAIN")
	name := os.Getenv("USERNAME")
	if dom != "" && name != "" {
		return dom + `\` + name
	}
	return name
}

// Install writes the task definition and registers it with `schtasks /create
// /xml`. Re-running overwrites the prior definition (/f) — idempotent.
func (s *schedTaskInstaller) Install() error {
	xmlBytes, err := generateTaskXML(s.opts, s.userID)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp("", "aplexica-task-*.xml")
	if err != nil {
		return fmt.Errorf("daemon: temp task xml: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	// schtasks expects a UTF-16 task definition file.
	if _, err := tmp.Write(utf16LEWithBOM(xmlBytes)); err != nil {
		tmp.Close()
		return fmt.Errorf("daemon: write task xml: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("daemon: close task xml: %w", err)
	}

	if s.runOverride != nil {
		return s.runOverride("create", tmpPath)
	}
	cmd := exec.Command("schtasks", "/create", "/tn", scheduledTaskName, "/xml", tmpPath, "/f")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("daemon: schtasks create %q: %w (output: %s)",
			scheduledTaskName, err, strings.TrimSpace(string(out)))
	}
	// Best-effort immediate start so `daemon install` behaves like the
	// launchd `load -w` / systemd `enable --now` installers. An
	// InteractiveToken task can only /run when a session exists, so this
	// fails benignly when nobody is signed in yet — the logon trigger
	// starts the daemon at next sign-in regardless. Hence the ignored error.
	_ = exec.Command("schtasks", "/run", "/tn", scheduledTaskName).Run()
	return nil
}

// Uninstall deregisters the task. A missing task is an idempotent no-op
// (matches the unix installers' contract).
func (s *schedTaskInstaller) Uninstall() error {
	if s.runOverride != nil {
		return s.runOverride("delete", "")
	}
	cmd := exec.Command("schtasks", "/delete", "/tn", scheduledTaskName, "/f")
	out, err := cmd.CombinedOutput()
	if err != nil {
		low := strings.ToLower(string(out))
		if strings.Contains(low, "cannot find") || strings.Contains(low, "does not exist") {
			return nil
		}
		return fmt.Errorf("daemon: schtasks delete %q: %w (output: %s)",
			scheduledTaskName, err, strings.TrimSpace(string(out)))
	}
	return nil
}
