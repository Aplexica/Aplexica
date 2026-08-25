// SPDX-License-Identifier: AGPL-3.0-or-later
package acf

import (
	"encoding/json"
	"sort"
	"strings"
	"time"
)

// LegacyAdjacentAssistantEchoRepairEventTag marks the narrowly authenticated
// repair of the residual U,A,A,U,A projection (or its exact materialized
// U,A,A,U,A,A,U conflict form) back to clean U,A,U,A. It is deliberately not
// shared with other projection sanitation: peers may use this immutable tag
// only for the exact adjacent-answer shapes below.
const LegacyAdjacentAssistantEchoRepairEventTag = "aplexica:legacy-adjacent-assistant-echo-repair"

// ConversationRepairCommandEventTag marks an event written by the offline
// `aplexica repair conversation --apply` command (cmd/aplexica/cmd_repair_conversation.go).
// It exists for auditability and forensics only — so `aplexica log` and any
// later investigation can identify a repair-command event without relying on
// Provenance.SourceAgent string-matching alone.
//
// This tag is deliberately NOT LegacyAdjacentAssistantEchoRepairEventTag and
// must never be treated as an alias for it: the two repairs authorize
// different, unrelated projections, and nothing reads this tag as a
// peer-side merge authorization. It is inert on every sync/merge path;
// making the offline repair's output survive a peer union merge is a
// separate, not-yet-solved design problem and out of scope for this tag.
const ConversationRepairCommandEventTag = "aplexica:conversation-echo-repair"

// RepairLegacyRetimestampedConversation reconciles two structured
// conversation snapshots when at least one carries the legacy materializer
// signature: two or more turns re-authored with one shared timestamp. Older
// Aplexica builds emitted that shape when copying a conversation between
// agents. A normal event-key merge includes the timestamp and consequently
// interpreted the re-authored copies as new turns.
//
// The repair compares the complete event bodies without Timestamp, removes
// only duplicates that combine a shared synthetic timestamp with an original
// timestamp, and then preserves the best common ordering. Legitimate repeated
// prompts remain repeated: equal-content occurrences are removed only from a
// provably mixed synthetic/original snapshot. The result is deterministic and
// commutative so peers converge on identical payload bytes.
//
// repaired is false unless the legacy signature is present. Callers must use
// their normal merge policy in that case.
func RepairLegacyRetimestampedConversation(a, b []ConversationEvent) (events []ConversationEvent, repaired bool) {
	legacyA := legacyRetimestampSignature(a)
	legacyB := legacyRetimestampSignature(b)
	if !legacyA && !legacyB {
		return nil, false
	}
	// Run the edge-echo proof against the original snapshots. The older legacy
	// candidate cleaner intentionally removes some mixed-timestamp duplicates;
	// doing that first would erase the user multiplicity evidence this stricter
	// rule relies on.
	if IsLegacyAssistantEchoCleanup(a, b) {
		return append([]ConversationEvent(nil), a...), true
	}
	if IsLegacyAssistantEchoCleanup(b, a) {
		return append([]ConversationEvent(nil), b...), true
	}

	cleanA := cleanLegacyConversationCandidate(a, legacyA)
	cleanB := cleanLegacyConversationCandidate(b, legacyB)
	keysA := legacyConversationKeys(cleanA)
	keysB := legacyConversationKeys(cleanB)

	// A malformed union often contains exactly the same logical turns as a
	// clean snapshot, only duplicated or reordered. Prefer the structurally
	// healthier sequence instead of manufacturing an SCS that preserves the
	// already-corrupt order.
	if sameStringMultiset(keysA, keysB) {
		chosen := betterLegacyConversationCandidate(cleanA, cleanB)
		return append([]ConversationEvent(nil), chosen...), true
	}
	if isStringSubsequence(keysA, keysB) {
		return append([]ConversationEvent(nil), cleanB...), true
	}
	if isStringSubsequence(keysB, keysA) {
		return append([]ConversationEvent(nil), cleanA...), true
	}

	return shortestCommonConversationSupersequence(cleanA, cleanB), true
}

// IsLegacyAssistantEchoCleanup reports whether shorter is the structurally
// proven clean side of the pre-v1.0.39 edge-echo corruption. It is exported so
// native adapter merges can require additional materialization provenance for
// the same content-removing decision that peer union can make structurally.
func IsLegacyAssistantEchoCleanup(shorter, longer []ConversationEvent) bool {
	if !legacyRetimestampSignature(shorter) && !legacyRetimestampSignature(longer) {
		return false
	}
	return legacyAssistantEchoCleanupCandidate(shorter, longer)
}

