package syncd

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter"
	"github.com/aplexica/aplexica/internal/plugin/proto"
	"github.com/aplexica/aplexica/internal/syncgate"
	"github.com/stretchr/testify/require"
)

func TestRedactionReplayBatchNeverMaterializesSupersededContent(t *testing.T) {
	root := realTempDir(t)
	store := &acf.Store{Root: root + "/store"}
	require.NoError(t, store.Init())

	local := newTestDevice(t, "windows-device")
	source := &fakeConvSource{name: "codex"}
	target := &fakeConvTarget{fakeConvSource: fakeConvSource{name: "claude-code"}}
	orch, err := NewOrchestrator(Config{
		Dir:               root,
		Adapters:          []adapter.Adapter{source, target},
		Store:             store,
		SyncGate:          syncgate.New(syncgate.Config{Agents: map[string]bool{"claude-code": true}}),
		LocalDeviceID:     local.id,
		DeviceKeyProvider: fixedKeyProvider{priv: local.priv},
	})
	require.NoError(t, err)
	defer orch.Close()

	now := time.Now().UTC()
	artifactID := acf.NewID()
	payload, err := json.Marshal(acf.ConversationPayload{
		Format: acf.ConversationFormatV1,
		Events: []acf.ConversationEvent{{
			Type: acf.EventTypeTurn, Role: "user", Timestamp: now,
			Content: []acf.ContentBlock{{Type: "text", Text: "content removed before replay completed"}},
		}},
	})
	require.NoError(t, err)
	visible := acf.Event{
		EventID: acf.NewID(), ArtifactID: artifactID, Type: acf.EventTypeCreate, Timestamp: now,
		Provenance: acf.Provenance{DeviceID: "mac-device", SourceAgent: "codex"}, Payload: payload,
	}
	visible.Hash, err = acf.ComputeHash(visible)
	require.NoError(t, err)
	redaction := acf.Event{
		EventID: acf.NewID(), ArtifactID: artifactID, Type: acf.EventTypeRedaction, Timestamp: now.Add(time.Second),
		Provenance: acf.Provenance{DeviceID: "mac-device", SourceAgent: "codex"}, ParentHash: visible.Hash,
	}
	redaction.Hash, err = acf.ComputeHash(redaction)
	require.NoError(t, err)

	toWire := func(event acf.Event, sequence uint64) proto.RemoteEvent {
		sealed, sealErr := sealEnvelope(event, acf.ScopeGlobal, nil, []recipient{{deviceID: local.id, pub: local.pub}})
		require.NoError(t, sealErr)
		return proto.RemoteEvent{
			BranchID: acf.MainBranch, ArtifactID: artifactID, EventID: event.EventID, ParentHash: event.ParentHash,
			EventHash: event.Hash, Kind: string(acf.KindConversation), Type: string(event.Type), Timestamp: event.Timestamp,
			Bytes: sealed, Sequence: sequence, Origin: event.Provenance.DeviceID, SourceAgent: event.Provenance.SourceAgent, Lane: LaneLive,
		}
	}
	wires := []proto.RemoteEvent{toWire(visible, 1), toWire(redaction, 2)}
	outcomes := orch.ImportInboundCanonicalResults(wires)
	require.Equal(t, []ImportOutcome{ImportApplied, ImportApplied}, outcomes)
	require.Zero(t, target.materialized(), "no intermediate native write may expose content removed later in the replay batch")

	artifact, err := store.ReadArtifact(acf.KindConversation, artifactID)
	require.NoError(t, err)
	require.True(t, artifact.Tombstoned)
	head, ok, err := store.LastEvent(acf.KindConversation, artifactID)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, redaction.EventID, head.EventID)
	require.Equal(t, acf.EventTypeRedaction, head.Type)

	evidence, err := orch.CanonicalEvidenceForTerminalInbound(wires[1], outcomes[1])
	require.NoError(t, err)
	require.Equal(t, redaction.EventID, evidence.EventID)
	require.NoError(t, orch.FinalizeInboundCanonicalEvidence(evidence))
	require.Zero(t, target.materialized(), "terminal redaction must never resurrect the superseded payload")
}
