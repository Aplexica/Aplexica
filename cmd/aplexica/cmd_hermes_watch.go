package main

import (
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter/hermes"
	"github.com/aplexica/aplexica/internal/hermeswatch"
	"github.com/aplexica/aplexica/internal/secrets"
	"github.com/spf13/cobra"
)

var (
	hermesWatchInterval  time.Duration
	hermesWatchSince     float64
	hermesWatchDB        string
	hermesWatchStore     string
	hermesWatchSecrets   string
	hermesWatchDirection string
	hermesWatchStateFile string
)

var hermesWatchCmd = &cobra.Command{
	Use:   "watch",
	Short: "Poll Hermes state.db on an interval, auto-export new/updated sessions to the canonical store",
	Long: `aplexica hermes watch runs in the foreground, polling the Hermes session
database (default ~/.hermes/state.db) every --interval seconds. New sessions
and sessions with new messages are exported to the canonical store as ACF
conversation artifacts.

Identity reconciliation makes the import idempotent: a session that already
exists in the store gets an "update" event appended, not a duplicate.

Use --since to seed the high-water mark (default 0 = full scan on first
tick). Use Ctrl-C or SIGTERM to stop cleanly.

To run unattended, install the daemon (which auto-starts hermeswatch when
state.db exists) — see "aplexica daemon install".`,
	RunE: func(cmd *cobra.Command, args []string) error {
		store := &acf.Store{Root: hermesWatchStore}
		if err := store.Init(); err != nil {
			return err
		}
		ss := &secrets.Store{Root: hermesWatchSecrets}
		if err := ss.Init(); err != nil {
			return err
		}
		a := hermes.New()
		a.SecretsStore = ss
		// Stamp the daemon's cloud identity so watch-authored events are
		// publishable by the outbound sweep (see cliCloudDeviceID).
		if deviceID := cliCloudDeviceID(); deviceID != "" {
			a.SetDeviceID(deviceID)
		}

		dir, err := parseDirection(hermesWatchDirection)
		if err != nil {
			return err
		}
		w := &hermeswatch.Watcher{
			Adapter:   a,
			Store:     store,
			DBPath:    hermesWatchDB,
			Interval:  hermesWatchInterval,
			Direction: dir,
			StateFile: hermesWatchStateFile,
			Logger:    stderrLogger{},
		}
		if hermesWatchSince > 0 {
			w.SetHWM(hermesWatchSince)
		}

		ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
		defer cancel()

		fmt.Fprintf(cmd.ErrOrStderr(), "hermeswatch: polling %s every %s (Ctrl-C to stop)\n", hermesWatchDB, hermesWatchInterval)
		return w.Run(ctx)
	},
}

// stderrLogger is a tiny hermeswatch.Logger that writes to stderr.
// Matches the signature expected by hermeswatch.Logger interface.
type stderrLogger struct{}

func (stderrLogger) Info(msg string, args ...any) {
	fmt.Fprintf(os.Stderr, "INFO  hermeswatch: %s %v\n", msg, args)
}

func (stderrLogger) Error(msg string, args ...any) {
	fmt.Fprintf(os.Stderr, "ERROR hermeswatch: %s %v\n", msg, args)
}

func init() {
	home, _ := os.UserHomeDir()
	defaultDB := filepath.Join(home, ".hermes", "state.db")
	defaultStore := filepath.Join(home, ".aplexica", "store")
	defaultSecrets := filepath.Join(home, ".aplexica", "secrets")

	hermesWatchCmd.Flags().StringVar(&hermesWatchDB, "db", defaultDB, "Hermes state.db path")
	hermesWatchCmd.Flags().StringVar(&hermesWatchStore, "store", defaultStore, "Canonical store root")
	hermesWatchCmd.Flags().StringVar(&hermesWatchSecrets, "secrets-root", defaultSecrets, "Secrets store root")
	hermesWatchCmd.Flags().DurationVar(&hermesWatchInterval, "interval", 5*time.Second, "Poll interval")
	hermesWatchCmd.Flags().Float64Var(&hermesWatchSince, "since", 0, "Seed high-water mark (unix epoch seconds); default 0 = full first scan")
	hermesWatchCmd.Flags().StringVar(&hermesWatchDirection, "direction", "both",
		"Sync direction: 'outbound' (state.db → canonical store), 'inbound' (canonical store → state.db), or 'both'")
	hermesWatchCmd.Flags().StringVar(&hermesWatchStateFile, "state-file", "",
		"Persist hwm + seenHeads to this path; empty = in-memory only")

	hermesCmd.AddCommand(hermesWatchCmd)
}

func parseDirection(s string) (hermeswatch.Direction, error) {
	switch s {
	case "outbound":
		return hermeswatch.DirectionOutbound, nil
	case "inbound":
		return hermeswatch.DirectionInbound, nil
	case "both":
		return hermeswatch.DirectionBoth, nil
	default:
		return "", fmt.Errorf("invalid --direction %q (expected outbound | inbound | both)", s)
	}
}
