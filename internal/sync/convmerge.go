package syncd

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/aplexica/aplexica/internal/acf"
)

// convRelation classifies an inbound full-state conversation payload against
// the local materialized state, at event granularity.
type convRelation int

const (
	convEqual          convRelation = iota // same event list
	convInboundStale                       // inbound is a strict prefix of local (old redelivery)
	convInboundExtends                     // local is a strict prefix of inbound (lossless fast-forward)
	convDiverged                           // both sides have events the other lacks
)

// convEventKeyFieldSep (ASCII unit separator) delimits the fields hashed into
// a conversation event key. It is part of the cross-device key derivation:
// changing it changes every event key, so it can never be altered without a
// flag day.
const convEventKeyFieldSep byte = 0x1f

// conversationEventKey identifies one logical conversation event across
// devices. A replicated event carries identical payload-embedded timestamp,
// role, type, and content on every device (the payload bytes travel inside the
// sealed envelope), so the key matches exactly for shared history and differs
// for divergent turns. Content is hashed via its JSON encoding so future block
// fields participate automatically.
func conversationEventKey(e acf.ConversationEvent) string {
	h := sha256.New()
	h.Write([]byte(e.Role))
	h.Write([]byte{convEventKeyFieldSep})
	h.Write([]byte(e.Type))
	h.Write([]byte{convEventKeyFieldSep})
	if b, err := json.Marshal(e.Content); err == nil {
		h.Write(b)
	}
	return fmt.Sprintf("%d\x1f%s", e.Timestamp.UTC().UnixNano(), hex.EncodeToString(h.Sum(nil)))
}

func conversationEventKeys(events []acf.ConversationEvent) []string {
	keys := make([]string, len(events))
	for i, e := range events {
		keys[i] = conversationEventKey(e)
	}
	return keys
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func conversationAttachmentKey(attachment acf.Attachment) string {
	// Data is deliberately json:"-": attachment bytes are transient blob-store
	// state, while this key compares the complete canonical wire metadata.
	encoded, _ := json.Marshal(attachment)
	return string(encoded)
}

func conversationAttachmentKeys(attachments []acf.Attachment) []string {
	keys := make([]string, len(attachments))
	for i, attachment := range attachments {
		keys[i] = conversationAttachmentKey(attachment)
	}
	sort.Strings(keys)
	return keys
}

// unionConversationAttachments returns a deterministic multiset union. Exact
// metadata duplicates retain the greater occurrence count from either side,
// so the operation is commutative and idempotent without collapsing a payload
// that intentionally references the same attachment more than once.
func unionConversationAttachments(local, inbound []acf.Attachment) []acf.Attachment {
	type countedAttachment struct {
		attachment acf.Attachment
		local      int
		inbound    int
	}
	byKey := make(map[string]countedAttachment, len(local)+len(inbound))
	add := func(attachments []acf.Attachment, localSide bool) {
		for _, attachment := range attachments {
			key := conversationAttachmentKey(attachment)
			entry := byKey[key]
			attachment.Data = nil
			entry.attachment = attachment
			if localSide {
				entry.local++
			} else {
				entry.inbound++
			}
			byKey[key] = entry
		}
	}
	add(local, true)
	add(inbound, false)

	keys := make([]string, 0, len(byKey))
	for key := range byKey {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	merged := make([]acf.Attachment, 0, len(local)+len(inbound))
	for _, key := range keys {
		entry := byKey[key]
		count := entry.local
		if entry.inbound > count {
			count = entry.inbound
		}
		for i := 0; i < count; i++ {
			merged = append(merged, entry.attachment)
		}
	}
	return merged
}

// isKeyPrefix reports whether p is a STRICT ordered prefix of full.
func isKeyPrefix(p, full []string) bool {
	if len(p) >= len(full) {
		return false
	}
	return stringSlicesEqual(p, full[:len(p)])
}

// inboundOnlyReplaysLocalTurns reports whether an inbound payload that strictly
// EXTENDS the local state adds nothing but turn events the local state already
// holds verbatim.
//
// That is the signature of a peer whose thread still carries duplicated turn
// blocks while this device's copy has been cleaned: adopting such a payload
// wholesale (the convInboundExtends arm) silently re-imports the duplicates,
// and an idle dirty peer re-serves its head whenever the recipient roster
// changes, so the corruption comes back on its own. Routing these to the
// convDiverged union instead is lossless — the union is a superset merge that
// can only drop an exact repeat — and unreachable unless a repeat exists.
//
// Identity is the COMPLETE canonical event body, not conversationEventKey:
// that key ignores CallID/ToolName/Input, so distinct parallel tool calls
// sharing one timestamp collide under it. A genuine new turn is never
// byte-identical to an existing one, timestamp included.
func inboundOnlyReplaysLocalTurns(local, inbound []acf.ConversationEvent) bool {
	if len(inbound) <= len(local) {
		return false
	}
	present := make(map[string]struct{}, len(local))
	for _, e := range local {
		if identity, ok := conversationEventIdentity(e); ok {
			present[identity] = struct{}{}
		}
	}
	replayed := false
	for _, e := range inbound[len(local):] {
		if e.Type != acf.EventTypeTurn {
			return false // a tool/system row this device lacks is real new content
		}
		identity, ok := conversationEventIdentity(e)
		if !ok {
			return false
		}
		if _, seen := present[identity]; !seen {
			return false
		}
		replayed = true
	}
	return replayed
}

// conversationEventIdentity fingerprints the complete canonical body of one
// conversation event, Timestamp included. Encoding failure reports ok=false so
// callers fail closed rather than treating an unencodable event as equal to
// anything. Mirrors the adapter-side helper of the same name.
func conversationEventIdentity(e acf.ConversationEvent) (string, bool) {
	encoded, err := json.Marshal(e)
	if err != nil {
		return "", false
	}
	return string(encoded), true
}

func classifyConversationEvents(local, inbound []acf.ConversationEvent) convRelation {
	lk, ik := conversationEventKeys(local), conversationEventKeys(inbound)
	switch {
	case stringSlicesEqual(lk, ik):
		return convEqual
	case isKeyPrefix(ik, lk):
		return convInboundStale
	case isKeyPrefix(lk, ik):
		return convInboundExtends
	default:
		return convDiverged
	}
}

// unionConversationEvents deterministically merges two event lists: every
// local event plus every inbound event not present locally, sorted
// chronologically with the event key as a total-order tie break. Both devices
// compute the IDENTICAL result for the same pair of inputs in either argument
// order — that is what terminates the cross-device merge exchange (the second
// round classifies convEqual and appends nothing).
func unionConversationEvents(local, inbound []acf.ConversationEvent) []acf.ConversationEvent {
	if repaired, ok := acf.RepairLegacyRetimestampedConversation(local, inbound); ok {
		return repaired
	}
	seen := map[string]struct{}{}
	merged := make([]acf.ConversationEvent, 0, len(local)+len(inbound))
	add := func(events []acf.ConversationEvent) {
		for _, e := range events {
			k := conversationEventKey(e)
			if _, ok := seen[k]; ok {
				continue
			}
			seen[k] = struct{}{}
			merged = append(merged, e)
		}
	}
	add(local)
	add(inbound)
	sort.SliceStable(merged, func(i, j int) bool {
		ti, tj := merged[i].Timestamp.UTC(), merged[j].Timestamp.UTC()
		if !ti.Equal(tj) {
			return ti.Before(tj)
		}
		return conversationEventKey(merged[i]) < conversationEventKey(merged[j])
	})
	return merged
}