// IsLegacyAdjacentAssistantEchoCleanup reports whether longer differs from a
// two-exchange portable conversation only by one adjacent, byte-equivalent
// copy of the first assistant answer whose Timestamp was re-authored. This is
// the exact residual shape emitted by the old generated-session feedback path:
//
//	clean:   [U1 A1 U2 A2 ...]
//	polluted:[U1 A1 A1 U2 A2 ...]
//
// Structure alone is intentionally not authorization to delete a turn: a user
// may legitimately receive the same assistant answer twice. Local callers must
// additionally authenticate the clean side against generated-session metadata;
// peers must require LegacyAdjacentAssistantEchoRepairEventTag on that clean
// head.
// Comparison covers the complete canonical event body and ignores only the
// timestamp changed by the historical materializer.
func IsLegacyAdjacentAssistantEchoCleanup(shorter, longer []ConversationEvent) bool {
	const (
		firstUserIndex               = 0
		firstAssistantIndex          = 1
		secondUserIndex              = 2
		secondAssistantIndex         = 3
		pollutedDuplicateAnswerIndex = 2
		pollutedSecondUserIndex      = 3
		pollutedSecondAnswerIndex    = 4
		cleanTurns                   = 4
	)
	if len(shorter) != cleanTurns || len(longer) != cleanTurns+1 {
		return false
	}
	for i, event := range shorter {
		wantRole := "user"
		if i%2 == 1 {
			wantRole = "assistant"
		}
		if event.Type != EventTypeTurn || event.Role != wantRole {
			return false
		}
	}

	shortKeys := legacyConversationKeys(shorter)
	longKeys := legacyConversationKeys(longer)
	return longKeys[firstUserIndex] == shortKeys[firstUserIndex] &&
		longKeys[firstAssistantIndex] == shortKeys[firstAssistantIndex] &&
		longKeys[pollutedDuplicateAnswerIndex] == shortKeys[firstAssistantIndex] &&
		longKeys[pollutedSecondUserIndex] == shortKeys[secondUserIndex] &&
		longKeys[pollutedSecondAnswerIndex] == shortKeys[secondAssistantIndex]
}

// IsLegacyAdjacentAssistantEchoConflictDelta recognizes the second half of
// the exact residual France conflict produced by the historical projection
// loop. After the dirty full U1,A1,A1,U2,A2 state, Claude appended a delta
// containing timestamp-re-authored A2,U2. A later authenticated Codex repair
// may use this helper together with exact event-chain and conflict-sidecar CAS
// checks; structure alone is never deletion authority.
func IsLegacyAdjacentAssistantEchoConflictDelta(clean, dirty, delta []ConversationEvent) bool {
	const (
		secondUserIndex      = 2
		secondAssistantIndex = 3
		conflictDeltaTurns   = 2
	)
	if !IsLegacyAdjacentAssistantEchoCleanup(clean, dirty) || len(delta) != conflictDeltaTurns {
		return false
	}
	deltaKeys := legacyConversationKeys(delta)
	cleanKeys := legacyConversationKeys(clean)
	return deltaKeys[0] == cleanKeys[secondAssistantIndex] &&
		deltaKeys[1] == cleanKeys[secondUserIndex]
}

// IsLegacyAdjacentAssistantEchoMaterializedConflictCleanup recognizes the
// exact materialized France conflict state:
//
//	clean:   [U1 A1 U2 A2]
//	polluted:[U1 A1 A1 U2 A2 A2 U2]
//
// The first five events are the dirty full snapshot and the last two are the
// historical Claude delta recognized above. Complete canonical event bodies
// are compared while ignoring only Timestamp. Callers still need authenticated
// write-time provenance before deleting anything.
func IsLegacyAdjacentAssistantEchoMaterializedConflictCleanup(clean, polluted []ConversationEvent) bool {
	const (
		dirtyFullTurns         = 5
		materializedDirtyTurns = 7
	)
	if len(polluted) != materializedDirtyTurns {
		return false
	}
	return IsLegacyAdjacentAssistantEchoConflictDelta(
		clean,
		polluted[:dirtyFullTurns],
		polluted[dirtyFullTurns:],
	)
}

// IsLegacyAdjacentAssistantEchoRepairCleanup recognizes only the two exact
// residual projections authorized by LegacyAdjacentAssistantEchoRepairEventTag.
func IsLegacyAdjacentAssistantEchoRepairCleanup(clean, polluted []ConversationEvent) bool {
	return IsLegacyAdjacentAssistantEchoCleanup(clean, polluted) ||
		IsLegacyAdjacentAssistantEchoMaterializedConflictCleanup(clean, polluted)
}

