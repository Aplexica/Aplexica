package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/syncrules"
	"github.com/spf13/cobra"
)

var (
	rulesPath      string
	rulesStoreRoot string
	rulesJSON      bool
)

var rulesCmd = &cobra.Command{
	Use:   "rules",
	Short: "Manage selective-sync rules",
	Long: `The selective-sync rule engine decides which artifacts the daemon may
fan out to which agents. Rules live in a TOML file
(~/.aplexica/rules.toml). Safe-by-default: no rules ship as
always-on defaults, so a fresh install fans out NOWHERE until the user
adds a rule. The classic defaults are available as opt-in presets.

Commands:
  list                  list active rules with provenance + match summary
  add <file>            append rules from a TOML file
  edit                  open the user rules file in $EDITOR
  remove <name>         remove a user-defined rule by name
  test <artifact-id>    evaluate every rule against the artifact and
                        explain the decision
`,
}

var rulesListCmd = &cobra.Command{
	Use:   "list",
	Short: "List the active (user-defined) rules in declaration order",
	Long: `Lists the rules in ~/.aplexica/rules.toml in declaration order.

Safe-by-default: no rules ship as always-on defaults, so a
fresh install lists nothing and the daemon fans out nowhere until a
rule is added. The classic defaults are available as opt-in presets.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		all, err := loadAllRules(rulesPath)
		if err != nil {
			return err
		}
		if rulesJSON {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(all)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "%-3s  %-32s  %-8s  %s\n", "#", "NAME", "SOURCE", "SUMMARY")
		for i, r := range all {
			fmt.Fprintf(cmd.OutOrStdout(), "%-3d  %-32s  %-8s  %s\n", i+1, r.Name, "user", summariseRule(r))
		}
		return nil
	},
}

// rejectReservedAssignTags returns an error if any rule's assign.tags
// entry uses a reserved namespace. Reserved tags (aplexica:* / fork-of:*
// / device:* / conflict:*) carry system routing/merge semantics and may
// not be written by user-facing surfaces (FR-05.11 / BRD-05 §4.2).
func rejectReservedAssignTags(rules []syncrules.Rule) error {
	for _, r := range rules {
		for _, t := range r.Assign.Tags {
			if acf.IsReservedArtifactTag(t) {
				return fmt.Errorf("rule %q: assign.tags entry %q uses a reserved namespace (aplexica:* / fork-of:* / device:* / conflict:* are system-only)", r.Name, t)
			}
		}
	}
	return nil
}

var rulesAddCmd = &cobra.Command{
	Use:   "add <file>",
	Short: "Append rules from a TOML file to the user rules",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		data, err := os.ReadFile(args[0])
		if err != nil {
			return fmt.Errorf("read %s: %w", args[0], err)
		}
		// Parse to validate before merging.
		fragment, err := syncrules.Parse(data)
		if err != nil {
			return err
		}
		// FR-05.11 / BRD-05 §4.2: a tag-assigning rule is a user-facing
		// tag-writing surface, so reject reserved-namespace assign.tags
		// (aplexica:* / fork-of:* / device:* / conflict:*) at creation
		// time — same guard the `tag add` command enforces.
		if err := rejectReservedAssignTags(fragment.Sync.Rules); err != nil {
			return err
		}
		user, err := loadUserRules(rulesPath)
		if err != nil {
			return err
		}
		// Detect duplicate names across user + new fragment.
		seen := map[string]bool{}
		for _, r := range user.Sync.Rules {
			seen[r.Name] = true
		}
		for _, r := range fragment.Sync.Rules {
			if seen[r.Name] {
				return fmt.Errorf("rule %q already exists in user rules", r.Name)
			}
		}
		user.Sync.Rules = append(user.Sync.Rules, fragment.Sync.Rules...)
		if err := writeUserRules(rulesPath, user); err != nil {
			return err
		}
		for _, r := range fragment.Sync.Rules {
			_ = journalRuleChange(rulesPath, "add", map[string]any{"name": r.Name})
		}
		fmt.Fprintf(cmd.OutOrStdout(), "added %d rule(s) to %s\n", len(fragment.Sync.Rules), rulesPath)
		return nil
	},
}

var rulesRemoveCmd = &cobra.Command{
	Use:   "remove <name>",
	Short: "Remove a user-defined rule by name (shipped defaults cannot be removed; override instead)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		user, err := loadUserRules(rulesPath)
		if err != nil {
			return err
		}
		name := args[0]
		filtered := user.Sync.Rules[:0]
		removed := false
		for _, r := range user.Sync.Rules {
			if r.Name == name {
				removed = true
				continue
			}
			filtered = append(filtered, r)
		}
		if !removed {
			return fmt.Errorf("no user rule named %q (shipped defaults cannot be removed; override with a higher-precedence rule instead)", name)
		}
		user.Sync.Rules = filtered
		if err := writeUserRules(rulesPath, user); err != nil {
			return err
		}
		_ = journalRuleChange(rulesPath, "remove", map[string]any{"name": name})
		fmt.Fprintf(cmd.OutOrStdout(), "removed rule %q from %s\n", name, rulesPath)
		return nil
	},
}

var rulesEditCmd = &cobra.Command{
	Use:   "edit",
	Short: "Open the user rules file in $EDITOR (creates if absent)",
	RunE: func(cmd *cobra.Command, args []string) error {
		editor := os.Getenv("EDITOR")
		if editor == "" {
			editor = "vi"
		}
		if _, err := os.Stat(rulesPath); errors.Is(err, os.ErrNotExist) {
			if err := os.MkdirAll(filepath.Dir(rulesPath), 0o755); err != nil {
				return err
			}
			seed := "# Aplexica user sync rules (safe-by-default: no rules = no fan-out).\n\n"
			if err := os.WriteFile(rulesPath, []byte(seed), 0o644); err != nil {
				return err
			}
		}
		c := exec.Command(editor, rulesPath)
		c.Stdin, c.Stdout, c.Stderr = os.Stdin, cmd.OutOrStdout(), cmd.ErrOrStderr()
		if err := c.Run(); err != nil {
			return fmt.Errorf("editor %s exited: %w", editor, err)
		}
		// Validate after edit.
		if _, err := loadUserRules(rulesPath); err != nil {
			return fmt.Errorf("rules file failed validation after edit: %w", err)
		}
		_ = journalRuleChange(rulesPath, "edit", nil)
		return nil
	},
}

// rules apply --retroactive — FR-05.15.
var (
	rulesApplyRetroactive bool
	rulesApplyRuleName    string
	rulesApplyDryRun      bool
)

var rulesApplyCmd = &cobra.Command{
	Use:   "apply",
	Short: "Apply tag-assigning rules retroactively to existing artifacts",
	Long: `By default, new tag-assigning rules apply only to artifacts ingested
AFTER the rule is added. This command opts in to retroactive
application:

  aplexica rules apply --retroactive [--rule <name>] [--dry-run]

--dry-run prints the per-artifact changes without writing.
--rule <name> scopes the retroactive pass to one specific rule.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if !rulesApplyRetroactive {
			return fmt.Errorf("--retroactive is required")
		}
		store := &acf.Store{Root: rulesStoreRoot}
		if err := store.Init(); err != nil {
			return err
		}
		rules, err := loadAllRules(rulesPath)
		if err != nil {
			return err
		}
		if rulesApplyRuleName != "" {
			filtered := rules[:0]
			for _, r := range rules {
				if r.Name == rulesApplyRuleName {
					filtered = append(filtered, r)
				}
			}
			if len(filtered) == 0 {
				return fmt.Errorf("rule %q not found", rulesApplyRuleName)
			}
			rules = filtered
		}
		eng, err := syncrules.New(rules)
		if err != nil {
			return err
		}
		type change struct {
			ArtifactID string   `json:"artifactId"`
			Kind       string   `json:"kind"`
			Added      []string `json:"added"`
		}
		var report []change
		for _, k := range []acf.Kind{acf.KindMemory, acf.KindSkill, acf.KindTool, acf.KindConversation} {
			arts, err := store.ListArtifacts(k)
			if err != nil {
				return err
			}
			for _, a := range arts {
				input := syncrules.Artifact{
					ArtifactID: a.ArtifactID,
					Kind:       string(k),
					Type:       string(k),
					Tags:       a.Tags,
					ScopeKind:  string(a.Scope),
				}
				if a.Project != nil {
					input.ProjectID = a.Project.ID
					input.ProjectEphemeral = a.Project.Ephemeral
				}
				// Recover origin agent from the genesis event's Provenance —
				// needed for match.agentSource predicates. Use the
				// including-compacted view (BRD-03 §4.8): after retention/GC
				// moves an artifact's earliest events into the .compacted layer,
				// ReadEvents()[0] is only the oldest SURVIVING event (often a
				// snapshot with empty provenance), so the active-only read would
				// resolve the wrong origin and drift from live fan-out (FR-05.6).
				// ReadEventsIncludingCompacted re-merges both layers and sorts by
				// timestamp, restoring the true genesis create as events[0].
				if events, eerr := store.ReadEventsIncludingCompacted(k, a.ArtifactID); eerr == nil && len(events) > 0 {
					input.OriginAgent = events[0].Provenance.SourceAgent
					input.OriginDevice = events[0].Provenance.DeviceID
				}
				dec := eng.Evaluate(input, syncrules.EvaluateOpts{
					InstalledAgents: []string{"claude-code", "codex", "hermes", "openclaw", "kilo"},
				})
				existing := map[string]struct{}{}
				for _, t := range a.Tags {
					existing[t] = struct{}{}
				}
				var added []string
				for _, t := range dec.AssignedTags {
					// Defense-in-depth: never write a reserved-namespace tag
					// onto an artifact, even if a rule somehow carries one
					// (FR-05.11 / BRD-05 §4.2). rules add rejects these at
					// creation; skip here in case an older rules file predates
					// that guard.
					if acf.IsReservedArtifactTag(t) {
						continue
					}
					if _, ok := existing[t]; !ok {
						added = append(added, t)
					}
				}
				if len(added) == 0 {
					continue
				}
				report = append(report, change{
					ArtifactID: a.ArtifactID,
					Kind:       string(k),
					Added:      added,
				})
				if !rulesApplyDryRun {
					a.Tags = append(a.Tags, added...)
					if err := store.WriteArtifact(a); err != nil {
						return err
					}
				}
			}
		}
		if rulesJSON {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{
				"dryRun":  rulesApplyDryRun,
				"changes": report,
				"count":   len(report),
			})
		}
		mode := "applied"
		if rulesApplyDryRun {
			mode = "would apply"
		}
		fmt.Fprintf(cmd.OutOrStdout(), "%s changes to %d artifact(s):\n", mode, len(report))
		for _, c := range report {
			fmt.Fprintf(cmd.OutOrStdout(), "  %s/%s  +%v\n", c.Kind, c.ArtifactID, c.Added)
		}
		return nil
	},
}

