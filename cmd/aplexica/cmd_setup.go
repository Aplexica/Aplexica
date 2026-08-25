package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aplexica/aplexica/internal/daemon"
	"github.com/aplexica/aplexica/internal/version"
	"github.com/spf13/cobra"
)

// setupCmd implements `aplexica setup` — the first-run wizard the installers
// invoke (and that users can re-run any time). It collects a few preferences
// (tray, web UI), persists them into the daemon's config.json, and — unless the
// user opts out — performs the full bootstrap: installs the per-OS daemon
// service + tray autostart, optionally the cloud plugin (--cloud), and starts
// everything. `aplexica setup --yes --install` is the one-command setup.
//
// Config-only mode is preserved for backward compatibility: `--yes` alone,
// `--install=no`, or answering "no" to the install prompt just writes config
// and prints the manual next-steps (so anything scripting the old wizard is
// unaffected).
var setupCmd = &cobra.Command{
	Use:   "setup",
	Short: "Configure and bring up Aplexica in one step",
	Long: `Configure and bring up Aplexica.

Interactively it asks:
  1. Enable the system tray app?
  2. Enable the local web UI?
  3. Install and start Aplexica now?   (default yes)
  4. Open the web UI now?              (when web is enabled)

With --install it performs the full bootstrap: installs the per-OS daemon
service + tray autostart, optionally the cloud plugin (--cloud <path>), and
starts the daemon — which brings up the embedded web UI and auto-discovers your
agents. The watched directory defaults to your home directory (override --dir).

One-command setup (non-interactive):
  aplexica setup --yes --install
  aplexica setup --yes --install --cloud /Library/Aplexica/RemotePlugins/aplexica-cloud/vX.Y.Z/aplexica-cloud-plugin \
    --cloud-initial-sequence N --cloud-initial-rollback-floor F \
    --cloud-initial-inventory-sha256 <independently-verified-sha256>

Config-only: --yes alone (or --install=no) just writes
~/.aplexica/state/config.json and prints the manual next steps. Change any
answer later with aplexica config set or by editing the file.`,
	Args: cobra.NoArgs,
	RunE: runSetup,
}

var (
	setupFlagTray                        string
	setupFlagWeb                         string
	setupFlagOpen                        string
	setupFlagAssume                      bool
	setupFlagInstall                     string
	setupFlagCloud                       string
	setupFlagCloudInitialSequence        uint64
	setupFlagCloudInitialRollbackFloor   uint64
	setupFlagCloudInitialInventorySHA256 string
	setupFlagCloudAllowLegacyOverlap     bool
	setupFlagDir                         string
)

const (
	setupDaemonReadyTimeout      = 3 * time.Minute
	setupDaemonReadyPollInterval = 100 * time.Millisecond
)

var setupDaemonProbe = func() error {
	_, _, err := sendControlCommand("status")
	return err
}

