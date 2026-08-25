package openclaw

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter"
	"github.com/aplexica/aplexica/internal/mcp"
)

// ImportTool reads an openclaw.json file, extracts its mcp.servers section
// into the canonical MCP schema, pulls env secrets into the secrets store,
// and writes the redacted canonical form as a tool artifact. Non-mcp content
// in openclaw.json (channels, models, agents, etc.) is intentionally dropped
// — tool artifacts carry only the MCP servers (matches the hermes config.yaml
// and codex config.toml patterns).
func (a *Adapter) ImportTool(ctx context.Context, store *acf.Store, nativePath string) (ids []string, err error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("openclaw: import cancelled: %w", err)
	}
	if a.SecretsStore == nil {
		return nil, fmt.Errorf("openclaw: ImportTool requires a SecretsStore")
	}
	abs, err := filepath.Abs(nativePath)
	if err != nil {
		return nil, fmt.Errorf("openclaw: resolve path: %w", err)
	}
	content, err := os.ReadFile(abs)
	if err != nil {
		return nil, fmt.Errorf("openclaw: read %s: %w", abs, err)
	}

	canonical, err := mcp.FromOpenClawJSON(content)
	if err != nil {
		return nil, fmt.Errorf("openclaw: %w", err)
	}
	extractedSecrets := mcp.ExtractSecrets(&canonical)
	encoded, err := mcp.Encode(canonical)
	if err != nil {
		return nil, fmt.Errorf("openclaw: %w", err)
	}

	scope := a.inferScope(abs)
	var projInfo = adapter.ProjectFromScope(scope, abs)
	scope, projInfo = adapter.DowngradeAdHocToGlobal(scope, projInfo)

	existing, found, err := store.FindBySourcePath(acf.KindTool, abs)
	if err != nil {
		return nil, err
	}

	payload, encErr := acf.EncodePayload(acf.ToolPayload{
		Format:  mcp.Format,
		Content: string(encoded),
	})
	if encErr != nil {
		return nil, fmt.Errorf("openclaw: encode tool payload: %w", encErr)
	}

	// Loop break: when BOTH the redacted payload AND every stored secret are
	// unchanged, a re-import (e.g. the daemon's startup InitialScan re-reading
	// an unchanged config) is a no-op — reuse the id, append no event. A secret
	// rotation leaves the payload identical but flips this to false so the
	// rotated value still fans out.
	if found {
		unchanged, uerr := adapter.ToolImportUnchanged(store, a.SecretsStore, existing.ArtifactID, payload, extractedSecrets)
		if uerr != nil {
			return nil, uerr
		}
		if unchanged {
			return []string{existing.ArtifactID}, nil
		}
	}

	now := time.Now().UTC()
	var id string
	var eventType acf.EventType
	var parentHash string
	createdNew := false
	if found {
		id = existing.ArtifactID
		eventType = acf.EventTypeUpdate
		var herr error
		parentHash, herr = adapter.RefreshMainBranchHead(store, acf.KindTool, &existing)
		if herr != nil {
			return nil, herr
		}
		existing.UpdatedAt = now
		if werr := store.WriteArtifact(existing); werr != nil {
			return nil, werr
		}
	} else {
		id = acf.NewID()
		eventType = acf.EventTypeCreate
		parentHash = ""
		artifact := acf.Artifact{
			AcfSchemaVersion: acf.SchemaVersion,
			ArtifactID:       id,
			Kind:             acf.KindTool,
			Scope:            scope,
			Project:          projInfo,
			Name:             filepath.Base(abs),
			SourcePath:       abs,
			CreatedAt:        now,
			UpdatedAt:        now,
		}
		if werr := store.WriteArtifact(artifact); werr != nil {
			return nil, werr
		}
		createdNew = true
	}

	defer func() {
		if err != nil && createdNew {
			_ = store.DeleteArtifact(acf.KindTool, id)
			adapter.RollbackToolSecrets(a.SecretsStore, id, extractedSecrets)
		}
	}()

	if serr := adapter.WriteToolSecrets(a.SecretsStore, id, extractedSecrets); serr != nil {
		err = fmt.Errorf("openclaw: %w", serr)
		return nil, err
	}

	event := acf.Event{
		EventID:    acf.NewID(),
		ArtifactID: id,
		Type:       eventType,
		Timestamp:  now,
		Provenance: acf.Provenance{
			DeviceID:       a.deviceID(),
			SourceAgent:    a.Name(),
			AgentVersion:   acf.UnknownAgentVersion,
			AdapterVersion: a.Version(),
		},
		Payload:    payload,
		ParentHash: parentHash,
	}
	if aerr := store.AppendEvent(acf.KindTool, event); aerr != nil {
		err = aerr
		return nil, err
	}
	return []string{id}, nil
}

