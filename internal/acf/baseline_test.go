package acf

// Aligned-chains delta-sync: baseline events and
// AdoptBaseline.
//
// A baseline event is a full-state checkpoint a RECEIVER appends when it
// adopts an origin device's materialized conversation state. It chains
// normally onto the local head (ParentHash), but after the append the
// artifact's head BOOKKEEPING points at the ORIGIN's head hash (AlignedHead)
// — not at the baseline's own hash — so subsequent verbatim origin delta
// events chain natively and both stores converge on identical head hashes
// (the alignment invariant, plan design rule 7). VerifyChain mirrors the
// same rule: a baseline resets the expected parent for subsequent events to
// its AlignedHead.
//
// These tests reuse the aligned-chains gate fixtures from
// hash_determinism_test.go (fixed ids/timestamps so origin chains are
// byte-reproducible).

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// seedAlignedOrigin builds the origin-store fixture the baseline tests adopt
// from: the gate genesis plus two delta updates. Returns the store, the
// stored head event (Hash populated — the verbatim wire form), and the full
// materialized ConversationFormatV1 payload at that head.
func seedAlignedOrigin(t *testing.T) (*Store, Event, json.RawMessage) {
	t.Helper()
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

	head, ok, err := origin.LastEvent(KindConversation, alignedGateArtifactID)
	require.NoError(t, err)
	require.True(t, ok)

	events, err := origin.ReadEvents(KindConversation, alignedGateArtifactID)
	require.NoError(t, err)
	payload, ok, err := MaterializedConversationPayloadBytes(events)
	require.NoError(t, err)
	require.True(t, ok)
	return origin, head, payload
}

// baselineFor mints the baseline event a receiver would adopt for the given
// origin head: full materialized payload, AlignedHead/AlignedEventID copied
// from the head, its own fresh EventID and receiver provenance. ParentHash is
// deliberately left empty — AdoptBaseline fills it in.
func baselineFor(head Event, payload json.RawMessage) Event {
	return Event{
		EventID:    "0197f000-ffff-7000-8000-000000000010",
		ArtifactID: alignedGateArtifactID,
		Type:       EventTypeBaseline,
		Timestamp:  time.Date(2026, 7, 1, 10, 5, 0, 0, time.UTC),
		Provenance: Provenance{
			DeviceID:       "device-receiver",
			SourceAgent:    "claude-code",
			AgentVersion:   UnknownAgentVersion,
			AdapterVersion: "0.1.0",
		},
		Payload:        payload,
		AlignedHead:    head.Hash,
		AlignedEventID: head.EventID,
	}
}

// TestAdoptBaseline_OntoEmptyArtifact adopts a baseline into a store that has
// never seen the artifact: the shell is minted, the baseline is the only
// logged event (chained from ""), and the head bookkeeping lands on
// AlignedHead — NOT on the baseline's own hash. The baseline is
// materializable: LatestPayloadEvent selects it and the conversation replay
// decodes its full payload (this is the acf walk syncd's conversationHead /
// latestPayloadBearingEvent delegates to).
func TestAdoptBaseline_OntoEmptyArtifact(t *testing.T) {
	origin, originHead, fullPayload := seedAlignedOrigin(t)

	recv := &Store{Root: t.TempDir()}
	require.NoError(t, recv.Init())

	baseline := baselineFor(originHead, fullPayload)
	require.NoError(t, recv.AdoptBaseline(KindConversation, baseline))

	// Shell minted with the importOneInbound defaults (global scope).
	art, err := recv.ReadArtifact(KindConversation, alignedGateArtifactID)
	require.NoError(t, err)
	require.Equal(t, KindConversation, art.Kind)
	require.Equal(t, ScopeGlobal, art.Scope)
	require.False(t, art.Tombstoned)

	// Head bookkeeping is the ORIGIN's head hash on both trackers.
	require.Equal(t, originHead.Hash, art.HeadEventHash,
		"HeadEventHash must be AlignedHead after adoption")
	require.Equal(t, originHead.Hash, art.BranchHeads[MainBranch],
		"BranchHeads[main] must be AlignedHead after adoption")

	// The baseline itself is readable via ReadEvents, genesis-chained, with
	// its own normally-computed hash (which is NOT the head).
	events, err := recv.ReadEvents(KindConversation, alignedGateArtifactID)
	require.NoError(t, err)
	require.Len(t, events, 1)
	stored := events[0]
	require.Equal(t, EventTypeBaseline, stored.Type)
	require.Equal(t, "", stored.ParentHash, "first-contact baseline chains from empty parent")
	require.Equal(t, originHead.Hash, stored.AlignedHead)
	require.Equal(t, originHead.EventID, stored.AlignedEventID)
	wantHash, err := ComputeHash(stored)
	require.NoError(t, err)
	require.Equal(t, wantHash, stored.Hash, "baseline hash is computed normally")
	require.NotEqual(t, stored.Hash, art.HeadEventHash,
		"the baseline's own hash must NOT become the head")

	// The adopted log verifies and materializes.
	require.NoError(t, VerifyChain(events))
	lp, ok := LatestPayloadEvent(events)
	require.True(t, ok, "a payload-bearing baseline is materializable")
	require.Equal(t, baseline.EventID, lp.EventID)

	gotMat, ok, err := MaterializedConversationPayload(events)
	require.NoError(t, err)
	require.True(t, ok)
	originEvents, err := origin.ReadEvents(KindConversation, alignedGateArtifactID)
	require.NoError(t, err)
	wantMat, ok, err := MaterializedConversationPayload(originEvents)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, wantMat, gotMat, "adopting the baseline reproduces the origin's materialized state")

	// Alignment invariant (plan design rule 7): both stores agree on the head.
	originArt, err := origin.ReadArtifact(KindConversation, alignedGateArtifactID)
	require.NoError(t, err)
	require.Equal(t, originArt.HeadEventHash, art.HeadEventHash)
}

