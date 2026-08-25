package codex

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

// ImportTool reads a Codex config.toml fragment, parses its [mcp_servers.*]
// tables into the canonical MCP schema, extracts env-block secrets into the
// secrets store, and writes the redacted canonical form as a tool artifact.
// Per ADR-0027, raw secret values NEVER reach the canonical store.
//
// Non-mcp_servers TOML content (model, marketplaces, plugins, etc.) is
// dropped on import — codex tool artifacts hold only the mcp_servers config.
//
// Identity reconciliation: looks up an existing tool artifact by absolute
// SourcePath before minting a new UUIDv7. If found, appends an "update"
// event and overwrites its secrets; otherwise creates a new artifact.
//
// Transactional rollback: on the create path only, an error after the fresh
// WriteArtifact triggers cleanup of BOTH the orphan artifact AND any
// secrets written in this call.
func (a *Adapter) ImportTool(ctx context.Context, store *acf.Store, nativePath string) (ids []string, err error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("codex: import cancelled: %w", err)
	}
	if a.SecretsStore == nil {
		return nil, fmt.Errorf("codex: ImportTool requires a SecretsStore (call New() or set Adapter.SecretsStore)")
	}
	abs, err := filepath.Abs(nativePath)
	if err != nil {
		return nil, fmt.Errorf("codex: resolve path: %w", err)
	}
	content, err := os.ReadFile(abs)
	if err != nil {
		return nil, fmt.Errorf("codex: read %s: %w", abs, err)
	}

	canonical, err := mcp.FromMCPTOML(content)
	if err != nil {
		return nil, fmt.Errorf("codex: %w", err)
	}
	extractedSecrets := mcp.ExtractSecrets(&canonical)
	encoded, err := mcp.Encode(canonical)
	if err != nil {
		return nil, fmt.Errorf("codex: %w", err)
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
		return nil, fmt.Errorf("codex: encode tool payload: %w", encErr)
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
		err = fmt.Errorf("codex: %w", serr)
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
// matching secrets from the secrets store, expands placeholders, and writes
// the result as a native codex config.toml fragment ([mcp_servers.*] only)
// to destPath.
//
// Refuses to export artifacts whose Format is not "acf.mcp.v1" — pre-v0.3.0
// tool artifacts must be re-imported (see v0.3.0 CHANGELOG for migration).
func (a *Adapter) ExportTool(ctx context.Context, store *acf.Store, artifactID, destPath string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("codex: export cancelled: %w", err)
	}
	if a.SecretsStore == nil {
		return fmt.Errorf("codex: ExportTool requires a SecretsStore")
	}
	// Replay through the shared tool-replay helper so tool export inherits the
	// same retention/prune resilience (FR-02.32) as memory/skill/conversation:
	// after CreateSnapshot+PruneArtifact the active log is snapshot-only, and
	// the helper decodes a payload-bearing snapshot or falls back to the
	// compacted layer rather than failing VerifyChain across the prune boundary.
	current, err := adapter.ReplayToolPayload(store, artifactID)
	if err != nil {
		return fmt.Errorf("codex: %w", err)
	}

	if current.Format != mcp.Format {
		return fmt.Errorf("codex: tool artifact %s has Format %q (expected %q) — re-import the source file to migrate (v0.3.0 changed the canonical tool format)",
			artifactID, current.Format, mcp.Format)
	}

	canonical, err := mcp.Decode([]byte(current.Content))
	if err != nil {
		return fmt.Errorf("codex: %w", err)
	}

	keys, err := a.SecretsStore.ListForArtifact(artifactID)
	if err != nil {
		return fmt.Errorf("codex: list secrets: %w", err)
	}
	secretsMap := make(map[string]string, len(keys))
	for _, k := range keys {
		v, err := a.SecretsStore.Get(artifactID, k)
		if err != nil {
			return fmt.Errorf("codex: get secret %s: %w", k, err)
		}
		secretsMap[k] = v
	}
	if err := mcp.ExpandSecrets(&canonical, secretsMap); err != nil {
		return fmt.Errorf("codex: %w", err)
	}

	out, err := mcp.ToMCPTOML(canonical)
	if err != nil {
		return fmt.Errorf("codex: %w", err)
	}

	return adapter.WriteSecretConfig(destPath, out)
}
