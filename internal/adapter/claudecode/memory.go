package claudecode

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter"
	"github.com/aplexica/aplexica/internal/atomicfile"
)

func memoryEncode(content []byte) (json.RawMessage, error) {
	return acf.EncodePayload(acf.MemoryPayload{Format: "markdown", Content: string(content)})
}

func memoryDecode(e acf.Event) (string, error) {
	p, err := acf.DecodeMemoryPayload(e)
	return p.Content, err
}

// ImportMemory reads a CLAUDE.md and writes one memory artifact.
func (a *Adapter) ImportMemory(ctx context.Context, store *acf.Store, nativePath string) ([]string, error) {
	return adapter.ImportOpaque(ctx, store, acf.KindMemory, a.opaqueParams(), nativePath, memoryEncode)
}

// ExportMemory replays the memory artifact's event log and writes the result.
//
// Two destinations get an anti-duplication strip before the write, because
// Claude Code already reads those entries from its /memory tool — writing them
// into CLAUDE.md too would duplicate the memory:
//
//   - THIS adapter's global ~/.claude/CLAUDE.md → strip the type:user auto-memory
//     bodies (gathered across every project's memory dir).
//   - A REGISTERED project's <P>/CLAUDE.md → strip that project's type:project
//     auto-memory bodies.
//
// The strip keeps the hand-authored CLAUDE.md pristine while still letting
// genuinely-new memories (from other agents, not in the auto-memory layer) land
// in it. Every other destination (other agents' files) materializes the merged
// view verbatim. The strip is the inverse of importGlobalMemory /
// importProjectMemory's compose, so each round-trip is byte-stable. Symmetric
// with codex.ExportMemory.
func (a *Adapter) ExportMemory(ctx context.Context, store *acf.Store, artifactID, destPath string) error {
	var stripBodies []string
	switch {
	case a.isGlobalClaudePath(destPath):
		stripBodies = a.userMemoryBodies()
	default:
		if projPath, ok := a.registeredProjectForClaudePath(destPath); ok {
			stripBodies = a.projectMemoryBodies(projPath)
		} else {
			return adapter.ExportOpaque(ctx, store, acf.KindMemory, artifactID, destPath, memoryDecode)
		}
	}
	content, tombstoned, err := adapter.ReplayOpaqueContent(store, acf.KindMemory, artifactID, memoryDecode)
	if err != nil {
		return err
	}
	if tombstoned {
		return adapter.ErrArtifactTombstoned
	}
	stripped := adapter.StripAppendedMemory(content, stripBodies)
	// Skip the write when it is a no-op (so a pristine CLAUDE.md keeps its exact
	// bytes/mtime in steady state) or when it would wipe a non-empty CLAUDE.md to
	// empty (defensive — never destroy the file).
	if cur, rerr := os.ReadFile(destPath); rerr == nil {
		if string(cur) == stripped {
			return nil
		}
		if strings.TrimSpace(stripped) == "" && len(cur) > 0 {
			return nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
		return fmt.Errorf("claudecode: mkdir dest: %w", err)
	}
	return atomicfile.WriteFile(destPath, []byte(stripped), 0o644)
}
