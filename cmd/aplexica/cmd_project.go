package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/audit"
	"github.com/aplexica/aplexica/internal/daemon"
	"github.com/aplexica/aplexica/internal/project"
	"github.com/spf13/cobra"
)

// projectStateDir holds the daemon state dir flag used by every
// project subcommand. Defaults to ~/.aplexica/state (matches the
// daemon's own default in cmd_daemon.go).
var projectStateDir string

var (
	projectMigrationExpectedSHA string
	projectMigrationApprovedSHA string
	projectMigrationRetainIDs   []string
	projectMigrationRemoveIDs   []string
)

// openProjectRegistry resolves projectStateDir to the registry file
// path and opens (or initializes) the registry. Shared by every
// project subcommand. Returns the open registry; the caller is
// responsible for handling any Save errors from subsequent Add /
// Update / Remove calls.
func openProjectRegistry() (*project.Registry, error) {
	dir := projectStateDir
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("locate home: %w", err)
		}
		dir = filepath.Join(home, ".aplexica", "state")
	}
	return project.NewRegistry(filepath.Join(dir, "projects.json"))
}

var projectCmd = &cobra.Command{
	Use:   "project",
	Short: "Manage the local project registry",
	Long: `aplexica project init/link/rename/list manages the per-user list of
projects this device knows about — the registry adapters and the
orchestrator consult to map canonical project IDs to local paths.

The registry lives at <state-dir>/projects.json (default:
~/.aplexica/state/projects.json) and persists in JSON. Atomic via
internal/atomicfile.

Typical workflow:
  - Clone a git repo locally. Aplexica detects it on the next scan
    and tags artifacts with the repo's canonical ID (from origin URL).
    No registry entry needed unless you want a custom display name
    or want to track the repo's local path explicitly.
  - Run "aplexica project init <name>" in a non-VCS directory to
    promote it to a tracked project. The directory gets a stable
    path-derived ID (local:<sha>:<dirname>) recorded in the registry.
  - Run "aplexica project link <id> <path>" when you receive a
    project-scoped artifact via cloud sync or import for a repo
    you've since cloned at a non-standard path. The link tells
    Aplexica to materialize pending artifacts for that ID to <path>.`,
}

var projectInitCmd = &cobra.Command{
	Use:   "init [name]",
	Short: "Promote the current directory to a tracked project",
	Long: `Detects the current directory's project identity (git remote URL,
hg, or path-derived ID) and registers it in the local projects.json.
The optional positional name becomes the DisplayName; otherwise the
detected directory basename is used.

--ephemeral marks the project so cross-device sync rules exclude it
by default. Useful for ad-hoc directories
where you want per-directory tracking on THIS device but don't want
the project to propagate to your other devices.`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cwd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("project init: cwd: %w", err)
		}
		info, err := project.Detect(cwd)
		if err != nil {
			return fmt.Errorf("project init: detect: %w", err)
		}
		ephemeral, _ := cmd.Flags().GetBool("ephemeral")
		info.Ephemeral = ephemeral

		reg, err := openProjectRegistry()
		if err != nil {
			return err
		}
		displayName := filepath.Base(cwd)
		if len(args) == 1 {
			displayName = args[0]
		}
		entry := project.Entry{
			ID:          info.ID,
			Path:        info.Path,
			VCS:         info.VCS,
			Ephemeral:   info.Ephemeral,
			DisplayName: displayName,
		}
		if existing, ok := reg.Get(info.ID); ok {
			// Get only returns LIVE entries, so a different recorded path
			// here means another still-existing clone of the same repository
			// currently holds this registration. Re-pointing it as a side
			// effect of init would silently de-register that clone via the Update
			// path the AddOrUpdate guard does not cover — displacement must be an explicit "aplexica
			// project link". Both paths are canonical physical paths (Detect
			// and the registry both EvalSymlinks), so string comparison is
			// the right same-location test.
			if existing.Path != entry.Path {
				return fmt.Errorf("project init: %q is already registered at %s, which still exists; use \"aplexica project link %s %s\" to re-point it deliberately", info.ID, existing.Path, info.ID, entry.Path)
			}
			// Same location: refresh VCS/ephemeral/display. Idempotent.
			existing.VCS = entry.VCS
			existing.Ephemeral = entry.Ephemeral
			if len(args) == 1 {
				existing.DisplayName = displayName
			}
			if err := reg.Update(existing); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "project: updated %s\n", existing.ID)
			return nil
		}
		if err := reg.Add(entry); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "project: initialized %s (%s)\n", entry.ID, entry.VCS)
		return nil
	},
}