var rulesTestCmd = &cobra.Command{
	Use:   "test <artifact-id>",
	Short: "Show every rule that matches an artifact and the merged routing decision",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		store := &acf.Store{Root: rulesStoreRoot}
		if err := store.Init(); err != nil {
			return err
		}
		kind, art, found, err := findArtifactByID(store, args[0])
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("artifact %s not found", args[0])
		}
		rules, err := loadAllRules(rulesPath)
		if err != nil {
			return err
		}
		eng, err := syncrules.New(rules)
		if err != nil {
			return err
		}
		input := syncrules.Artifact{
			ArtifactID: art.ArtifactID,
			Kind:       string(kind),
			Type:       string(kind),
			Tags:       art.Tags,
			ScopeKind:  string(art.Scope),
		}
		if art.Project != nil {
			input.ProjectID = art.Project.ID
			input.ProjectEphemeral = art.Project.Ephemeral
		}
		// Reconstruct the origin from the genesis event's Provenance. Use the
		// including-compacted view so retention/GC compaction (BRD-03 §4.8) of
		// the earliest events does not shift events[0] to the oldest surviving
		// event — that would make the __originatingAgent__ token and
		// match.agentSource explanations drift from live fan-out (FR-05.6).
		if events, eerr := store.ReadEventsIncludingCompacted(kind, art.ArtifactID); eerr == nil && len(events) > 0 {
			input.OriginAgent = events[0].Provenance.SourceAgent
			input.OriginDevice = events[0].Provenance.DeviceID
		}
		decision := eng.Evaluate(input, syncrules.EvaluateOpts{
			InstalledAgents: []string{"claude-code", "codex", "hermes", "openclaw", "kilo"},
		})
		if rulesJSON {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{
				"artifact": input,
				"decision": decision,
			})
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Artifact %s (kind=%s, tags=%v):\n", art.ArtifactID, kind, art.Tags)
		fmt.Fprintf(cmd.OutOrStdout(), "  matched rules: %s\n", strings.Join(decision.MatchedRules, ", "))
		fmt.Fprintf(cmd.OutOrStdout(), "  allowed agents: %s\n", strings.Join(decision.AllowedAgents, ", "))
		fmt.Fprintf(cmd.OutOrStdout(), "  denied agents:  %s\n", strings.Join(decision.DeniedAgents, ", "))
		fmt.Fprintf(cmd.OutOrStdout(), "  assigned tags:  %s\n", strings.Join(decision.AssignedTags, ", "))
		fmt.Fprintf(cmd.OutOrStdout(), "  remote-allowed: %t\n", decision.RemoteAllowed)
		fmt.Fprintf(cmd.OutOrStdout(), "  mode:           %s\n", decision.Mode)
		fmt.Fprintf(cmd.OutOrStdout(), "  skillMode:      %s\n", decision.SkillMode)
		inc := "(unset)"
		if decision.IncludeSecrets != nil {
			inc = fmt.Sprintf("%t", *decision.IncludeSecrets)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "  includeSecrets: %s\n", inc)
		return nil
	},
}

