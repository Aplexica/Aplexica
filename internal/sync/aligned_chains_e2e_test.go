package syncd

import (
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Aligned-chains E2E — the big-file scenario.
//
// The scenario the whole design exists for: a conversation whose FULL
// materialized state is far beyond the 4 MB live-lane cap (the fixture remains
// compact enough for the test suite) must
// still replicate with O(new turn) wire cost. The receiver adopts ONE
// retained full-state baseline, and every subsequent turn crosses the wire
// as a small lane=live delta that chains natively onto the aligned head —
// no per-turn full-state transfer, no repeat adoption (design rules 4/5/7).
// ---------------------------------------------------------------------------

// incompressibleText returns n bytes of deterministic pseudo-random base-36
// filler. Real large sessions do not gzip away, so the fixture must not
// either: sealEnvelope gzips bodies over 64 KiB, and REPEATED filler would
// compress the ~10 MB create to a few KB — under the live-lane cap — dodging
// the giant-head size gate this E2E exists to exercise. Random base-36 text
// carries ~5.17 bits/char of entropy, so gzip cannot shrink the sealed
// create below ~65% of its size (comfortably over the 4 MB cap).
func incompressibleText(rng *rand.Rand, n int) string {
	const charset = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, n)
	_, _ = rng.Read(b) // math/rand Read never fails
	for i, c := range b {
		b[i] = charset[int(c)%len(charset)]
	}
	return string(b)
}

