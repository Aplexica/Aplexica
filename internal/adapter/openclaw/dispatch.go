package openclaw

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter"
	"github.com/aplexica/aplexica/internal/mcp"
)

// Import auto-detects the artifact kind from the filename.
//
//	MEMORY.md, AGENTS.md, CLAUDE.md, DREAMS.md → ImportMemory
//	SKILL.md                                   → ImportSkill
//	openclaw.json[c|5]                         → ImportTool (mcp.servers section)
//	memory/YYYY-MM-DD[-slug].md                → ImportMemory (daily note)
//	*.jsonl                                    → ImportConversation (opaque)
//	anything else                              → error
func (a *Adapter) Import(ctx context.Context, store *acf.Store, nativePath string) ([]string, error) {
	base := filepath.Base(nativePath)
	switch base {
	case "MEMORY.md", "AGENTS.md", "CLAUDE.md", "DREAMS.md":
		return a.ImportMemory(ctx, store, nativePath)
	case "SKILL.md":
		return a.ImportSkill(ctx, store, nativePath)
	case "openclaw.json", "openclaw.jsonc", "openclaw.json5":
		return a.ImportTool(ctx, store, nativePath)
	}
	if isDailyNoteFilename(base) {
		return a.ImportMemory(ctx, store, nativePath)
	}
	// Session-store sidecars are NOT transcripts: the index, per-session
	// trajectory telemetry, and backend bridge state live next to the
	// <uuid>.jsonl transcripts and churn on every turn. Skip them silently —
	// erroring here would accrue quarantine strikes against the adapter just
	// for watching its own session dir.
	if base == "sessions.json" ||
		strings.HasSuffix(base, ".trajectory.jsonl") ||
		strings.HasSuffix(base, ".trajectory-path.json") ||
		strings.HasSuffix(base, ".codex-app-server.json") {
		return nil, nil
	}
	if filepath.Ext(base) == ".jsonl" {
		return a.ImportConversation(ctx, store, nativePath)
	}
	return nil, fmt.Errorf("openclaw: unrecognized filename %q (expected MEMORY.md, AGENTS.md, CLAUDE.md, DREAMS.md, SKILL.md, openclaw.json[c|5], memory/YYYY-MM-DD[-slug].md, or *.jsonl): %w", base, adapter.ErrNotHandled)
}