var projectLinkCmd = &cobra.Command{
	Use:   "link <id> <path>",
	Short: "Map a canonical project ID to a local path",
	Long: `Tells Aplexica that the canonical project ID <id> is checked out
locally at <path>. Used to materialize artifacts from
"~/.aplexica/store/pending/<id>/" once you clone or link the
project on this device.

The ID can be a normalized git remote URL ("github.com/owner/repo"),
a path-derived ID ("local:abc123:dirname"), or a name the user has
previously registered via "aplexica project init". The path is
made absolute before storage.`,
	Args: cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		id, path := args[0], args[1]
		abs, err := filepath.Abs(path)
		if err != nil {
			return fmt.Errorf("project link: abs path: %w", err)
		}
		if _, statErr := os.Stat(abs); statErr != nil {
			return fmt.Errorf("project link: path %s: %w", abs, statErr)
		}
		// Detect the VCS at the linked path so future scans don't
		// have to re-derive it. (The user might be linking a path
		// that doesn't actually match the canonical ID — that's
		// their responsibility; we trust the explicit link.)
		info, _ := project.Detect(abs)
		reg, err := openProjectRegistry()
		if err != nil {
			return err
		}
		if existing, ok := reg.Get(id); ok {
			existing.Path = abs
			existing.VCS = info.VCS
			if err := reg.Update(existing); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "project: re-linked %s → %s\n", id, abs)
			return nil
		}
		entry := project.Entry{
			ID:          id,
			Path:        abs,
			VCS:         info.VCS,
			DisplayName: filepath.Base(abs),
		}
		if err := reg.Add(entry); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "project: linked %s → %s\n", id, abs)
		// v0.58.0: BRD-02 §4.13 materialize-on-link. Send a refanout
		// request to the running daemon so the newly-linked project's
		// previously-pending artifacts materialize without waiting for
		// a fresh edit. Best-effort — if the daemon isn't running OR
		// the control socket is unreachable, we soft-warn and let
		// the next normal sync pick them up.
		sockPath := filepath.Join(projectStateDir, "aplexicad.sock")
		if projectStateDir == "" {
			home, _ := os.UserHomeDir()
			sockPath = filepath.Join(home, ".aplexica", "state", "aplexicad.sock")
		}
		if resp, err := daemon.SendCommand(sockPath, daemon.Request{
			Command: "refanout", ProjectID: id,
		}); err == nil && resp.OK {
			if d, ok := resp.Data.(map[string]any); ok {
				if n, ok := d["refanouted"].(float64); ok && n > 0 {
					fmt.Fprintf(cmd.OutOrStdout(),
						"project: materialized %.0f pending artifacts to %s\n", n, abs)
				}
			}
		}
		// If daemon was unreachable OR returned an error, we don't
		// surface it — link succeeded; materialization is opportunistic.
		return nil
	},
}

var projectRenameCmd = &cobra.Command{
	Use:   "rename <id> <new-display-name>",
	Short: "Change a project's display name (ID unchanged)",
	Long: `Updates the human-readable label for a registered project. The
canonical ID is immutable; only the DisplayName changes. Useful
for non-VCS projects whose default "local:<sha>:<dirname>" ID
isn't friendly to read.`,
	Args: cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		id, newName := args[0], args[1]
		reg, err := openProjectRegistry()
		if err != nil {
			return err
		}
		existing, ok := reg.Get(id)
		if !ok {
			return fmt.Errorf("project rename: %q not registered", id)
		}
		existing.DisplayName = newName
		if err := reg.Update(existing); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "project: renamed %s → display %q\n", id, newName)
		return nil
	},
}

var projectListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all registered projects",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		reg, err := openProjectRegistry()
		if err != nil {
			return err
		}
		entries := reg.ListAll()
		asJSON, _ := cmd.Flags().GetBool("json")
		if asJSON {
			b, err := json.MarshalIndent(entries, "", "  ")
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), string(b))
			return nil
		}
		if len(entries) == 0 {
			fmt.Fprintln(cmd.OutOrStdout(), "(no projects registered)")
			return nil
		}
		fmt.Fprintf(cmd.OutOrStdout(), "%-40s  %-5s  %s\n", "ID", "VCS", "PATH")
		for _, e := range entries {
			marker := ""
			if e.Ephemeral {
				marker = " (ephemeral)"
			}
			if e.Inactive {
				marker += " (inactive)"
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%-40s  %-5s  %s%s\n",
				e.ID, e.VCS, e.Path, marker)
		}
		return nil
	},
}

var projectMigrateV3Cmd = &cobra.Command{
	Use:   "migrate-v3",
	Short: "Safely migrate an exact Registry v2 snapshot to Registry v3",
	Long: `Registry v3 migration is a two-phase, fail-closed operation. Stop the
daemon first. "plan" binds physical paths, file identities, missing/inactive
paths, collision decisions, and exact output bytes to an independently
reviewable SHA-256. "apply" accepts only that approved plan digest, rechecks
the v2 bytes and every physical identity, writes no-clobber backup/report
evidence, then atomically/fsync installs revision 1.`,
}

var projectMigrateV3PlanCmd = &cobra.Command{
	Use:   "plan",
	Short: "Create an immutable Registry v3 migration plan",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		stateDir, err := resolvedProjectStateDir()
		if err != nil {
			return err
		}
		instanceLock, err := acquireProjectMigrationDaemonLock(stateDir)
		if err != nil {
			return err
		}
		defer func() { _ = instanceLock.Release() }()
		result, err := project.CreateRegistryV3MigrationPlan(project.RegistryV3PlanOptions{
			StateDir: stateDir, ExpectedInputSHA256: projectMigrationExpectedSHA,
			RetainIDs: projectMigrationRetainIDs, RemoveIDs: projectMigrationRemoveIDs,
		})
		if err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Registry v3 migration plan: %s\n", result.PlanPath)
		fmt.Fprintf(cmd.OutOrStdout(), "input-sha256: %s\nplan-sha256: %s\n", result.InputSHA256, result.PlanSHA256)
		fmt.Fprintf(cmd.OutOrStdout(), "projects=%d active=%d inactive=%d collisions=%d removed=%d\n",
			result.ProjectCount, result.ActiveCount, result.InactiveCount, result.CollisionCount, result.RemovedCount)
		return nil
	},
}

var projectMigrateV3ApplyCmd = &cobra.Command{
	Use:   "apply",
	Short: "Apply one independently approved Registry v3 migration plan",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		stateDir, err := resolvedProjectStateDir()
		if err != nil {
			return err
		}
		instanceLock, err := acquireProjectMigrationDaemonLock(stateDir)
		if err != nil {
			return err
		}
		defer func() { _ = instanceLock.Release() }()
		planDigestBytes, err := hexToSHA256(projectMigrationApprovedSHA)
		if err != nil {
			return err
		}
		txnID := acf.NewID()
		recorder := &audit.FileRecorder{Root: filepath.Join(stateDir, "audit")}
		if err := recorder.BeginTransaction(context.Background(), txnID, audit.Event{Code: "project.registry_v3_migrated",
			Fields: []audit.Field{audit.HashPrefix("plan_sha256", planDigestBytes)}}); err != nil {
			return fmt.Errorf("project: begin Registry v3 migration audit: %w", err)
		}
		result, applyErr := project.ApplyRegistryV3Migration(project.RegistryV3ApplyOptions{
			StateDir: stateDir, ApprovedPlanSHA256: projectMigrationApprovedSHA,
		})
		outcome := "success"
		if applyErr != nil {
			outcome = "failed"
		}
		if auditErr := recorder.CompleteTransaction(context.Background(), txnID, outcome); auditErr != nil {
			return fmt.Errorf("project: complete Registry v3 migration audit: %w", auditErr)
		}
		if applyErr != nil {
			return applyErr
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Registry v3 installed: %s\n", result.RegistryPath)
		fmt.Fprintf(cmd.OutOrStdout(), "registry-sha256: %s\nbackup: %s\ncollision-report: %s\n",
			result.RegistrySHA256, result.BackupPath, result.CollisionReportPath)
		fmt.Fprintf(cmd.OutOrStdout(), "projects=%d active=%d inactive=%d collisions=%d tombstones=%d\n",
			result.ProjectCount, result.ActiveCount, result.InactiveCount, result.CollisionCount, result.TombstoneCount)
		return nil
	},
}