func init() {
	setupCmd.Flags().StringVar(&setupFlagTray, "tray", "",
		"Pre-answer the tray question (yes|no). Skips the prompt when set.")
	setupCmd.Flags().StringVar(&setupFlagWeb, "web", "",
		"Pre-answer the web UI question (yes|no). Skips the prompt when set.")
	setupCmd.Flags().StringVar(&setupFlagOpen, "open", "",
		"Pre-answer the open-browser question (yes|no). Skips the prompt when set.")
	setupCmd.Flags().BoolVar(&setupFlagAssume, "yes", false,
		"Accept all defaults (tray=yes, web=yes, open=no). Implies non-interactive.")
	setupCmd.Flags().StringVar(&setupFlagInstall, "install", "",
		"Install + start the full stack (yes|no). `--install` alone = yes. "+
			"When unset: interactive prompt (default yes); `--yes` alone stays config-only.")
	// `--install` with no value means "yes" — the common one-liner form.
	setupCmd.Flags().Lookup("install").NoOptDefVal = "yes"
	setupCmd.Flags().StringVar(&setupFlagCloud, "cloud", "",
		"Exact installed aplexica-cloud-plugin path to verify + enable (macOS requires /Library/Aplexica/RemotePlugins/aplexica-cloud/<version>/aplexica-cloud-plugin).")
	setupCmd.Flags().Uint64Var(&setupFlagCloudInitialSequence, "cloud-initial-sequence", 0,
		"Exact out-of-band release sequence for the first v2 cloud-plugin install.")
	setupCmd.Flags().Uint64Var(&setupFlagCloudInitialRollbackFloor, "cloud-initial-rollback-floor", 0,
		"Exact out-of-band rollback floor for the first v2 cloud-plugin install.")
	setupCmd.Flags().StringVar(&setupFlagCloudInitialInventorySHA256, "cloud-initial-inventory-sha256", "",
		"Exact out-of-band signed inventory SHA-256 for the first v2 cloud-plugin install.")
	setupCmd.Flags().BoolVar(&setupFlagCloudAllowLegacyOverlap, "cloud-allow-legacy-overlap", false,
		"Explicitly migrate only the compiled exact legacy overlap cloud plugin.")
	setupCmd.Flags().StringVar(&setupFlagDir, "dir", "",
		"Directory the daemon watches (default: your home directory).")
	rootCmd.AddCommand(setupCmd)
}

func runSetup(cmd *cobra.Command, _ []string) error {
	return setupWithIO(cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr())
}

