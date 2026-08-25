package kilo

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter"
	"github.com/aplexica/aplexica/internal/mcp"
	"github.com/aplexica/aplexica/internal/project"
)

// ImportTool reads a Kilo kilo.jsonc file, parses it (with JSONC comment
// stripping) into the canonical MCP schema, extracts env-block secrets
// into the secrets store, and writes the redacted canonical form as a
// tool artifact. Per ADR-0027, raw secret values NEVER reach the canonical
// store.
//
// Identity reconciliation: same-source-path re-imports append an "update"
// event to the existing artifact instead of minting a new UUIDv7.
//
// Transactional rollback: on the create path, an AppendEvent failure cleans
// up BOTH the orphan artifact AND any secrets written in this call.
//
// JSONC comments are dropped on round-trip (Aplexica emits plain JSON);
// users authoring kilo.jsonc with hand-comments will see them removed on
// re-import.
func (a *Adapter) ImportTool(ctx context.Context, store *acf.Store, nativePath string) (ids []string, err error) {
	return a.importToolWith(ctx, store, nativePath, mcp.FromKiloJSONC)
}

// ImportLegacyMCPTool imports a legacy `.kilocode/mcp.json` file (the flat
// {"mcpServers": {...}} shape from the VS Code-extension era) as a tool
// artifact. Kilo migrated to `kilo.jsonc`; Export always writes the current
// kilo.jsonc form, so this is a read-only compatibility path that lets users
// with pre-migration projects import their MCP config instead of hitting an
// "unrecognized filename" error.
func (a *Adapter) ImportLegacyMCPTool(ctx context.Context, store *acf.Store, nativePath string) ([]string, error) {
	return a.importToolWith(ctx, store, nativePath, mcp.FromMCPJSON)
}

// importToolWith is the shared tool-import implementation, parameterized by the
// native-config parser (kilo.jsonc vs. legacy mcp.json).
func (a *Adapter) importToolWith(ctx context.Context, store *acf.Store, nativePath string, parse func([]byte) (mcp.Canonical, error)) (ids []string, err error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("kilo: import cancelled: %w", err)
	}
	if a.SecretsStore == nil {
		return nil, fmt.Errorf("kilo: ImportTool requires a SecretsStore (call New() or set Adapter.SecretsStore)")
	}
	abs, err := filepath.Abs(nativePath)
	if err != nil {
		return nil, fmt.Errorf("kilo: resolve path: %w", err)
	}
	content, err := os.ReadFile(abs)
	if err != nil {
		return nil, fmt.Errorf("kilo: read %s: %w", abs, err)
	}

	canonical, err := parse(content)
	if err != nil {
		return nil, fmt.Errorf("kilo: %w", err)
	}
	extractedSecrets := mcp.ExtractSecrets(&canonical)
	encoded, err := mcp.Encode(canonical)
	if err != nil {
		return nil, fmt.Errorf("kilo: %w", err)
	}

	scope := a.inferScope(abs)
	var projInfo = adapter.ProjectFromScope(scope, abs)
	// A path inferScope marks global is kilo's own global-state root (its
	// user-level config/skills/data); it stays global regardless of an ancestor
	// registered project. Mirrors the guard in adapter.ImportOpaqueContent so a
	// registered $HOME (the daemon's implicit --dir project) doesn't recapture
	// kilo's global MCP config into the home project's local scope. Only
	// non-global paths consult the registry override / ad-hoc downgrade.
	if scope != acf.ScopeGlobal {
		if regScope, projID, registered := adapter.ResolveRegisteredScope(a.Registry, abs); registered && regScope == acf.ScopeProject {
			scope = acf.ScopeProject
			if entry, ok := a.Registry.Get(projID); ok {
				projInfo = &project.ProjectInfo{
					ID:        entry.ID,
					Path:      entry.Path,
					VCS:       entry.VCS,
					Ephemeral: entry.Ephemeral,
				}
			}
		} else {
			scope, projInfo = adapter.DowngradeAdHocToGlobal(scope, projInfo)
		}
	}

	existing, found, err := store.FindBySourcePath(acf.KindTool, abs)
	if err != nil {
		return nil, err
	}

	payload, encErr := acf.EncodePayload(acf.ToolPayload{
		Format:  mcp.Format,
		Content: string(encoded),
	})
	if encErr != nil {
		return nil, fmt.Errorf("kilo: encode tool payload: %w", encErr)
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
		err = fmt.Errorf("kilo: %w", serr)
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
// the result as a native Kilo kilo.jsonc file (plain JSON; no comments).
//
// Refuses to export artifacts whose Format is not "acf.mcp.v1" — pre-v0.3.0
// tool artifacts must be re-imported.
func (a *Adapter) ExportTool(ctx context.Context, store *acf.Store, artifactID, destPath string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("kilo: export cancelled: %w", err)
	}
	if a.SecretsStore == nil {
		return fmt.Errorf("kilo: ExportTool requires a SecretsStore")
	}
	// Replay through the shared tool-replay helper so tool export inherits the
	// same retention/prune resilience (FR-02.32) as memory/skill/conversation:
	// after CreateSnapshot+PruneArtifact the active log is snapshot-only, and
	// the helper decodes a payload-bearing snapshot or falls back to the
	// compacted layer rather than failing VerifyChain across the prune boundary.
	current, err := adapter.ReplayToolPayload(store, artifactID)
	if err != nil {
		return fmt.Errorf("kilo: %w", err)
	}

	if current.Format != mcp.Format {
		return fmt.Errorf("kilo: tool artifact %s has Format %q (expected %q) — re-import the source file to migrate (v0.3.0 changed the canonical tool format)",
			artifactID, current.Format, mcp.Format)
	}

	canonical, err := mcp.Decode([]byte(current.Content))
	if err != nil {
		return fmt.Errorf("kilo: %w", err)
	}

	keys, err := a.SecretsStore.ListForArtifact(artifactID)
	if err != nil {
		return fmt.Errorf("kilo: list secrets: %w", err)
	}
	secretsMap := make(map[string]string, len(keys))
	for _, k := range keys {
		v, err := a.SecretsStore.Get(artifactID, k)
		if err != nil {
			return fmt.Errorf("kilo: get secret %s: %w", k, err)
		}
		secretsMap[k] = v
	}
	if err := mcp.ExpandSecrets(&canonical, secretsMap); err != nil {
		return fmt.Errorf("kilo: %w", err)
	}

	out, err := mcp.ToKiloJSONC(canonical)
	if err != nil {
		return fmt.Errorf("kilo: %w", err)
	}

	return adapter.WriteSecretConfig(destPath, out)
}