// ExportTool reads a tool artifact, decodes its canonical payload, fetches
// secrets, expands placeholders, and writes the result as a native
// openclaw.json file. If destPath already exists, the existing file is read
// and merged with the canonical's mcp.servers section so non-MCP top-level
// keys (channels, agents, models, etc.) and any non-servers fields under
// `mcp` survive the export. The servers map itself is fully replaced — each
// Aplexica export is the source of truth for the MCP-server set. The write
// is atomic via atomicfile.WriteFile.
func (a *Adapter) ExportTool(ctx context.Context, store *acf.Store, artifactID, destPath string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("openclaw: export cancelled: %w", err)
	}
	if a.SecretsStore == nil {
		return fmt.Errorf("openclaw: ExportTool requires a SecretsStore")
	}
	// Replay through the shared tool-replay helper so tool export inherits the
	// same retention/prune resilience (FR-02.32) as memory/skill/conversation:
	// after CreateSnapshot+PruneArtifact the active log is snapshot-only, and
	// the helper decodes a payload-bearing snapshot or falls back to the
	// compacted layer rather than failing VerifyChain across the prune boundary.
	current, err := adapter.ReplayToolPayload(store, artifactID)
	if err != nil {
		return fmt.Errorf("openclaw: %w", err)
	}

	if current.Format != mcp.Format {
		return fmt.Errorf("openclaw: tool artifact %s has Format %q (expected %q) — re-import to migrate",
			artifactID, current.Format, mcp.Format)
	}

	canonical, err := mcp.Decode([]byte(current.Content))
	if err != nil {
		return fmt.Errorf("openclaw: %w", err)
	}

	keys, err := a.SecretsStore.ListForArtifact(artifactID)
	if err != nil {
		return fmt.Errorf("openclaw: list secrets: %w", err)
	}
	secretsMap := make(map[string]string, len(keys))
	for _, k := range keys {
		v, err := a.SecretsStore.Get(artifactID, k)
		if err != nil {
			return fmt.Errorf("openclaw: get secret %s: %w", k, err)
		}
		secretsMap[k] = v
	}
	if err := mcp.ExpandSecrets(&canonical, secretsMap); err != nil {
		return fmt.Errorf("openclaw: %w", err)
	}

	out, err := mcp.ToOpenClawJSON(canonical)
	if err != nil {
		return fmt.Errorf("openclaw: %w", err)
	}
	// If destPath already exists, merge with the existing content so non-MCP
	// top-level keys (channels, agents, models, etc.) survive the export —
	// and keep the existing file MODE: the root ~/.openclaw/openclaw.json
	// holds the gateway auth token and must not be loosened to world-readable.
	if existing, rerr := os.ReadFile(destPath); rerr == nil {
		merged, merr := mcp.MergeIntoOpenClawJSON(existing, canonical)
		if merr != nil {
			return fmt.Errorf("openclaw: merge with existing %s: %w", destPath, merr)
		}
		out = merged
	}
	return adapter.WriteSecretConfig(destPath, out)
}
