package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"github.com/aplexica/aplexica/internal/daemon"
	"github.com/aplexica/aplexica/internal/web"
	"github.com/spf13/cobra"
)

// webCmd is the parent of every `aplexica web …` subcommand. The
// subcommands either talk to the running daemon via its UDS control
// socket (for state that lives inside the daemon process — bootstrap
// tokens, sessions) or read/write the on-disk config (for the
// enable/disable verbs).
var webCmd = &cobra.Command{
	Use:   "web",
	Short: "Manage the local web UI listener",
	Long: `Manage the local web UI listener that serves the embedded portal SPA on loopback HTTP.

Subcommands:
  issue-token         Mint a one-time bootstrap URL (talks to the running daemon).
  open                Mint a URL and open the default browser at it.
  port                Print the current listener port from portinfo.json.
  revoke-sessions     Invalidate every active web session.
  enable / disable    Toggle the listener via the config file.`,
}

func init() {
	rootCmd.AddCommand(webCmd)
	webCmd.AddCommand(webIssueTokenCmd)
	webCmd.AddCommand(webOpenCmd)
	webCmd.AddCommand(webPortCmd)
	webCmd.AddCommand(webRevokeSessionsCmd)
	webCmd.AddCommand(webEnableCmd)
	webCmd.AddCommand(webDisableCmd)
}

