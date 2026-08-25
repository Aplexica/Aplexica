package codex

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter"
)

// Import auto-detects the artifact kind from the filename and dispatches.
//
//	AGENTS.md → ImportMemory
//	SKILL.md  → ImportSkill
//	*.jsonl   → ImportConversation
//	*.toml    → ImportTool
//	anything else → error
func (a *Adapter) Import(ctx context.Context, store *acf.Store, nativePath string) ([]string, error) {
	// Codex's global memory is composed from AGENTS.md + the managed
	// ~/.codex/memories/*.md layer into ONE artifact keyed by AGENTS.md, so a
	// change to EITHER source recomposes the same canonical memory (rather than
	// minting a second global-memory artifact that would clobber AGENTS.md on a
	// single-file NativePath fan-out). Project-scope AGENTS.md (anywhere else)
	// still imports verbatim through the normal memory path below.
	if a.isGlobalAgentsPath(nativePath) || a.isGlobalMemoriesFile(nativePath) {
		return a.importGlobalMemory(ctx, store)
	}
	base := filepath.Base(nativePath)
	switch base {
	case "AGENTS.md":
		return a.ImportMemory(ctx, store, nativePath)
	case "SKILL.md":
		return a.ImportSkill(ctx, store, nativePath)
	}
	switch filepath.Ext(base) {
	case ".jsonl":
		// Codex persists spawned worker/reviewer threads beside user-owned
		// rollouts. They are implementation details of the parent task, not
		// independent conversations. Importing them exposes every worker prompt
		// in both CLIs and multiplies them again during cross-device fan-out.
		internal, internalErr := SessionIsInternal(nativePath)
		if internalErr != nil {
			return nil, internalErr
		}
		if internal {
			return nil, nil
		}
		// Bidirectional thread merge (see claudecode): an Aplexica-materialized
		// rollout carries aplexica_thread_id; merge by text turns (no-op if
		// unchanged = loop break; continuation propagates). Native rollouts
		// fall through to a normal import.
		// A native Codex rollout has no Aplexica marker in its first
		// session_meta row. Check only that bounded row before considering the
		// merge path: active rollouts can exceed 100 MiB, and reading/splitting
		// the complete file merely to discover that it is native caused a CPU
		// and allocation burst on every append. Aplexica-generated rollouts
		// always stamp their first session_meta row.
		hasThreadMarker, markerErr := codexSessionHasAplexicaThreadMarker(nativePath)
		if markerErr != nil {
			return nil, markerErr
		}
		if hasThreadMarker {
			raw, rerr := os.ReadFile(nativePath)
			if rerr != nil {
				return nil, rerr
			}
			ready, readyErr := SessionReadyForImport(bytes.NewReader(raw))
			if readyErr != nil {
				return nil, readyErr
			}
			if !ready {
				// The latest native turn is still in flight. Treat these bytes as
				// handled but do not commit a partial canonical update. A later
				// assistant append changes the path fingerprint and retries. This
				// check belongs here (not only in the fast scanner), because the
				// recursive filesystem watcher reaches Import directly.
				return nil, nil
			}
			if ref, ok := codexThreadRef(raw); ok {
				// EncodeCanonical deliberately projects an Aplexica-generated
				// continuation to portable user/assistant text. Authenticate that
				// sanitizer at the marker boundary so the merge can replace a
				// legacy equal-visible head that still contains injected Codex
				// harness/tool rows.
				// The durable Aplexica thread marker authenticates this rollout as
				// our own materialization even after a newer fan-out has already
				// removed the old harness/tool rows from the native file. Always
				// carry the pre-v1.0.39 visible projection as repair proof so a
				// still-polluted canonical head can be replaced on the next scan.
				ref.AuthenticatedGeneratedPath = a.authenticatedGeneratedConversationPath(store, nativePath, raw, ref)
				if ref.AuthenticatedGeneratedPath {
					ref.SanitizedPortableProjection = true
					ref.SanitizedLegacyTurns = generatedCodexLegacyVisibleTurns(raw)
				}
				if events, eerr := EncodeCanonical(raw); eerr == nil {
					events, _ = sanitizeGeneratedMaterializedEchoes(ref, events)
					if ids, handled, merr := adapter.MergeConversationByThreadRef(
						ctx, store, a.opaqueParams(), ref, events, adapter.EncodeCanonicalConversationPayload,
					); handled {
						if merr == nil && ref.AuthenticatedGeneratedPath {
							a.bestEffortQuarantineCodexThreadDuplicates(
								nativePath,
								ref.ArtifactID,
								ref.BranchID,
								acf.ExtractTextTurns(events),
							)
						}
						return ids, merr
					}
				}
			}
		}
		return a.ImportConversation(ctx, store, nativePath)
	case ".toml":
		return a.ImportTool(ctx, store, nativePath)
	}
	return nil, fmt.Errorf("codex: unrecognized filename %q (expected AGENTS.md, SKILL.md, memories/*.md, *.jsonl, or *.toml): %w", base, adapter.ErrNotHandled)
}

