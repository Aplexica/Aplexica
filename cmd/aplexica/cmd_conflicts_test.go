package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/conflicts"
	"github.com/stretchr/testify/require"
)

func runConflictsCmd(t *testing.T, args ...string) (string, error) {
	t.Helper()
	t.Cleanup(func() {
		conflictsRoot = ""
		resolvePickIdx = 0
		resolveStoreRoot = ""
	})
	return runRoot(t, append([]string{"conflicts"}, args...)...)
}

// TestConflictsResolve_PicksRemoteHeadAbsentFromLocalStore is the B3 fix: a
// remote inbound conflict head is never appended to the local log, so resolving
// it by EventID lookup in the local store used to error "winner event <id> not
// found in artifact log". The full remote payload now lives in the local-only
// conflict sidecar (Head.FullPayload), so resolve --pick of the remote head
// succeeds and writes a real EventTypeResolution event carrying that full
// payload (BRD-04 §6.3 / FR-04.8).
func TestConflictsResolve_PicksRemoteHeadAbsentFromLocalStore(t *testing.T) {
	tmp := t.TempDir()
	storeRoot := filepath.Join(tmp, "store")
	conflictsDir := filepath.Join(tmp, "conflicts")

	id := seedMemoryArtifact(t, storeRoot, "claude-code", "# local edit\n")

	store := &acf.Store{Root: storeRoot}
	localEvents, err := store.ReadEvents(acf.KindMemory, id)
	require.NoError(t, err)
	require.Len(t, localEvents, 1)
	localHead := localEvents[0]

	// The remote head: a divergent edit that was NEVER appended to the local
	// log. Its full payload exceeds the 200-char preview cap and is preserved
	// only in the conflict sidecar.
	remoteContent := strings.Repeat("remote-divergent-content ", 40)
	remotePayload, err := acf.EncodePayload(acf.MemoryPayload{Format: "markdown", Content: remoteContent})
	require.NoError(t, err)
	require.Greater(t, len(remotePayload), 200)
	remoteEventID := acf.NewID()

	cs := &conflicts.Store{Root: conflictsDir}
	require.NoError(t, cs.Init())
	require.NoError(t, cs.Record(conflicts.Conflict{
		ArtifactID: id,
		Kind:       acf.KindMemory,
		Heads: []conflicts.Head{
			{
				SourceAgent:   localHead.Provenance.SourceAgent,
				EventID:       localHead.EventID,
				ContentSHA256: localHead.Hash,
			},
			{
				SourceAgent:    "codex",
				EventID:        remoteEventID, // absent from the local store
				ContentSHA256:  "remote-hash",
				PayloadPreview: string(remotePayload[:200]),
				FullPayload:    json.RawMessage(remotePayload),
			},
		},
	}))

	out, err := runConflictsCmd(t, "resolve", id,
		"--pick", "1",
		"--conflicts-root", conflictsDir,
		"--store", storeRoot,
	)
	require.NoError(t, err, "resolve of an absent remote head must succeed; out:\n%s", out)

	// A real EventTypeResolution event must be appended carrying the FULL remote
	// payload.
	final, err := store.ReadEvents(acf.KindMemory, id)
	require.NoError(t, err)
	var resolution *acf.Event
	for i := range final {
		if final[i].Type == acf.EventTypeResolution {
			resolution = &final[i]
		}
	}
	require.NotNil(t, resolution, "expected an EventTypeResolution event in the log")
	require.JSONEq(t, string(remotePayload), string(resolution.Payload),
		"resolution payload must equal the full remote payload")
	require.NoError(t, acf.VerifyChain(final), "resolved chain must remain intact")

	// The conflict file must be cleared.
	_, err = cs.Get(id)
	require.Error(t, err, "resolve must clear the conflict file")
}
