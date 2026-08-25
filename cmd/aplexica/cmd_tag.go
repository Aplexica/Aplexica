package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/spf13/cobra"
)

var (
	tagStoreRoot     string
	tagJSON          bool
	tagDescribeDesc  string
	tagDescribeColor string
	tagDescribeScope string
)

var tagCmd = &cobra.Command{
	Use:   "tag",
	Short: "Artifact tag operations",
}

var tagAddCmd = &cobra.Command{
	Use:   "add <artifact-id> <tag>",
	Short: "Add a tag to an artifact (rejects reserved namespaces)",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		id, raw := args[0], args[1]
		tag, err := acf.NormalizeArtifactTag(raw)
		if err != nil {
			return err
		}
		if acf.IsReservedArtifactTag(tag) {
			return fmt.Errorf("tag %q uses a reserved namespace (aplexica:* / fork-of:* / device:* / conflict:* are system-only)", tag)
		}
		store := &acf.Store{Root: tagStoreRoot}
		if err := store.Init(); err != nil {
			return err
		}
		kind, _, found, err := findArtifactByID(store, id)
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("artifact %s not found", id)
		}
		tags, err := store.AddArtifactTag(kind, id, tag)
		if err != nil {
			return err
		}
		if tagJSON {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"tags": tags})
		}
		fmt.Fprintf(cmd.OutOrStdout(), "added tag %q to %s (tags now: %s)\n",
			tag, id, strings.Join(tags, ", "))
		return nil
	},
}

var tagRemoveCmd = &cobra.Command{
	Use:   "remove <artifact-id> <tag>",
	Short: "Remove a tag from an artifact",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		id, raw := args[0], args[1]
		tag, err := acf.NormalizeArtifactTag(raw)
		if err != nil {
			return err
		}
		store := &acf.Store{Root: tagStoreRoot}
		if err := store.Init(); err != nil {
			return err
		}
		kind, _, found, err := findArtifactByID(store, id)
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("artifact %s not found", id)
		}
		tags, err := store.RemoveArtifactTag(kind, id, tag)
		if err != nil {
			return err
		}
		if tagJSON {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"tags": tags})
		}
		fmt.Fprintf(cmd.OutOrStdout(), "removed tag %q from %s (tags now: %s)\n",
			tag, id, strings.Join(tags, ", "))
		return nil
	},
}

var tagListCmd = &cobra.Command{
	Use:   "list [artifact-id]",
	Short: "List tags on an artifact, or every tag in use across the store",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		store := &acf.Store{Root: tagStoreRoot}
		if err := store.Init(); err != nil {
			return err
		}
		var tags []string
		if len(args) == 1 {
			kind, _, found, err := findArtifactByID(store, args[0])
			if err != nil {
				return err
			}
			if !found {
				return fmt.Errorf("artifact %s not found", args[0])
			}
			a, err := store.ReadArtifact(kind, args[0])
			if err != nil {
				return err
			}
			tags = a.Tags
		} else {
			var err error
			tags, err = store.ListArtifactTags()
			if err != nil {
				return err
			}
		}
		if tagJSON {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(tags)
		}
		if len(tags) == 0 {
			fmt.Fprintln(cmd.OutOrStdout(), "(no tags)")
			return nil
		}
		for _, t := range tags {
			fmt.Fprintln(cmd.OutOrStdout(), t)
		}
		return nil
	},
}

var tagRenameCmd = &cobra.Command{
	Use:   "rename <old> <new>",
	Short: "Rename a tag across every artifact in the store",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		old, errO := acf.NormalizeArtifactTag(args[0])
		if errO != nil {
			return errO
		}
		raw := args[1]
		newTag, errN := acf.NormalizeArtifactTag(raw)
		if errN != nil {
			return errN
		}
		if acf.IsReservedArtifactTag(newTag) {
			return fmt.Errorf("cannot rename to reserved namespace %q", newTag)
		}
		store := &acf.Store{Root: tagStoreRoot}
		if err := store.Init(); err != nil {
			return err
		}
		count, err := store.RenameArtifactTag(old, newTag)
		if err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "renamed %q → %q across %d artifact(s)\n", old, newTag, count)
		return nil
	},
}

var tagDescribeCmd = &cobra.Command{
	Use:   "describe <tag>",
	Short: "Show or update metadata for a tag",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		tag, err := acf.NormalizeArtifactTag(args[0])
		if err != nil {
			return err
		}
		store := &acf.Store{Root: tagStoreRoot}
		if err := store.Init(); err != nil {
			return err
		}
		meta, err := store.LoadTagMetadata(tag)
		if err != nil {
			return err
		}
		mutated := false
		if cmd.Flags().Changed("description") {
			meta.Description = tagDescribeDesc
			mutated = true
		}
		if cmd.Flags().Changed("color") {
			meta.Color = tagDescribeColor
			mutated = true
		}
		if cmd.Flags().Changed("scope") {
			meta.Scope = tagDescribeScope
			mutated = true
		}
		if mutated {
			meta.Tag = tag
			if err := store.WriteTagMetadata(meta); err != nil {
				return err
			}
		}
		if tagJSON {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(meta)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "tag: %s\n", meta.Tag)
		if meta.Description != "" {
			fmt.Fprintf(cmd.OutOrStdout(), "description: %s\n", meta.Description)
		}
		if meta.Color != "" {
			fmt.Fprintf(cmd.OutOrStdout(), "color: %s\n", meta.Color)
		}
		if meta.Scope != "" {
			fmt.Fprintf(cmd.OutOrStdout(), "scope: %s\n", meta.Scope)
		}
		if !meta.CreatedAt.IsZero() {
			fmt.Fprintf(cmd.OutOrStdout(), "created: %s\n", meta.CreatedAt.Format("2006-01-02 15:04:05Z"))
		}
		return nil
	},
}

func init() {
	home, _ := os.UserHomeDir()
	defaultStore := filepath.Join(home, ".aplexica", "store")

	for _, c := range []*cobra.Command{tagAddCmd, tagRemoveCmd, tagListCmd, tagRenameCmd, tagDescribeCmd} {
		c.Flags().StringVar(&tagStoreRoot, "store", defaultStore, "Canonical store root")
		c.Flags().BoolVar(&tagJSON, "json", false, "Emit JSON instead of plain text")
	}
	tagDescribeCmd.Flags().StringVar(&tagDescribeDesc, "description", "",
		"Set the tag's human-readable description")
	tagDescribeCmd.Flags().StringVar(&tagDescribeColor, "color", "",
		"Set the tag's display color (hex, e.g., #3aa1ff)")
	tagDescribeCmd.Flags().StringVar(&tagDescribeScope, "scope", "",
		"Set the tag's scope label (\"personal\" / \"team\")")

	tagCmd.AddCommand(tagAddCmd, tagRemoveCmd, tagListCmd, tagRenameCmd, tagDescribeCmd)
	rootCmd.AddCommand(tagCmd)
}
