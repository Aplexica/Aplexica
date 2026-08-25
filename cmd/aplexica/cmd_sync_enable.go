package main

import (
	"fmt"
	"io"
	"sort"

	"github.com/aplexica/aplexica/internal/daemon"
	"github.com/spf13/cobra"
)

// knownAgentNames is the V1 built-in adapter set, used only to warn on
// likely typos in `aplexica sync enable <agent>`. Unknown names are still
// accepted (third-party adapter plugins may add their own).
var knownAgentNames = map[string]struct{}{
	"claude-code": {}, "codex": {}, "hermes": {}, "openclaw": {}, "kilo": {},
}

var syncEnableAll bool
var syncDisableAll bool

// syncEnableCmd: `aplexica sync enable <agent>... | --all`
//
// Implements the FR-03.3 "await config" hand-off: discovery + import happen
// automatically, but cross-agent fan-out to a target agent is withheld until
// the user enables it here.
var syncEnableCmd = &cobra.Command{
	Use:   "enable [agent...]",
	Short: "Allow cross-agent fan-out to the named agent(s) (or --all)",
	Long: `Enable cross-agent sync (fan-out) for one or more discovered agents.

Aplexica discovers installed agents and imports their state automatically, but
it does NOT move data between agents until you enable sync here (the
"discover, then await config" default).

  aplexica sync enable codex          # fan out to codex
  aplexica sync enable codex hermes    # fan out to several
  aplexica sync enable --all           # fan out to every installed agent

Run 'aplexica daemon reload' afterward to apply.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfgPath, err := daemonConfigPath()
		if err != nil {
			return err
		}
		return setSyncEnabled(cfgPath, args, syncEnableAll, true, cmd.OutOrStdout())
	},
}

// syncDisableCmd: `aplexica sync disable <agent>... | --all`
var syncDisableCmd = &cobra.Command{
	Use:   "disable [agent...]",
	Short: "Withhold cross-agent fan-out from the named agent(s) (or --all)",
	Long: `Disable cross-agent sync (fan-out) for one or more agents. The agent is
still discovered and its state still imports into the canonical store; only
the outbound export to it is withheld.

  aplexica sync disable codex
  aplexica sync disable --all

Run 'aplexica daemon reload' afterward to apply.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfgPath, err := daemonConfigPath()
		if err != nil {
			return err
		}
		return setSyncEnabled(cfgPath, args, syncDisableAll, false, cmd.OutOrStdout())
	},
}

// syncAgentsCmd: `aplexica sync agents` — show per-agent fan-out enablement.
var syncAgentsCmd = &cobra.Command{
	Use:   "agents",
	Short: "Show which agents are enabled for cross-agent fan-out",
	RunE: func(cmd *cobra.Command, _ []string) error {
		cfgPath, err := daemonConfigPath()
		if err != nil {
			return err
		}
		cfg, err := daemon.LoadConfig(cfgPath)
		if err != nil {
			return err
		}
		out := cmd.OutOrStdout()
		fmt.Fprintf(out, "Fan-out (await-config gate):\n")
		fmt.Fprintf(out, "  all agents: %v\n", cfg.Sync.All)
		if len(cfg.Sync.Agents) == 0 {
			fmt.Fprintln(out, "  per-agent overrides: (none)")
		} else {
			fmt.Fprintln(out, "  per-agent overrides:")
			names := make([]string, 0, len(cfg.Sync.Agents))
			for n := range cfg.Sync.Agents {
				names = append(names, n)
			}
			sort.Strings(names)
			for _, n := range names {
				state := "disabled"
				if cfg.Sync.Agents[n] {
					state = "enabled"
				}
				fmt.Fprintf(out, "    %-12s %s\n", n, state)
			}
		}
		if !cfg.Sync.All && len(cfg.Sync.Agents) == 0 {
			fmt.Fprintln(out, "\nNo agents enabled — discovered agents import to the store but")
			fmt.Fprintln(out, "fan-out is withheld. Enable with: aplexica sync enable <agent> | --all")
		}
		return nil
	},
}

// setSyncEnabled mutates cfg.Sync at cfgPath. Factored out of the cobra RunE
// so it is unit-testable against a temp config path.
func setSyncEnabled(cfgPath string, agents []string, all, enabled bool, w io.Writer) error {
	if !all && len(agents) == 0 {
		return fmt.Errorf("specify one or more agent names, or --all")
	}
	cfg, err := daemon.LoadConfig(cfgPath)
	if err != nil {
		return err
	}
	if all {
		cfg.Sync.All = enabled
	} else {
		if cfg.Sync.Agents == nil {
			cfg.Sync.Agents = map[string]bool{}
		}
		for _, a := range agents {
			if _, known := knownAgentNames[a]; !known {
				fmt.Fprintf(w, "note: %q is not a built-in agent name; setting it anyway\n", a)
			}
			cfg.Sync.Agents[a] = enabled
		}
	}
	if err := daemon.WriteConfig(cfgPath, cfg); err != nil {
		return err
	}
	verb := "enabled"
	if !enabled {
		verb = "disabled"
	}
	if all {
		fmt.Fprintf(w, "Cross-agent fan-out %s for all installed agents.\n", verb)
	} else {
		fmt.Fprintf(w, "Cross-agent fan-out %s for: %v\n", verb, agents)
	}
	fmt.Fprintln(w, "Reload the daemon to apply:")
	fmt.Fprintln(w, "  aplexica daemon reload")
	return nil
}

func init() {
	syncEnableCmd.Flags().BoolVar(&syncEnableAll, "all", false, "apply to every installed agent")
	syncDisableCmd.Flags().BoolVar(&syncDisableAll, "all", false, "apply to every installed agent")
	syncCmd.AddCommand(syncEnableCmd, syncDisableCmd, syncAgentsCmd)
}
