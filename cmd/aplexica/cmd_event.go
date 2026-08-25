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
	eventStoreRoot string
	eventTagJSON   bool
)

var eventCmd = &cobra.Command{
	Use:   "event",
	Short: "Event-level operations (tags, inspection)",
}

var eventTagCmd = &cobra.Command{
	Use:   "tag",
	Short: "Add, remove, or list tags on individual events",
}

var eventTagAddCmd = &cobra.Command{
	Use:   "add <event-id-or-hash> <tag>",
	Short: "Add a tag to an event (rejects reserved namespaces)",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		eventRef, tag := args[0], args[1]
		if acf.IsReservedEventTag(tag) {
			return fmt.Errorf("tag %q uses a reserved namespace (aplexica:* / auto:* are system-only)", tag)
		}
		return runEventTagOp(cmd, eventRef, tag, true)
	},
}

var eventTagRemoveCmd = &cobra.Command{
	Use:   "remove <event-id-or-hash> <tag>",
	Short: "Remove a tag from an event",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runEventTagOp(cmd, args[0], args[1], false)
	},
}

var eventTagListCmd = &cobra.Command{
	Use:   "list <event-id-or-hash>",
	Short: "List tags on a single event",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		store := &acf.Store{Root: eventStoreRoot}
		if err := store.Init(); err != nil {
			return err
		}
		kind, artifactID, ev, err := resolveEventRef(store, args[0])
		if err != nil {
			return err
		}
		tags, err := store.EventTagsFor(kind, artifactID, ev.Hash, ev.EventTags)
		if err != nil {
			return err
		}
		if eventTagJSON {
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

var eventTagListAllCmd = &cobra.Command{
	Use:   "list-all <artifact-id>",
	Short: "List every distinct event tag used across an artifact's history",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		store := &acf.Store{Root: eventStoreRoot}
		if err := store.Init(); err != nil {
			return err
		}
		kind, _, found, err := findArtifactByID(store, args[0])
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("artifact %s not found", args[0])
		}
		tags, err := store.ListAllEventTags(kind, args[0])
		if err != nil {
			return err
		}
		if eventTagJSON {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(tags)
		}
		if len(tags) == 0 {
			fmt.Fprintln(cmd.OutOrStdout(), "(no event tags on this artifact)")
			return nil
		}
		for _, t := range tags {
			fmt.Fprintln(cmd.OutOrStdout(), t)
		}
		return nil
	},
}

func runEventTagOp(cmd *cobra.Command, eventRef, tag string, add bool) error {
	store := &acf.Store{Root: eventStoreRoot}
	if err := store.Init(); err != nil {
		return err
	}
	kind, artifactID, ev, err := resolveEventRef(store, eventRef)
	if err != nil {
		return err
	}
	var resulting []string
	if add {
		resulting, err = store.AddEventTag(kind, artifactID, ev.Hash, tag)
	} else {
		resulting, err = store.RemoveEventTag(kind, artifactID, ev.Hash, tag)
	}
	if err != nil {
		return err
	}
	verb := "added"
	if !add {
		verb = "removed"
	}
	if eventTagJSON {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{
			"event":  ev.EventID,
			"hash":   ev.Hash,
			"action": verb,
			"tag":    tag,
			"tags":   resulting,
		})
	}
	fmt.Fprintf(cmd.OutOrStdout(), "%s tag %q on event %s (current tags: %s)\n",
		verb, tag, shortHashOf(ev.Hash), strings.Join(resulting, ", "))
	return nil
}

// resolveEventRef walks every artifact kind looking for an event whose
// EventID or Hash matches ref. Returns the kind, artifact ID, and a
// snapshot of the event. O(N) over all events in the store; for V1 this
// is acceptable given expected store sizes.
func resolveEventRef(store *acf.Store, ref string) (acf.Kind, string, acf.Event, error) {
	for _, k := range []acf.Kind{acf.KindMemory, acf.KindSkill, acf.KindTool, acf.KindConversation} {
		arts, err := store.ListArtifacts(k)
		if err != nil {
			continue
		}
		for _, a := range arts {
			events, err := store.ReadEvents(k, a.ArtifactID)
			if err != nil {
				continue
			}
			for _, e := range events {
				if e.EventID == ref || e.Hash == ref {
					return k, a.ArtifactID, e, nil
				}
			}
		}
	}
	return "", "", acf.Event{}, fmt.Errorf("event %q not found in any artifact", ref)
}

func init() {
	home, _ := os.UserHomeDir()
	defaultStore := filepath.Join(home, ".aplexica", "store")

	for _, c := range []*cobra.Command{eventTagAddCmd, eventTagRemoveCmd, eventTagListCmd, eventTagListAllCmd} {
		c.Flags().StringVar(&eventStoreRoot, "store", defaultStore, "Canonical store root")
		c.Flags().BoolVar(&eventTagJSON, "json", false, "Emit JSON instead of plain text")
	}

	eventTagCmd.AddCommand(eventTagAddCmd, eventTagRemoveCmd, eventTagListCmd, eventTagListAllCmd)
	eventCmd.AddCommand(eventTagCmd)
	rootCmd.AddCommand(eventCmd)
}