// TestAdoptBaseline_OntoExistingChain adopts onto a receiver that already has
// its own divergent local chain: the old events are retained, the baseline
// chains onto the local head, the head bookkeeping switches to AlignedHead,
// and the origin's next VERBATIM delta then appends natively (ParentHash ==
// AlignedHead). VerifyChain passes over the full
// [local genesis…, baseline, foreign-parent event] log, and replay
// materializes the ORIGIN's content (the baseline supersedes the pre-adoption
// local state).
func TestAdoptBaseline_OntoExistingChain(t *testing.T) {
	origin, originHead, fullPayload := seedAlignedOrigin(t)

	// Receiver with a divergent local chain: same artifact id, different
	// events (distinct EventIDs → distinct hashes).
	recv := &Store{Root: t.TempDir()}
	require.NoError(t, recv.Init())
	require.NoError(t, recv.WriteArtifact(alignedGateArtifact()))
	localGenesis := alignedGateGenesis(t)
	localGenesis.EventID = "0197f000-1234-7000-8000-000000000020"
	localGenesis.Provenance.DeviceID = "device-receiver"
	localGenesisHash, err := ComputeHash(localGenesis)
	require.NoError(t, err)
	require.NoError(t, recv.AppendEvent(KindConversation, localGenesis))
	localUpdate := alignedGateDelta(t, "0197f000-1234-7000-8000-000000000021",
		"local-only turn", time.Date(2026, 7, 1, 10, 3, 0, 0, time.UTC), localGenesisHash)
	localUpdateHash, err := ComputeHash(localUpdate)
	require.NoError(t, err)
	require.NoError(t, recv.AppendEvent(KindConversation, localUpdate))

	baseline := baselineFor(originHead, fullPayload)
	require.NoError(t, recv.AdoptBaseline(KindConversation, baseline))

	// Old events retained; baseline chained onto the LOCAL head.
	events, err := recv.ReadEvents(KindConversation, alignedGateArtifactID)
	require.NoError(t, err)
	require.Len(t, events, 3)
	require.Equal(t, localGenesis.EventID, events[0].EventID)
	require.Equal(t, localUpdate.EventID, events[1].EventID)
	require.Equal(t, EventTypeBaseline, events[2].Type)
	require.Equal(t, localUpdateHash, events[2].ParentHash,
		"baseline chains onto the local head at adoption time")

	// Head switched to AlignedHead.
	art, err := recv.ReadArtifact(KindConversation, alignedGateArtifactID)
	require.NoError(t, err)
	require.Equal(t, originHead.Hash, art.HeadEventHash)
	require.Equal(t, originHead.Hash, art.BranchHeads[MainBranch])

	// Origin authors the next delta; the receiver appends it VERBATIM
	// (ParentHash == AlignedHead) — native chain extension, no reconcile.
	delta3 := alignedGateDelta(t, "0197f000-abcd-7000-8000-000000000006",
		"delta turn three", time.Date(2026, 7, 1, 10, 6, 0, 0, time.UTC), originHead.Hash)
	require.NoError(t, origin.AppendEvent(KindConversation, delta3))
	originDelta3, ok, err := origin.LastEvent(KindConversation, alignedGateArtifactID)
	require.NoError(t, err)
	require.True(t, ok)

	require.NoError(t, recv.AppendEvent(KindConversation, originDelta3),
		"an append whose ParentHash equals AlignedHead must succeed after adoption")
	recvHead, ok, err := recv.LastEvent(KindConversation, alignedGateArtifactID)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, originDelta3, recvHead, "the verbatim delta is stored byte-identically, hash included")

	// Alignment invariant: both stores' bookkeeping agrees after the delta.
	originArt, err := origin.ReadArtifact(KindConversation, alignedGateArtifactID)
	require.NoError(t, err)
	recvArt, err := recv.ReadArtifact(KindConversation, alignedGateArtifactID)
	require.NoError(t, err)
	require.Equal(t, originArt.HeadEventHash, recvArt.HeadEventHash)
	require.Equal(t, originArt.BranchHeads[MainBranch], recvArt.BranchHeads[MainBranch])

	// VerifyChain over [local genesis, local update, baseline, foreign-parent
	// delta] passes: the baseline reset the expected parent to AlignedHead.
	full, err := recv.ReadEvents(KindConversation, alignedGateArtifactID)
	require.NoError(t, err)
	require.Len(t, full, 4)
	require.NoError(t, VerifyChain(full))

	// Replay materializes the ORIGIN's thread (baseline supersedes the
	// pre-adoption local turns), extended by the delta.
	gotMat, ok, err := MaterializedConversationPayload(full)
	require.NoError(t, err)
	require.True(t, ok)
	originEvents, err := origin.ReadEvents(KindConversation, alignedGateArtifactID)
	require.NoError(t, err)
	wantMat, ok, err := MaterializedConversationPayload(originEvents)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, wantMat, gotMat, "receiver materializes the origin's content after adoption + delta")
}