func TestAlignedChains_LargeConversationSyncs(t *testing.T) {
	// This protocol-level test exercises aligned chains with a transport that
	// supports a large retained frame. The production realtime transport cap is
	// intentionally smaller and reports the documented no-baseline residual;
	// widening the seam here keeps the alignment algorithm covered without
	// asserting a capability the deployed transport does not have.
	restoreRetainedCap := remotePublishRetainedMaxBytes
	remotePublishRetainedMaxBytes = 96 << 20
	t.Cleanup(func() { remotePublishRetainedMaxBytes = restoreRetainedCap })

	const (
		fillerTurns     = 42
		fillerTurnBytes = 256 << 10 // 42 × 256 KiB ≈ 10.5 MiB of turn text
		deltaTurns      = 5
	)

	devA, devB := newTestDevice(t, "device-A"), newTestDevice(t, "device-B")
	pubA, pubB := &stubRemotePublisher{}, &stubRemotePublisher{}
	oA, storeA := newStoreOrch(t, pubA, devA, Recipient{DeviceID: devB.id, PubKey: devB.pub})
	oB, storeB := newStoreOrch(t, pubB, devB, Recipient{DeviceID: devA.id, PubKey: devA.pub})
	_ = pubB // B authors nothing in this scenario; its publisher must stay silent
	// The big-file deployment posture: the authoring device runs
	// with the generic inbound ingest cap disabled — sessions of this size are
	// admitted by the session-file cap, not refused as runaway blobs.
	// Construction-time field; safe before Run.
	oA.cfg.MaxArtifactBytes = -1

	// A holds a conversation whose FULL materialized payload is ~10 MB.
	rng := rand.New(rand.NewSource(0xA11C)) // deterministic fixture
	t0 := time.Now().UTC().Add(-time.Hour)
	filler := make([]acf.ConversationEvent, 0, fillerTurns)
	for i := 0; i < fillerTurns; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		filler = append(filler, turnEv(role, incompressibleText(rng, fillerTurnBytes), t0.Add(time.Duration(i)*time.Second)))
	}
	artID, head := seedConversation(t, storeA, devA.id, filler...)
	require.GreaterOrEqual(t, len(head.Payload), 10<<20,
		"fixture premise: the full materialized payload is ~10 MB")

	require.True(t, oA.forwardCommitted(artID))

	// The giant create seals over the live-lane cap: it must skip the live
	// lane ENTIRELY (design rule 5 — the daemon would only dead-letter it)
	// and ship as the retained full state alone.
	require.Empty(t, laneEvents(pubA, LaneLive),
		"a giant create must skip the live lane entirely (sealed size over the 4 MB cap)")
	retained := laneEvents(pubA, LaneRetained)
	require.Len(t, retained, 1)
	require.Greater(t, len(retained[0].Bytes), remotePublishLiveMaxBytes,
		"fixture premise: even sealed+gzipped, the full state exceeds the live-lane cap")

	// B adopts the one retained full state as an aligned baseline.
	retainedCur := 0
	outcomes := deliverLane(t, pubA, oB, LaneRetained, &retainedCur)
	require.Equal(t, []ImportOutcome{ImportApplied}, outcomes,
		"B must adopt A's retained full state as a baseline")
	require.Equal(t, 1, retainedCur, "B consumed exactly one retained event")
	require.Equal(t, 1, countBaselines(t, storeB, artID))
	require.Equal(t, mainHeadHash(t, storeA, artID), mainHeadHash(t, storeB, artID),
		"adoption must align B's head bookkeeping with A's head hash")
	baselineCached, baselineCacheOK, err := storeB.ValidatedCachedMaterializedConversationPayload(artID)
	require.NoError(t, err)
	require.True(t, baselineCacheOK, "retained prompt baseline must prime the validated fan-out cache")
	require.Len(t, baselineCached.Events, fillerTurns)

	// From here on ONLY the live lane is delivered. Any reconcile machinery
	// (needs-baseline, re-adoption) firing on B would betray a broken chain.
	bus := &capturingBus{}
	oB.cfg.EventPublisher = bus // construction-time field; safe before Run

	liveCur := 0
	for i := 1; i <= deltaTurns; i++ {
		appendConversationDelta(t, storeA, devA.id, artID,
			fmt.Sprintf("delta-%d", i), t0.Add(time.Hour+time.Duration(i)*time.Second))
		require.True(t, oA.forwardCommitted(artID))
	}

	live := takeLane(pubA, LaneLive, &liveCur)
	require.Len(t, live, deltaTurns, "each delta commit ships exactly one live event")
	for i, out := range live {
		require.Lessf(t, len(out.Bytes), 64*1024,
			"live delta %d must stay O(new turn) on the wire regardless of the ~10 MB history", i+1)
	}
	liveOutcomes := importWires(t, oB, live)
	for i, oc := range liveOutcomes {
		require.Equalf(t, ImportApplied, oc, "live delta %d must chain natively", i+1)
	}
	answerCached, answerCacheOK, err := storeB.ValidatedCachedMaterializedConversationPayload(artID)
	require.NoError(t, err)
	require.True(t, answerCacheOK, "live answer deltas must advance the validated fan-out cache")
	require.Len(t, answerCached.Events, fillerTurns+deltaTurns)

	// Converged: both devices hold the identical thread…
	turnsA, turnsB := localTurns(t, storeA, artID), localTurns(t, storeB, artID)
	require.Len(t, turnsB, fillerTurns+deltaTurns)
	require.True(t, acf.TextTurnsEqual(turnsA, turnsB),
		"both devices must hold the identical thread")
	// …and aligned: equal head BOOKKEEPING, not just equal content (design
	// rule 7 — the invariant that makes the next delta apply too).
	require.Equal(t, mainHeadHash(t, storeA, artID), mainHeadHash(t, storeB, artID),
		"aligned-chains invariant: head bookkeeping equal after live-only deltas")

	// NO retained event was needed after the first: B never adopted again,
	// never flagged needs-baseline, and holds exactly the setup baseline.
	require.Equal(t, 1, countBaselines(t, storeB, artID),
		"B must never need adoption after the first baseline")
	require.False(t, oB.needsBaselinePending(artID))
	require.False(t, bus.has("remote.needs_baseline"),
		"no live delta may land on a chain gap in this scenario")
	require.False(t, bus.has("remote.baseline_adopted"),
		"no re-adoption after the deltas start flowing")

	// A publishes a retained recovery point per commit (design rule 5) — B
	// simply never needed another one. And B, a pure receiver, published
	// nothing (adoption and native chaining are not local authorship).
	require.Len(t, laneEvents(pubA, LaneRetained), 1+deltaTurns)
	require.Equal(t, 0, pubB.Count(), "the receiving device must not publish anything back")
}
