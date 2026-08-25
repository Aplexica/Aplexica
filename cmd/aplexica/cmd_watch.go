package main

import (
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/watcher"
	"github.com/spf13/cobra"
)

var (
	watchStoreRoot   string
	watchSecretsRoot string
	watchQuiet       time.Duration
	watchRecursive   bool
)

var watchCmd = &cobra.Command{
	Use:   "watch <adapter> <dir>",
	Short: "Watch a directory for changes and import each settled file via the named adapter",
	Long: `Watches a single directory (non-recursive) for filesystem events. Each event
is debounced: events for the same path within the quiet period collapse
into a single logical change, and a content-hash dedup suppresses events
that didn't actually change the file. When a file settles, it is
imported via the named adapter's Import method — the same code path the
` + "`aplexica import`" + ` subcommand uses.

This command does NOT fan out changes to other adapters, does NOT run as
a daemon, and does NOT auto-start at login.

Ctrl-C stops the watcher cleanly.`,
	Args: cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		adapterName := args[0]
		dir := args[1]

		store := &acf.Store{Root: watchStoreRoot}
		if err := store.Init(); err != nil {
			return err
		}
		ad, err := buildAdapter(adapterName, watchSecretsRoot)
		if err != nil {
			return err
		}

		ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
		defer cancel()

		out := cmd.OutOrStdout()

		absDir, _ := filepath.Abs(dir)
		onSettled := func(path string) {
			ids, err := ad.Import(ctx, store, path)
			// Display a relative path when possible so log lines include subdir context.
			displayPath := path
			if rel, relErr := filepath.Rel(absDir, path); relErr == nil {
				displayPath = rel
			}
			if err != nil {
				fmt.Fprintf(out, "watch: skip %s: %v\n", displayPath, err)
				return
			}
			for _, id := range ids {
				fmt.Fprintf(out, "watch: imported %s (%s) → %s\n",
					displayPath, adapterName, id)
			}
		}

		debouncer := watcher.NewDebouncer(watchQuiet, onSettled)
		defer debouncer.Stop()

		var w *watcher.Watcher
		var srcCloser func() error
		if watchRecursive {
			src, serr := watcher.NewRecursiveSource(dir)
			if serr != nil {
				return serr
			}
			w = watcher.NewWatcherWithSource(src, debouncer)
			srcCloser = src.Close
		} else {
			ww, werr := watcher.NewWatcher(dir, debouncer)
			if werr != nil {
				return werr
			}
			w = ww
			srcCloser = w.Close
		}
		defer srcCloser()
		w.OnError = func(werr error) {
			fmt.Fprintf(out, "watch: error: %v\n", werr)
		}

		mode := "non-recursive"
		if watchRecursive {
			mode = "recursive"
		}
		fmt.Fprintf(out, "watch: monitoring %s for %s (%s, quiet=%v); Ctrl-C to stop\n",
			dir, adapterName, mode, watchQuiet)
		w.Run(ctx)
		fmt.Fprintln(out, "watch: stopped")
		return nil
	},
}

func init() {
	home, _ := os.UserHomeDir()
	watchCmd.Flags().StringVar(&watchStoreRoot, "store",
		filepath.Join(home, ".aplexica", "store"),
		"Canonical store root directory")
	watchCmd.Flags().StringVar(&watchSecretsRoot, "secrets-root",
		filepath.Join(home, ".aplexica", "secrets"),
		"Secrets store root directory")
	watchCmd.Flags().DurationVar(&watchQuiet, "quiet", 500*time.Millisecond,
		"Quiet period before reading a settled file")
	watchCmd.Flags().BoolVarP(&watchRecursive, "recursive", "r", false,
		"Watch the directory tree recursively (new subdirectories are auto-registered as they appear)")
	rootCmd.AddCommand(watchCmd)
}