// TestAdoptBaseline_RejectsMalformedEvents pins the validation contract: a
// baseline must be typed baseline, main-branch, payload-bearing, and carry
// both AlignedHead and AlignedEventID (the re-align tiebreak key).
func TestAdoptBaseline_RejectsMalformedEvents(t *testing.T) {
	_, originHead, fullPayload := seedAlignedOrigin(t)

	cases := []struct {
		name   string
		mutate func(*Event)
	}{
		{"wrong type", func(e *Event) { e.Type = EventTypeUpdate }},
		{"empty aligned head", func(e *Event) { e.AlignedHead = "" }},
		{"empty aligned event id", func(e *Event) { e.AlignedEventID = "" }},
		{"missing payload", func(e *Event) { e.Payload = nil }},
		{"null payload", func(e *Event) { e.Payload = json.RawMessage("null") }},
		{"non-main branch", func(e *Event) { e.Branch = "side" }},
		{"empty artifact id", func(e *Event) { e.ArtifactID = "" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recv := &Store{Root: t.TempDir()}
			require.NoError(t, recv.Init())
			ev := baselineFor(originHead, fullPayload)
			tc.mutate(&ev)
			require.Error(t, recv.AdoptBaseline(KindConversation, ev))
		})
	}
}

// TestAdoptBranchBaseline_WithoutForkAncestry proves that a retained
// checkpoint can restore a side branch on a receiver that never observed its
// fork. The checkpoint is a virtual branch root, leaves main untouched, and
// aligns the side head so the origin's next verbatim delta appends normally.
func TestAdoptBranchBaseline_WithoutForkAncestry(t *testing.T) {
	recv := &Store{Root: t.TempDir()}
	require.NoError(t, recv.Init())
	id := "019f0000-0000-7000-8000-000000000190"
	now := seedProjectionArtifact(t, recv, id)

	main := appendProjectionEvent(t, recv, Event{
		EventID:    acfTestID("branch-baseline-main"),
		ArtifactID: id,
		Type:       EventTypeCreate,
		Branch:     MainBranch,
		Timestamp:  now,
		Payload:    testConversationPayload(t, "main question", "main answer"),
	})
	alignedSideHead := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	baseline := Event{
		EventID:        acfTestID("branch-baseline-review"),
		ArtifactID:     id,
		Type:           EventTypeBaseline,
		Branch:         "review",
		Timestamp:      now.Add(time.Minute),
		Payload:        testConversationPayload(t, "review question", "review answer"),
		AlignedHead:    alignedSideHead,
		AlignedEventID: acfTestID("remote-review-head"),
	}
	require.NoError(t, recv.AdoptBranchBaseline(KindConversation, baseline))

	art, err := recv.ReadArtifact(KindConversation, id)
	require.NoError(t, err)
	require.Equal(t, main.Hash, art.HeadEventHash)
	require.Equal(t, main.Hash, art.BranchHeads[MainBranch])
	require.Equal(t, alignedSideHead, art.BranchHeads["review"])

	next := appendProjectionEvent(t, recv, Event{
		EventID:    acfTestID("branch-baseline-next"),
		ArtifactID: id,
		Type:       EventTypeUpdate,
		Branch:     "review",
		Timestamp:  now.Add(2 * time.Minute),
		ParentHash: alignedSideHead,
		Payload:    testConversationDelta(t, "review follow-up"),
	})
	require.NotEmpty(t, next.Hash)
	mainNext := appendProjectionEvent(t, recv, Event{
		EventID:    acfTestID("branch-baseline-main-next"),
		ArtifactID: id,
		Type:       EventTypeUpdate,
		Branch:     MainBranch,
		Timestamp:  now.Add(3 * time.Minute),
		ParentHash: main.Hash,
		Payload:    testConversationDelta(t, "main follow-up"),
	})
	lastReview, found, err := recv.LastEventByBranch(KindConversation, id, "review")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, next.EventID, lastReview.EventID,
		"branch tail lookup must scan past an interleaved main event")

	projected, err := recv.ProjectEventsForBranch(KindConversation, id, "review", BranchProjectionOpts{})
	require.NoError(t, err)
	require.Equal(t, []string{baseline.EventID, next.EventID}, projectionEventIDs(projected))
	payload, ok, err := MaterializedConversationPayload(projected)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, []string{"review question", "review answer", "review follow-up"}, projectionTexts(t, payload))

	all, err := recv.ReadEvents(KindConversation, id)
	require.NoError(t, err)
	require.NoError(t, VerifyChain(all))
	malformedRoot := all[1]
	malformedRoot.Payload = nil
	malformedRoot.Hash, err = ComputeHash(malformedRoot)
	require.NoError(t, err)
	require.Error(t, VerifyChain([]Event{malformedRoot}),
		"only a payload-bearing baseline may stand in for missing fork ancestry")
	mainProjected, err := recv.ProjectEventsForBranch(KindConversation, id, MainBranch, BranchProjectionOpts{})
	require.NoError(t, err)
	require.Equal(t, []string{main.EventID, mainNext.EventID}, projectionEventIDs(mainProjected))
}