// authenticatedGeneratedConversationPath binds deletion-authorizing native
// metadata to the one rollout path and session id Aplexica deterministically
// materialized for this existing artifact/branch. The durable marker remains
// useful for ordinary append-only merging when this check fails, but cannot by
// itself authorize removal of an ambiguous repeated assistant answer.
func (a *Adapter) authenticatedGeneratedConversationPath(
	store *acf.Store,
	nativePath string,
	raw []byte,
	ref adapter.ThreadRef,
) bool {
	art, err := store.ReadArtifact(acf.KindConversation, ref.ArtifactID)
	if err != nil {
		return false
	}
	materialized, head, ok, err := store.MaterializedConversationHeadFromStore(ref.ArtifactID)
	if err != nil || !ok {
		return false
	}
	// The persisted head may be a compact delta. Bind the already reconstructed
	// full projection to the transient planning event so path calculation does
	// not reject an otherwise valid generated rollout merely because its newest
	// canonical event is incremental.
	head.MaterializedConversation = &materialized
	plan, ok, err := a.conversationSessionPlan(art, head)
	if err != nil || !ok || plan.branchID != normalizeCodexBranchID(ref.BranchID) {
		return false
	}
	nativeAbs, err := filepath.Abs(nativePath)
	if err != nil {
		return false
	}
	destAbs, err := filepath.Abs(plan.dest)
	if err != nil || filepath.Clean(nativeAbs) != filepath.Clean(destAbs) {
		return false
	}
	return codexNativeSessionID(raw) == plan.sessionID &&
		plan.sessionID == codexSessionID(ref.ArtifactID, ref.BranchID)
}

func codexNativeSessionID(raw []byte) string {
	for _, line := range bytes.Split(raw, []byte{'\n'}) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var row struct {
			Type    string `json:"type"`
			Payload struct {
				ID string `json:"id"`
			} `json:"payload"`
		}
		if json.Unmarshal(line, &row) != nil {
			continue
		}
		if row.Type == "session_meta" {
			return row.Payload.ID
		}
	}
	return ""
}

// SessionIsInternal classifies only Codex's explicit subagent metadata.
// Prompt text is intentionally ignored: scheduled tasks and automations are
// user-owned conversations even when their first prompt looks machine-made.
// Native-backup policy shares this classifier so the same worker rollouts that
// are excluded from the canonical store are not multiplied into safety/cloud
// snapshots either.
func SessionIsInternal(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, fmt.Errorf("codex: open session metadata: %w", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, scanBufInitial), scanBufMax)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var envelope struct {
			Type    string `json:"type"`
			Payload struct {
				ThreadSource string          `json:"thread_source"`
				Source       json.RawMessage `json:"source"`
			} `json:"payload"`
		}
		if json.Unmarshal(line, &envelope) != nil || envelope.Type != "session_meta" {
			return false, nil
		}
		if envelope.Payload.ThreadSource == "subagent" {
			return true, nil
		}
		var source map[string]json.RawMessage
		if json.Unmarshal(envelope.Payload.Source, &source) == nil {
			_, isSubagent := source["subagent"]
			return isSubagent, nil
		}
		return false, nil
	}
	if err := scanner.Err(); err != nil {
		return false, fmt.Errorf("codex: read session metadata: %w", err)
	}
	return false, nil
}

