package kilo

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter"
)

// Import auto-detects the artifact kind from the filename.
//
//	AGENTS.md  → ImportMemory
//	AGENT.md   → ImportMemory (Kilo's singular fallback)
//	SKILL.md   → ImportSkill
//	kilo.jsonc → ImportTool
//	mcp.json   → ImportLegacyMCPTool (legacy .kilocode/mcp.json, flat shape)
//	*.db       → ImportConversationsFromDB (read-only Kilo session DB import)
//	*.jsonl    → error: not applicable (Kilo uses a SQLite session DB,
//	             not per-conversation JSONL files)
//	anything else → error: unrecognized filename
//
// Kilo conversation kind is intentionally absent in V1. Current Kilo stores
// sessions in a SQLite DB; this adapter only uses that DB for project discovery
// until conversation import/export has a lossless mapping.
func (a *Adapter) Import(ctx context.Context, store *acf.Store, nativePath string) ([]string, error) {
	base := filepath.Base(nativePath)
	switch base {
	case "AGENTS.md", "AGENT.md":
		return a.ImportMemory(ctx, store, nativePath)
	case "SKILL.md":
		return a.ImportSkill(ctx, store, nativePath)
	case "kilo.jsonc":
		return a.ImportTool(ctx, store, nativePath)
	case "mcp.json":
		// Legacy .kilocode/mcp.json (pre-kilo.jsonc, flat "mcpServers" shape).
		return a.ImportLegacyMCPTool(ctx, store, nativePath)
	}
	if isKiloRuleMarkdown(nativePath) {
		return a.ImportMemory(ctx, store, nativePath)
	}
	if filepath.Ext(base) == ".db" {
		return a.ImportConversationsFromDB(ctx, store, nativePath, 0)
	}
	if filepath.Ext(base) == ".jsonl" {
		return nil, fmt.Errorf("kilo: conversation JSONL import is not applicable — Kilo stores sessions in a SQLite DB; import kilo.db instead")
	}
	return nil, fmt.Errorf("kilo: unrecognized filename %q (expected AGENTS.md, AGENT.md, SKILL.md, kilo.jsonc, mcp.json, *.db, or .kilo/rules/*.md): %w", base, adapter.ErrNotHandled)
}

