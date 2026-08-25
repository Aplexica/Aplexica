package hermes

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter"
	"github.com/aplexica/aplexica/internal/atomicfile"
)

func memoryEncode(content []byte) (json.RawMessage, error) {
	// Project-memory sections in the central file are materialization
	// artifacts (see ExportMemory); the canonical global memory must
	// never absorb them.
	clean := adapter.StripProjectSections(string(content))
	return acf.EncodePayload(acf.MemoryPayload{Format: "markdown", Content: clean})
}

func memoryDecode(e acf.Event) (string, error) {
	p, err := acf.DecodeMemoryPayload(e)
	return p.Content, err
}

// ImportMemory reads a MEMORY.md or USER.md and writes one memory artifact.
// Uses Format: "markdown" — interoperable with codex + kilo memories.
func (a *Adapter) ImportMemory(ctx context.Context, store *acf.Store, nativePath string) ([]string, error) {
	return adapter.ImportOpaque(ctx, store, acf.KindMemory, a.opaqueParams(), nativePath, memoryEncode)
}

// ExportMemory replays the memory artifact's event log and writes the result.
// Project-scoped artifacts are UPSERTED into the central memory file as a
// delimited "## Project:" section instead of overwriting it — install-rooted
// agents never read project folders, so this is how project context reaches
// them. The matching import side (memoryEncode) strips these sections so the
// global memory artifact never absorbs the mirrors.
func (a *Adapter) ExportMemory(ctx context.Context, store *acf.Store, artifactID, destPath string) error {
	if art, rerr := store.ReadArtifact(acf.KindMemory, artifactID); rerr == nil && art.Scope == acf.ScopeProject {
		content, tombstoned, perr := adapter.ReplayOpaqueContent(store, acf.KindMemory, artifactID, memoryDecode)
		if perr != nil {
			return perr
		}
		if tombstoned {
			return adapter.ErrArtifactTombstoned
		}
		existing := ""
		if data, ferr := os.ReadFile(destPath); ferr == nil {
			existing = string(data)
		}
		key := art.ArtifactID
		title := art.Name
		if art.Project != nil && art.Project.ID != "" {
			key = art.Project.ID + ":" + art.Name
			title = filepath.Base(art.Project.Path) + " — " + art.Name
		}
		composed := adapter.UpsertProjectSection(existing, key, title, content)
		if merr := os.MkdirAll(filepath.Dir(destPath), 0o755); merr != nil {
			return merr
		}
		return atomicfile.WriteFile(destPath, []byte(composed), 0o644)
	}
	return adapter.ExportOpaque(ctx, store, acf.KindMemory, artifactID, destPath, memoryDecode)
}
