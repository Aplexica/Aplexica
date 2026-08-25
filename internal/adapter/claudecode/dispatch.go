package claudecode

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter"
)

// Import auto-detects the artifact kind from the filename and dispatches to
// the appropriate per-kind importer.
//
//	CLAUDE.md  → ImportMemory
//	AGENTS.md  → ImportMemory  (FR-02.12; AAIF project-instructions standard)
//	SKILL.md   → ImportSkill
//	*.jsonl    → ImportConversation
//	anything else → error
func (a *Adapter) Import(ctx context.Context, store *acf.Store, nativePath string) ([]string, error) {
	// Claude Desktop stores user-facing titles in its own catalog, separately
	// from the shared CLI transcript. Treat a catalog change as a request to
	// re-import that exact transcript so metadata-only title assignment/rename
	// fans out through the normal conversation path. The catalog record itself
	// is never encoded as an ACF artifact.
	if a.isDesktopSessionRecordPath(nativePath) {
		record, ok := readDesktopSessionRecord(nativePath)
		if !ok || record.CLISessionID == "" {
			return nil, nil
		}
		transcript, ok := a.desktopTranscriptPath(record)
		if !ok {
			return nil, nil
		}
		// Route the transcript back through Import, not directly to the native
		// importer. Aplexica-generated transcripts carry a canonical thread
		// marker that must be merged before any path-keyed artifact is created.
		// Bypassing that guard turns each Desktop catalog save into a new
		// conversation, which is then materialized and saved again indefinitely.
		return a.Import(ctx, store, transcript)
	}
	// Global memory: the hand-authored ~/.claude/CLAUDE.md folds the type:user
	// auto-memory bodies (across every project's memory dir) into the single
	// CLAUDE.md-keyed GLOBAL artifact (attributed to claude-code), so a personal
	// memory added via Claude Code's /memory tool fans out to the other agents
	// instead of being mis-imported by a basename-collision adapter (hermes'
	// MEMORY.md). See globalmemory.go.
	if a.isGlobalClaudePath(nativePath) {
		return a.importGlobalMemory(ctx, store)
	}
	// An auto-memory topic edit: always recompose GLOBAL (type:user bodies move
	// across dirs), THEN — if this dir's encoded cwd maps to a REGISTERED local
	// project — recompose that project's memory too (type:project bodies). The
	// type tag inside each topic, not the trigger file, decides the destination;
	// running both recomposes keeps every surface consistent regardless of which
	// topic changed. Return the union of artifact ids.
	if enc, ok := a.autoMemoryEnc(nativePath); ok {
		ids, err := a.importGlobalMemory(ctx, store)
		if err != nil {
			return nil, err
		}
		if projPath, hit := a.projectForEnc(enc); hit {
			pids, perr := a.importProjectMemory(ctx, store, projPath)
			if perr != nil {
				return nil, perr
			}
			ids = append(ids, pids...)
		}
		return ids, nil
	}
	base := filepath.Base(nativePath)
	switch base {
	case "CLAUDE.md", "AGENTS.md":
		// v0.78.0: claude-code reads AGENTS.md as a memory artifact
		// per the AAIF cross-tool standard (FR-02.12 / BRD-02 §6.1).
		// Both filenames route through the same memory importer; the
		// SourcePath in the resulting Artifact preserves which native
		// form was read so Export can write back to the same path.
		//
		// v0.7.0: a REGISTERED local project's own <P>/CLAUDE.md routes
		// through importProjectMemory so its type:project auto-memory
		// bodies compose in (a no-op compose when there are none, so the
		// plain-CLAUDE.md case is unchanged). AGENTS.md never carries
		// auto-memory and stays on the plain importer.
		if base == "CLAUDE.md" {
			if projPath, ok := a.registeredProjectForClaudePath(nativePath); ok {
				return a.importProjectMemory(ctx, store, projPath)
			}
		}
		return a.ImportMemory(ctx, store, nativePath)
	case "SKILL.md":
		return a.ImportSkill(ctx, store, nativePath)
	case ".mcp.json", ".claude.json":
		// ~/.claude.json is Claude Code's user config; its mcpServers key
		// is where `claude mcp add -s user` writes. mcp.FromMCPJSON reads
		// just that key, so the same importer handles both files.
		return a.ImportTool(ctx, store, nativePath)
	}
	if filepath.Ext(base) == ".jsonl" {
		// Claude stores spawned agents and workflow journals under subagents/
		// and also marks sidechain rows explicitly. Those files belong to the
		// parent task and must never become standalone synced conversations.
		internal, internalErr := claudeSessionIsInternal(nativePath)
		if internalErr != nil {
			return nil, internalErr
		}
		if internal {
			return nil, nil
		}
		// Bidirectional thread merge: if this session was materialized by
		// Aplexica, merge by canonical thread+branch — a no-op if unchanged
		// (loop break), or a continuation that propagates to the other agents.
		// Native sessions fall through.
		// Every Aplexica-generated Claude row carries the thread marker, while a
		// native session's first row does not. Probe only that bounded first row
		// before reading the entire session for merge: otherwise an active
		// 100+ MiB native transcript is redundantly read and split on every
		// append before the incremental conversation importer even runs.
		hasThreadMarker, markerErr := claudeSessionHasAplexicaThreadMarker(nativePath)
		if markerErr != nil {
			return nil, markerErr
		}
		if hasThreadMarker {
			raw, rerr := os.ReadFile(nativePath)
			if rerr != nil {
				return nil, rerr
			}
			if ref, ok := claudeSessionThreadRef(raw); ok {
				if events, eerr := EncodeCanonical(raw); eerr == nil {
					if ids, handled, merr := adapter.MergeConversationByThreadRef(
						ctx, store, a.opaqueParams(), ref, events, adapter.EncodeCanonicalConversationPayload,
					); handled {
						return ids, merr
					}
				}
			}
		}
		return a.ImportConversation(ctx, store, nativePath)
	}
	return nil, fmt.Errorf("claudecode: unrecognized filename %q (expected CLAUDE.md, AGENTS.md, SKILL.md, .mcp.json, or *.jsonl): %w", base, adapter.ErrNotHandled)
}

