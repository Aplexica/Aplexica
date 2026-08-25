package main

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aplexica/aplexica/internal/daemon"
	"github.com/aplexica/aplexica/internal/plugin/proto"
	"github.com/aplexica/aplexica/internal/plugin/secureexec"
	"github.com/aplexica/aplexica/internal/plugin/truststate"
	"github.com/spf13/cobra"
)

// remoteCmd is the parent of every `aplexica remote …` subcommand.
// Manages the configured remote-transport plugin: which executable
// to load, what sync mode to use, when to enable it.
//
// The Cloud edition's `aplexica-cloud-plugin` is the canonical user
// of these settings, but anything that implements the remote-plugin ABI (see
// internal/plugin/proto/remote_messages.go) can be
// pointed at via `aplexica remote install <path>`.
var remoteCmd = &cobra.Command{
	Use:   "remote",
	Short: "Manage the remote-transport plugin (Aplexica Cloud, self-hosted relay, BYO)",
	Long: `Manage the daemon's remote-transport plugin configuration.

Subcommands:
  install <path>            Point at a remote-plugin executable + enable it
  verify <path>             Verify its publisher signature, digest, and capabilities
  uninstall                 Disable + forget the remote plugin
  status                    Show current connectivity + counters
  mode <manual|scheduled>   Switch sync cadence (realtime requires Pro+)
  enable / disable          Toggle without losing the configured path

The Cloud plugin ships separately as aplexica-cloud-plugin. After installing
the Cloud subscription via your account portal, point the daemon at it:

    aplexica remote install /Library/Aplexica/RemotePlugins/aplexica-cloud/vX.Y.Z/aplexica-cloud-plugin \
      --initial-sequence N --initial-rollback-floor F \
      --initial-inventory-sha256 <independently-verified-sha256>

On macOS the selected binary, adjacent manifest, and release inventory must
first be installed as the exact root-owned, ACL-free, read-only versioned tree
under /Library/Aplexica/RemotePlugins/aplexica-cloud/. User-owned, Homebrew,
symlinked, mutable, and unversioned paths fail closed. The OSS daemon never
uses a hosted Git service as runtime trust and never silently falls back to a
different plugin path.`,
}

func init() {
	rootCmd.AddCommand(remoteCmd)
	remoteCmd.AddCommand(remoteInstallCmd)
	remoteCmd.AddCommand(remoteVerifyCmd)
	remoteCmd.AddCommand(remoteUninstallCmd)
	remoteCmd.AddCommand(remoteStatusCmd)
	remoteCmd.AddCommand(remoteModeCmd)
	remoteCmd.AddCommand(remoteEnableCmd)
	remoteCmd.AddCommand(remoteDisableCmd)
	remoteCmd.AddCommand(remoteTransitionCmd)
	remoteTransitionCmd.AddCommand(remoteTransitionSubmitCmd)
	remoteVerifyCmd.Flags().Bool("json", false, "emit deterministic JSON verification evidence")
	remoteInstallCmd.Flags().Uint64("initial-sequence", 0, "exact out-of-band sequence for the first v2 install")
	remoteInstallCmd.Flags().Uint64("initial-rollback-floor", 0, "exact out-of-band rollback floor for the first v2 install")
	remoteInstallCmd.Flags().String("initial-inventory-sha256", "", "exact out-of-band signed inventory SHA-256 for the first v2 install")
	remoteInstallCmd.Flags().Bool("allow-legacy-overlap", false, "explicitly checkpoint the compiled exact v1 overlap artifact")
}

type remotePluginVerifier func(string) (proto.VerifiedRemotePlugin, error)

var requiredRemotePluginCapabilities = [...]string{
	proto.CapabilityInboundAckV2,
	proto.CapabilityPairStdinV1,
	proto.CapabilityTrustProtocolV1,
}