// loadUserRules reads the user's rules TOML file, returning an empty
// Config when the file is absent.
func loadUserRules(path string) (syncrules.Config, error) {
	var out syncrules.Config
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return out, fmt.Errorf("read user rules: %w", err)
	}
	cfg, err := syncrules.Parse(data)
	if err != nil {
		return cfg, err
	}
	return cfg, nil
}

// loadAllRules returns the user rules in declaration order.
//
// Safe-by-default (reverses BRD-05 §6 #1): the shipped defaults are no
// longer merged in — they are offered as opt-in presets instead (see
// syncrules.ParseDefault / DefaultRulesTOML). On a fresh install this
// is empty, so `GET /api/rules` and `aplexica rules list` show only
// what the user has explicitly added.
func loadAllRules(userPath string) ([]syncrules.Rule, error) {
	user, err := loadUserRules(userPath)
	if err != nil {
		return nil, err
	}
	return append([]syncrules.Rule{}, user.Sync.Rules...), nil
}

// writeUserRules serialises a Config back to TOML at path.
func writeUserRules(path string, cfg syncrules.Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	header := "# Aplexica user sync rules (auto-generated; safe to hand-edit).\n# Safe-by-default: with no rules, the daemon fans out nowhere.\n\n"
	if _, err := f.WriteString(header); err != nil {
		return err
	}
	enc := toml.NewEncoder(f)
	return enc.Encode(cfg)
}

