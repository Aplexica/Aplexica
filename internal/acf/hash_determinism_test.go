package acf

// Aligned-chains delta-sync determinism gate.
//
// The aligned-chains design rests on two store-level properties; this file
// characterizes both so any regression breaks loudly before the sync layer
// starts depending on them:
//
//  1. Hash determinism across stores. ComputeHash (hash.go) is SHA-256 over
//     the canonical JSON encoding of the event with Hash zeroed — it reads
//     nothing from the store. Two stores that append byte-identical events
//     therefore record byte-identical Hash values, which is what lets a
//     receiver re-append an origin's verbatim delta event and land on the
//     exact same head hash.
//
//  2. headHashForAppend (store.go) consults artifact BOOKKEEPING, not just
//     the log tail. Characterized mechanism, as read from store.go:
//       - For MAIN-branch appends it first reads the Artifact JSON and takes
//         a.HeadEventHash, overridden by a.BranchHeads["main"] when that is
//         non-empty.
//       - If that bookkeeping head is non-empty, it is authoritative whether
//         or not it equals the incoming event's ParentHash. A mismatch is
//         rejected without consulting the potentially multi-gigabyte log.
//       - Only empty/missing bookkeeping falls back to HeadHashByBranch, a
//         backward tail scan whose per-branch latest hash gates the append.
//     WriteArtifact deliberately leaves HeadEventHash caller-writeable, so
//     pointing bookkeeping at a FOREIGN hash — one that appears nowhere in
//     the local log — makes AppendEvent accept exactly the events whose
//     ParentHash equals that foreign hash. Baseline adoption
//     is built on precisely this: adopt a full-state baseline, set the head
//     bookkeeping to the origin's head hash, and subsequent verbatim origin
//     deltas chain natively.

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// alignedGateArtifactID is the fixed conversation artifact id shared by the
// aligned-chains gate fixtures. Every fixture field is pinned so two stores
// receive byte-identical inputs.
const alignedGateArtifactID = "0197f000-aaaa-7000-8000-000000000001"

func alignedGateArtifact() Artifact {
	created := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	return Artifact{
		AcfSchemaVersion: SchemaVersion,
		ArtifactID:       alignedGateArtifactID,
		Kind:             KindConversation,
		Scope:            ScopeProject,
		Name:             "0197f000-1111-7000-8000-00000000abcd.jsonl",
		CreatedAt:        created,
		UpdatedAt:        created,
	}
}

func TestAppendEvent_BookkeepingMismatchDoesNotReplayEventLog(t *testing.T) {
	s := &Store{Root: t.TempDir()}
	require.NoError(t, s.Init())
	art := alignedGateArtifact()
	art.HeadEventHash = "aligned-origin-head"
	art.BranchHeads = map[string]string{MainBranch: art.HeadEventHash}
	require.NoError(t, s.WriteArtifact(art))

	// A malformed local tail proves the mismatch path does not consult the
	// event log. Aligned baseline bookkeeping is authoritative even though its
	// head may not appear in that log at all.
	eventPath := filepath.Join(s.Root, eventsRel(KindConversation, art.ArtifactID))
	require.NoError(t, os.MkdirAll(filepath.Dir(eventPath), 0o700))
	require.NoError(t, os.WriteFile(eventPath, []byte("not-json\n"), 0o600))

	err := s.AppendEvent(KindConversation, Event{
		EventID:    NewID(),
		ArtifactID: art.ArtifactID,
		Type:       EventTypeUpdate,
		Timestamp:  time.Now().UTC(),
		ParentHash: "unknown-parent",
	})
	require.ErrorIs(t, err, ErrHeadMismatch)
	require.NotContains(t, err.Error(), "parse", "a mismatched parent must fail from O(1) bookkeeping without replaying the log")
}

func alignedGateProvenance() Provenance {
	return Provenance{
		DeviceID:       "device-origin",
		SourceAgent:    "claude-code",
		AgentVersion:   UnknownAgentVersion,
		AdapterVersion: "0.1.0",
	}
}

// alignedGateGenesis is the fixed conversation-create genesis: a full
// ConversationFormatV1 payload, fixed timestamp/provenance/ids.
func alignedGateGenesis(t *testing.T) Event {
	t.Helper()
	ts := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	payload, err := EncodePayload(ConversationPayload{
		Format: ConversationFormatV1,
		Events: []ConversationEvent{{
			Type:      EventTypeTurn,
			Timestamp: ts,
			Role:      "user",
			Content:   []ContentBlock{{Type: "text", Text: "seed turn"}},
		}},
	})
	require.NoError(t, err)
	return Event{
		EventID:    "0197f000-bbbb-7000-8000-000000000002",
		ArtifactID: alignedGateArtifactID,
		Type:       EventTypeCreate,
		Timestamp:  ts,
		Provenance: alignedGateProvenance(),
		Payload:    payload,
	}
}

