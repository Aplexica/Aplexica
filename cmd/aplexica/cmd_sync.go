package main

import (
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter"
	"github.com/aplexica/aplexica/internal/adapter/claudecode"
	"github.com/aplexica/aplexica/internal/adapter/codex"
	"github.com/aplexica/aplexica/internal/adapter/hermes"
	"github.com/aplexica/aplexica/internal/adapter/kilo"
	"github.com/aplexica/aplexica/internal/secrets"
	syncd "github.com/aplexica/aplexica/internal/sync"
	"github.com/spf13/cobra"
)

var (
	syncStoreRoot   string
	syncSecretsRoot string
	syncQuiet       time.Duration
	syncGuardWindow time.Duration
	syncRecursive   bool
)

var syncCmd = &cobra.Command{
	Use:   "sync <dir>",
	Short: "Watch a directory and keep all installed agents in sync (foreground)",
	Long: `Watches <dir> for filesystem changes. Each settled change is imported
via the primary adapter for its filename (the alphabetically-first
adapter whose Import accepts the file), then fanned out to every other
installed adapter's native location for the artifact's kind.

Adapters are discovered from the agents installed on this machine; run
'aplexica adapters list' to see the current roster.

Memory, skill, and MCP tool kinds fan out in full. Conversations fan out
too — each agent's session schema differs structurally, so a conversation
is transcoded into each target's native format rather than copied. Only
the most recent conversations per agent are back-filled (see
'aplexica backfill'); live conversations are never capped.

Fan-out is opt-in per agent: Aplexica imports from every installed agent
but writes to none until you run 'aplexica sync enable <agent>'.

An in-process recursion guard suppresses events for paths the orchestrator
just wrote (within the --guard-window). Without the guard, the orchestrator's
own fan-out writes would feed back as inbound events and infinite-loop.

This foreground command does NOT run as a daemon, does NOT auto-start at
login, and does NOT recurse into subdirectories.

Ctrl-C stops the orchestrator cleanly.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		dir := args[0]

		store := &acf.Store{Root: syncStoreRoot}
		if err := store.Init(); err != nil {
			return err
		}

		// Shared secrets store across all adapters (single user, single host).
		ss := &secrets.Store{Root: syncSecretsRoot}
		if err := ss.Init(); err != nil {
			return err
		}

		cc := claudecode.New()
		cc.SecretsStore = ss
		cx := codex.New()
		cx.SecretsStore = ss
		k := kilo.New()
		k.SecretsStore = ss
		h := hermes.New()
		h.SecretsStore = ss
		// Foreground sync usually runs INSTEAD of the daemon, in which case
		// this resolves nothing and the hostname default stands — but when a
		// daemon is also up, matching its cloud identity keeps every authored
		// head publishable (see cliCloudDeviceID).
		if deviceID := cliCloudDeviceID(); deviceID != "" {
			cc.SetDeviceID(deviceID)
			cx.SetDeviceID(deviceID)
			k.SetDeviceID(deviceID)
			h.SetDeviceID(deviceID)
		}

		ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
		defer cancel()

		orch, err := syncd.NewOrchestrator(syncd.Config{
			Dir:         dir,
			Adapters:    []adapter.Adapter{cc, cx, k, h},
			Store:       store,
			QuietPeriod: syncQuiet,
			GuardWindow: syncGuardWindow,
			Recursive:   syncRecursive,
		})
		if err != nil {
			return err
		}
		defer orch.Close()

		out := cmd.OutOrStdout()
		mode := "non-recursive"
		if syncRecursive {
			mode = "recursive"
		}
		fmt.Fprintf(out, "sync: watching %s (%s, 4 adapters: claude-code, codex, kilo, hermes); Ctrl-C to stop\n", dir, mode)
		orch.Run(ctx)
		fmt.Fprintln(out, "sync: stopped")
		return nil
	},
}

func init() {
	home, _ := os.UserHomeDir()
	syncCmd.Flags().StringVar(&syncStoreRoot, "store",
		filepath.Join(home, ".aplexica", "store"),
		"Canonical store root directory")
	syncCmd.Flags().StringVar(&syncSecretsRoot, "secrets-root",
		filepath.Join(home, ".aplexica", "secrets"),
		"Secrets store root directory")
	syncCmd.Flags().DurationVar(&syncQuiet, "quiet", 500*time.Millisecond,
		"Debouncer quiet period before reading a settled file")
	syncCmd.Flags().DurationVar(&syncGuardWindow, "guard-window", 5*time.Second,
		"Recursion-guard suppression window for the orchestrator's own writes")
	syncCmd.Flags().BoolVarP(&syncRecursive, "recursive", "r", false,
		"Watch the directory tree recursively (new subdirectories are auto-registered as they appear)")
	rootCmd.AddCommand(syncCmd)
}
