package kilo

import (
	"context"
	"encoding/json"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter"
)

func memoryEncode(content []byte) (json.RawMessage, error) {
	return acf.EncodePayload(acf.MemoryPayload{Format: "markdown", Content: string(content)})
}

func memoryDecode(e acf.Event) (string, error) {
	p, err := acf.DecodeMemoryPayload(e)
	return p.Content, err
}

// ImportMemory reads an AGENTS.md and writes one memory artifact.
// Uses Format: "markdown" so the artifact is structurally interchangeable
// with codex-imported AGENTS.md memories.
func (a *Adapter) ImportMemory(ctx context.Context, store *acf.Store, nativePath string) ([]string, error) {
	return adapter.ImportOpaque(ctx, store, acf.KindMemory, a.opaqueParams(), nativePath, memoryEncode)
}

// ExportMemory replays the memory artifact's event log and writes the result.
func (a *Adapter) ExportMemory(ctx context.Context, store *acf.Store, artifactID, destPath string) error {
	return adapter.ExportOpaque(ctx, store, acf.KindMemory, artifactID, destPath, memoryDecode)
}