func preflightRemotePlugin(rawPath string, verify remotePluginVerifier) (string, proto.VerifiedRemotePlugin, error) {
	execPath, err := filepath.Abs(rawPath)
	if err != nil {
		return "", proto.VerifiedRemotePlugin{}, fmt.Errorf("resolve executable path: %w", err)
	}
	verified, err := verify(execPath)
	if err != nil {
		return "", proto.VerifiedRemotePlugin{}, fmt.Errorf("verify signed remote plugin: %w", err)
	}
	missing := make([]string, 0, len(requiredRemotePluginCapabilities))
	for _, capability := range requiredRemotePluginCapabilities {
		if !verified.Manifest.HasCapability(capability) {
			missing = append(missing, capability)
		}
	}
	if len(missing) != 0 {
		return "", proto.VerifiedRemotePlugin{}, fmt.Errorf("remote plugin lacks required capabilities: %s", strings.Join(missing, ", "))
	}
	return execPath, verified, nil
}

// ─────────────────────────────────────────────────────────────────────
// install <path>
// ─────────────────────────────────────────────────────────────────────

var remoteInstallCmd = &cobra.Command{
	Use:   "install <path-to-remote-plugin-executable>",
	Short: "Verify, configure, and enable a signed remote-transport plugin",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		bootstrap, err := remoteInstallBootstrap(cmd)
		if err != nil {
			return err
		}
		return runRemoteInstallWithBootstrap(cmd, args[0], verifyRemotePluginWithCompiledTrust, bootstrap)
	},
}

func runRemoteInstall(cmd *cobra.Command, rawPath string, verify remotePluginVerifier) error {
	return runRemoteInstallWithBootstrap(cmd, rawPath, verify, truststate.Bootstrap{})
}

func runRemoteInstallWithBootstrap(cmd *cobra.Command, rawPath string, verify remotePluginVerifier, bootstrap truststate.Bootstrap) error {
	return runRemoteInstallWithBootstrapAndLayout(cmd, rawPath, verify, bootstrap, secureexec.ValidateInstalledRemotePlugin)
}

type remotePluginInstallLayoutValidator func(string, string, string) error

func runRemoteInstallWithBootstrapAndLayout(cmd *cobra.Command, rawPath string, verify remotePluginVerifier, bootstrap truststate.Bootstrap, validateLayout remotePluginInstallLayoutValidator) error {
	// Verification intentionally completes before resolving, creating, loading,
	// or writing the daemon config. A failed signature, digest, trusted-path, or
	// capability check must leave even an existing configuration byte-for-byte
	// unchanged.
	execPath, verified, err := preflightRemotePlugin(rawPath, verify)
	if err != nil {
		return fmt.Errorf("remote plugin preflight failed: %w", err)
	}
	if validateLayout == nil {
		return fmt.Errorf("remote plugin install layout validation unavailable")
	}
	if err := validateLayout(execPath, verified.Manifest.PluginID, verified.Manifest.PluginVersion); err != nil {
		return fmt.Errorf("remote plugin install layout rejected: %w", err)
	}

	cfgPath, err := daemonConfigPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o700); err != nil {
		return fmt.Errorf("ensure state dir: %w", err)
	}
	checkpointStore := truststate.Store{Root: filepath.Join(filepath.Dir(cfgPath), "remote-plugin-trust")}
	checkpoint, err := checkpointStore.Accept(execPath, verified, remotePluginTrustPolicy(), bootstrap)
	if err != nil {
		return fmt.Errorf("authorize remote plugin release: %w", err)
	}
	cfg, err := daemon.LoadConfig(cfgPath)
	if err != nil {
		return fmt.Errorf("load %s: %w", cfgPath, err)
	}
	cfg.Remote.Executable = execPath
	enabled := true
	cfg.Remote.Enabled = &enabled
	if cfg.Remote.SyncMode == "" {
		cfg.Remote.SyncMode = "scheduled"
	}
	if err := daemon.WriteConfig(cfgPath, cfg); err != nil {
		return fmt.Errorf("write %s: %w", cfgPath, err)
	}
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "Remote plugin installed: %s\n", execPath)
	if checkpoint.ManifestVersion == 2 {
		fmt.Fprintf(out, "Accepted release: %s (sequence %d, rollback floor %d)\n", checkpoint.PluginVersion, checkpoint.Sequence, checkpoint.RollbackFloor)
	} else {
		fmt.Fprintf(out, "Accepted release: %s (legacy overlap; upgrade to manifest v2 before retirement)\n", checkpoint.PluginVersion)
	}
	fmt.Fprintf(out, "Sync mode: %s\n", daemon.RemoteSyncMode(cfg))
	fmt.Fprintln(out, "Restart or reload the daemon to start the plugin:")
	fmt.Fprintln(out, "  aplexica daemon reload")
	return nil
}

