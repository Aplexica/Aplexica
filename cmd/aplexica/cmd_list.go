package main

import (
	"fmt"
	"os"
	"path/filepath"
	"text/tabwriter"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/spf13/cobra"
)

var (
	listStoreRoot    string
	listKindFilter   string
	listScopeFilter  string
	listSourceFilter string
)

var listCmd = &cobra.Command{
	Use:   "list",
	Short: "List artifacts in the canonical store",
	Long: `List artifacts in the canonical store, with optional filters.

Filters: --kind=memory|skill|tool|conversation, --scope=global|project|namespace,
--source-agent=claude-code|codex.

Output columns: ID, KIND, NAME, SCOPE, SOURCE, CREATED.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		store := &acf.Store{Root: listStoreRoot}

		kinds := []acf.Kind{acf.KindMemory, acf.KindSkill, acf.KindTool, acf.KindConversation}
		if listKindFilter != "" {
			kinds = []acf.Kind{acf.Kind(listKindFilter)}
		}

		type listRow struct {
			art acf.Artifact
			// source is the pre-resolved SOURCE value captured during the
			// source-agent filter pass; nil when no filter ran, signalling the
			// print loop to read the first event itself.
			source *string
		}
		var rows []listRow
		for _, k := range kinds {
			artifacts, err := store.ListArtifacts(k)
			if err != nil {
				return err
			}
			for _, a := range artifacts {
				if listScopeFilter != "" && string(a.Scope) != listScopeFilter {
					continue
				}
				// Source-agent filter requires walking event provenance — for V0.1.7
				// we read the first event of each artifact only. The source resolved
				// here is reused for the SOURCE column so we don't re-read events.
				var src *string
				if listSourceFilter != "" {
					events, err := store.ReadEvents(a.Kind, a.ArtifactID)
					if err != nil {
						return err
					}
					if len(events) == 0 || events[0].Provenance.SourceAgent != listSourceFilter {
						continue
					}
					s := events[0].Provenance.SourceAgent
					src = &s
				}
				rows = append(rows, listRow{art: a, source: src})
			}
		}

		if len(rows) == 0 {
			fmt.Fprintln(cmd.OutOrStdout(), "(no artifacts match)")
			return nil
		}

		w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 2, 2, ' ', 0)
		fmt.Fprintln(w, "ID\tKIND\tNAME\tSCOPE\tSOURCE\tCREATED")
		for _, r := range rows {
			source := "?"
			if r.source != nil {
				source = *r.source
			} else {
				events, err := store.ReadEvents(r.art.Kind, r.art.ArtifactID)
				if err == nil && len(events) > 0 {
					source = events[0].Provenance.SourceAgent
				}
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
				r.art.ArtifactID,
				r.art.Kind,
				r.art.Name,
				r.art.Scope,
				source,
				r.art.CreatedAt.Format(time.RFC3339),
			)
		}
		return w.Flush()
	},
}

func init() {
	home, _ := os.UserHomeDir()
	listCmd.Flags().StringVar(&listStoreRoot, "store",
		filepath.Join(home, ".aplexica", "store"),
		"Canonical store root directory")
	listCmd.Flags().StringVar(&listKindFilter, "kind", "",
		"Filter by kind (memory|skill|tool|conversation)")
	listCmd.Flags().StringVar(&listScopeFilter, "scope", "",
		"Filter by scope (global|project|namespace)")
	listCmd.Flags().StringVar(&listSourceFilter, "source-agent", "",
		"Filter by source agent (claude-code|codex)")
	rootCmd.AddCommand(listCmd)
}
