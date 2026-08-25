package codex

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter"
)

// Codex exposes memory through TWO global surfaces: the hand-authored
// ~/.codex/AGENTS.md (the documented, user-controlled instruction file) and the
// auto-managed ~/.codex/memories/*.md layer that Codex's consolidation pass
// writes and reads back at session start. Whole-file memory replication assumes
// ONE canonical memory file per agent, so the two surfaces can't both be peer
// artifacts — a second global-memory artifact would map to the same single
// NativePath (AGENTS.md / CLAUDE.md) and clobber the first.
//
// This file folds memories/*.md INTO the single AGENTS.md-keyed global-memory
// artifact (composeGlobalMemory) so a memory captured from the managed layer
// fans out to other agents — while the export side (ExportMemory) strips those
// same entries back out before writing AGENTS.md (stripMemoriesEntries) so the
// hand-authored AGENTS.md is never polluted with a memory it already holds in
// memories/. The two transforms are inverses, which keeps the cross-agent
// round-trip byte-stable.

func (a *Adapter) globalRoot() string       { return filepath.Join(a.HomeDir, ".codex") }
func (a *Adapter) globalAgentsPath() string { return filepath.Join(a.globalRoot(), "AGENTS.md") }
func (a *Adapter) memoriesDir() string      { return filepath.Join(a.globalRoot(), "memories") }
func (a *Adapter) sessionsDir() string      { return filepath.Join(a.globalRoot(), "sessions") }

// ConversationDocDir is where Aplexica writes rendered transcripts of
// conversations that originated in OTHER agents (adapter.ConversationDocTarget),
// under ~/.codex/aplexica/conversations/. Not the native sessions/ tree (those
// are Codex's own rollouts), so it is never re-imported.
func (a *Adapter) ConversationDocDir() (string, bool) {
	if a.HomeDir == "" {
		return "", false
	}
	return filepath.Join(a.globalRoot(), "aplexica", "conversations"), true
}

// isGlobalAgentsPath reports whether p is THIS adapter's global
// ~/.codex/AGENTS.md (the only memory destination that gets the memories strip).
func (a *Adapter) isGlobalAgentsPath(p string) bool {
	if a.HomeDir == "" {
		return false
	}
	ap, err := filepath.Abs(p)
	if err != nil {
		return false
	}
	return filepath.Clean(ap) == filepath.Clean(a.globalAgentsPath())
}

// isGlobalMemoriesFile reports whether p is a markdown file directly inside this
// adapter's ~/.codex/memories directory.
func (a *Adapter) isGlobalMemoriesFile(p string) bool {
	if a.HomeDir == "" || filepath.Ext(p) != ".md" {
		return false
	}
	ap, err := filepath.Abs(p)
	if err != nil {
		return false
	}
	return filepath.Dir(filepath.Clean(ap)) == filepath.Clean(a.memoriesDir())
}

// readMemoriesContents reads ~/.codex/memories/*.md in sorted filename order.
// A missing directory or unreadable file yields no entries (best-effort).
// Thin forwarder to the shared adapter helper (the compose/strip logic is
// shared with claude-code's auto-memory layer).
func readMemoriesContents(memDir string) []string {
	return adapter.ReadMarkdownDir(memDir)
}

// composeGlobalMemory folds the ~/.codex/memories/*.md entries into AGENTS.md.
// Thin forwarder to adapter.ComposeAppendedMemory (see its doc for semantics).
func composeGlobalMemory(agentsBody string, memContents []string) string {
	return adapter.ComposeAppendedMemory(agentsBody, memContents)
}

// stripMemoriesEntries is the inverse of composeGlobalMemory. Thin forwarder to
// adapter.StripAppendedMemory.
func stripMemoriesEntries(content string, memContents []string) string {
	return adapter.StripAppendedMemory(content, memContents)
}

// importGlobalMemory composes ~/.codex/AGENTS.md + ~/.codex/memories/*.md into
// the single AGENTS.md-keyed global-memory artifact. It is invoked for a change
// to AGENTS.md OR to any memories file; either way the artifact's SourcePath is
// AGENTS.md, so memories edits update the one canonical memory instead of
// minting a colliding second global-memory artifact.
func (a *Adapter) importGlobalMemory(ctx context.Context, store *acf.Store) ([]string, error) {
	agentsPath := a.globalAgentsPath()
	body, err := os.ReadFile(agentsPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("codex: read AGENTS.md: %w", err)
	}
	composed := composeGlobalMemory(string(body), readMemoriesContents(a.memoriesDir()))
	return adapter.ImportOpaqueContent(
		ctx, store, acf.KindMemory, a.opaqueParams(), agentsPath, []byte(composed), memoryEncode,
	)
}
