package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter"
)

// conversationEncode is a method (not a free function) so it can read
// CanonicalConversations off the Adapter to choose between legacy opaque
// and structured canonical mode.
func (a *Adapter) conversationEncode(content []byte) (json.RawMessage, error) {
	if a.CanonicalConversations {
		events, err := EncodeCanonical(content)
		if err != nil {
			return nil, err
		}
		return acf.EncodePayload(acf.ConversationPayload{
			Format: acf.ConversationFormatV1,
			Events: events,
		})
	}
	return acf.EncodePayload(acf.ConversationPayload{
		Format:  "codex.session.jsonl",
		Content: string(content),
	})
}

// conversationDecode reads either format transparently — Export always
// handles both, regardless of the adapter's CanonicalConversations setting.
func conversationDecode(e acf.Event) (string, error) {
	p, err := acf.DecodeConversationPayload(e)
	if err != nil {
		return "", err
	}
	if p.Format == acf.ConversationFormatV1 {
		jsonl, derr := DecodeCanonical(p.Events)
		if derr != nil {
			return "", derr
		}
		return string(jsonl), nil
	}
	return p.Content, nil
}

// ImportConversation reads a Codex session .jsonl file (typically under
// ~/.codex/sessions/<year>/<month>/<day>/rollout-*.jsonl) and writes one
// conversation artifact. The payload format depends on
// Adapter.CanonicalConversations: false (default) → legacy opaque
// codex.session.jsonl; true → acf.conversation.v1.
func (a *Adapter) ImportConversation(ctx context.Context, store *acf.Store, nativePath string) ([]string, error) {
	var (
		ids []string
		err error
	)
	if a.CanonicalConversations {
		// Releases before v1.0.39 stored Codex's developer/system harness and
		// transient assistant commentary in native conversation artifacts. If
		// this source's current head still has that unmistakable system residue,
		// reconstruct the old projection from the same file and replace it only
		// after the shared repair helper proves the head is an exact prefix. This
		// migrates existing Hermes-visible leaks without erasing a continuation
		// that may have arrived from another device.
		if repairIDs, repaired, repairErr := a.repairLegacyNativeProjection(ctx, store, nativePath); repairErr != nil {
			return nil, repairErr
		} else if repaired {
			return a.applyConversationTitle(store, nativePath, repairIDs)
		}
		ids, err = adapter.ImportCanonicalConversationFile(ctx, store, a.opaqueParams(), nativePath, a.conversationCache().encodeFile)
	} else {
		ids, err = adapter.ImportOpaque(ctx, store, acf.KindConversation, a.opaqueParams(), nativePath, a.conversationEncode)
	}
	if err != nil {
		return nil, err
	}
	return a.applyConversationTitle(store, nativePath, ids)
}

func (a *Adapter) repairLegacyNativeProjection(ctx context.Context, store *acf.Store, nativePath string) ([]string, bool, error) {
	abs, err := filepath.Abs(nativePath)
	if err != nil {
		return nil, false, fmt.Errorf("codex: resolve legacy projection path: %w", err)
	}
	art, found, err := store.FindBySourcePath(acf.KindConversation, abs)
	if err != nil || !found {
		return nil, false, err
	}
	current, provenHead, ok, err := store.MaterializedConversationHeadFromStore(art.ArtifactID)
	if err != nil || !ok || current.Format != acf.ConversationFormatV1 {
		return nil, false, err
	}
	localAuthoritativeHead := a.deviceID() != "" &&
		provenHead.Provenance.SourceAgent == a.Name() &&
		provenHead.Provenance.DeviceID == a.deviceID()
	// Adapter-version drift by itself is not deletion evidence. Most old heads
	// are already clean, and forcing them through a full native reconstruction
	// made a single active 100+ MiB rollout monopolize every scan. Only residue
	// that cannot belong in a portable conversation justifies reading the source
	// and attempting the exact source-derived repair below.
	needsRepair := false
	for _, event := range current.Events {
		if legacyCodexProjectionResidue(event) {
			needsRepair = true
			break
		}
	}
	if !needsRepair {
		return nil, false, nil
	}
	snapshot, err := a.conversationCache().snapshotFile(abs)
	if err != nil {
		return nil, false, err
	}
	clean := snapshot.events
	legacy := snapshot.legacyEvents
	if !localAuthoritativeHead {
		return adapter.RepairCanonicalConversationProjection(
			ctx, store, a.opaqueParams(), abs, legacy, clean,
		)
	}
	// Older releases sometimes wrapped/re-timestamped native rows or merged
	// unrelated portable prefix/suffix events, so the exact splice proof above
	// cannot classify the whole head. On a same-device Codex head, remove only
	// execution messages whose role+content is independently proven to be in
	// legacy-minus-clean for these exact source bytes. Every unclassified event
	// is retained byte-for-byte.
	base := current.Events
	projected := false
	if replacement, proven := adapter.SanitizedConversationProjection(current.Events, legacy, clean); proven {
		base = replacement
		projected = true
	}
	sanitized, changed := sanitizeLegacyCodexExecutionEvents(base, legacy, clean)
	if !changed && !projected {
		return nil, false, nil
	}
	if !changed {
		sanitized = base
	}
	ids, replaceErr := adapter.ReplaceCanonicalConversationProjectionAtHead(
		ctx, store, a.opaqueParams(), art.ArtifactID, provenHead, sanitized, current.Attachments,
	)
	return ids, true, replaceErr
}