// claudeSessionIsInternal uses Claude's structural path and row metadata,
// never conversation text, so ordinary user sessions and automations remain
// eligible for sync.
func claudeSessionIsInternal(path string) (bool, error) {
	for dir := filepath.Clean(filepath.Dir(path)); ; dir = filepath.Dir(dir) {
		if filepath.Base(dir) == "subagents" {
			return true, nil
		}
		next := filepath.Dir(dir)
		if next == dir {
			break
		}
	}

	f, err := os.Open(path)
	if err != nil {
		return false, fmt.Errorf("claudecode: open session metadata: %w", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, scanBufInitial), scanBufMax)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var row struct {
			IsSidechain bool   `json:"isSidechain"`
			AgentID     string `json:"agentId"`
		}
		if json.Unmarshal(line, &row) != nil {
			return false, nil
		}
		return row.IsSidechain || row.AgentID != "", nil
	}
	if err := scanner.Err(); err != nil {
		return false, fmt.Errorf("claudecode: read session metadata: %w", err)
	}
	return false, nil
}

func claudeSessionHasAplexicaThreadMarker(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, fmt.Errorf("claudecode: open session marker: %w", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, scanBufInitial), scanBufMax)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var row struct {
			AplexicaThreadID string `json:"aplexicaThreadId"`
		}
		return json.Unmarshal(line, &row) == nil && row.AplexicaThreadID != "", nil
	}
	if err := scanner.Err(); err != nil {
		return false, fmt.Errorf("claudecode: read session marker: %w", err)
	}
	return false, nil
}

