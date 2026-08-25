package hermes

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter"
)

// Import auto-detects the artifact kind from the filename.
//
//	MEMORY.md, USER.md → ImportMemory
//	AGENTS.md          → ImportMemory  (FR-02.12; AAIF cross-tool standard)
//	SOUL.md            → ImportMemory  (~/.hermes/SOUL.md agent-identity file)
//	SKILL.md           → ImportSkill
//	config.yaml,
//	hermes.yaml,
//	hermes.yml         → ImportTool
//	state.db,
//	*.db               → ImportConversationsFromDB (all sessions, since=0)
//	anything else      → error
func (a *Adapter) Import(ctx context.Context, store *acf.Store, nativePath string) ([]string, error) {
	base := filepath.Base(nativePath)
	switch base {
	case "MEMORY.md", "USER.md", "AGENTS.md", "SOUL.md":
		// v0.78.0: hermes reads AGENTS.md as a memory artifact per the
		// AAIF cross-tool standard (FR-02.12 / BRD-02 §6.1). The
		// memory importer preserves SourcePath so a round-trip via
		// NativePath can route it back to the same filename when the
		// receiving Export carries it.
		return a.ImportMemory(ctx, store, nativePath)
	case "SKILL.md":
		return a.ImportSkill(ctx, store, nativePath)
	case "config.yaml", "hermes.yaml", "hermes.yml":
		return a.ImportTool(ctx, store, nativePath)
	}
	if filepath.Ext(base) == ".db" {
		return a.ImportConversationsFromDB(ctx, store, nativePath, 0)
	}
	return nil, fmt.Errorf("hermes: unrecognized filename %q (expected MEMORY.md, USER.md, AGENTS.md, SOUL.md, SKILL.md, config.yaml, or *.db): %w", base, adapter.ErrNotHandled)
}

// Export dispatches by artifact kind.
func (a *Adapter) Export(ctx context.Context, store *acf.Store, artifactID, destPath string) error {
	if _, err := store.ReadArtifact(acf.KindMemory, artifactID); err == nil {
		return a.ExportMemory(ctx, store, artifactID, destPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("hermes: read memory artifact %s: %w", artifactID, err)
	}
	if _, err := store.ReadArtifact(acf.KindSkill, artifactID); err == nil {
		return a.ExportSkill(ctx, store, artifactID, destPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("hermes: read skill artifact %s: %w", artifactID, err)
	}
	if _, err := store.ReadArtifact(acf.KindTool, artifactID); err == nil {
		return a.ExportTool(ctx, store, artifactID, destPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("hermes: read tool artifact %s: %w", artifactID, err)
	}
	if _, err := store.ReadArtifact(acf.KindConversation, artifactID); err == nil {
		return a.ExportConversationsToDB(ctx, store, artifactID, destPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("hermes: read conversation artifact %s: %w", artifactID, err)
	}
	return fmt.Errorf("hermes: artifact %s not found as memory, skill, tool, or conversation", artifactID)
}

// NativePath returns where hermes natively writes the given artifact inside
// contextDir:
//   - memory: contextDir/MEMORY.md (or USER.md if Name matches; default MEMORY.md)
//   - skill:  contextDir/SKILL.md
//   - tool:   contextDir/config.yaml
//   - conversation: HomeDir/.hermes/state.db (always global; supports=false
//     when HomeDir is unset since sessions can only live in the global DB)
//
// Global-scope memory artifacts route under HomeDir/.hermes/memories/.
func (a *Adapter) NativePath(artifact acf.Artifact, contextDir string) (string, bool, error) {
	if contextDir == "" && artifact.Scope != acf.ScopeGlobal {
		return "", false, fmt.Errorf("hermes: NativePath needs a contextDir for non-global artifacts")
	}
	root := contextDir
	if artifact.Scope == acf.ScopeGlobal && a.HomeDir != "" {
		switch artifact.Kind {
		case acf.KindMemory:
			root = filepath.Join(a.HomeDir, ".hermes", "memories")
		default:
			root = filepath.Join(a.HomeDir, ".hermes")
		}
	}
	switch artifact.Kind {
	case acf.KindMemory:
		// Hermes only READS its central memory (memories/MEMORY.md +
		// USER.md); a file dropped in a project folder is invisible to
		// it. Project-scoped memory therefore routes to the CENTRAL
		// file, where ExportMemory upserts it as a delimited
		// "## Project:" section (adapter.UpsertProjectSection).
		if artifact.Scope == acf.ScopeProject && a.HomeDir != "" {
			return filepath.Join(a.HomeDir, ".hermes", "memories", "MEMORY.md"), true, nil
		}
		// SOUL.md is the agent-identity memory file and lives at
		// ~/.hermes/SOUL.md — at the .hermes root, NOT under memories/.
		if artifact.Name == "SOUL.md" {
			if artifact.Scope == acf.ScopeGlobal && a.HomeDir != "" {
				return filepath.Join(a.HomeDir, ".hermes", "SOUL.md"), true, nil
			}
			return filepath.Join(contextDir, "SOUL.md"), true, nil
		}
		// Default to MEMORY.md; preserve USER.md or AGENTS.md when
		// explicitly named (v0.78.0 added AGENTS.md support per
		// FR-02.12 / BRD-02 §6.1).
		switch artifact.Name {
		case "USER.md", "AGENTS.md":
			return filepath.Join(root, artifact.Name), true, nil
		}
		return filepath.Join(root, "MEMORY.md"), true, nil
	case acf.KindSkill:
		// Hermes discovers skills from ~/.hermes/skills/<category>/<name>/
		// SKILL.md (two-level: category dirs hold per-skill dirs). Synced
		// skills land under an "aplexica" category, mirroring the native
		// layout — a bare ~/.hermes/SKILL.md is never loaded.
		if artifact.Scope == acf.ScopeGlobal && a.HomeDir != "" {
			return filepath.Join(a.HomeDir, ".hermes", "skills", "aplexica", adapter.SkillDirName(artifact), "SKILL.md"), true, nil
		}
		return filepath.Join(root, "SKILL.md"), true, nil
	case acf.KindTool:
		return filepath.Join(root, "config.yaml"), true, nil
	case acf.KindConversation:
		if a.HomeDir == "" {
			return "", false, nil
		}
		return filepath.Join(a.HomeDir, ".hermes", "state.db"), true, nil
	}
	return "", false, nil
}

// HandlesFormat returns true for the payload formats hermes can materialize
// via Export. Memory/skill/tool use shared interop formats so cross-adapter
// fan-out works for those kinds; conversation accepts BOTH the hermes-specific
// SessionBundleFormat ("acf.hermes.session.v1") AND the cross-agent canonical
// format ("acf.conversation.v1") — the decoder dispatches by Format.
func (a *Adapter) HandlesFormat(kind acf.Kind, format string) bool {
	switch kind {
	case acf.KindMemory:
		return format == "markdown"
	case acf.KindSkill:
		return format == "skill.md"
	case acf.KindTool:
		return format == "acf.mcp.v1"
	case acf.KindConversation:
		return format == SessionBundleFormat || format == acf.ConversationFormatV1 || format == acf.ConversationDeltaFormatV1
	}
	return false
}