// TestAppendEvent_BaselineBookkeeping pins design rule 2 at the AppendEvent
// level, independent of the AdoptBaseline entry point: appending a baseline
// re-points the head bookkeeping at AlignedHead, clears a tombstone (a
// baseline re-asserts full content, like create/update/resolution), and a
// baseline without AlignedHead is refused outright (it would corrupt the head
// bookkeeping to "").
func TestAppendEvent_BaselineBookkeeping(t *testing.T) {
	_, originHead, fullPayload := seedAlignedOrigin(t)

	recv := &Store{Root: t.TempDir()}
	require.NoError(t, recv.Init())
	require.NoError(t, recv.WriteArtifact(alignedGateArtifact()))
	genesis := alignedGateGenesis(t)
	genesisHash, err := ComputeHash(genesis)
	require.NoError(t, err)
	require.NoError(t, recv.AppendEvent(KindConversation, genesis))

	// Tombstone the artifact.
	redaction := Event{
		EventID:    "0197f000-1234-7000-8000-000000000030",
		ArtifactID: alignedGateArtifactID,
		Type:       EventTypeRedaction,
		Timestamp:  time.Date(2026, 7, 1, 10, 4, 0, 0, time.UTC),
		Provenance: alignedGateProvenance(),
		ParentHash: genesisHash,
	}
	redactionHash, err := ComputeHash(redaction)
	require.NoError(t, err)
	require.NoError(t, recv.AppendEvent(KindConversation, redaction))
	art, err := recv.ReadArtifact(KindConversation, alignedGateArtifactID)
	require.NoError(t, err)
	require.True(t, art.Tombstoned)

	// A baseline missing AlignedHead is refused before any write.
	bad := baselineFor(originHead, fullPayload)
	bad.AlignedHead = ""
	bad.ParentHash = redactionHash
	require.Error(t, recv.AppendEvent(KindConversation, bad))

	// A well-formed baseline appended directly (chained onto the local head)
	// lands the bookkeeping on AlignedHead and clears the tombstone.
	baseline := baselineFor(originHead, fullPayload)
	baseline.ParentHash = redactionHash
	require.NoError(t, recv.AppendEvent(KindConversation, baseline))
	art, err = recv.ReadArtifact(KindConversation, alignedGateArtifactID)
	require.NoError(t, err)
	require.Equal(t, originHead.Hash, art.HeadEventHash)
	require.Equal(t, originHead.Hash, art.BranchHeads[MainBranch])
	require.False(t, art.Tombstoned, "a baseline re-asserts content and clears the tombstone")
}