func resolvedProjectStateDir() (string, error) {
	if projectStateDir != "" {
		if !filepath.IsAbs(projectStateDir) || filepath.Clean(projectStateDir) != projectStateDir {
			return "", fmt.Errorf("project: migration --state-dir must be clean and absolute")
		}
		return projectStateDir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate home: %w", err)
	}
	return filepath.Join(home, ".aplexica", "state"), nil
}

func acquireProjectMigrationDaemonLock(stateDir string) (*daemon.Lock, error) {
	lockPath := filepath.Join(stateDir, "aplexicad.lock")
	if info, err := os.Lstat(lockPath); err == nil && (info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular()) {
		return nil, fmt.Errorf("project: unsafe daemon instance lock object")
	} else if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("project: inspect daemon instance lock: %w", err)
	}
	lock, err := daemon.Acquire(lockPath)
	if err != nil {
		return nil, fmt.Errorf("project: Registry migration requires the daemon to be stopped and exclusively locked: %w", err)
	}
	return lock, nil
}

func hexToSHA256(value string) ([sha256.Size]byte, error) {
	var result [sha256.Size]byte
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != sha256.Size || value != hex.EncodeToString(decoded) {
		return result, fmt.Errorf("project: approved plan SHA-256 must be 64 lowercase hexadecimal characters")
	}
	copy(result[:], decoded)
	return result, nil
}

var projectRemoveCmd = &cobra.Command{
	Use:   "remove <id>",
	Short: "Remove a project from the registry (does not delete artifacts)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		stateDir := projectStateDir
		if stateDir == "" {
			home, _ := os.UserHomeDir()
			stateDir = filepath.Join(home, ".aplexica", "state")
		}
		sockPath := filepath.Join(stateDir, "aplexicad.sock")
		if resp, controlErr := daemon.SendCommand(sockPath, daemon.Request{Command: "project-remove", ProjectID: args[0]}); controlErr == nil {
			if !resp.OK {
				return fmt.Errorf("project: daemon removal failed: %s", resp.Error)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "project: removed %s\n", args[0])
			return nil
		}

		// Direct mutation is allowed only while this process owns the daemon
		// instance lock. This closes the socket-check/startup race: a live
		// daemon keeps the lock, and a daemon cannot start while fallback
		// mutation is in progress.
		instanceLock, err := daemon.Acquire(filepath.Join(stateDir, "aplexicad.lock"))
		if err != nil {
			return fmt.Errorf("project: daemon control unavailable and running-daemon exclusion failed: %w", err)
		}
		defer func() { _ = instanceLock.Release() }()
		reg, err := openProjectRegistry()
		if err != nil {
			return err
		}
		if err := revokeProject(reg, nil, nil, &audit.FileRecorder{Root: filepath.Join(stateDir, "audit")}, args[0]); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "project: removed %s\n", args[0])
		return nil
	},
}

