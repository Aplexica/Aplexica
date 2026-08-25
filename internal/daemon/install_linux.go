//go:build linux

package daemon

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const systemdServiceName = "aplexicad.service"

func newPlatformInstaller(opts InstallOptions) Installer {
	return &systemdInstaller{opts: opts}
}

type systemdInstaller struct {
	opts            InstallOptions
	unitDirOverride string
}

func (s *systemdInstaller) PlatformLabel() string { return "systemd --user" }

func (s *systemdInstaller) unitPath() string {
	if s.unitDirOverride != "" {
		return filepath.Join(s.unitDirOverride, systemdServiceName)
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "systemd", "user", systemdServiceName)
}

func (s *systemdInstaller) Install() error {
	path := s.unitPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("daemon: systemd unit dir: %w", err)
	}
	if err := os.WriteFile(path, []byte(s.generateUnit()), 0o644); err != nil {
		return fmt.Errorf("daemon: write unit: %w", err)
	}
	if out, err := exec.Command("systemctl", "--user", "daemon-reload").CombinedOutput(); err != nil {
		return fmt.Errorf("daemon: systemctl daemon-reload: %w (output: %s)", err, strings.TrimSpace(string(out)))
	}
	if out, err := exec.Command("systemctl", "--user", "enable", "--now", systemdServiceName).CombinedOutput(); err != nil {
		return fmt.Errorf("daemon: systemctl enable --now: %w (output: %s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (s *systemdInstaller) Uninstall() error {
	path := s.unitPath()
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil
	}
	_ = exec.Command("systemctl", "--user", "disable", "--now", systemdServiceName).Run()
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("daemon: remove unit: %w", err)
	}
	_ = exec.Command("systemctl", "--user", "daemon-reload").Run()
	return nil
}

func (s *systemdInstaller) generateUnit() string {
	args := []string{"daemon", "serve", "--dir", s.opts.Dir}
	if s.opts.StoreRoot != "" {
		args = append(args, "--store", s.opts.StoreRoot)
	}
	if s.opts.SecretsRoot != "" {
		args = append(args, "--secrets-root", s.opts.SecretsRoot)
	}
	if s.opts.StateDir != "" {
		args = append(args, "--state-dir", s.opts.StateDir)
	}
	if s.opts.LogDir != "" {
		args = append(args, "--log-dir", s.opts.LogDir)
	}
	if s.opts.Quiet != "" {
		args = append(args, "--quiet", s.opts.Quiet)
	}
	if s.opts.GuardWindow != "" {
		args = append(args, "--guard-window", s.opts.GuardWindow)
	}
	if s.opts.Recursive {
		args = append(args, "--recursive")
	}
	if s.opts.HermesWatch != nil {
		if *s.opts.HermesWatch {
			args = append(args, "--hermes-watch=true")
		} else {
			args = append(args, "--hermes-watch=false")
		}
	}
	if s.opts.HermesWatchInterval != "" {
		args = append(args, "--hermes-watch-interval", s.opts.HermesWatchInterval)
	}
	if s.opts.HermesDB != "" {
		args = append(args, "--hermes-db", s.opts.HermesDB)
	}
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = quoteSystemdArg(a)
	}
	execStart := quoteSystemdArg(s.opts.AplexicaPath) + " " + strings.Join(quoted, " ")

	return fmt.Sprintf(`[Unit]
Description=Aplexica sync daemon
After=default.target

[Service]
Type=simple
ExecStart=%s
Restart=on-failure
RestartSec=5

[Install]
WantedBy=default.target
`, execStart)
}

// quoteSystemdArg wraps a single ExecStart token in double quotes when it
// contains whitespace or a character systemd treats specially, escaping
// embedded double-quotes and backslashes with a backslash per systemd's
// C-style ExecStart quoting rules so the value re-splits as exactly one
// argument. Tokens with no special characters (flag names, durations,
// space-free paths) pass through unchanged. Mirrors quoteWinArg on the
// Windows installer; launchd needs no equivalent because it emits each arg
// as a separate <string> element.
func quoteSystemdArg(s string) string {
	if s == "" {
		return `""`
	}
	needsQuoting := false
	for _, r := range s {
		if r == ' ' || r == '\t' || r == '\n' || r == '"' || r == '\\' || r == '\'' {
			needsQuoting = true
			break
		}
	}
	if !needsQuoting {
		return s
	}
	var b strings.Builder
	b.WriteByte('"')
	for _, c := range s {
		if c == '"' || c == '\\' {
			b.WriteByte('\\')
		}
		b.WriteRune(c)
	}
	b.WriteByte('"')
	return b.String()
}
