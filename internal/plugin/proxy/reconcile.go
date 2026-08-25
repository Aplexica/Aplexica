package proxy

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/plugin/proto"
)

// Reconciler owns the daemon-side responsibilities the spec assigns to
// the daemon for plugin imports: identity reconciliation by SourcePath,
// event ID minting, parent_hash chain maintenance, atomic write.
type Reconciler struct {
	Store          *acf.Store
	DeviceID       string
	SourceAgent    string // e.g. "claude-code" — populated from the plugin's manifest name
	AdapterVersion string // plugin's PluginVersion from the initialize response
}

// Apply writes one ImportedItem to the store. Returns the resulting
// artifact ID. If an artifact with the same SourcePath+Kind already
// exists, an update event is appended to it; otherwise a new artifact
// is minted.
//
// Mirrors adapter.ImportOpaque (internal/adapter/opaque.go:46) but
// without an encoder closure — the plugin has already produced the
// typed payload.
func (r Reconciler) Apply(ctx context.Context, item proto.ImportedItem, causedBy string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	abs, err := filepath.Abs(item.SourcePath)
	if err != nil {
		return "", fmt.Errorf("plugin/proxy: resolve source path: %w", err)
	}

	existing, found, err := r.Store.FindBySourcePath(item.Kind, abs)
	if err != nil {
		return "", err
	}

	now := time.Now().UTC()
	var (
		id         string
		eventType  acf.EventType
		parentHash string
		createdNew bool
	)
	if found {
		id = existing.ArtifactID
		eventType = acf.EventTypeUpdate
		parentHash = existing.HeadEventHash
		existing.UpdatedAt = now
		if werr := r.Store.WriteArtifact(existing); werr != nil {
			return "", werr
		}
	} else {
		id = acf.NewID()
		eventType = acf.EventTypeCreate
		artifact := acf.Artifact{
			AcfSchemaVersion: acf.SchemaVersion,
			ArtifactID:       id,
			Kind:             item.Kind,
			Scope:            item.Scope,
			Name:             item.Name,
			SourcePath:       abs,
			CreatedAt:        now,
			UpdatedAt:        now,
		}
		if werr := r.Store.WriteArtifact(artifact); werr != nil {
			return "", werr
		}
		createdNew = true
	}

	event := acf.Event{
		EventID:    acf.NewID(),
		ArtifactID: id,
		Type:       eventType,
		Timestamp:  now,
		Provenance: acf.Provenance{
			DeviceID:       r.DeviceID,
			SourceAgent:    r.SourceAgent,
			AdapterVersion: r.AdapterVersion,
			CausedBy:       causedBy,
		},
		Payload:    item.Payload,
		ParentHash: parentHash,
	}
	if aerr := r.Store.AppendEvent(item.Kind, event); aerr != nil {
		if createdNew {
			_ = r.Store.DeleteArtifact(item.Kind, id)
		}
		return "", aerr
	}
	return id, nil
}
