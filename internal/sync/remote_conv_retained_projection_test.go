package syncd

import (
	"context"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter"
	"github.com/stretchr/testify/require"
)

const (
	retainedTestOriginDevice   = "11111111-1111-4111-8111-111111111111"
	retainedTestReceiverDevice = "22222222-2222-4222-8222-222222222222"
)

func retainedTestTurn(ts time.Time, role, text string) acf.ConversationEvent {
	return acf.ConversationEvent{
		Type: acf.EventTypeTurn, Role: role, Timestamp: ts,
		Content: []acf.ContentBlock{{Type: "text", Text: text}},
	}
}

func retainedTestStamps(evs []acf.ConversationEvent) []string {
	out := make([]string, 0, len(evs))
	for _, e := range evs {
		out = append(out, e.Timestamp.UTC().Format(time.RFC3339Nano)+"|"+e.Role)
	}
	return out
}

// TestRetainedConversation_ReStampedBaseDoesNotBlockDuplicateOnReceiver
// models the regression end-to-end across two independent ACF stores:
//
//	origin   = Codex creates U1 and A1; Claude Code then continues the generated
//	           session after re-stamping its base rows.
//	receiver = a peer catches up from retained full-state baselines.
//
// Before the fix the origin's retained snapshot carried the re-stamped base, the
// receiver classified convDiverged and union-merged, producing a
// [U1,A1,U1,A1,U2] BLOCK duplicate in a locally authored, empty-adapterVersion,
// full acf.conversation.v1 event.
func TestRetainedConversation_ReStampedBaseDoesNotBlockDuplicateOnReceiver(t *testing.T) {
	var (
		tU1 = time.Date(2026, 1, 2, 3, 4, 5, 123000000, time.UTC)
		tA1 = time.Date(2026, 1, 2, 3, 4, 6, 456000000, time.UTC)
		tU2 = time.Date(2026, 1, 2, 3, 4, 10, 789000000, time.UTC)
		tE1 = time.Date(2026, 1, 2, 3, 4, 7, 234000000, time.UTC) // materializer re-stamp of U1
		tE2 = time.Date(2026, 1, 2, 3, 4, 8, 567000000, time.UTC) // materializer re-stamp of A1
	)
	u1 := retainedTestTurn(tU1, "user", "What is two plus two?")
	a1 := retainedTestTurn(tA1, "assistant", "Four.")
	u2 := retainedTestTurn(tU2, "user", "What is three plus three?")
	e1 := retainedTestTurn(tE1, "user", "What is two plus two?")
	e2 := retainedTestTurn(tE2, "assistant", "Four.")

	originStore := &acf.Store{Root: t.TempDir()}
	require.NoError(t, originStore.Init())
	origin := &Orchestrator{cfg: Config{Store: originStore, LocalDeviceID: retainedTestOriginDevice}}

	recvStore := &acf.Store{Root: t.TempDir()}
	require.NoError(t, recvStore.Init())
	recv := &Orchestrator{cfg: Config{Store: recvStore, LocalDeviceID: retainedTestReceiverDevice}}

	// --- origin: codex creates the thread with U1 -------------------------
	artID := "019e0000-0000-7000-8000-0000000000aa"
	now := time.Now().UTC()
	require.NoError(t, originStore.WriteArtifact(acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion, ArtifactID: artID, Kind: acf.KindConversation,
		Scope: acf.ScopeGlobal, Name: "conv", SourcePath: t.TempDir() + "/rollout.jsonl",
		CreatedAt: now, UpdatedAt: now,
	}))
	createPayload, err := adapter.EncodeCanonicalConversationPayload([]acf.ConversationEvent{u1})
	require.NoError(t, err)
	require.NoError(t, originStore.AppendEvent(acf.KindConversation, acf.Event{
		EventID: acf.NewID(), ArtifactID: artID, Type: acf.EventTypeCreate, Timestamp: now,
		Provenance: acf.Provenance{DeviceID: retainedTestOriginDevice, SourceAgent: "codex",
			AgentVersion: acf.UnknownAgentVersion, AdapterVersion: "0.9.3"},
		Payload: createPayload,
	}))

	codexParams := adapter.OpaqueParams{DeviceID: retainedTestOriginDevice, SourceAgent: "codex", AdapterVersion: "0.9.3"}
	claudeParams := adapter.OpaqueParams{DeviceID: retainedTestOriginDevice, SourceAgent: "claude-code", AdapterVersion: "0.14.2"}
	ref := adapter.ThreadRef{ArtifactID: artID, BranchID: acf.MainBranch}

	// receiver catches up on the create's retained snapshot (clean, n=1).
	require.Equal(t, ImportApplied, deliverRetainedForTest(t, origin, recv, artID))

	// origin round 2: codex answers A1 (native stamps) → retained n=2, clean.
	_, _, err = adapter.MergeConversationByThreadRef(context.Background(), originStore, codexParams, ref,
		[]acf.ConversationEvent{u1, a1}, adapter.EncodeCanonicalConversationPayload)
	require.NoError(t, err)
	require.Equal(t, ImportApplied, deliverRetainedForTest(t, origin, recv, artID))

	recvHead, ok, err := recvStore.LastEvent(acf.KindConversation, artID)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, acf.EventTypeBaseline, recvHead.Type, "receiver caught up by adopting a baseline")
	recvProj, ok, err := recvStore.MaterializedConversationPayloadFromStore(artID)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, []string{
		"2026-01-02T03:04:05.123Z|user", "2026-01-02T03:04:06.456Z|assistant",
	}, retainedTestStamps(recvProj.Events))

	// --- origin: the user continues in Claude Code ------------------------
	// The claude-code adapter parses its own GENERATED session; its base rows
	// carry session_transcode's base.Add(index*time.Second) stamps.
	_, _, err = adapter.MergeConversationByThreadRef(context.Background(), originStore, claudeParams, ref,
		[]acf.ConversationEvent{e1, e2, u2}, adapter.EncodeCanonicalConversationPayload)
	require.NoError(t, err)

	// The origin's own LOG stays clean, as does any peer that chains the
	// verbatim live delta.
	originEvents, err := originStore.ReadEvents(acf.KindConversation, artID)
	require.NoError(t, err)
	originProj, ok, err := acf.MaterializedConversationPayload(originEvents)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, []string{
		"2026-01-02T03:04:05.123Z|user", "2026-01-02T03:04:06.456Z|assistant",
		"2026-01-02T03:04:10.789Z|user",
	}, retainedTestStamps(originProj.Events))

	// --- receiver: the third retained snapshot ----------------------------
	outcome := deliverRetainedForTest(t, origin, recv, artID)
	require.Equal(t, ImportApplied, outcome)

	finalHead, ok, err := recvStore.LastEvent(acf.KindConversation, artID)
	require.NoError(t, err)
	require.True(t, ok)
	finalProj, ok, err := recvStore.MaterializedConversationPayloadFromStore(artID)
	require.NoError(t, err)
	require.True(t, ok)
	t.Logf("receiver head: type=%s dev=%s adapterVersion=%q turns=%v",
		finalHead.Type, finalHead.Provenance.DeviceID, finalHead.Provenance.AdapterVersion,
		retainedTestStamps(finalProj.Events))

	// REGRESSION ASSERTIONS — each one fails on the pre-fix tree.
	require.Equal(t, []string{
		"2026-01-02T03:04:05.123Z|user", "2026-01-02T03:04:06.456Z|assistant",
		"2026-01-02T03:04:10.789Z|user",
	}, retainedTestStamps(finalProj.Events), "receiver must fast-forward, not block-duplicate")
	require.Equal(t, acf.EventTypeBaseline, finalHead.Type,
		"receiver must adopt the origin baseline, not author a union merge")
	require.Equal(t, retainedTestOriginDevice, finalHead.Provenance.DeviceID,
		"receiving device must not author a conversation event here")
	require.NotEmpty(t, finalHead.Provenance.AdapterVersion,
		"an empty adapterVersion is the signature of the receiver-authored union merge")
}

// deliverRetainedForTest builds the origin's lane=retained companion for its current
// head exactly as forwardCommitted does and feeds it to the receiver's
// reconcileRetainedConversation.
func deliverRetainedForTest(t *testing.T, origin, recv *Orchestrator, artID string) ImportOutcome {
	t.Helper()
	art, err := origin.cfg.Store.ReadArtifact(acf.KindConversation, artID)
	require.NoError(t, err)
	head, ok, err := origin.cfg.Store.LastEvent(acf.KindConversation, artID)
	require.NoError(t, err)
	require.True(t, ok)
	retained, ok, _, err := origin.retainedConversationEvent(art, head)
	require.NoError(t, err)
	require.True(t, ok)
	retained.EventID = RetainedWireEventID(head.EventID, origin.cfg.LocalDeviceID)
	outcome, handled, err := recv.reconcileRetainedConversation(retained, acf.ScopeGlobal, nil, false)
	require.NoError(t, err)
	require.True(t, handled)
	return outcome
}