// legacyAssistantEchoCleanupCandidate recognizes only the exact historical
// edge-echo layout emitted by the pre-v1.0.39 reconciliation race:
//
//	clean:   [U1 A1 U2 A2 ... Un An]
//	polluted:[A1 U1 A1 U2 A2 ... Un An An]
//
// Event comparison covers the complete canonical event body and ignores only
// Timestamp, which that legacy materializer re-authored. Requiring at least two
// completed user/assistant exchanges, exactly two surplus rows, and both exact
// edge echoes prevents the cleanup from deleting legitimate assistant-first
// threads, continuations, non-text blocks, tags, or native adapter metadata.
func legacyAssistantEchoCleanupCandidate(shorter, longer []ConversationEvent) bool {
	const minimumCompletedExchanges = 2
	minimumTurns := minimumCompletedExchanges * 2
	if len(shorter) < minimumTurns || len(shorter)%2 != 0 || len(longer) != len(shorter)+2 {
		return false
	}
	for i, event := range shorter {
		wantRole := "user"
		if i%2 == 1 {
			wantRole = "assistant"
		}
		if event.Type != EventTypeTurn || event.Role != wantRole {
			return false
		}
	}

	shortKeys := legacyConversationKeys(shorter)
	longKeys := legacyConversationKeys(longer)
	for i, key := range shortKeys {
		if longKeys[i+1] != key {
			return false
		}
	}
	return longKeys[0] == shortKeys[1] &&
		longKeys[len(longKeys)-1] == shortKeys[len(shortKeys)-1]
}

func legacyConversationContentKey(ev ConversationEvent) string {
	copy := ev
	copy.Timestamp = time.Time{}
	b, _ := json.Marshal(copy)
	return string(b)
}

func legacyConversationKeys(events []ConversationEvent) []string {
	keys := make([]string, len(events))
	for i := range events {
		keys[i] = legacyConversationContentKey(events[i])
	}
	return keys
}

func timestampKey(ts time.Time) string {
	return ts.UTC().Format(time.RFC3339Nano)
}

// legacyRetimestampSignature recognizes both a clean legacy mirror (all turns
// share one timestamp) and a previously merged mirror (a shared-timestamp
// cohort plus an equal-content original at another timestamp).
func legacyRetimestampSignature(events []ConversationEvent) bool {
	if len(events) < 2 {
		return false
	}
	timestampCounts := make(map[string]int, len(events))
	contentTimestamps := make(map[string]map[string]struct{}, len(events))
	for _, ev := range events {
		ts := timestampKey(ev.Timestamp)
		timestampCounts[ts]++
		key := legacyConversationContentKey(ev)
		if contentTimestamps[key] == nil {
			contentTimestamps[key] = map[string]struct{}{}
		}
		contentTimestamps[key][ts] = struct{}{}
	}
	if len(timestampCounts) == 1 {
		for ts := range timestampCounts {
			return ts != timestampKey(time.Time{})
		}
	}
	hasCohort := false
	for ts, count := range timestampCounts {
		if ts != timestampKey(time.Time{}) && count >= 2 {
			hasCohort = true
			break
		}
	}
	if !hasCohort {
		return false
	}
	for _, timestamps := range contentTimestamps {
		if len(timestamps) >= 2 {
			return true
		}
	}
	return false
}

func cleanLegacyConversationCandidate(events []ConversationEvent, malformed bool) []ConversationEvent {
	if !malformed {
		return append([]ConversationEvent(nil), events...)
	}
	timestampCounts := make(map[string]int, len(events))
	for _, ev := range events {
		timestampCounts[timestampKey(ev.Timestamp)]++
	}

	// If the snapshot already mixed originals and generated copies, keep the
	// original occurrence and discard only its same-content synthetic twin.
	nonCohortContent := map[string]bool{}
	for _, ev := range events {
		if timestampCounts[timestampKey(ev.Timestamp)] == 1 {
			nonCohortContent[legacyConversationContentKey(ev)] = true
		}
	}
	out := make([]ConversationEvent, 0, len(events))
	for _, ev := range events {
		key := legacyConversationContentKey(ev)
		if timestampCounts[timestampKey(ev.Timestamp)] >= 2 && nonCohortContent[key] {
			continue
		}
		if isLegacyNoResponsePlaceholder(ev) {
			continue
		}
		out = append(out, ev)
	}
	return out
}

func isLegacyNoResponsePlaceholder(ev ConversationEvent) bool {
	if ev.Type != EventTypeTurn || ev.Role != "assistant" {
		return false
	}
	turns := ExtractTextTurns([]ConversationEvent{ev})
	return len(turns) == 1 && strings.TrimSpace(turns[0].Text) == "No response requested."
}