// Export dispatches by artifact kind, which it reads from the store. Tries
// memory first, then skill, then conversation, returning a clear error if none
// matches.
func (a *Adapter) Export(ctx context.Context, store *acf.Store, artifactID, destPath string) error {
	if _, err := store.ReadArtifact(acf.KindMemory, artifactID); err == nil {
		return a.ExportMemory(ctx, store, artifactID, destPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("claudecode: read memory artifact %s: %w", artifactID, err)
	}
	if _, err := store.ReadArtifact(acf.KindSkill, artifactID); err == nil {
		return a.ExportSkill(ctx, store, artifactID, destPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("claudecode: read skill artifact %s: %w", artifactID, err)
	}
	if _, err := store.ReadArtifact(acf.KindConversation, artifactID); err == nil {
		return a.ExportConversation(ctx, store, artifactID, destPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("claudecode: read conversation artifact %s: %w", artifactID, err)
	}
	if _, err := store.ReadArtifact(acf.KindTool, artifactID); err == nil {
		return a.ExportTool(ctx, store, artifactID, destPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("claudecode: read tool artifact %s: %w", artifactID, err)
	}
	return fmt.Errorf("claudecode: artifact %s not found as memory, skill, conversation, or tool", artifactID)
}

// NativePath returns where claude-code natively writes the given artifact
// inside contextDir:
//   - memory: contextDir/CLAUDE.md
//   - skill:  contextDir/SKILL.md
//   - tool:   contextDir/.mcp.json
//   - conversation: not supported for cross-adapter fan-out (each agent's
//     session format differs structurally even when the wire is .jsonl).
//
// Global-scope artifacts (Scope == ScopeGlobal) ignore contextDir and route
// under HomeDir/.claude/. Project-scope and namespace-scope use contextDir.
func (a *Adapter) NativePath(artifact acf.Artifact, contextDir string) (string, bool, error) {
	if contextDir == "" && artifact.Scope != acf.ScopeGlobal {
		return "", false, fmt.Errorf("claudecode: NativePath needs a contextDir for non-global artifacts")
	}
	root := contextDir
	if artifact.Scope == acf.ScopeGlobal && a.HomeDir != "" {
		root = filepath.Join(a.HomeDir, ".claude")
	}
	switch artifact.Kind {
	case acf.KindMemory:
		return filepath.Join(root, "CLAUDE.md"), true, nil
	case acf.KindSkill:
		// Claude Code discovers personal skills ONLY under
		// ~/.claude/skills/<name>/SKILL.md — a bare ~/.claude/SKILL.md is
		// never loaded, and a single shared path would make every global
		// skill overwrite every other. Project skills use
		// <repo>/.claude/skills/<name>/SKILL.md, shared by CLI and Desktop.
		if artifact.Scope == acf.ScopeGlobal && a.HomeDir != "" {
			return filepath.Join(a.HomeDir, ".claude", "skills", adapter.SkillDirName(artifact), "SKILL.md"), true, nil
		}
		return filepath.Join(root, ".claude", "skills", adapter.SkillDirName(artifact), "SKILL.md"), true, nil
	case acf.KindTool:
		// Global MCP servers go into ~/.claude.json's mcpServers key — the
		// ONLY user-scope location Claude Code reads (`claude mcp list`).
		// A bare ~/.claude/.mcp.json is never loaded;
		// same class as the skills dead-drop). Project scope keeps the
		// project-root .mcp.json, which Claude Code does read from cwd.
		if artifact.Scope == acf.ScopeGlobal && a.HomeDir != "" {
			return a.userConfigPath(), true, nil
		}
		return filepath.Join(root, ".mcp.json"), true, nil
	case acf.KindConversation:
		// claudecode CAN write a session .jsonl, but cross-adapter
		// conversation fan-out is intentionally unsupported in v0.7.0
		// because the session schema differs structurally between agents.
		return "", false, nil
	}
	return "", false, nil
}

// HandlesFormat returns true for the payload formats claude-code can
// materialize via Export. Memory and skill use shared interop formats so
// cross-adapter fan-out works for those kinds; tool uses the canonical
// acf.mcp.v1; conversation is the claude-code-specific JSONL.
func (a *Adapter) HandlesFormat(kind acf.Kind, format string) bool {
	switch kind {
	case acf.KindMemory:
		return format == "markdown"
	case acf.KindSkill:
		return format == "skill.md"
	case acf.KindTool:
		return format == "acf.mcp.v1"
	case acf.KindConversation:
		// Accept BOTH formats so fan-out continues to work whether the
		// artifact was imported with --canonical or not.
		return format == "claude-code.session.jsonl" || format == acf.ConversationFormatV1 || format == acf.ConversationDeltaFormatV1
	}
	return false
}
