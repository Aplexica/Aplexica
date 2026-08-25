package acf

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// ComputeHash returns the SHA-256 hex digest of the canonical JSON encoding of e
// with the Hash field zeroed. This guarantees that storing the computed hash back
// into e.Hash does not change subsequent recomputation.
func ComputeHash(e Event) (string, error) {
	e.Hash = ""
	canonical, err := json.Marshal(e)
	if err != nil {
		return "", fmt.Errorf("acf: marshal event for hash: %w", err)
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}

// VerifyChain checks each event in order: its Hash matches ComputeHash(e),
// and ParentHash continues the per-branch chain. The first event on each
// branch must either have ParentHash == "" (main branch genesis), be a fork
// event whose ParentHash references an event already seen on another branch,
// or be a payload-bearing baseline used as a self-contained recovery root for
// a side branch whose fork ancestry is unavailable on this device. Fork events
// themselves are the first event of their branch and reference a prior event
// on the source branch.
//
// A baseline event (EventTypeBaseline, aligned-chains delta sync) chains
// onto its branch head like any other event, but then RESETS the expected
// parent for subsequent events to its AlignedHead — the origin device's
// head hash it adopted. That mirrors AppendEvent's head bookkeeping, so a
// receiver log of the shape [local events…, baseline, verbatim origin
// events…] verifies even though the origin events' parents never appear as
// local hashes. A baseline with an empty AlignedHead is malformed.
func VerifyChain(events []Event) error {
	branchHead := map[string]string{}
	seenHash := map[string]bool{}
	for i, e := range events {
		want, err := ComputeHash(e)
		if err != nil {
			return fmt.Errorf("acf: event %d (%s): %w", i, e.EventID, err)
		}
		if e.Hash != want {
			return fmt.Errorf("acf: event %d (%s): Hash %q does not match recomputed %q",
				i, e.EventID, e.Hash, want)
		}
		b := e.Branch
		if b == "" {
			b = MainBranch
		}
		if e.Type == EventTypeForkOuter {
			if _, exists := branchHead[b]; exists {
				return fmt.Errorf("acf: event %d (%s): fork onto branch %q which already exists",
					i, e.EventID, b)
			}
			if e.ParentHash == "" || !seenHash[e.ParentHash] {
				return fmt.Errorf("acf: event %d (%s): fork ParentHash %q does not reference a prior event",
					i, e.EventID, e.ParentHash)
			}
		} else {
			head, seen := branchHead[b]
			isSideBaselineRoot := !seen && b != MainBranch && e.Type == EventTypeBaseline &&
				e.ParentHash == "" && HasPayload(e.Payload)
			if !seen && b != MainBranch && !isSideBaselineRoot {
				return fmt.Errorf("acf: event %d (%s): non-fork event is first on non-main branch %q (only a fork or recovery baseline may introduce a branch)",
					i, e.EventID, b)
			}
			if e.ParentHash != head {
				return fmt.Errorf("acf: event %d (%s): ParentHash %q does not match head of branch %q (%q)",
					i, e.EventID, e.ParentHash, b, head)
			}
		}
		newHead := e.Hash
		if e.Type == EventTypeBaseline {
			if e.AlignedHead == "" {
				return fmt.Errorf("acf: event %d (%s): baseline event has empty alignedHead",
					i, e.EventID)
			}
			if e.AlignedEventID == "" {
				return fmt.Errorf("acf: event %d (%s): baseline event has empty alignedEventId",
					i, e.EventID)
			}
			if !HasPayload(e.Payload) {
				return fmt.Errorf("acf: event %d (%s): baseline event has no full-state payload",
					i, e.EventID)
			}
			// The baseline re-aligns the chain: subsequent events chain onto
			// the ORIGIN head it names, not onto the baseline itself.
			newHead = e.AlignedHead
		}
		branchHead[b] = newHead
		seenHash[e.Hash] = true
	}
	return nil
}