func sameStringMultiset(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	counts := make(map[string]int, len(a))
	for _, key := range a {
		counts[key]++
	}
	for _, key := range b {
		counts[key]--
	}
	for _, count := range counts {
		if count != 0 {
			return false
		}
	}
	return true
}

func isStringSubsequence(shorter, longer []string) bool {
	if len(shorter) > len(longer) {
		return false
	}
	i := 0
	for _, key := range longer {
		if i < len(shorter) && shorter[i] == key {
			i++
		}
	}
	return i == len(shorter)
}

type legacyConversationScore struct {
	roleViolations int
	timeReversals  int
	cohortTurns    int
	canonicalJSON  string
}

func scoreLegacyConversation(events []ConversationEvent) legacyConversationScore {
	score := legacyConversationScore{}
	timestampCounts := make(map[string]int, len(events))
	for _, ev := range events {
		timestampCounts[timestampKey(ev.Timestamp)]++
	}
	turns := ExtractTextTurns(events)
	for i := 1; i < len(turns); i++ {
		if turns[i-1].Role == turns[i].Role {
			score.roleViolations++
		}
	}
	for i := 1; i < len(events); i++ {
		if events[i].Timestamp.Before(events[i-1].Timestamp) {
			score.timeReversals++
		}
	}
	for _, ev := range events {
		if timestampCounts[timestampKey(ev.Timestamp)] >= 2 {
			score.cohortTurns++
		}
	}
	b, _ := json.Marshal(events)
	score.canonicalJSON = string(b)
	return score
}

func betterLegacyConversationCandidate(a, b []ConversationEvent) []ConversationEvent {
	sa, sb := scoreLegacyConversation(a), scoreLegacyConversation(b)
	less := func(x, y legacyConversationScore) bool {
		if x.roleViolations != y.roleViolations {
			return x.roleViolations < y.roleViolations
		}
		if x.timeReversals != y.timeReversals {
			return x.timeReversals < y.timeReversals
		}
		if x.cohortTurns != y.cohortTurns {
			return x.cohortTurns < y.cohortTurns
		}
		return x.canonicalJSON < y.canonicalJSON
	}
	if less(sb, sa) {
		return b
	}
	return a
}

// shortestCommonConversationSupersequence preserves every unique divergent
// turn while matching re-timestamped copies by their timestamp-free body.
// Lexical tie-breaking makes the construction commutative.
func shortestCommonConversationSupersequence(a, b []ConversationEvent) []ConversationEvent {
	ka, kb := legacyConversationKeys(a), legacyConversationKeys(b)
	dp := make([][]int, len(a)+1)
	for i := range dp {
		dp[i] = make([]int, len(b)+1)
	}
	for i := len(a); i >= 0; i-- {
		for j := len(b); j >= 0; j-- {
			switch {
			case i == len(a):
				dp[i][j] = len(b) - j
			case j == len(b):
				dp[i][j] = len(a) - i
			case ka[i] == kb[j]:
				dp[i][j] = 1 + dp[i+1][j+1]
			case dp[i+1][j] < dp[i][j+1]:
				dp[i][j] = 1 + dp[i+1][j]
			default:
				dp[i][j] = 1 + dp[i][j+1]
			}
		}
	}

	out := make([]ConversationEvent, 0, dp[0][0])
	for i, j := 0, 0; i < len(a) || j < len(b); {
		switch {
		case i == len(a):
			out = append(out, b[j:]...)
			j = len(b)
		case j == len(b):
			out = append(out, a[i:]...)
			i = len(a)
		case ka[i] == kb[j]:
			out = append(out, canonicalLegacyEvent(a[i], b[j]))
			i++
			j++
		case dp[i+1][j] < dp[i][j+1] || (dp[i+1][j] == dp[i][j+1] && ka[i] < kb[j]):
			out = append(out, a[i])
			i++
		default:
			out = append(out, b[j])
			j++
		}
	}
	return out
}

func canonicalLegacyEvent(a, b ConversationEvent) ConversationEvent {
	// Prefer an event whose timestamp is not zero. If both are populated,
	// canonical JSON order supplies a stable peer-independent winner.
	if a.Timestamp.IsZero() != b.Timestamp.IsZero() {
		if a.Timestamp.IsZero() {
			return b
		}
		return a
	}
	candidates := []ConversationEvent{a, b}
	sort.Slice(candidates, func(i, j int) bool {
		ib, _ := json.Marshal(candidates[i])
		jb, _ := json.Marshal(candidates[j])
		return string(ib) < string(jb)
	})
	return candidates[0]
}