// alignedGateDelta is a fixed conversation UPDATE carrying an append-only
// ConversationDeltaFormatV1 payload — the exact event shape the live lane
// will ship verbatim.
func alignedGateDelta(t *testing.T, eventID, text string, ts time.Time, parentHash string) Event {
	t.Helper()
	payload, err := EncodePayload(ConversationPayload{
		Format: ConversationDeltaFormatV1,
		Events: []ConversationEvent{{
			Type:      EventTypeTurn,
			Timestamp: ts,
			Role:      "assistant",
			Content:   []ContentBlock{{Type: "text", Text: text}},
		}},
	})
	require.NoError(t, err)
	return Event{
		EventID:    eventID,
		ArtifactID: alignedGateArtifactID,
		Type:       EventTypeUpdate,
		Timestamp:  ts,
		Provenance: alignedGateProvenance(),
		Payload:    payload,
		ParentHash: parentHash,
	}
}

// TestComputeHash_DeterministicAcrossStores appends the SAME genesis + update
// events into two independent stores and requires both to record the same
// head hash — equal to ComputeHash of the pre-append event. This is gate
// property 1: hashes are a pure function of event bytes, never of store
// state, so re-appending an origin's verbatim event reproduces its hash.
func TestComputeHash_DeterministicAcrossStores(t *testing.T) {
	genesis := alignedGateGenesis(t)
	genesisHash, err := ComputeHash(genesis)
	require.NoError(t, err)

	update := alignedGateDelta(t, "0197f000-cccc-7000-8000-000000000003",
		"delta turn one", time.Date(2026, 7, 1, 10, 1, 0, 0, time.UTC), genesisHash)
	wantUpdateHash, err := ComputeHash(update)
	require.NoError(t, err)

	seed := func(t *testing.T) *Store {
		t.Helper()
		s := &Store{Root: t.TempDir()}
		require.NoError(t, s.Init())
		require.NoError(t, s.WriteArtifact(alignedGateArtifact()))
		require.NoError(t, s.AppendEvent(KindConversation, genesis))
		require.NoError(t, s.AppendEvent(KindConversation, update))
		return s
	}
	a := seed(t)
	b := seed(t)

	lastA, okA, err := a.LastEvent(KindConversation, alignedGateArtifactID)
	require.NoError(t, err)
	require.True(t, okA)
	lastB, okB, err := b.LastEvent(KindConversation, alignedGateArtifactID)
	require.NoError(t, err)
	require.True(t, okB)

	require.Equal(t, wantUpdateHash, lastA.Hash,
		"GATE: stored head hash must equal ComputeHash of the pre-append event — AppendEvent must not perturb hashed bytes")
	require.Equal(t, lastA.Hash, lastB.Hash,
		"GATE: two stores appending identical event bytes must record identical head hashes")
	require.Equal(t, lastA, lastB,
		"the stored head events themselves must be identical across stores")

	// Bookkeeping lands on the same value in both stores — HeadEventHash and
	// BranchHeads[main] both track the appended head (store.go AppendEvent).
	artA, err := a.ReadArtifact(KindConversation, alignedGateArtifactID)
	require.NoError(t, err)
	artB, err := b.ReadArtifact(KindConversation, alignedGateArtifactID)
	require.NoError(t, err)
	require.Equal(t, wantUpdateHash, artA.HeadEventHash)
	require.Equal(t, wantUpdateHash, artA.BranchHeads[MainBranch])
	require.Equal(t, artA.HeadEventHash, artB.HeadEventHash)
	require.Equal(t, artA.BranchHeads[MainBranch], artB.BranchHeads[MainBranch])
}

