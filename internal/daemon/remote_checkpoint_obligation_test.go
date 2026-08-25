package daemon

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRemoteCheckpointObligationStoreRestartAndPathSafety(t *testing.T) {
	root := filepath.Join(t.TempDir(), "checkpoint-obligations")
	store := &RemoteCheckpointObligationStore{Root: root}
	if err := store.Init(); err != nil {
		t.Fatal(err)
	}
	record := RemoteCheckpointObligationV1{
		ScopeID: "namespace\\..\\outside/child", ArtifactID: "artifact\\branch/../id",
		BranchID: "feature\\windows/path", Kind: "conversation",
		HeadEventID: "event-1", HeadEventHash: watermarkTestDigest("head"),
		Reason: "canonical-history-compacted",
	}
	if err := store.Put(record); err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || strings.ContainsAny(entries[0].Name(), `/\\`) || entries[0].Name() != obligationKey(record.ScopeID, record.ArtifactID, record.BranchID)+".json" {
		t.Fatalf("obligation identity leaked into path: %+v", entries)
	}

	restarted := &RemoteCheckpointObligationStore{Root: root}
	if err := restarted.Init(); err != nil {
		t.Fatal(err)
	}
	got, err := restarted.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ScopeID != record.ScopeID || got[0].ArtifactID != record.ArtifactID || got[0].BranchID != record.BranchID {
		t.Fatalf("restart lost obligation: %+v", got)
	}

	path := filepath.Join(root, entries[0].Name())
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("checkpoint_prepared_at")) || bytes.Contains(raw, []byte("checkpoint_committed_at")) {
		t.Fatal("zero lifecycle fields changed the schema-v1 checksum shape")
	}
	if err := os.WriteFile(path, append(raw, []byte("\n{}")...), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.List(); err == nil {
		t.Fatal("tampered obligation unexpectedly passed checksum validation")
	}
}

func TestRemoteCheckpointObligationRejectsPartialOrUnknownGeneration(t *testing.T) {
	store := &RemoteCheckpointObligationStore{Root: filepath.Join(t.TempDir(), "checkpoint-obligations")}
	if err := store.Init(); err != nil {
		t.Fatal(err)
	}
	for _, value := range []RemoteCheckpointObligationV1{
		{ScopeID: "account", Reason: "partial", AccessGeneration: 1},
		{ScopeID: "account", Reason: "unknown", AccessGeneration: 1, AccessSetHash: watermarkTestDigest("access"), SecurityGeneration: 1, SecurityBarrier: watermarkTestDigest("barrier"), KeyMode: "future-mode"},
		{ScopeID: "account", Reason: "bad-version", AccessGeneration: 1, AccessSetHash: watermarkTestDigest("access"), SecurityGeneration: 1, SecurityBarrier: watermarkTestDigest("barrier"), KeyMode: "recipient-wrap-v2", KeyVersion: 4},
	} {
		if err := store.Put(value); err == nil {
			t.Fatalf("invalid generation accepted: %+v", value)
		}
	}
}
