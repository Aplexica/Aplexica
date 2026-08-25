package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/spf13/cobra"
)

// Pin / unpin attach (or remove) a retention pin tag on an artifact. A
// pinned artifact carries retention.Config.PinTags[0] (default "pinned")
// in acf.Artifact.Tags, which PruneArtifact and EvictAttachments already
// honor as an exemption from all pruning/eviction (BRD-03 §4.8.2/§4.8.4).
//
// The kind is auto-detected by probing each kind in turn for the given ID,
// mirroring `aplexica snapshot <id>`.

var pinStoreRoot string

// pinTagFlag overrides which configured retention pin tag is applied/removed
// (BRD-03 §4.8.5 `--tag`). Empty => retention.pin_tags[0] via pinTag().
var pinTagFlag string

// defaultPinTag is the pin tag used when the resolved retention config has
// no pin tags configured (defensive; DefaultConfig always supplies "pinned"
// as PinTags[0]).
const defaultPinTag = "pinned"

var pinCmd = &cobra.Command{
	Use:   "pin <id>",
	Short: "Exempt an artifact from retention pruning/eviction",
	Long: `Add the retention pin tag (retention.pin_tags[0], default "pinned") to an
artifact's Tags. Pinned artifacts are exempt from snapshot-driven pruning and
attachment eviction. Idempotent: a no-op if the artifact is already pinned.

The kind is auto-detected by probing each kind in turn for the given ID.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runPin(cmd, args[0], true)
	},
}

var unpinCmd = &cobra.Command{
	Use:   "unpin <id>",
	Short: "Remove the retention pin tag from an artifact",
	Long: `Remove the retention pin tag (retention.pin_tags[0], default "pinned") from
an artifact's Tags, making it eligible for pruning/eviction again. Idempotent:
a no-op if the artifact is not pinned.

The kind is auto-detected by probing each kind in turn for the given ID.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runPin(cmd, args[0], false)
	},
}

// pinTag returns the effective pin tag to add/remove. It is sourced from the
// resolved retention config (retention.pin_tags[0]); if that is unavailable
// or empty it falls back to defaultPinTag.
func pinTag() string {
	if cfg, err := resolveRetentionConfig(); err == nil &&
		len(cfg.PinTags) > 0 && cfg.PinTags[0] != "" {
		return cfg.PinTags[0]
	}
	return defaultPinTag
}

// resolvePinTag returns the pin tag to apply/remove. With no --tag flag it is
// retention.pin_tags[0] (pinTag). With --tag set, the tag MUST be one of the
// configured retention.pin_tags so the prune/evict engine honors it as an
// exemption; an unconfigured tag is rejected.
func resolvePinTag(flag string) (string, error) {
	if flag == "" {
		return pinTag(), nil
	}
	cfg, err := resolveRetentionConfig()
	if err != nil {
		return "", fmt.Errorf("resolve retention config to validate --tag: %w", err)
	}
	for _, t := range cfg.PinTags {
		if t == flag {
			return flag, nil
		}
	}
	return "", fmt.Errorf("--tag %q is not a configured retention pin tag (configured: %v)", flag, cfg.PinTags)
}

// runPin adds (add=true) or removes (add=false) the pin tag on the artifact
// identified by id, auto-detecting its kind.
func runPin(cmd *cobra.Command, id string, add bool) error {
	store := &acf.Store{Root: pinStoreRoot}
	if err := store.Init(); err != nil {
		return err
	}
	tag, err := resolvePinTag(pinTagFlag)
	if err != nil {
		return err
	}
	for _, k := range []acf.Kind{acf.KindMemory, acf.KindSkill, acf.KindTool, acf.KindConversation} {
		art, err := store.ReadArtifact(k, id)
		if err != nil {
			continue
		}
		changed := applyPinTag(&art, tag, add)
		if changed {
			if werr := store.WriteArtifact(art); werr != nil {
				return werr
			}
		}
		verb := "pinned"
		if !add {
			verb = "unpinned"
		}
		if changed {
			fmt.Fprintf(cmd.OutOrStdout(), "%s %s (kind %s, tag %q)\n", verb, id, k, tag)
		} else {
			fmt.Fprintf(cmd.OutOrStdout(), "%s %s already %s (no-op)\n", k, id, verb)
		}
		return nil
	}
	return fmt.Errorf("artifact %s not found in any kind", id)
}

// applyPinTag adds or removes tag from a.Tags and reports whether the slice
// changed. Adds dedupe; removes are a no-op when absent.
func applyPinTag(a *acf.Artifact, tag string, add bool) bool {
	has := false
	for _, t := range a.Tags {
		if t == tag {
			has = true
			break
		}
	}
	if add {
		if has {
			return false
		}
		a.Tags = append(a.Tags, tag)
		return true
	}
	if !has {
		return false
	}
	out := a.Tags[:0]
	for _, t := range a.Tags {
		if t != tag {
			out = append(out, t)
		}
	}
	a.Tags = out
	return true
}

func init() {
	home, _ := os.UserHomeDir()
	defaultStore := filepath.Join(home, ".aplexica", "store")
	pinCmd.Flags().StringVar(&pinStoreRoot, "store", defaultStore, "Canonical store root")
	unpinCmd.Flags().StringVar(&pinStoreRoot, "store", defaultStore, "Canonical store root")
	pinCmd.Flags().StringVar(&pinTagFlag, "tag", "", `Pin tag to apply (must be a configured retention.pin_tags entry; default retention.pin_tags[0], e.g. "pinned")`)
	unpinCmd.Flags().StringVar(&pinTagFlag, "tag", "", `Pin tag to remove (must be a configured retention.pin_tags entry; default retention.pin_tags[0])`)
	rootCmd.AddCommand(pinCmd)
	rootCmd.AddCommand(unpinCmd)
}