func codexSessionHasAplexicaThreadMarker(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, fmt.Errorf("codex: open session marker: %w", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, scanBufInitial), scanBufMax)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var envelope struct {
			Type    string `json:"type"`
			Payload struct {
				AplexicaThreadID string `json:"aplexica_thread_id"`
			} `json:"payload"`
		}
		return json.Unmarshal(line, &envelope) == nil &&
			envelope.Type == "session_meta" &&
			envelope.Payload.AplexicaThreadID != "", nil
	}
	if err := scanner.Err(); err != nil {
		return false, fmt.Errorf("codex: read session marker: %w", err)
	}
	return false, nil
}

// Export dispatches by artifact kind, which it reads from the store.
func (a *Adapter) Export(ctx context.Context, store *acf.Store, artifactID, destPath string) error {
	if _, err := store.ReadArtifact(acf.KindMemory, artifactID); err == nil {
		return a.ExportMemory(ctx, store, artifactID, destPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("codex: read memory artifact %s: %w", artifactID, err)
	}
	if _, err := store.ReadArtifact(acf.KindSkill, artifactID); err == nil {
		return a.ExportSkill(ctx, store, artifactID, destPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("codex: read skill artifact %s: %w", artifactID, err)
	}
	if _, err := store.ReadArtifact(acf.KindConversation, artifactID); err == nil {
		return a.ExportConversation(ctx, store, artifactID, destPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("codex: read conversation artifact %s: %w", artifactID, err)
	}
	if _, err := store.ReadArtifact(acf.KindTool, artifactID); err == nil {
		return a.ExportTool(ctx, store, artifactID, destPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("codex: read tool artifact %s: %w", artifactID, err)
	}
	return fmt.Errorf("codex: artifact %s not found as memory, skill, conversation, or tool", artifactID)
}

// NativePath returns where codex natively writes the given artifact inside
// contextDir:
//   - memory: contextDir/AGENTS.md
//   - skill:  contextDir/.agents/skills/<name>/SKILL.md
//   - tool:   contextDir/.codex/config.toml
//   - conversation: not supported for cross-adapter fan-out.
//
// Global-scope artifacts route under HomeDir/.codex/.
func (a *Adapter) NativePath(artifact acf.Artifact, contextDir string) (string, bool, error) {
	if contextDir == "" && artifact.Scope != acf.ScopeGlobal {
		return "", false, fmt.Errorf("codex: NativePath needs a contextDir for non-global artifacts")
	}
	root := contextDir
	if artifact.Scope == acf.ScopeGlobal && a.HomeDir != "" {
		root = filepath.Join(a.HomeDir, ".codex")
	}
	switch artifact.Kind {
	case acf.KindMemory:
		return filepath.Join(root, "AGENTS.md"), true, nil
	case acf.KindSkill:
		// Codex CLI and Desktop share the Agent Skills locations:
		// $HOME/.agents/skills/<name>/SKILL.md for user scope and
		// <repo>/.agents/skills/<name>/SKILL.md for project scope. A bare
		// SKILL.md is never loaded and makes multiple skills collide.
		if artifact.Scope == acf.ScopeGlobal && a.HomeDir != "" {
			return filepath.Join(a.userSkillsDir(), adapter.SkillDirName(artifact), "SKILL.md"), true, nil
		}
		return filepath.Join(root, ".agents", "skills", adapter.SkillDirName(artifact), "SKILL.md"), true, nil
	case acf.KindTool:
		if artifact.Scope == acf.ScopeGlobal {
			return filepath.Join(root, "config.toml"), true, nil
		}
		return filepath.Join(root, ".codex", "config.toml"), true, nil
	case acf.KindConversation:
		return "", false, nil
	}
	return "", false, nil
}

// HandlesFormat returns true for the payload formats codex can materialize
// via Export. Memory/skill/tool use shared interop formats so cross-adapter
// fan-out works for those kinds; conversation is the codex-specific JSONL.
func (a *Adapter) HandlesFormat(kind acf.Kind, format string) bool {
	switch kind {
	case acf.KindMemory:
		return format == "markdown"
	case acf.KindSkill:
		return format == "skill.md"
	case acf.KindTool:
		return format == "acf.mcp.v1"
	case acf.KindConversation:
		return format == "codex.session.jsonl" || format == acf.ConversationFormatV1 || format == acf.ConversationDeltaFormatV1
	}
	return false
}