func legacyCodexProjectionResidue(event acf.ConversationEvent) bool {
	if event.Type == acf.EventTypeSystemNote || event.Role == "system" ||
		event.Type == acf.EventTypeToolCall || event.Type == acf.EventTypeToolResult {
		return true
	}
	if event.Role != "assistant" || len(event.NativeExtras) == 0 {
		return false
	}
	var extras struct {
		Phase string `json:"phase"`
	}
	return json.Unmarshal(event.NativeExtras, &extras) == nil &&
		extras.Phase != "" && !codexFinalAnswerPhase(extras.Phase)
}

func sanitizeLegacyCodexExecutionEvents(current, legacy, clean []acf.ConversationEvent) ([]acf.ConversationEvent, bool) {
	var removed []acf.ConversationEvent
	cleanIndex := 0
	for i := range legacy {
		if cleanIndex < len(clean) && reflect.DeepEqual(legacy[i], clean[cleanIndex]) {
			cleanIndex++
			continue
		}
		if _, ok := codexExecutionMessageKey(legacy[i]); ok {
			removed = append(removed, legacy[i])
		}
	}
	if cleanIndex != len(clean) || len(removed) == 0 {
		return nil, false
	}
	deleteAt := make([]bool, len(current))
	matched := make([]bool, len(removed))
	// Codex-native developer/system rows are always local execution policy,
	// never portable conversation. Older conversion paths represented the same
	// row both as a system turn and as a system_note wrapper, so source-counted
	// deletion would otherwise remove one copy and leave the duplicate visible
	// in downstream legacy projections.
	for i, event := range current {
		if event.Type == acf.EventTypeSystemNote || event.Role == "system" {
			deleteAt[i] = true
		}
	}
	// Prefer an exact source-derived event identity. This retains a remote or
	// legitimate final turn that happens to share the commentary's text.
	for ri := range removed {
		for ci := range current {
			if !deleteAt[ci] && reflect.DeepEqual(removed[ri], current[ci]) {
				deleteAt[ci] = true
				matched[ri] = true
				break
			}
		}
	}
	// Older materializers sometimes changed timestamps or wrapper metadata. For
	// those rows, fall back to role+content but consume at most one current row
	// per proven removed source row (multiset semantics, never a delete-all set).
	for ri := range removed {
		if matched[ri] {
			continue
		}
		key, _ := codexExecutionMessageKey(removed[ri])
		for ci := range current {
			if deleteAt[ci] {
				continue
			}
			if currentKey, ok := codexExecutionMessageKey(current[ci]); ok && currentKey == key {
				deleteAt[ci] = true
				matched[ri] = true
				break
			}
		}
	}
	removedCount := 0
	for _, remove := range deleteAt {
		if remove {
			removedCount++
		}
	}
	if removedCount == 0 {
		return nil, false
	}
	out := make([]acf.ConversationEvent, 0, len(current)-removedCount)
	for i, event := range current {
		if !deleteAt[i] {
			out = append(out, event)
		}
	}
	return out, true
}

func codexExecutionMessageKey(event acf.ConversationEvent) (string, bool) {
	if event.Type == acf.EventTypeToolCall || event.Type == acf.EventTypeToolResult {
		identity := struct {
			Type     string             `json:"type"`
			CallID   string             `json:"call_id,omitempty"`
			ToolName string             `json:"tool_name,omitempty"`
			Input    json.RawMessage    `json:"input,omitempty"`
			Content  []acf.ContentBlock `json:"content,omitempty"`
			IsError  bool               `json:"is_error,omitempty"`
		}{event.Type, event.CallID, event.ToolName, event.Input, event.Content, event.IsError}
		encoded, err := json.Marshal(identity)
		if err != nil {
			return "", false
		}
		return "tool\x00" + string(encoded), true
	}
	role := event.Role
	if event.Type == acf.EventTypeSystemNote || role == "system" {
		role = "system"
	}
	if role != "system" && role != "assistant" {
		return "", false
	}
	content, err := json.Marshal(event.Content)
	if err != nil || len(content) == 0 {
		return "", false
	}
	return role + "\x00" + string(content), true
}

// applyConversationTitle promotes Codex Desktop's thread_name into the ACF
// artifact's human label. SourcePath remains the stable native identity, so a
// later Codex rename updates presentation without minting a new artifact.
func (a *Adapter) applyConversationTitle(store *acf.Store, nativePath string, ids []string) ([]string, error) {
	for _, id := range ids {
		art, err := store.ReadArtifact(acf.KindConversation, id)
		if err != nil {
			return nil, fmt.Errorf("codex: read conversation title target: %w", err)
		}
		title := adapter.ResolveConversationTitle(a.HomeDir, a.Name(), nativePath, art.Name)
		if title == "" || title == art.Name {
			continue
		}
		if _, err := adapter.PersistConversationTitle(store, id, title, a.deviceID(), a.Name(), a.Version()); err != nil {
			return nil, fmt.Errorf("codex: persist conversation title: %w", err)
		}
	}
	return ids, nil
}

// ExportConversation replays the conversation artifact's event log and writes the result.
func (a *Adapter) ExportConversation(ctx context.Context, store *acf.Store, artifactID, destPath string) error {
	return adapter.ExportCanonicalConversation(ctx, store, artifactID, destPath, DecodeCanonical, conversationDecode)
}
