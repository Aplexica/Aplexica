package syncd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/stretchr/testify/require"
)

func TestMaterializeRemoteCheckpointBindsExactCurrentHeadCoverageAndGeneration(t *testing.T) {
	roster, device := signedTestRoster(t)
	var barrier [32]byte
	barrier[0] = 23
	orchestrator, store := newV2InboundOrchestratorForTest(t, roster, device, barrier, "device-a")
	artifactID, _, head := seedConversationWithDelta(t, store, "device-a")
	generation := recoveryGenerationForTest(roster, barrier)

	materialized, err := orchestrator.MaterializeRemoteCheckpoint(context.Background(), RemoteCheckpointMaterializeRequest{
		ScopeID: "account", ArtifactID: artifactID, BranchID: acf.MainBranch, Kind: string(acf.KindConversation),
		Coverage: 17, Generation: generation,
	})
	require.NoError(t, err)
	require.Equal(t, head.EventID, materialized.HeadEventID)
	require.Equal(t, head.Hash, materialized.HeadHash)
	require.Equal(t, generation, materialized.Generation)
	require.Equal(t, uint64(17), materialized.Event.CheckpointCoverage)
	require.True(t, validLowerHexSHA256(materialized.Event.CheckpointGeneration))
	require.Equal(t, LaneRetained, materialized.Event.Lane)
	require.Equal(t, head.Hash, materialized.Event.CheckpointAlignmentHash)
	require.Equal(t, RetainedWireEventID(head.EventID, "device-a"), materialized.Event.EventID)

	body, header, err := OpenEnvelopeV2(materialized.Event.Bytes, roster, "device-a", device.WrapPrivate)
	require.NoError(t, err)
	require.Equal(t, materialized.Event.EventID, header.Routing.WireEventID)
	require.Equal(t, LaneRetained, header.Routing.Lane)
	require.Equal(t, head.Hash, body.Event.AlignedHead)
	require.Equal(t, head.EventID, body.Event.AlignedEventID)
	payload, err := acf.DecodeConversationPayload(body.Event)
	require.NoError(t, err)
	require.Len(t, payload.Events, 2, "checkpoint must contain the exact full current conversation")
}

func TestMaterializeRemoteCheckpointRejectsExpectedHeadBeforeBodyRead(t *testing.T) {
	roster, device := signedTestRoster(t)
	var barrier [32]byte
	barrier[0] = 25
	orchestrator, store := newV2InboundOrchestratorForTest(t, roster, device, barrier, "device-a")
	artifactID, _ := seedArtifact(t, store, acf.KindMemory, "device-a")
	// Make any canonical body read observably fail. Artifact metadata still
	// contains the durably recorded branch-head hash used by the preflight.
	eventsPath := filepath.Join(store.Root, "events", "memories", artifactID+".jsonl")
	require.NoError(t, os.WriteFile(eventsPath, []byte("not-json\n"), 0o600))

	_, err := orchestrator.MaterializeRemoteCheckpoint(context.Background(), RemoteCheckpointMaterializeRequest{
		ScopeID: "account", ArtifactID: artifactID, BranchID: acf.MainBranch, Kind: string(acf.KindMemory), Coverage: 9,
		ExpectedAlignmentHash: strings.Repeat("f", 64),
	})
	require.ErrorIs(t, err, ErrRemoteCheckpointUnavailable)
	require.NotContains(t, err.Error(), "parse event", "stale alignment must fail before reading canonical bodies")
}

func TestMaterializeRemoteCheckpointRejectsStaleGenerationAndCanSelectVerifiedCurrentGeneration(t *testing.T) {
	roster, device := signedTestRoster(t)
	var barrier [32]byte
	barrier[0] = 24
	orchestrator, store := newV2InboundOrchestratorForTest(t, roster, device, barrier, "device-a")
	artifactID, head := seedArtifact(t, store, acf.KindMemory, "device-a")
	stale := recoveryGenerationForTest(roster, barrier)
	stale.SecurityGeneration++

	_, err := orchestrator.MaterializeRemoteCheckpoint(context.Background(), RemoteCheckpointMaterializeRequest{
		ScopeID: "account", ArtifactID: artifactID, BranchID: acf.MainBranch, Kind: string(acf.KindMemory),
		Coverage: 4, Generation: stale,
	})
	require.ErrorIs(t, err, ErrRemoteCheckpointGenerationSuperseded)

	current, err := orchestrator.MaterializeRemoteCheckpoint(context.Background(), RemoteCheckpointMaterializeRequest{
		ScopeID: "account", ArtifactID: artifactID, BranchID: acf.MainBranch, Kind: string(acf.KindMemory), Coverage: 4,
	})
	require.NoError(t, err)
	require.Equal(t, head.EventID, current.HeadEventID)
	require.Equal(t, recoveryGenerationForTest(roster, barrier), current.Generation)
}
