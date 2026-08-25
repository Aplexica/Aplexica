package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
)

func appendConflictResolutionEvent(store *acf.Store, kind acf.Kind, artifactID string, payload json.RawMessage, sourceAgent, deviceID string) error {
	if store == nil {
		return errors.New("conflicts: canonical store not wired")
	}
	now := time.Now().UTC()
	parentHash, err := ensureResolutionArtifactShell(store, kind, artifactID, now)
	if err != nil {
		return err
	}
	return store.AppendEvent(kind, acf.Event{
		EventID:    acf.NewID(),
		ArtifactID: artifactID,
		Type:       acf.EventTypeResolution,
		Timestamp:  now,
		Provenance: acf.Provenance{DeviceID: deviceID, SourceAgent: sourceAgent},
		Payload:    append(json.RawMessage(nil), payload...),
		ParentHash: parentHash,
	})
}

func ensureResolutionArtifactShell(store *acf.Store, kind acf.Kind, artifactID string, now time.Time) (string, error) {
	art, err := store.ReadArtifact(kind, artifactID)
	if err == nil {
		return mainBranchHead(art), nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}

	head, err := store.HeadHashByBranch(kind, artifactID, acf.MainBranch)
	if err != nil {
		return "", err
	}
	shell := acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion,
		ArtifactID:       artifactID,
		Kind:             kind,
		Scope:            acf.ScopeGlobal,
		CreatedAt:        now,
		UpdatedAt:        now,
		HeadEventHash:    head,
	}
	if head != "" {
		shell.BranchHeads = map[string]string{acf.MainBranch: head}
	}
	if err := store.WriteArtifact(shell); err != nil {
		return "", fmt.Errorf("conflicts: recreate artifact shell: %w", err)
	}
	return head, nil
}

func mainBranchHead(art acf.Artifact) string {
	if art.BranchHeads != nil {
		if head := art.BranchHeads[acf.MainBranch]; head != "" {
			return head
		}
	}
	return art.HeadEventHash
}