// summariseRule returns a one-line summary of a rule for `rules list`.
func summariseRule(r syncrules.Rule) string {
	parts := []string{}
	if len(r.Match.Tag) > 0 {
		parts = append(parts, "tag="+strings.Join(r.Match.Tag, ","))
	}
	if len(r.Match.Type) > 0 {
		parts = append(parts, "type="+strings.Join(r.Match.Type, ","))
	}
	if len(r.Match.AgentSource) > 0 {
		parts = append(parts, "agentSource="+strings.Join(r.Match.AgentSource, ","))
	}
	if len(r.Route.Agents) > 0 {
		parts = append(parts, "→"+strings.Join(r.Route.Agents, ","))
	}
	if r.Route.Remote == "exclude" {
		parts = append(parts, "remote=exclude")
	}
	if len(r.Assign.Tags) > 0 {
		parts = append(parts, "assign="+strings.Join(r.Assign.Tags, ","))
	}
	if len(parts) == 0 {
		return "(catch-all)"
	}
	return strings.Join(parts, " ")
}

// journalRuleChange writes one JSONL line to
// ~/.aplexica/logs/rule-changes.jsonl per FR-05.13.
func journalRuleChange(rulesFile, op string, fields map[string]any) error {
	logsDir := filepath.Join(filepath.Dir(filepath.Dir(rulesFile)), "logs")
	if err := os.MkdirAll(logsDir, 0o755); err != nil {
		return err
	}
	entry := map[string]any{
		"at":        time.Now().UTC().Format(time.RFC3339Nano),
		"op":        op,
		"rulesFile": rulesFile,
	}
	for k, v := range fields {
		entry[k] = v
	}
	data, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(logsDir, "rule-changes.jsonl"),
		os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(data, '\n'))
	return err
}

func init() {
	home, _ := os.UserHomeDir()
	defaultRulesPath := filepath.Join(home, ".aplexica", "rules.toml")
	defaultStore := filepath.Join(home, ".aplexica", "store")

	for _, c := range []*cobra.Command{rulesListCmd, rulesAddCmd, rulesRemoveCmd, rulesEditCmd, rulesTestCmd, rulesApplyCmd} {
		c.Flags().StringVar(&rulesPath, "rules-file", defaultRulesPath, "Path to the user rules.toml")
	}
	rulesListCmd.Flags().BoolVar(&rulesJSON, "json", false, "Emit JSON instead of plain text")
	rulesTestCmd.Flags().BoolVar(&rulesJSON, "json", false, "Emit JSON instead of plain text")
	rulesTestCmd.Flags().StringVar(&rulesStoreRoot, "store", defaultStore, "Canonical store root")
	rulesApplyCmd.Flags().BoolVar(&rulesApplyRetroactive, "retroactive", false,
		"Required: opt in to retroactive application")
	rulesApplyCmd.Flags().StringVar(&rulesApplyRuleName, "rule", "",
		"Limit retroactive application to one named rule (default: every tag-assigning rule)")
	rulesApplyCmd.Flags().BoolVar(&rulesApplyDryRun, "dry-run", false,
		"Print what would change without modifying any artifact")
	rulesApplyCmd.Flags().BoolVar(&rulesJSON, "json", false, "Emit JSON report instead of plain text")
	rulesApplyCmd.Flags().StringVar(&rulesStoreRoot, "store", defaultStore, "Canonical store root")

	rulesCmd.AddCommand(rulesListCmd, rulesAddCmd, rulesRemoveCmd, rulesEditCmd, rulesTestCmd, rulesApplyCmd)
	rootCmd.AddCommand(rulesCmd)
}
