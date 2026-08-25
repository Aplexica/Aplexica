package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
	"unicode/utf8"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/spf13/cobra"
)

var (
	showStoreRoot string
	showFull      bool
)

var showCmd = &cobra.Command{
	Use:   "show <artifact-id>",
	Short: "Show an artifact's metadata, event history, and payload summary",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		store := &acf.Store{Root: showStoreRoot}
		id := args[0]

		// Find which kind this artifact is. Try each in order.
		var found acf.Artifact
		var foundKind acf.Kind
		ok := false
		for _, k := range []acf.Kind{acf.KindMemory, acf.KindSkill, acf.KindTool, acf.KindConversation} {
			a, err := store.ReadArtifact(k, id)
			if err == nil {
				found = a
				foundKind = k
				ok = true
				break
			}
		}
		if !ok {
			// Fallback: the id wasn't an ArtifactID. Resolve it as a native agent
			// identifier (e.g. a Claude Code session-id / .jsonl basename), which
			// lives in the artifact's Name/SourcePath rather than its id.
			k, a, nf, ferr := store.FindByNativeID(id)
			if ferr != nil {
				return ferr
			}
			if nf {
				found, foundKind, ok = a, k, true
			}
		}
		if !ok {
			return fmt.Errorf("artifact %s not found in canonical store", id)
		}
		// Normalize to the canonical ArtifactID so the event/payload reads below
		// work whether the user passed an ArtifactID or a native session-id.
		id = found.ArtifactID

		out := cmd.OutOrStdout()
		fmt.Fprintln(out, "Artifact:")
		fmt.Fprintf(out, "  ID:         %s\n", found.ArtifactID)
		fmt.Fprintf(out, "  Kind:       %s\n", found.Kind)
		fmt.Fprintf(out, "  Scope:      %s\n", found.Scope)
		fmt.Fprintf(out, "  Name:       %s\n", found.Name)
		fmt.Fprintf(out, "  Created:    %s\n", found.CreatedAt.Format(time.RFC3339))
		fmt.Fprintf(out, "  Updated:    %s\n", found.UpdatedAt.Format(time.RFC3339))
		fmt.Fprintf(out, "  HeadHash:   %s\n", found.HeadEventHash)
		fmt.Fprintf(out, "  Schema:     %s\n", found.AcfSchemaVersion)
		fmt.Fprintln(out)

		events, err := store.ReadEvents(foundKind, id)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "Events (%d):\n", len(events))
		for i, e := range events {
			fmt.Fprintf(out, "  [%d] %s  type=%s  source=%s  adapter=%s\n",
				i,
				e.Timestamp.Format(time.RFC3339),
				e.Type,
				e.Provenance.SourceAgent,
				e.Provenance.AdapterVersion,
			)
			fmt.Fprintf(out, "      event_id=%s  hash=%s\n",
				e.EventID, e.Hash[:16]+"...")
		}
		fmt.Fprintln(out)

		// Payload summary — first 200 chars of the latest event's Content field,
		// or the full payload if --full was set.
		if len(events) > 0 {
			latest := events[len(events)-1]
			var content string
			switch foundKind {
			case acf.KindMemory:
				p, _ := acf.DecodeMemoryPayload(latest)
				content = p.Content
			case acf.KindSkill:
				p, _ := acf.DecodeSkillPayload(latest)
				content = p.Content
			case acf.KindConversation:
				p, _ := acf.DecodeConversationPayload(latest)
				content = p.Content
			case acf.KindTool:
				p, _ := acf.DecodeToolPayload(latest)
				content = p.Content
			}
			fmt.Fprintln(out, "Latest payload:")
			if showFull {
				fmt.Fprintln(out, content)
			} else {
				fmt.Fprintln(out, summarize(content, 200))
			}
		}
		return nil
	},
}

// summarize returns content truncated to at most maxLen bytes, with a
// "...(N more chars)" suffix. The cut point is backed up to a UTF-8 rune
// boundary so a multibyte rune is never split (which would render as U+FFFD).
// The remaining count is reported in runes, consistent with the boundary.
func summarize(content string, maxLen int) string {
	if len(content) <= maxLen {
		return content
	}
	cut := maxLen
	for cut > 0 && !utf8.RuneStart(content[cut]) {
		cut--
	}
	remaining := utf8.RuneCountInString(content[cut:])
	return content[:cut] + fmt.Sprintf("...(%d more chars; use --full to see all)", remaining)
}

func init() {
	home, _ := os.UserHomeDir()
	showCmd.Flags().StringVar(&showStoreRoot, "store",
		filepath.Join(home, ".aplexica", "store"),
		"Canonical store root directory")
	showCmd.Flags().BoolVar(&showFull, "full", false,
		"Print the full payload content (default: 200-char preview)")
	rootCmd.AddCommand(showCmd)
}