// Export dispatches by artifact kind.
func (a *Adapter) Export(ctx context.Context, store *acf.Store, artifactID, destPath string) error {
	if _, err := store.ReadArtifact(acf.KindMemory, artifactID); err == nil {
		return a.ExportMemory(ctx, store, artifactID, destPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("openclaw: read memory artifact %s: %w", artifactID, err)
	}
	if _, err := store.ReadArtifact(acf.KindSkill, artifactID); err == nil {
		return a.ExportSkill(ctx, store, artifactID, destPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("openclaw: read skill artifact %s: %w", artifactID, err)
	}
	if _, err := store.ReadArtifact(acf.KindTool, artifactID); err == nil {
		return a.ExportTool(ctx, store, artifactID, destPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("openclaw: read tool artifact %s: %w", artifactID, err)
	}
	if _, err := store.ReadArtifact(acf.KindConversation, artifactID); err == nil {
		return a.ExportConversation(ctx, store, artifactID, destPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("openclaw: read conversation artifact %s: %w", artifactID, err)
	}
	return fmt.Errorf("openclaw: artifact %s not found", artifactID)
}

// NativePath returns where openclaw natively writes the given artifact
// inside contextDir:
//   - memory:       contextDir/MEMORY.md (or daily-note path under memory/)
//   - skill:        contextDir/SKILL.md
//   - tool:         contextDir/openclaw.json
//   - conversation: contextDir/<artifact.Name> (defaults to session.jsonl)
//
// Global-scope memory artifacts route under <HomeDir>/.openclaw/workspace/.
func (a *Adapter) NativePath(artifact acf.Artifact, contextDir string) (string, bool, error) {
	if contextDir == "" && artifact.Scope != acf.ScopeGlobal {
		return "", false, fmt.Errorf("openclaw: NativePath needs a contextDir for non-global artifacts")
	}
	root := contextDir
	if artifact.Scope == acf.ScopeGlobal && a.HomeDir != "" {
		root = filepath.Join(a.HomeDir, ".openclaw", "workspace")
	}
	switch artifact.Kind {
	case acf.KindMemory:
		// OpenClaw only READS its central workspace memory; project-scoped
		// memory routes there and ExportMemory upserts it as a delimited
		// "## Project:" section (adapter.UpsertProjectSection).
		if artifact.Scope == acf.ScopeProject && a.HomeDir != "" {
			return filepath.Join(a.HomeDir, ".openclaw", "workspace", "MEMORY.md"), true, nil
		}
		// Daily-note artifacts route under <root>/memory/<name>.
		if isDailyNoteFilename(artifact.Name) {
			return filepath.Join(root, "memory", artifact.Name), true, nil
		}
		// Prefer MEMORY.md as the canonical filename; if the artifact's Name
		// is one of the alternate forms, preserve it (AGENTS.md / CLAUDE.md /
		// DREAMS.md). Default → MEMORY.md.
		switch artifact.Name {
		case "AGENTS.md", "CLAUDE.md", "DREAMS.md":
			return filepath.Join(root, artifact.Name), true, nil
		}
		return filepath.Join(root, "MEMORY.md"), true, nil
	case acf.KindSkill:
		// OpenClaw discovers workspace skills under
		// ~/.openclaw/workspace/skills/<name>/SKILL.md (verified live:
		// listed as "✓ ready", source "openclaw-workspace"). A bare
		// workspace SKILL.md is never loaded — and would sit in the
		// workspace root OpenClaw ingests as context.
		if artifact.Scope == acf.ScopeGlobal && a.HomeDir != "" {
			return filepath.Join(a.HomeDir, ".openclaw", "workspace", "skills", adapter.SkillDirName(artifact), "SKILL.md"), true, nil
		}
		return filepath.Join(root, "SKILL.md"), true, nil
	case acf.KindTool:
		// OpenClaw reads MCP servers from the ROOT config
		// ~/.openclaw/openclaw.json (mcp.servers) — a workspace/openclaw.json
		// is never consulted by `openclaw mcp list` (same
		// dead-drop class as the skills and ~/.claude.json fixes). ExportTool
		// merges the mcp section into the existing config, so the gateway/
		// channel/agent keys survive.
		if artifact.Scope == acf.ScopeGlobal && a.HomeDir != "" {
			return filepath.Join(a.HomeDir, ".openclaw", "openclaw.json"), true, nil
		}
		return filepath.Join(root, "openclaw.json"), true, nil
	case acf.KindConversation:
		// Per-agent sessions natively live under
		// ~/.openclaw/agents/<id>/sessions/. For an arbitrary contextDir we
		// keep the session's filename verbatim (defaults to session.jsonl).
		name := artifact.Name
		if name == "" {
			name = "session.jsonl"
		}
		return filepath.Join(root, name), true, nil
	}
	return "", false, nil
}

// HandlesFormat returns true for the shared interop formats this adapter
// supports.
func (a *Adapter) HandlesFormat(kind acf.Kind, format string) bool {
	switch kind {
	case acf.KindMemory:
		return format == "markdown"
	case acf.KindSkill:
		return format == "skill.md"
	case acf.KindTool:
		return format == mcp.Format // "acf.mcp.v1"
	case acf.KindConversation:
		return format == SessionJSONLFormat || format == acf.ConversationFormatV1 || format == acf.ConversationDeltaFormatV1
	}
	return false
}

// isDailyNoteFilename returns true for YYYY-MM-DD.md and YYYY-MM-DD-<slug>.md.
// Used by Import (filename-based routing) and NativePath (output routing).
func isDailyNoteFilename(name string) bool {
	if !strings.HasSuffix(name, ".md") {
		return false
	}
	stem := strings.TrimSuffix(name, ".md")
	// minimum: 10 chars "YYYY-MM-DD"
	if len(stem) < 10 {
		return false
	}
	prefix := stem[:10]
	// Must be NNNN-NN-NN with digits and dashes in the right places.
	for i, r := range prefix {
		if i == 4 || i == 7 {
			if r != '-' {
				return false
			}
			continue
		}
		if r < '0' || r > '9' {
			return false
		}
	}
	if len(stem) == 10 {
		return true
	}
	// After "YYYY-MM-DD" expect "-<slug>".
	if stem[10] != '-' {
		return false
	}
	return len(stem) > 11
}