var projectAddCmd = &cobra.Command{
	Use:   "add <path>",
	Short: "Register a project folder for syncing",
	Long: `Registers the directory at <path> in the local projects.json so
Aplexica knows about it for syncing. Unlike "project init" (which
operates on the current directory), "project add" accepts an explicit
path and is designed for scripted / portal-driven registration.

--scope controls how the project participates in cross-device sync:
  local   (default) sync only among adapters on this device, keyed
          by the absolute path.
  global  memory composes into the global union visible on all devices.

--agents bounds folder-local fan-out to the named adapters only.
Empty means "all installed agents" (same as omitting the flag).

The command is idempotent: running it twice for the same path updates
the existing registry entry rather than failing.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		scope, _ := cmd.Flags().GetString("scope")
		agentsCSV, _ := cmd.Flags().GetString("agents")
		return runProjectAdd(cmd, args[0], scope, agentsCSV)
	},
}

// runProjectAdd is the testable core of the "project add" subcommand.
// It resolves path → absolute, validates scope, detects VCS/ID, opens
// the registry, upserts the entry, and prints a confirmation line.
func runProjectAdd(cmd *cobra.Command, rawPath, scope, agentsCSV string) error {
	// Validate scope first so we fail fast before any I/O.
	switch scope {
	case "local", "global":
		// valid
	default:
		return fmt.Errorf("project add: --scope must be \"local\" or \"global\", got %q", scope)
	}

	abs, err := filepath.Abs(rawPath)
	if err != nil {
		return fmt.Errorf("project add: abs path: %w", err)
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return fmt.Errorf("project add: path %s: %w", abs, err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("project add: %s is not a directory", abs)
	}

	// Parse agents CSV → sorted slice (empty CSV → nil, meaning "all agents").
	var agents []string
	if agentsCSV != "" {
		for _, a := range strings.Split(agentsCSV, ",") {
			a = strings.TrimSpace(a)
			if a != "" {
				agents = append(agents, a)
			}
		}
	}

	// Detect VCS/ID. Fall back to path-derived ID on error.
	var id, vcs string
	info, err := project.Detect(abs)
	if err != nil {
		id = project.PathDerivedID(abs)
		vcs = "none"
	} else {
		id = info.ID
		vcs = info.VCS
	}

	reg, err := openProjectRegistry()
	if err != nil {
		return err
	}

	entry := project.Entry{
		ID:          id,
		Path:        abs,
		VCS:         vcs,
		Scope:       scope,
		Agents:      agents,
		DisplayName: filepath.Base(abs),
	}
	if err := reg.AddOrUpdate(entry); err != nil {
		return err
	}

	fmt.Fprintf(cmd.OutOrStdout(), "project: registered %s (%s) -> %s\n", id, scope, abs)
	return nil
}

func init() {
	projectCmd.PersistentFlags().StringVar(&projectStateDir, "state-dir", "",
		"daemon state directory (default: ~/.aplexica/state)")
	projectInitCmd.Flags().Bool("ephemeral", false,
		"mark the project as ephemeral (excluded from default cross-device sync)")
	projectListCmd.Flags().Bool("json", false, "emit machine-readable JSON")
	projectAddCmd.Flags().String("scope", "local",
		`sync scope: "local" (this device only) or "global" (all devices)`)
	projectAddCmd.Flags().String("agents", "",
		"comma-separated list of agents for folder-local fan-out (empty = all agents)")
	projectMigrateV3PlanCmd.Flags().StringVar(&projectMigrationExpectedSHA, "expected-input-sha256", "",
		"independently measured exact SHA-256 of projects.json v2")
	projectMigrateV3PlanCmd.Flags().StringArrayVar(&projectMigrationRetainIDs, "retain-id", nil,
		"project ID explicitly retained from a physical-path collision (repeatable)")
	projectMigrateV3PlanCmd.Flags().StringArrayVar(&projectMigrationRemoveIDs, "remove-id", nil,
		"project ID explicitly removed from a physical-path collision (repeatable)")
	projectMigrateV3ApplyCmd.Flags().StringVar(&projectMigrationApprovedSHA, "approve-plan-sha256", "",
		"independently reviewed exact SHA-256 of the immutable migration plan")
	projectMigrateV3Cmd.AddCommand(projectMigrateV3PlanCmd, projectMigrateV3ApplyCmd)

	projectCmd.AddCommand(projectInitCmd)
	projectCmd.AddCommand(projectLinkCmd)
	projectCmd.AddCommand(projectRenameCmd)
	projectCmd.AddCommand(projectListCmd)
	projectCmd.AddCommand(projectRemoveCmd)
	projectCmd.AddCommand(projectAddCmd)
	projectCmd.AddCommand(projectMigrateV3Cmd)
	rootCmd.AddCommand(projectCmd)
}