func remoteInstallBootstrap(cmd *cobra.Command) (truststate.Bootstrap, error) {
	sequence, err := cmd.Flags().GetUint64("initial-sequence")
	if err != nil {
		return truststate.Bootstrap{}, err
	}
	floor, err := cmd.Flags().GetUint64("initial-rollback-floor")
	if err != nil {
		return truststate.Bootstrap{}, err
	}
	digestText, err := cmd.Flags().GetString("initial-inventory-sha256")
	if err != nil {
		return truststate.Bootstrap{}, err
	}
	legacy, err := cmd.Flags().GetBool("allow-legacy-overlap")
	if err != nil {
		return truststate.Bootstrap{}, err
	}
	var digest [32]byte
	if digestText != "" {
		raw, decodeErr := hex.DecodeString(digestText)
		if decodeErr != nil || len(raw) != len(digest) || hex.EncodeToString(raw) != digestText {
			return truststate.Bootstrap{}, fmt.Errorf("initial inventory SHA-256 must be exactly 64 lowercase hex characters")
		}
		copy(digest[:], raw)
	}
	return truststate.Bootstrap{LegacyMigration: legacy, Sequence: sequence, RollbackFloor: floor, InventorySHA256: digest}, nil
}

// ─────────────────────────────────────────────────────────────────────
// verify <path>
// ─────────────────────────────────────────────────────────────────────

type remotePluginVerificationEvidence struct {
	Verified           bool     `json:"verified"`
	Executable         string   `json:"executable"`
	ManifestPath       string   `json:"manifestPath"`
	PluginID           string   `json:"pluginId"`
	PluginVersion      string   `json:"pluginVersion"`
	BinarySHA256       string   `json:"binarySha256"`
	PublisherKeySHA256 string   `json:"publisherKeySha256"`
	ManifestVersion    uint16   `json:"manifestVersion"`
	ReleaseSequence    uint64   `json:"releaseSequence,omitempty"`
	RollbackFloor      uint64   `json:"rollbackFloor,omitempty"`
	ManifestSHA256     string   `json:"manifestSha256"`
	InventorySHA256    string   `json:"inventorySha256,omitempty"`
	Capabilities       []string `json:"capabilities"`
	ProtocolMin        uint16   `json:"protocolMin"`
	ProtocolMax        uint16   `json:"protocolMax"`
}

var remoteVerifyCmd = &cobra.Command{
	Use:   "verify <path-to-remote-plugin-executable>",
	Short: "Verify a remote plugin without changing daemon configuration",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		asJSON, err := cmd.Flags().GetBool("json")
		if err != nil {
			return err
		}
		return runRemoteVerify(cmd, args[0], asJSON, verifyRemotePluginWithCompiledTrust)
	},
}

func runRemoteVerify(cmd *cobra.Command, rawPath string, asJSON bool, verify remotePluginVerifier) error {
	execPath, verified, err := preflightRemotePlugin(rawPath, verify)
	if err != nil {
		return fmt.Errorf("remote plugin preflight failed: %w", err)
	}
	manifest := verified.Manifest
	evidence := remotePluginVerificationEvidence{
		Verified:           true,
		Executable:         execPath,
		ManifestPath:       execPath + proto.RemotePluginManifestSuffix,
		PluginID:           manifest.PluginID,
		PluginVersion:      manifest.PluginVersion,
		BinarySHA256:       hex.EncodeToString(manifest.BinarySHA256[:]),
		PublisherKeySHA256: hex.EncodeToString(verified.PublisherKeySHA256[:]),
		ManifestVersion:    manifest.Version, ReleaseSequence: manifest.Sequence, RollbackFloor: manifest.RollbackFloor,
		ManifestSHA256: hex.EncodeToString(verified.ManifestSHA256[:]),
		Capabilities:   append([]string(nil), manifest.Capabilities...),
		ProtocolMin:    manifest.ProtocolMin,
		ProtocolMax:    manifest.ProtocolMax,
	}
	if verified.InventorySHA256 != ([32]byte{}) {
		evidence.InventorySHA256 = hex.EncodeToString(verified.InventorySHA256[:])
	}
	if asJSON {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(evidence)
	}
	out := cmd.OutOrStdout()
	fmt.Fprintln(out, "Remote plugin verification: OK")
	fmt.Fprintf(out, "Executable: %s\n", evidence.Executable)
	fmt.Fprintf(out, "Manifest: %s\n", evidence.ManifestPath)
	fmt.Fprintf(out, "Plugin: %s %s\n", evidence.PluginID, evidence.PluginVersion)
	fmt.Fprintf(out, "Binary SHA-256: %s\n", evidence.BinarySHA256)
	fmt.Fprintf(out, "Publisher key SHA-256: %s\n", evidence.PublisherKeySHA256)
	fmt.Fprintf(out, "Manifest SHA-256: %s\n", evidence.ManifestSHA256)
	if evidence.ManifestVersion == 2 {
		fmt.Fprintf(out, "Release sequence: %d (rollback floor %d)\n", evidence.ReleaseSequence, evidence.RollbackFloor)
		fmt.Fprintf(out, "Inventory SHA-256: %s\n", evidence.InventorySHA256)
	}
	fmt.Fprintf(out, "Capabilities: %s\n", strings.Join(evidence.Capabilities, ","))
	fmt.Fprintf(out, "Protocol: %d-%d\n", evidence.ProtocolMin, evidence.ProtocolMax)
	return nil
}

