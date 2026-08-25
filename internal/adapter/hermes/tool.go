package hermes

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

// ImportTool reads a Hermes config.yaml file, parses its mcp_servers
// section into the canonical MCP schema, extracts env secrets into the
// secrets store, and writes the redacted canonical form as a tool
// artifact. Non-mcp_servers content in config.yaml is intentionally
// dropped — hermes tool artifacts carry only the mcp_servers config
// (matches the codex pattern for config.toml).
//
// Identity reconciliation + transactional rollback inherited from the
// shared pattern used by claudecode, codex, and kilo.
func (a *Adapter) ImportTool(ctx context.Context, store *acf.Store, nativePath string) (ids []string, err error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("hermes: import cancelled: %w", err)
	}
	if a.SecretsStore == nil {
		return nil, fmt.Errorf("hermes: ImportTool requires a SecretsStore")
	}
	abs, err := filepath.Abs(nativePath)
	if err != nil {
		return nil, fmt.Errorf("hermes: resolve path: %w", err)
	}
	content, err := os.ReadFile(abs)
	if err != nil {
		return nil, fmt.Errorf("hermes: read %s: %w", abs, err)
	}

	canonical, err := mcp.FromHermesYAML(content)
	if err != nil {
		return nil, fmt.Errorf("hermes: %w", err)
	}
	extractedSecrets := mcp.ExtractSecrets(&canonical)
	encoded, err := mcp.Encode(canonical)
	if err != nil {
		return nil, fmt.Errorf("hermes: %w", err)
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
		return nil, fmt.Errorf("hermes: encode tool payload: %w", encErr)
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
		err = fmt.Errorf("hermes: %w", serr)
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
// the result as a native Hermes config.yaml (mcp_servers section only).
func (a *Adapter) ExportTool(ctx context.Context, store *acf.Store, artifactID, destPath string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("hermes: export cancelled: %w", err)
	}
	if a.SecretsStore == nil {
		return fmt.Errorf("hermes: ExportTool requires a SecretsStore")
	}
	events, err := store.ReadEvents(acf.KindTool, artifactID)
	if err != nil {
		return fmt.Errorf("hermes: read events: %w", err)
	}
	if len(events) == 0 {
		return fmt.Errorf("hermes: no events for artifact %s", artifactID)
	}
	if err := acf.VerifyChain(events); err != nil {
		return fmt.Errorf("hermes: event log is invalid: %w", err)
	}

	var current acf.ToolPayload
	for _, e := range events {
		switch e.Type {
		case acf.EventTypeCreate, acf.EventTypeUpdate, acf.EventTypeResolution:
			// EventTypeResolution (v0.34.0) re-asserts a winning payload
			// after conflict resolution — replay identical to create/update.
			decoded, err := acf.DecodeToolPayload(e)
			if err != nil {
				return fmt.Errorf("hermes: %w", err)
			}
			current = decoded
		case acf.EventTypeRedaction:
			current = acf.ToolPayload{Format: current.Format}
		}
	}

	if current.Format != mcp.Format {
		return fmt.Errorf("hermes: tool artifact %s has Format %q (expected %q) — re-import to migrate",
			artifactID, current.Format, mcp.Format)
	}

	canonical, err := mcp.Decode([]byte(current.Content))
	if err != nil {
		return fmt.Errorf("hermes: %w", err)
	}

	keys, err := a.SecretsStore.ListForArtifact(artifactID)
	if err != nil {
		return fmt.Errorf("hermes: list secrets: %w", err)
	}
	secretsMap := make(map[string]string, len(keys))
	for _, k := range keys {
		v, err := a.SecretsStore.Get(artifactID, k)
		if err != nil {
			return fmt.Errorf("hermes: get secret %s: %w", k, err)
		}
		secretsMap[k] = v
	}
	if err := mcp.ExpandSecrets(&canonical, secretsMap); err != nil {
		return fmt.Errorf("hermes: %w", err)
	}

	out, err := mcp.ToHermesYAML(canonical)
	if err != nil {
		return fmt.Errorf("hermes: %w", err)
	}

	return adapter.WriteSecretConfig(destPath, out)
}