// setupWithIO is the testable variant of runSetup: it takes explicit
// I/O streams so tests can drive answers without touching os.Stdin.
//
// Returns an error only when the underlying config read/write or
// browser launch fails. Cancelled prompts (EOF on stdin) abort
// gracefully with a one-line message and a nil error.
func setupWithIO(in io.Reader, out, errOut io.Writer) error {
	state, err := daemonStatePath()
	if err != nil {
		return err
	}
	cfgPath := filepath.Join(state, "config.json")

	fmt.Fprintf(out, "Welcome to Aplexica %s.\n\n", version.Version)
	fmt.Fprintln(out, "Setting up your defaults. Change any of them later in")
	fmt.Fprintln(out, "~/.aplexica/state/config.json or via `aplexica config set`.")
	fmt.Fprintln(out)

	// The tray ships enabled by default on every OS (opt-out) — single
	// source of truth in daemon.TrayEnabledDefault. So the wizard's prompt
	// defaults to "[Y/n]" everywhere, and a non-interactive `setup --yes`
	// brings the tray up on macOS/Linux too (not just Windows).
	trayDefault := daemon.TrayEnabledDefault()
	webDefault := true
	openDefault := true

	// Wrap the input in a single bufio.Reader so successive prompts
	// share the same buffer — creating a new bufio.Reader per call
	// would discard input bytes the buffer had already pre-read.
	reader := bufio.NewReader(in)

	tray, err := askYN(reader, out, errOut, "Enable the system tray app?", trayDefault, setupFlagTray, setupFlagAssume)
	if err != nil {
		return err
	}
	web, err := askYN(reader, out, errOut, "Enable the local web UI?", webDefault, setupFlagWeb, setupFlagAssume)
	if err != nil {
		return err
	}

	// Decide whether to perform the full bootstrap (install the service +
	// tray, start the daemon) or just write config. `--install` (yes|no)
	// presets it; interactively the default is yes so a bare `aplexica setup`
	// brings everything up. `--yes` ALONE stays config-only, so anything
	// scripting the historical wizard is unaffected.
	install, err := decideInstall(reader, out, errOut)
	if err != nil {
		return err
	}

	// "Open now" only makes sense when web is enabled.
	openNow := false
	if web {
		openNow, err = askYN(reader, out, errOut, "Open the web UI now?", openDefault, setupFlagOpen, setupFlagAssume)
		if err != nil {
			return err
		}
	}

	// Persist the answers.
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o700); err != nil {
		return fmt.Errorf("ensure state dir: %w", err)
	}
	cfg, err := daemon.LoadConfig(cfgPath)
	if err != nil {
		return fmt.Errorf("load %s: %w", cfgPath, err)
	}
	cfg.Tray.Enabled = boolPtr(tray)
	cfg.Web.Enabled = boolPtr(web)
	if err := daemon.WriteConfig(cfgPath, cfg); err != nil {
		return fmt.Errorf("write %s: %w", cfgPath, err)
	}

	fmt.Fprintln(out)
	fmt.Fprintln(out, "✓ Saved settings to", cfgPath)

	if install {
		// Full bootstrap: install the service + tray and start the daemon by
		// composing the existing per-OS commands.
		dir := setupFlagDir
		if dir == "" {
			home, herr := os.UserHomeDir()
			if herr != nil || home == "" {
				return fmt.Errorf("resolve home directory for the watched dir (pass --dir): %w", herr)
			}
			dir = home
		}
		fmt.Fprintln(out)
		runner := newSetupRunner(out, errOut)
		cloudTrust := cloudPluginBootstrapOptions{InitialSequence: setupFlagCloudInitialSequence,
			InitialRollbackFloor: setupFlagCloudInitialRollbackFloor, InitialInventorySHA256: setupFlagCloudInitialInventorySHA256,
			AllowLegacyOverlap: setupFlagCloudAllowLegacyOverlap}
		if err := runSetupBootstrapWithTrust(runner, dir, tray, setupFlagCloud, cloudTrust, out); err != nil {
			return err
		}
		fmt.Fprintln(out, "→ Waiting for the daemon to become ready…")
		fmt.Fprintln(out, "  First startup can take a few minutes while Aplexica creates safety backups.")
		if err := waitForSetupDaemon(setupDaemonReadyTimeout, setupDaemonReadyPollInterval); err != nil {
			fmt.Fprintln(errOut, "Aplexica was installed, but the daemon did not become ready.")
			fmt.Fprintln(errOut, "Check `aplexica status` and `aplexica daemon logs`, then retry")
			fmt.Fprintln(errOut, "`aplexica setup --yes --install`.")
			return err
		}
		printSetupSummary(out, tray, web, setupFlagCloud != "")
	} else {
		fmt.Fprintln(out)
		// Next-steps hint, tailored to what the user just enabled.
		fmt.Fprintln(out, "Next steps:")
		fmt.Fprintln(out, "  1. Install the daemon as a user service:")
		if tray {
			fmt.Fprintln(out, "       aplexica daemon install --dir \"$HOME\" --tray")
		} else {
			fmt.Fprintln(out, "       aplexica daemon install --dir \"$HOME\"")
		}
		fmt.Fprintln(out, "  2. Start the daemon:")
		fmt.Fprintln(out, "       aplexica daemon start --dir \"$HOME\"")
		fmt.Fprintln(out, "  3. Sanity check:")
		fmt.Fprintln(out, "       aplexica status")
		fmt.Fprintln(out, "  (or just re-run `aplexica setup --install` to do all of this automatically.)")
		fmt.Fprintln(out)
	}

	if openNow {
		fmt.Fprintln(out, "Attempting to open the web UI...")
		// Don't propagate browser-launch failures — the wizard's
		// happy-path completed; the browser launch is a nicety.
		if data, _, err := sendControlCommand("web-issue-bootstrap-file"); err != nil {
			fmt.Fprintln(errOut, "Couldn't open the web UI automatically.")
			fmt.Fprintln(errOut, "Click the tray icon → Open Aplexica, or run `aplexica web open`.")
		} else {
			// Daemon up + web running — same code path as
			// `aplexica web open`, inlined here so we don't have
			// to re-invoke ourselves.
			if m, ok := data.(map[string]any); ok {
				if path, _ := m["path"].(string); path != "" {
					_ = openInBrowser(path)
				}
			}
		}
	}

	return nil
}