// TestAppendEvent_ChainsOntoHeadSetViaWriteArtifact is gate property 2 and
// mirrors baseline adoption end to end at store level:
//
//	origin: genesis → delta1 → delta2            (head hashes H0 → H1 → H2)
//	receiver: local stand-in baseline event      (log tail = B ≠ anything on origin)
//	receiver bookkeeping := H1 via WriteArtifact (adoption's aligned-head write)
//	receiver appends origin's VERBATIM delta2    (ParentHash = H1) → must succeed
//
// The control step proves the pass is not vacuous: before the bookkeeping
// write, the very same append is rejected with ErrHeadMismatch. If
// headHashForAppend read only the log tail, the post-bookkeeping append would
// fail identically — that is the gate-failure condition.
func TestAppendEvent_ChainsOntoHeadSetViaWriteArtifact(t *testing.T) {
	// Origin store: genesis + delta1 committed, then delta2 authored on top.
	origin := &Store{Root: t.TempDir()}
	require.NoError(t, origin.Init())
	require.NoError(t, origin.WriteArtifact(alignedGateArtifact()))

	genesis := alignedGateGenesis(t)
	genesisHash, err := ComputeHash(genesis)
	require.NoError(t, err)
	require.NoError(t, origin.AppendEvent(KindConversation, genesis))

	delta1 := alignedGateDelta(t, "0197f000-cccc-7000-8000-000000000003",
		"delta turn one", time.Date(2026, 7, 1, 10, 1, 0, 0, time.UTC), genesisHash)
	delta1Hash, err := ComputeHash(delta1)
	require.NoError(t, err)
	require.NoError(t, origin.AppendEvent(KindConversation, delta1))

	delta2 := alignedGateDelta(t, "0197f000-dddd-7000-8000-000000000004",
		"delta turn two", time.Date(2026, 7, 1, 10, 2, 0, 0, time.UTC), delta1Hash)
	require.NoError(t, origin.AppendEvent(KindConversation, delta2))

	// The verbatim wire event: exactly what the origin stored (Hash populated).
	originHead, ok, err := origin.LastEvent(KindConversation, alignedGateArtifactID)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, delta1Hash, originHead.ParentHash)

	// Receiver store: same artifact shell, but its log holds only a LOCAL
	// stand-in for the future baseline event — genesis-chained (ParentHash "")
	// with its own EventID, so its hash matches nothing on the origin.
	recv := &Store{Root: t.TempDir()}
	require.NoError(t, recv.Init())
	require.NoError(t, recv.WriteArtifact(alignedGateArtifact()))
	baseline := alignedGateGenesis(t)
	baseline.EventID = "0197f000-eeee-7000-8000-000000000005"
	baseline.Provenance.DeviceID = "device-receiver"
	require.NoError(t, recv.AppendEvent(KindConversation, baseline))
	baselineHash, err := ComputeHash(baseline)
	require.NoError(t, err)

	// Control: with bookkeeping still at the local baseline hash, the origin's
	// delta2 has an unknown parent and must be rejected.
	err = recv.AppendEvent(KindConversation, originHead)
	require.ErrorIs(t, err, ErrHeadMismatch,
		"control: a foreign-parent append must fail while neither bookkeeping nor the log knows the parent")

	// Baseline adoption's head write: point bookkeeping at the ORIGIN's prior
	// head (delta1Hash) via WriteArtifact — no append produced this value, and
	// it appears nowhere in the receiver's log.
	art, err := recv.ReadArtifact(KindConversation, alignedGateArtifactID)
	require.NoError(t, err)
	require.Equal(t, baselineHash, art.HeadEventHash, "sanity: bookkeeping tracked the local baseline append")
	art.HeadEventHash = delta1Hash
	if art.BranchHeads == nil {
		art.BranchHeads = map[string]string{}
	}
	art.BranchHeads[MainBranch] = delta1Hash
	require.NoError(t, recv.WriteArtifact(art))

	// Direct probe of the mechanism: headHashForAppend must surface the
	// authoritative bookkeeping value, NOT the log tail (baselineHash).
	head, err := recv.headHashForAppend(KindConversation, alignedGateArtifactID, MainBranch, delta1Hash)
	require.NoError(t, err)
	require.Equal(t, delta1Hash, head,
		"GATE: headHashForAppend must consult artifact bookkeeping (HeadEventHash/BranchHeads[main]); a log-tail-only answer breaks the aligned-chains design")

	// Behavior: the verbatim origin event now chains natively.
	require.NoError(t, recv.AppendEvent(KindConversation, originHead),
		"GATE: an append whose ParentHash equals the WriteArtifact-set head must succeed (baseline adoption)")

	// Determinism across stores again, this time via the adoption path: the
	// receiver stored the origin's event byte-identically, hash included.
	recvHead, ok, err := recv.LastEvent(KindConversation, alignedGateArtifactID)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, originHead, recvHead,
		"the re-appended verbatim event must be stored byte-identically, hash included")

	// Both stores' bookkeeping now agrees on the same head — the alignment
	// invariant (plan design rule 7).
	originArt, err := origin.ReadArtifact(KindConversation, alignedGateArtifactID)
	require.NoError(t, err)
	recvArt, err := recv.ReadArtifact(KindConversation, alignedGateArtifactID)
	require.NoError(t, err)
	require.Equal(t, originArt.HeadEventHash, recvArt.HeadEventHash,
		"after adopting the aligned head and appending the verbatim delta, both stores must agree on HeadEventHash")
	require.Equal(t, originArt.BranchHeads[MainBranch], recvArt.BranchHeads[MainBranch])
}
