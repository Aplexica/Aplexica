package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"text/tabwriter"

	"github.com/aplexica/aplexica/internal/config"
	"github.com/spf13/cobra"
)

// Per BRD-10 §10.3 / FR-10.10 the CLI MUST support:
//
//   aplexica config show [--key <path>]
//   aplexica config set <key> <value> [--layer user|system|project]
//   aplexica config unset <key> [--layer user|system|project]
//   aplexica config diff
//   aplexica config validate <file>
//   aplexica config edit
//
// `show` MUST display provenance per key. `set` / `unset` default to the
// user layer when --layer is omitted.

var (
	configSystemPath  string
	configUserPath    string
	configProjectPath string
	configShowKey     string
	configSetLayer    string
	configUnsetLayer  string
	configCLISets     []string // -c / --config-set key=value, repeatable
)

var configCmd = &cobra.Command{
	Use:   "config",
	Short: "Show and edit the layered configuration",
	Long: `Read, write, diff, validate, and edit the configuration.

Aplexica reads configuration from six layers in precedence (low → high):
  1. Shipped defaults (embedded defaults.toml; read-only)
  2. System          /etc/aplexica/config.toml | %PROGRAMDATA%\Aplexica\config.toml
  3. User            ~/.aplexica/config.toml
  4. Project         <project-root>/.aplexica/config.toml
  5. Environment     APLEXICA_<KEY>=<value>  (not yet wired)
  6. CLI flags       --config-set <key>=<value> (not yet wired)

Each effective value records which layer set it ("provenance"), so
'config show' can answer "why is daemon.project_scan_interval 30m?".`,
}

var configShowCmd = &cobra.Command{
	Use:   "show",
	Short: "Print the effective merged config with provenance per key",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		eff, err := config.Load(currentLoadOpts())
		if err != nil {
			return err
		}
		out := cmd.OutOrStdout()
		if configShowKey != "" {
			v, layer, ok := eff.Get(configShowKey)
			if !ok {
				return fmt.Errorf("config: key %q is not defined", configShowKey)
			}
			fmt.Fprintf(out, "%s = %s   (from %s)\n", configShowKey, v, layer)
			return nil
		}
		w := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
		fmt.Fprintln(w, "KEY\tVALUE\tFROM")
		for _, k := range eff.Keys() {
			v, layer, _ := eff.Get(k)
			fmt.Fprintf(w, "%s\t%s\t%s\n", k, v, layer)
		}
		return w.Flush()
	},
}

var configSetCmd = &cobra.Command{
	Use:   "set <key> <value>",
	Short: "Write a key=value pair to the named layer (default: user)",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		layer := configSetLayer
		if layer == "" {
			layer = "user"
		}
		path, err := resolveLayerPath(layer)
		if err != nil {
			return err
		}
		// ParseLayer validates the layer name is one of system/user/project.
		if _, err := config.ParseLayer(layer); err != nil {
			return err
		}
		if err := config.SetKey(path, args[0], args[1]); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "wrote %s = %s   (in %s layer at %s)\n",
			args[0], args[1], layer, path)
		return nil
	},
}

var configUnsetCmd = &cobra.Command{
	Use:   "unset <key>",
	Short: "Remove a key from the named layer (default: user)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		layer := configUnsetLayer
		if layer == "" {
			layer = "user"
		}
		path, err := resolveLayerPath(layer)
		if err != nil {
			return err
		}
		if _, err := config.ParseLayer(layer); err != nil {
			return err
		}
		if err := config.UnsetKey(path, args[0]); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "removed %s from %s layer at %s\n",
			args[0], layer, path)
		return nil
	},
}

var configDiffCmd = &cobra.Command{
	Use:   "diff",
	Short: "Show keys where the effective value differs from the shipped default",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		defs, err := config.DefaultsEffective()
		if err != nil {
			return err
		}
		eff, err := config.Load(currentLoadOpts())
		if err != nil {
			return err
		}
		out := cmd.OutOrStdout()
		w := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
		fmt.Fprintln(w, "KEY\tDEFAULT\tEFFECTIVE\tFROM")
		// Walk the union of keys so an override that names a key absent
		// from defaults is still visible (e.g. user added a tunable
		// before the defaults.toml has it).
		seen := map[string]bool{}
		var all []string
		for _, k := range defs.Keys() {
			if !seen[k] {
				all = append(all, k)
				seen[k] = true
			}
		}
		for _, k := range eff.Keys() {
			if !seen[k] {
				all = append(all, k)
				seen[k] = true
			}
		}
		sort.Strings(all)
		diffs := 0
		for _, k := range all {
			dv, _, dok := defs.Get(k)
			ev, layer, _ := eff.Get(k)
			if dok && layer == config.LayerShipped && dv == ev {
				continue
			}
			if !dok {
				fmt.Fprintf(w, "%s\t(unset)\t%s\t%s\n", k, ev, layer)
				diffs++
				continue
			}
			if dv != ev || layer != config.LayerShipped {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", k, dv, ev, layer)
				diffs++
			}
		}
		if err := w.Flush(); err != nil {
			return err
		}
		fmt.Fprintf(out, "\n%d keys differ from shipped defaults.\n", diffs)
		return nil
	},
}