func waitForSetupDaemon(timeout, pollInterval time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		if err := setupDaemonProbe(); err == nil {
			return nil
		} else {
			lastErr = err
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return fmt.Errorf("daemon did not become ready within %s: %w", timeout, lastErr)
		}
		delay := pollInterval
		if delay <= 0 || delay > remaining {
			delay = remaining
		}
		time.Sleep(delay)
	}
}

// decideInstall resolves whether to run the full bootstrap. `--install`
// (yes|no) wins when set; otherwise `--yes` keeps the historical config-only
// behavior, and a plain interactive run prompts (default yes).
func decideInstall(reader *bufio.Reader, out, errOut io.Writer) (bool, error) {
	if setupFlagInstall != "" {
		v, ok := parseYN(setupFlagInstall)
		if !ok {
			return false, fmt.Errorf("invalid --install %q (want yes/no)", setupFlagInstall)
		}
		return v, nil
	}
	if setupFlagAssume {
		// `--yes` alone keeps the historical config-only behavior.
		return false, nil
	}
	return askYN(reader, out, errOut, "Install and start Aplexica now?", true, "", false)
}

// printSetupSummary reports what the bootstrap brought up and how to reach it.
func printSetupSummary(out io.Writer, tray, web, cloud bool) {
	fmt.Fprintln(out)
	fmt.Fprintln(out, "✓ Aplexica is set up and running.")
	fmt.Fprintln(out, "  • Daemon: installed as a per-user service (auto-starts at login).")
	if tray {
		fmt.Fprintln(out, "  • Tray: installed (starts at login; click it to open the web UI).")
	}
	if web {
		fmt.Fprintln(out, "  • Web UI: run `aplexica web open`, or click the tray → Open Aplexica.")
	}
	if cloud {
		fmt.Fprintln(out, "  • Cloud: plugin installed. Pair this device at https://app.aplexica.com,")
		fmt.Fprintln(out, "    then enter the code on the Connect to Cloud page (or `aplexica remote pair <code>`).")
	}
	fmt.Fprintln(out, "  • Agents auto-discover — install Claude Code, Codex, etc. and they sync automatically.")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Check status anytime with: aplexica status")
	fmt.Fprintln(out)
}

// askYN prints "prompt [Y/n]" or "[y/N]" depending on def, reads a
// line from the supplied bufio.Reader, and returns the parsed boolean.
// EOF or empty line accepts the default.
//
// preset (non-empty) overrides the prompt entirely and uses the value
// directly — used by --tray / --web / --open headless flags.
//
// assume (true) short-circuits to def — used by --yes.
//
// The caller must pass the SAME *bufio.Reader to successive calls so
// the buffer's pre-read bytes carry between prompts.
func askYN(reader *bufio.Reader, out, errOut io.Writer, prompt string, def bool, preset string, assume bool) (bool, error) {
	if preset != "" {
		v, ok := parseYN(preset)
		if !ok {
			return false, fmt.Errorf("invalid value %q (want yes/no, y/n, true/false)", preset)
		}
		return v, nil
	}
	if assume {
		return def, nil
	}
	suffix := "[Y/n]"
	if !def {
		suffix = "[y/N]"
	}
	fmt.Fprintf(out, "  %s %s ", prompt, suffix)
	line, err := reader.ReadString('\n')
	if err != nil && err != io.EOF {
		return false, err
	}
	line = strings.TrimSpace(line)
	if line == "" {
		fmt.Fprintln(out)
		return def, nil
	}
	v, ok := parseYN(line)
	if !ok {
		fmt.Fprintf(errOut, "  (didn't understand %q; assuming default)\n", line)
		return def, nil
	}
	return v, nil
}

// parseYN normalizes the common variants for "yes" / "no" / "true" /
// "false". Case-insensitive; empty string returns (false, false) so
// callers can substitute the default themselves.
func parseYN(s string) (value bool, ok bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "y", "yes", "true", "1":
		return true, true
	case "n", "no", "false", "0":
		return false, true
	}
	return false, false
}

func boolPtr(b bool) *bool { return &b }