// defaultStateDir is the single resolver for the daemon's state
// directory: APLEXICA_STATE_DIR if set, otherwise ~/.aplexica/state.
// Both the daemon's --state-dir flag default and the web subcommands
// (via daemonStatePath) go through this so the two surfaces always agree
// on where the control socket / portinfo.json / config.json live.
func defaultStateDir() (string, error) {
	if env := os.Getenv("APLEXICA_STATE_DIR"); env != "" {
		return env, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	return filepath.Join(home, ".aplexica", "state"), nil
}

// daemonStatePath returns the daemon's state directory for the web
// subcommands, resolved identically to the daemon's --state-dir default
// (see defaultStateDir). Resolved independently so the web verbs work
// without parsing daemon flags.
func daemonStatePath() (string, error) {
	return defaultStateDir()
}

// daemonControlSocket returns the absolute UDS path used by the
// running daemon. Mirrors the path written by daemon serve.
func daemonControlSocket() (string, error) {
	state, err := daemonStatePath()
	if err != nil {
		return "", err
	}
	return filepath.Join(state, "aplexicad.sock"), nil
}

// daemonConfigPath returns the absolute config.json path used by the
// running daemon. Web enable/disable writes through this.
func daemonConfigPath() (string, error) {
	state, err := daemonStatePath()
	if err != nil {
		return "", err
	}
	return filepath.Join(state, "config.json"), nil
}

// sendControlCommand RPCs the daemon and returns the decoded
// response.Data plus the OK bit. The caller asserts the Data shape.
func sendControlCommand(cmd string) (any, bool, error) {
	sock, err := daemonControlSocket()
	if err != nil {
		return nil, false, err
	}
	resp, err := daemon.SendCommand(sock, daemon.Request{Command: cmd})
	if err != nil {
		return nil, false, fmt.Errorf("daemon not reachable at %s: %w", sock, err)
	}
	if !resp.OK {
		return nil, false, fmt.Errorf("daemon: %s", resp.Error)
	}
	return resp.Data, true, nil
}

// ─────────────────────────────────────────────────────────────────────
// issue-token
// ─────────────────────────────────────────────────────────────────────

var webIssueTokenCmd = &cobra.Command{
	Use:   "issue-token",
	Short: "Issue a one-time bootstrap URL for the local web UI (prints URL to stdout)",
	Long: `Mint a fresh one-time bootstrap token from the running daemon and print the full URL.

The URL is valid for 60 seconds and can be consumed exactly once. Open it in a browser to start an authenticated session — the daemon mints session cookies on the bootstrap exchange.

Errors:
  - daemon not reachable: the daemon isn't running (run aplexica daemon start)
  - web UI not running:   the daemon is up but the listener was disabled or failed to bind`,
	Args: cobra.NoArgs,
	RunE: func(_ *cobra.Command, _ []string) error {
		data, _, err := sendControlCommand("web-issue-token")
		if err != nil {
			return err
		}
		m, ok := data.(map[string]any)
		if !ok {
			return fmt.Errorf("unexpected response shape: %T", data)
		}
		url, _ := m["url"].(string)
		if url == "" {
			return fmt.Errorf("daemon returned empty URL")
		}
		fmt.Println(url)
		return nil
	},
}

// ─────────────────────────────────────────────────────────────────────
// open
// ─────────────────────────────────────────────────────────────────────

var webOpenCmd = &cobra.Command{
	Use:   "open",
	Short: "Issue a bootstrap URL and open it in the default browser",
	Args:  cobra.NoArgs,
	RunE: func(_ *cobra.Command, _ []string) error {
		data, _, err := sendControlCommand("web-issue-bootstrap-file")
		if err != nil {
			return err
		}
		m, _ := data.(map[string]any)
		path, _ := m["path"].(string)
		if path == "" {
			return fmt.Errorf("daemon returned empty bootstrap path")
		}
		return openInBrowser(path)
	},
}

// openInBrowser launches the user's default browser at url. The per-OS
// launcher is selected by the inline runtime.GOOS switch below. The
// fallback path swallows specific errors that just surface "no GUI"
// environments (headless CI, SSH session without X11 forwarding) and
// prints the URL for the user to open manually.
func openInBrowser(url string) error {
	var c *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		c = exec.Command("open", url)
	case "linux":
		c = exec.Command("xdg-open", url)
	case "windows":
		// rundll32 is more reliable than `start` from a non-shell
		// process; the design spec calls this out under §12.3.
		c = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		fmt.Println(url)
		fmt.Fprintln(os.Stderr, "openInBrowser: unsupported platform; open the URL above manually")
		return nil
	}
	if err := c.Start(); err != nil {
		fmt.Println(url)
		fmt.Fprintln(os.Stderr, "openInBrowser: launch failed; open the URL above manually:", err)
		return nil
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────
// port
// ─────────────────────────────────────────────────────────────────────

var webPortCmd = &cobra.Command{
	Use:   "port",
	Short: "Print the current local web UI listener port (reads portinfo.json)",
	Args:  cobra.NoArgs,
	RunE: func(_ *cobra.Command, _ []string) error {
		state, err := daemonStatePath()
		if err != nil {
			return err
		}
		path := filepath.Join(state, "portinfo.json")
		info, err := web.ReadPortInfo(path)
		if err != nil {
			return fmt.Errorf("read portinfo.json (is the daemon running?): %w", err)
		}
		fmt.Println(info.Port)
		return nil
	},
}

// ─────────────────────────────────────────────────────────────────────
// revoke-sessions
// ─────────────────────────────────────────────────────────────────────

var webRevokeSessionsCmd = &cobra.Command{
	Use:   "revoke-sessions",
	Short: "Invalidate every active local web UI session",
	Args:  cobra.NoArgs,
	RunE: func(_ *cobra.Command, _ []string) error {
		data, _, err := sendControlCommand("web-revoke-sessions")
		if err != nil {
			return err
		}
		m, _ := data.(map[string]any)
		n, _ := m["revoked"].(float64)
		fmt.Printf("Revoked %d session(s).\n", int(n))
		return nil
	},
}

// ─────────────────────────────────────────────────────────────────────
// enable / disable — writes to ~/.aplexica/state/config.json
// ─────────────────────────────────────────────────────────────────────

var webEnableCmd = &cobra.Command{
	Use:   "enable",
	Short: "Enable the local web UI in the daemon's config (takes effect on next start/reload)",
	Args:  cobra.NoArgs,
	RunE: func(_ *cobra.Command, _ []string) error {
		return setWebEnabled(true)
	},
}

var webDisableCmd = &cobra.Command{
	Use:   "disable",
	Short: "Disable the local web UI in the daemon's config (takes effect on next start/reload)",
	Args:  cobra.NoArgs,
	RunE: func(_ *cobra.Command, _ []string) error {
		return setWebEnabled(false)
	},
}

// setWebEnabled loads the daemon's config, flips Web.Enabled, and
// writes it back atomically. If the daemon is running, the user
// must either reload (aplexica daemon reload) or restart to pick up
// the change — this CLI doesn't decide for them.
func setWebEnabled(enabled bool) error {
	path, err := daemonConfigPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("ensure state dir: %w", err)
	}
	cfg, err := daemon.LoadConfig(path)
	if err != nil {
		return fmt.Errorf("load %s: %w", path, err)
	}
	cfg.Web.Enabled = &enabled
	if err := daemon.WriteConfig(path, cfg); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	verb := "enabled"
	if !enabled {
		verb = "disabled"
	}
	fmt.Printf("web UI %s in %s.\n", verb, path)
	fmt.Println("Reload or restart the daemon for the change to take effect:")
	fmt.Println("  aplexica daemon reload")
	return nil
}

// Suppress an unused-import warning if json is dropped from a future
// edit — keep the import handy because cmd_web.go's siblings
// frequently need it for response parsing.
var _ = json.NewDecoder