var configValidateCmd = &cobra.Command{
	Use:   "validate <file>",
	Short: "Range-check a TOML file against the embedded schema",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		body, err := os.ReadFile(args[0])
		if err != nil {
			return fmt.Errorf("validate: %w", err)
		}
		errs, warns, err := config.ValidateBody(body)
		if err != nil {
			return err
		}
		out := cmd.OutOrStdout()
		for _, w := range warns {
			fmt.Fprintln(cmd.ErrOrStderr(), "WARN:", w)
		}
		if len(errs) > 0 {
			for _, e := range errs {
				fmt.Fprintln(cmd.ErrOrStderr(), "ERR :", e)
			}
			return fmt.Errorf("validate: %d schema violation(s) in %s", len(errs), args[0])
		}
		fmt.Fprintf(out, "ok: %s passes schema validation (%d warning(s))\n", args[0], len(warns))
		return nil
	},
}

var configSchemaCmd = &cobra.Command{
	Use:   "schema",
	Short: "Print the embedded configuration schema as JSON",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		b, err := config.SchemaJSON()
		if err != nil {
			return err
		}
		fmt.Fprintln(cmd.OutOrStdout(), string(b))
		return nil
	},
}

var configDocsCmd = &cobra.Command{
	Use:   "docs",
	Short: "Print the configuration schema as human-readable Markdown",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		fmt.Fprint(cmd.OutOrStdout(), config.SchemaMarkdown())
		return nil
	},
}

var configEditCmd = &cobra.Command{
	Use:   "edit",
	Short: "Open the user-layer config in $EDITOR (or $VISUAL); re-validate on save",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		path, err := resolveLayerPath("user")
		if err != nil {
			return err
		}
		if _, err := os.Stat(path); os.IsNotExist(err) {
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(path, []byte(
				"# Aplexica user config (override shipped defaults here).\n"+
					"# See `aplexica config show` for the full list of keys.\n"),
				0o644); err != nil {
				return err
			}
		}
		editor := os.Getenv("VISUAL")
		if editor == "" {
			editor = os.Getenv("EDITOR")
		}
		if editor == "" {
			editor = "vi"
		}
		c := exec.Command(editor, path)
		c.Stdin = os.Stdin
		c.Stdout = cmd.OutOrStdout()
		c.Stderr = cmd.ErrOrStderr()
		if err := c.Run(); err != nil {
			return fmt.Errorf("config edit: %s exited with: %w", editor, err)
		}
		// Re-validate on save.
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := config.Validate(body); err != nil {
			return fmt.Errorf("config edit: file did not parse after edit: %w", err)
		}
		fmt.Fprintln(cmd.OutOrStdout(), "ok: user config is valid")
		return nil
	},
}

// currentLoadOpts assembles the LoadOptions from the persistent flags
// (or platform defaults when a flag is left empty). Env layer is taken
// from os.Environ() so APLEXICA_<KEY>=<value> overrides surface to
// `config show` automatically.
func currentLoadOpts() config.LoadOptions {
	sys, usr, proj := config.DefaultSources()
	if configSystemPath != "" {
		sys = configSystemPath
	}
	if configUserPath != "" {
		usr = configUserPath
	}
	if configProjectPath != "" {
		proj = configProjectPath
	}
	return config.LoadOptions{
		SystemPath:   sys,
		UserPath:     usr,
		ProjectPath:  proj,
		Env:          os.Environ(),
		CLIOverrides: configCLISets,
	}
}

// resolveLayerPath returns the on-disk path for the named writeable
// layer, honoring any --system-path / --user-path / --project-path
// overrides.
func resolveLayerPath(layer string) (string, error) {
	sys, usr, _ := config.DefaultSources()
	switch layer {
	case "system":
		if configSystemPath != "" {
			return configSystemPath, nil
		}
		return sys, nil
	case "user":
		if configUserPath != "" {
			return configUserPath, nil
		}
		if usr == "" {
			return "", fmt.Errorf("config: cannot resolve user-config path (no $HOME?)")
		}
		return usr, nil
	case "project":
		if configProjectPath == "" {
			return "", fmt.Errorf("config: --project-path is required when --layer=project")
		}
		return configProjectPath, nil
	}
	return "", fmt.Errorf("config: unknown layer %q", layer)
}

func init() {
	configCmd.PersistentFlags().StringVar(&configSystemPath, "system-path", "",
		"override the system-layer config path (default: platform-specific)")
	configCmd.PersistentFlags().StringVar(&configUserPath, "user-path", "",
		"override the user-layer config path (default: ~/.aplexica/config.toml)")
	configCmd.PersistentFlags().StringVar(&configProjectPath, "project-path", "",
		"project-layer config path (no default — supply when in a project context)")
	configCmd.PersistentFlags().StringSliceVarP(&configCLISets, "config-set", "c", nil,
		"override key=value, repeatable; highest-precedence layer")

	configShowCmd.Flags().StringVar(&configShowKey, "key", "",
		"print only this key (otherwise list every effective key)")

	configSetCmd.Flags().StringVar(&configSetLayer, "layer", "user",
		"target layer (system|user|project)")
	configUnsetCmd.Flags().StringVar(&configUnsetLayer, "layer", "user",
		"target layer (system|user|project)")

	configCmd.AddCommand(configShowCmd, configSetCmd, configUnsetCmd,
		configDiffCmd, configValidateCmd, configEditCmd,
		configSchemaCmd, configDocsCmd)
	rootCmd.AddCommand(configCmd)
}
