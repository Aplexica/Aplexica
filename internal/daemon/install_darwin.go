//go:build darwin

package daemon

import (
	"encoding/xml"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const launchdLabel = "com.aplexica.aplexicad"

func newPlatformInstaller(opts InstallOptions) Installer {
	return &launchdInstaller{opts: opts}
}

type launchdInstaller struct {
	opts InstallOptions
	// plistDirOverride overrides the default ~/Library/LaunchAgents path for tests.
	plistDirOverride string
}

func (l *launchdInstaller) PlatformLabel() string { return "launchd LaunchAgent" }

func (l *launchdInstaller) plistPath() string {
	if l.plistDirOverride != "" {
		return filepath.Join(l.plistDirOverride, launchdLabel+".plist")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "LaunchAgents", launchdLabel+".plist")
}

func (l *launchdInstaller) Install() error {
	path := l.plistPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("daemon: launchd plist dir: %w", err)
	}
	content, err := l.generatePlist()
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, content, 0o644); err != nil {
		return fmt.Errorf("daemon: write plist: %w", err)
	}
	// Best-effort unload of any prior registration — ignore error.
	_ = exec.Command("launchctl", "unload", path).Run()
	cmd := exec.Command("launchctl", "load", "-w", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("daemon: launchctl load: %w (output: %s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (l *launchdInstaller) Uninstall() error {
	path := l.plistPath()
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil // idempotent
	}
	// launchctl unload -w: deregister + persist removal.
	if out, err := exec.Command("launchctl", "unload", "-w", path).CombinedOutput(); err != nil {
		// Continue to file removal even if unload fails (the file may
		// be the source of truth that needs deletion regardless).
		_ = out
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("daemon: remove plist: %w", err)
	}
	return nil
}

func (l *launchdInstaller) generatePlist() ([]byte, error) {
	// Build the ProgramArguments array.
	args := []string{l.opts.AplexicaPath, "daemon", "serve", "--dir", l.opts.Dir}
	if l.opts.StoreRoot != "" {
		args = append(args, "--store", l.opts.StoreRoot)
	}
	if l.opts.SecretsRoot != "" {
		args = append(args, "--secrets-root", l.opts.SecretsRoot)
	}
	if l.opts.StateDir != "" {
		args = append(args, "--state-dir", l.opts.StateDir)
	}
	if l.opts.LogDir != "" {
		args = append(args, "--log-dir", l.opts.LogDir)
	}
	if l.opts.Quiet != "" {
		args = append(args, "--quiet", l.opts.Quiet)
	}
	if l.opts.GuardWindow != "" {
		args = append(args, "--guard-window", l.opts.GuardWindow)
	}
	if l.opts.Recursive {
		args = append(args, "--recursive")
	}
	if l.opts.HermesWatch != nil {
		args = append(args, "--hermes-watch")
		if *l.opts.HermesWatch {
			args = append(args, "true")
		} else {
			args = append(args, "false")
		}
	}
	if l.opts.HermesWatchInterval != "" {
		args = append(args, "--hermes-watch-interval", l.opts.HermesWatchInterval)
	}
	if l.opts.HermesDB != "" {
		args = append(args, "--hermes-db", l.opts.HermesDB)
	}

	var argsXML strings.Builder
	for _, a := range args {
		argsXML.WriteString("    <string>")
		xml.EscapeText(&argsXML, []byte(a)) //nolint:errcheck
		argsXML.WriteString("</string>\n")
	}

	xmlBody := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>%s</string>
  <key>ProgramArguments</key>
  <array>
%s  </array>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
  <key>ProcessType</key>
  <string>Background</string>
</dict>
</plist>
`, launchdLabel, argsXML.String())
	return []byte(xmlBody), nil
}
