package acf

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func newTestEvent(parentHash string) Event {
	payload, _ := json.Marshal(MemoryPayload{Format: "markdown", Content: "# Hello\n"})
	return Event{
		EventID:    "01956a39-1234-7890-abcd-ef0123456789",
		ArtifactID: "01956a39-aaaa-7890-abcd-ef0123456789",
		Type:       "create",
		Timestamp:  time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC),
		Provenance: Provenance{DeviceID: "dev-1", SourceAgent: "claude-code", AdapterVersion: "0.1.0"},
		Payload:    payload,
		ParentHash: parentHash,
	}
}

func TestComputeHash_IsDeterministic(t *testing.T) {
	e := newTestEvent("")
	h1, err := ComputeHash(e)
	require.NoError(t, err)
	h2, err := ComputeHash(e)
	require.NoError(t, err)
	require.Equal(t, h1, h2, "hashing the same event twice must yield the same digest")
	require.Len(t, h1, 64, "hex SHA-256 is 64 chars")
}

func TestComputeHash_ExcludesHashField(t *testing.T) {
	e := newTestEvent("")
	h1, err := ComputeHash(e)
	require.NoError(t, err)
	e.Hash = "junk-value"
	h2, err := ComputeHash(e)
	require.NoError(t, err)
	require.Equal(t, h1, h2, "Hash field must NOT affect ComputeHash output")
}

func TestComputeHash_ParentHashAffectsResult(t *testing.T) {
	h1, _ := ComputeHash(newTestEvent(""))
	h2, _ := ComputeHash(newTestEvent("nonempty-parent"))
	require.NotEqual(t, h1, h2, "ParentHash MUST affect the digest — it is part of the chain")
}

func TestComputeHash_PayloadContentAffectsResult(t *testing.T) {
	e1 := newTestEvent("")
	e2 := newTestEvent("")
	tamperedPayload, _ := json.Marshal(MemoryPayload{Format: "markdown", Content: "# Tampered\n"})
	e2.Payload = tamperedPayload
	h1, _ := ComputeHash(e1)
	h2, _ := ComputeHash(e2)
	require.NotEqual(t, h1, h2, "Payload.Content MUST affect the digest")
}

func TestVerifyChain_AcceptsValidChain(t *testing.T) {
	e1 := newTestEvent("")
	e1.Hash, _ = ComputeHash(e1)
	e2 := newTestEvent(e1.Hash)
	e2.EventID = "01956a39-1234-7890-abcd-ef0123456790"
	e2.Hash, _ = ComputeHash(e2)
	require.NoError(t, VerifyChain([]Event{e1, e2}))
}

func TestVerifyChain_RejectsBrokenChain(t *testing.T) {
	e1 := newTestEvent("")
	e1.Hash, _ = ComputeHash(e1)
	e2 := newTestEvent("WRONG-parent-hash")
	e2.Hash, _ = ComputeHash(e2)
	require.Error(t, VerifyChain([]Event{e1, e2}))
}

// TestVerifyChain_RejectsSecondGenesisOnNewBranch guards against a non-fork
// event introducing a brand-new non-main branch as a second genesis. BRD-02
// §4.5 designates `fork` as the only branch-divergence event, and §4.1.2/3
// model each artifact as a single-rooted append-only Merkle log; a side branch
// that appears without a fork (ParentHash == "" on a non-main branch) is a
// malformed two-root chain and must be rejected.
func TestVerifyChain_RejectsSecondGenesisOnNewBranch(t *testing.T) {
	e1 := newTestEvent("")
	e1.Branch = MainBranch
	e1.Hash, _ = ComputeHash(e1)

	e2 := newTestEvent("") // ParentHash == "" — a second genesis
	e2.EventID = "01956a39-1234-7890-abcd-ef0123456790"
	e2.Type = EventTypeUpdate
	e2.Branch = "rogue" // brand-new branch, not introduced by a fork
	e2.Hash, _ = ComputeHash(e2)

	require.Error(t, VerifyChain([]Event{e1, e2}),
		"a non-fork event must not introduce a new branch as a second genesis")
}

// TestVerifyChain_AcceptsForkIntroducingBranch ensures the fix does not break
// the legitimate case: a fork event may introduce a new branch, and
// continuation events on that branch chain off the fork as usual.
func TestVerifyChain_AcceptsForkIntroducingBranch(t *testing.T) {
	e1 := newTestEvent("")
	e1.Branch = MainBranch
	e1.Hash, _ = ComputeHash(e1)

	fork := newTestEvent(e1.Hash)
	fork.EventID = "01956a39-1234-7890-abcd-ef0123456791"
	fork.Type = EventTypeForkOuter
	fork.Branch = "topic"
	fork.ForkSourceBranch = MainBranch
	fork.Hash, _ = ComputeHash(fork)

	cont := newTestEvent(fork.Hash)
	cont.EventID = "01956a39-1234-7890-abcd-ef0123456792"
	cont.Type = EventTypeUpdate
	cont.Branch = "topic"
	cont.Hash, _ = ComputeHash(cont)

	require.NoError(t, VerifyChain([]Event{e1, fork, cont}),
		"a fork may legitimately introduce a branch and be continued by non-fork events")
}