// TestVerifyChain_BaselineRules covers the failure half of design rule 3: the
// reset applies exactly at the baseline — chaining onto the baseline's OWN
// hash is now a break, tampering still fails, and a baseline without an
// AlignedHead is malformed.
func TestVerifyChain_BaselineRules(t *testing.T) {
	_, originHead, fullPayload := seedAlignedOrigin(t)

	recv := &Store{Root: t.TempDir()}
	require.NoError(t, recv.Init())
	require.NoError(t, recv.AdoptBaseline(KindConversation, baselineFor(originHead, fullPayload)))
	events, err := recv.ReadEvents(KindConversation, alignedGateArtifactID)
	require.NoError(t, err)
	require.Len(t, events, 1)
	storedBaseline := events[0]

	// Sanity: [baseline, event-chained-on-AlignedHead] verifies.
	next := alignedGateDelta(t, "0197f000-abcd-7000-8000-000000000007",
		"post-adoption turn", time.Date(2026, 7, 1, 10, 7, 0, 0, time.UTC), storedBaseline.AlignedHead)
	next.Hash, err = ComputeHash(next)
	require.NoError(t, err)
	require.NoError(t, VerifyChain([]Event{storedBaseline, next}))

	// An event chained onto the baseline's OWN hash breaks the chain: the
	// baseline re-pointed the expected parent at AlignedHead.
	wrongParent := alignedGateDelta(t, "0197f000-abcd-7000-8000-000000000008",
		"wrong parent turn", time.Date(2026, 7, 1, 10, 8, 0, 0, time.UTC), storedBaseline.Hash)
	wrongParent.Hash, err = ComputeHash(wrongParent)
	require.NoError(t, err)
	require.Error(t, VerifyChain([]Event{storedBaseline, wrongParent}),
		"chaining onto the baseline's own hash must fail — the expected parent is AlignedHead")

	// A genuinely broken chain still fails: tampered event hash.
	tampered := []Event{storedBaseline, next}
	tampered[1].Hash = "deadbeef"
	require.Error(t, VerifyChain(tampered))

	// A baseline without AlignedHead is malformed (recompute its hash so the
	// aligned-head check — not the hash check — is what trips).
	noAligned := storedBaseline
	noAligned.AlignedHead = ""
	noAligned.Hash, err = ComputeHash(noAligned)
	require.NoError(t, err)
	require.Error(t, VerifyChain([]Event{noAligned}))
}

// TestLatestPayloadEvent_AcceptsPayloadBearingBaseline pins the shared
// backward walk both syncd's conversationHead (latestPayloadBearingEvent) and
// the hermes exporter delegate to: a payload-bearing baseline is the newest
// materializable event; a payload-less one is skipped (mirroring the legacy
// snapshot shape). LatestEventFormat mirrors the same acceptance so the
// fan-out format gate reads the format off the baseline.
func TestLatestPayloadEvent_AcceptsPayloadBearingBaseline(t *testing.T) {
	_, originHead, fullPayload := seedAlignedOrigin(t)
	create := alignedGateGenesis(t)
	baseline := baselineFor(originHead, fullPayload)

	got, ok := LatestPayloadEvent([]Event{create, baseline})
	require.True(t, ok)
	require.Equal(t, baseline.EventID, got.EventID, "the payload-bearing baseline is the latest payload event")

	format, ok := LatestEventFormat([]Event{create, baseline})
	require.True(t, ok)
	require.Equal(t, ConversationFormatV1, format)

	noBody := baseline
	noBody.Payload = nil
	got, ok = LatestPayloadEvent([]Event{create, noBody})
	require.True(t, ok)
	require.Equal(t, create.EventID, got.EventID, "a payload-less baseline is skipped, like a legacy snapshot")
}