// ─────────────────────────────────────────────────────────────────────
// uninstall
// ─────────────────────────────────────────────────────────────────────

var remoteUninstallCmd = &cobra.Command{
	Use:   "uninstall",
	Short: "Disable and forget the remote plugin (leaves canonical store + pairing intact)",
	Args:  cobra.NoArgs,
	RunE: func(_ *cobra.Command, _ []string) error {
		cfgPath, err := daemonConfigPath()
		if err != nil {
			return err
		}
		cfg, err := daemon.LoadConfig(cfgPath)
		if err != nil {
			return err
		}
		cfg.Remote = daemon.RemoteConfig{}
		if err := daemon.WriteConfig(cfgPath, cfg); err != nil {
			return err
		}
		fmt.Println("Remote plugin uninstalled.")
		fmt.Println("Note: canonical store + pairing state are untouched.")
		fmt.Println("Restart or reload the daemon:")
		fmt.Println("  aplexica daemon reload")
		return nil
	},
}

// ─────────────────────────────────────────────────────────────────────
// status
// ─────────────────────────────────────────────────────────────────────

var remoteStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show remote plugin connectivity, counters, and restart history",
	Args:  cobra.NoArgs,
	RunE: func(_ *cobra.Command, _ []string) error {
		cfgPath, err := daemonConfigPath()
		if err != nil {
			return err
		}
		cfg, err := daemon.LoadConfig(cfgPath)
		if err != nil {
			return err
		}
		fmt.Println("=== Remote plugin configuration ===")
		fmt.Printf("Executable:        %s\n", strOrDash(cfg.Remote.Executable))
		fmt.Printf("Enabled:           %v\n", daemon.RemoteEnabled(cfg))
		fmt.Printf("Sync mode:         %s\n", daemon.RemoteSyncMode(cfg))
		fmt.Printf("Scheduled cadence: %v\n", daemon.RemoteScheduledInterval(cfg))
		fmt.Println()
		fmt.Println("Live connectivity status comes from the running daemon via")
		fmt.Println("`aplexica status` (which shows the cached ConnState from the")
		fmt.Println("plugin's last remote.conn_state notification).")
		// V1.1: when the control-socket RPC for plugin status lands,
		// wire a live query here. Today the daemon does not expose
		// per-plugin status via UDS; the `aplexica status` surface
		// surfaces it via the rolling activity-overlay path.
		return nil
	},
}

// ─────────────────────────────────────────────────────────────────────
// mode <manual|scheduled>
// ─────────────────────────────────────────────────────────────────────