// Export dispatches by artifact kind. v0.4.1 covers memory, skill, and tool;
// conversation kind is not applicable to Kilo and an artifact of that kind
// passed to this Export will error with a clear message.
func (a *Adapter) Export(ctx context.Context, store *acf.Store, artifactID, destPath string) error {
	if _, err := store.ReadArtifact(acf.KindMemory, artifactID); err == nil {
		return a.ExportMemory(ctx, store, artifactID, destPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("kilo: read memory artifact %s: %w", artifactID, err)
	}
	if _, err := store.ReadArtifact(acf.KindSkill, artifactID); err == nil {
		return a.ExportSkill(ctx, store, artifactID, destPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("kilo: read skill artifact %s: %w", artifactID, err)
	}
	if _, err := store.ReadArtifact(acf.KindTool, artifactID); err == nil {
		return a.ExportTool(ctx, store, artifactID, destPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("kilo: read tool artifact %s: %w", artifactID, err)
	}
	if _, err := store.ReadArtifact(acf.KindConversation, artifactID); err == nil {
		return fmt.Errorf("kilo: artifact %s is a conversation, which is not yet exportable by the Kilo adapter", artifactID)
	}
	return fmt.Errorf("kilo: artifact %s not found as memory, skill, or tool", artifactID)
}

// NativePath returns where kilo natively writes the given artifact inside
// contextDir:
//   - memory: contextDir/AGENTS.md
//   - skill:  contextDir/.kilo/skills/<name>/SKILL.md  (Kilo discovers skills
//     only under .kilo/skills/<name>/; a project-root SKILL.md is NOT found)
//   - tool:   contextDir/kilo.jsonc
//   - conversation: NOT APPLICABLE in V1 (Kilo stores sessions in a SQLite DB,
//     not as per-conversation files).
//
// Kilo project artifacts use contextDir as their write root. Kilo global
// config artifacts route to the documented user-level config/skills roots.
func (a *Adapter) NativePath(artifact acf.Artifact, contextDir string) (string, bool, error) {
	root := contextDir
	if artifact.Scope == acf.ScopeGlobal {
		if a.HomeDir == "" {
			return "", false, nil
		}
		root = a.configRoot()
	} else if contextDir == "" {
		return "", false, fmt.Errorf("kilo: NativePath needs a contextDir (Kilo is project-scoped)")
	}
	switch artifact.Kind {
	case acf.KindMemory:
		if rel, ok := kiloRuleRelPath(artifact.SourcePath); ok {
			if artifact.Scope == acf.ScopeGlobal {
				return filepath.Join(a.HomeDir, rel), true, nil
			}
			return filepath.Join(root, rel), true, nil
		}
		return filepath.Join(root, "AGENTS.md"), true, nil
	case acf.KindSkill:
		if artifact.Scope == acf.ScopeGlobal {
			return filepath.Join(a.legacyHomeRoot(), "skills", kiloSkillDirName(artifact), "SKILL.md"), true, nil
		}
		return filepath.Join(root, ".kilo", "skills", kiloSkillDirName(artifact), "SKILL.md"), true, nil
	case acf.KindTool:
		return filepath.Join(root, "kilo.jsonc"), true, nil
	case acf.KindConversation:
		return "", false, nil
	}
	return "", false, nil
}

func isKiloRuleMarkdown(path string) bool {
	if filepath.Ext(path) != ".md" {
		return false
	}
	_, ok := kiloRuleRelPath(path)
	return ok
}

func kiloRuleRelPath(path string) (string, bool) {
	if path == "" {
		return "", false
	}
	parts := splitPath(filepath.Clean(path))
	for i := 0; i+1 < len(parts); i++ {
		if (parts[i] == ".kilo" || parts[i] == ".kilocode") && isKiloRulesDir(parts[i+1]) {
			return filepath.Join(parts[i:]...), true
		}
	}
	return "", false
}

func isKiloRulesDir(name string) bool {
	return name == "rules" || strings.HasPrefix(name, "rules-")
}

func splitPath(path string) []string {
	var out []string
	for {
		dir, base := filepath.Split(path)
		if base != "" {
			out = append([]string{base}, out...)
		}
		dir = strings.TrimSuffix(dir, string(filepath.Separator))
		if dir == "" || dir == path {
			break
		}
		path = dir
	}
	return out
}

// kiloSkillDirName recovers the skill's directory name for the
// .kilo/skills/<name>/SKILL.md layout. It prefers the source skill directory
// (…/skills/<name>/SKILL.md → "<name>"), then the artifact name minus a .md
// suffix, falling back to "skill".
//
// Dot-prefixed parents are rejected: a single-file global skill imported
// from an agent CONFIG ROOT (~/.claude/SKILL.md) otherwise exports as
// skills/.claude/SKILL.md — the config root's basename is never a skill
// name (E2E F3 finding).
func kiloSkillDirName(artifact acf.Artifact) string {
	// Logic promoted to adapter.SkillDirName when the per-name skills
	// layout was extended to the other adapters; kilo keeps this alias.
	return adapter.SkillDirName(artifact)
}

// HandlesFormat returns true for the payload formats kilo can materialize
// via Export. Memory/skill/tool use shared interop formats so cross-adapter
// fan-out works for those kinds; conversation remains false because Kilo DB
// import is read-only and no native write-back path is implemented yet.
func (a *Adapter) HandlesFormat(kind acf.Kind, format string) bool {
	switch kind {
	case acf.KindMemory:
		return format == "markdown"
	case acf.KindSkill:
		return format == "skill.md"
	case acf.KindTool:
		return format == "acf.mcp.v1"
	case acf.KindConversation:
		return false
	}
	return false
}
