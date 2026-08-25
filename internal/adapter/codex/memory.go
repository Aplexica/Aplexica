package codex

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

// ImportMemory reads an AGENTS.md and writes one memory artifact.
func (a *Adapter) ImportMemory(ctx context.Context, store *acf.Store, nativePath string) ([]string, error) {
	return adapter.ImportOpaque(ctx, store, acf.KindMemory, a.opaqueParams(), nativePath, memoryEncode)
}

// ExportMemory replays the memory artifact's event log and writes the result.
//
// When the destination is THIS adapter's global ~/.codex/AGENTS.md, it first
// strips any entries already present in the ~/.codex/memories/*.md layer (the
// anti-duplication guard): Codex already reads those from memories/, so writing
// them into AGENTS.md too would duplicate the memory. This keeps the
// hand-authored AGENTS.md pristine while still letting genuinely-new memories
// (from other agents, not in memories/) land in it. Every other destination
// (project AGENTS.md, other agents' files) materializes verbatim.
func (a *Adapter) ExportMemory(ctx context.Context, store *acf.Store, artifactID, destPath string) error {
	if !a.isGlobalAgentsPath(destPath) {
		return adapter.ExportOpaque(ctx, store, acf.KindMemory, artifactID, destPath, memoryDecode)
	}
	content, tombstoned, err := adapter.ReplayOpaqueContent(store, acf.KindMemory, artifactID, memoryDecode)
	if err != nil {
		return err
	}
	if tombstoned {
		return adapter.ErrArtifactTombstoned
	}
	stripped := stripMemoriesEntries(content, readMemoriesContents(a.memoriesDir()))
	// Skip the write when it is a no-op (so a pristine AGENTS.md keeps its
	// exact bytes/mtime in steady state) or when it would wipe a non-empty
	// AGENTS.md to empty (defensive — never destroy the file).
	if cur, rerr := os.ReadFile(destPath); rerr == nil {
		if string(cur) == stripped {
			return nil
		}
		if strings.TrimSpace(stripped) == "" && len(cur) > 0 {
			return nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
		return fmt.Errorf("codex: mkdir dest: %w", err)
	}
	return atomicfile.WriteFile(destPath, []byte(stripped), 0o644)
}