var remoteModeCmd = &cobra.Command{
	Use:   "mode <manual|scheduled>",
	Short: "Switch sync cadence (manual fires only on explicit command; scheduled fires every interval)",
	Args:  cobra.ExactArgs(1),
	RunE: func(_ *cobra.Command, args []string) error {
		mode := args[0]
		switch mode {
		case "manual", "scheduled":
		case "realtime":
			return fmt.Errorf("realtime mode requires the Pro entitlement; current tier is Personal")
		default:
			return fmt.Errorf("unknown mode %q (want manual|scheduled)", mode)
		}
		cfgPath, err := daemonConfigPath()
		if err != nil {
			return err
		}
		cfg, err := daemon.LoadConfig(cfgPath)
		if err != nil {
			return err
		}
		cfg.Remote.SyncMode = mode
		if err := daemon.WriteConfig(cfgPath, cfg); err != nil {
			return err
		}
		fmt.Printf("Sync mode set to %q.\n", mode)
		fmt.Println("Reload the daemon:")
		fmt.Println("  aplexica daemon reload")
		return nil
	},
}

// ─────────────────────────────────────────────────────────────────────
// enable / disable
// ─────────────────────────────────────────────────────────────────────

var remoteEnableCmd = &cobra.Command{
	Use:   "enable",
	Short: "Enable the configured remote plugin (no-op if not installed)",
	Args:  cobra.NoArgs,
	RunE:  func(_ *cobra.Command, _ []string) error { return setRemoteEnabled(true) },
}

var remoteDisableCmd = &cobra.Command{
	Use:   "disable",
	Short: "Disable the configured remote plugin without losing its install path",
	Args:  cobra.NoArgs,
	RunE:  func(_ *cobra.Command, _ []string) error { return setRemoteEnabled(false) },
}

// ─────────────────────────────────────────────────────────────────────
// transition submit <signed-plan.json>
// ─────────────────────────────────────────────────────────────────────

const signedDeviceTransitionPlanMax = int64(8 << 20)

var remoteTransitionCmd = &cobra.Command{
	Use:   "transition",
	Short: "Manage signed device-access and namespace-rekey transitions",
	Args:  cobra.NoArgs,
}

var remoteTransitionSubmitCmd = &cobra.Command{
	Use:   "submit <signed-plan.json>",
	Short: "Authenticate, relay, and install an authorized device transition",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		blob, err := readSignedDeviceTransitionPlan(args[0])
		if err != nil {
			return err
		}
		state, err := daemonStatePath()
		if err != nil {
			return err
		}
		response, err := daemon.SendCommandWithTimeout(
			filepath.Join(state, "aplexicad.sock"),
			daemon.Request{Command: "device-transition-submit", PlanBlob: blob},
			70*time.Second,
		)
		if err != nil {
			return fmt.Errorf("submit signed device transition to daemon: %w", err)
		}
		if !response.OK {
			return fmt.Errorf("device transition rejected: %s", response.Error)
		}
		fmt.Fprintln(cmd.OutOrStdout(), "Signed device transition plan accepted.")
		return nil
	},
}

func readSignedDeviceTransitionPlan(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open signed device transition plan: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat signed device transition plan: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > signedDeviceTransitionPlanMax {
		return nil, fmt.Errorf("signed device transition plan must be a non-empty regular file no larger than %d bytes", signedDeviceTransitionPlanMax)
	}
	blob, err := io.ReadAll(io.LimitReader(file, signedDeviceTransitionPlanMax+1))
	if err != nil {
		return nil, fmt.Errorf("read signed device transition plan: %w", err)
	}
	if int64(len(blob)) > signedDeviceTransitionPlanMax {
		return nil, fmt.Errorf("signed device transition plan exceeds %d bytes", signedDeviceTransitionPlanMax)
	}
	return blob, nil
}

func setRemoteEnabled(enabled bool) error {
	cfgPath, err := daemonConfigPath()
	if err != nil {
		return err
	}
	cfg, err := daemon.LoadConfig(cfgPath)
	if err != nil {
		return err
	}
	if cfg.Remote.Executable == "" {
		return fmt.Errorf("no remote plugin configured; run `aplexica remote install <path>` first")
	}
	cfg.Remote.Enabled = &enabled
	if err := daemon.WriteConfig(cfgPath, cfg); err != nil {
		return err
	}
	verb := "enabled"
	if !enabled {
		verb = "disabled"
	}
	fmt.Printf("Remote plugin %s.\n", verb)
	fmt.Println("Reload the daemon:")
	fmt.Println("  aplexica daemon reload")
	return nil
}

func strOrDash(s string) string {
	if s == "" {
		return "(not configured)"
	}
	return s
}
