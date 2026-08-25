package openclaw

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter"
)

// SessionJSONLFormat is the ConversationPayload.Format for the legacy opaque
// OpenClaw conversation payload (shipped in v0.24.0). The structured-mode
// payload uses acf.ConversationFormatV1 (added in v0.24.1).
const SessionJSONLFormat = "openclaw.session.jsonl"

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
		Format:  SessionJSONLFormat,
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

// ImportConversation reads an OpenClaw session transcript JSONL (typically
// under ~/.openclaw/agents/<id>/sessions/) and writes one conversation
// artifact. The payload format depends on Adapter.CanonicalConversations:
// false (default) → legacy opaque "openclaw.session.jsonl"; true →
// acf.conversation.v1 (structured event list).
func (a *Adapter) ImportConversation(ctx context.Context, store *acf.Store, nativePath string) ([]string, error) {
	// A transcript the daemon materialized into OpenClaw. If the user CONTINUED
	// it in OpenClaw, route the new turns through the loop-safe thread merge so
	// the continuation propagates to the other agents (mirrors hermes/kilo). The
	// synced sessionId is a truncated forward hash of the artifact id, so recover
	// the canonical thread via a recomputed forward map. An unchanged echo (or an
	// id we can't map) is NOT re-imported as a new openclaw-sourced artifact.
	if sessionFileIsCanonicalImport(nativePath) {
		if store == nil {
			return nil, nil
		}
		ref, ok := openclawSessionThreadRef(nativePath, store)
		if !ok {
			return nil, nil
		}
		content, rerr := os.ReadFile(nativePath)
		if rerr != nil {
			return nil, nil
		}
		events, eerr := EncodeCanonical(content)
		if eerr != nil {
			return nil, nil
		}
		ids, handled, merr := adapter.MergeConversationByThreadRef(ctx, store, a.opaqueParams(), ref, events, adapter.EncodeCanonicalConversationPayload)
		if !handled {
			return nil, nil
		}
		return ids, merr
	}
	if a.CanonicalConversations {
		return adapter.ImportCanonicalConversation(ctx, store, a.opaqueParams(), nativePath, EncodeCanonical)
	}
	return adapter.ImportOpaque(ctx, store, acf.KindConversation, a.opaqueParams(), nativePath, a.conversationEncode)
}

// openclawSyncedThreadMap maps each conversation artifact's deterministic
// OpenClaw synced sessionId to its artifact id, so a re-imported synced
// transcript is reconciled to its canonical thread. nil on a store-read error
// (the caller then skips the transcript).
func openclawSyncedThreadMap(store *acf.Store) map[string]string {
	refMap := openclawSyncedThreadRefMap(store)
	if refMap == nil {
		return nil
	}
	out := make(map[string]string, len(refMap))
	for sessionID, ref := range refMap {
		if ref.BranchID == acf.MainBranch {
			out[sessionID] = ref.ArtifactID
		}
	}
	return out
}

func openclawSyncedThreadRefMap(store *acf.Store) map[string]adapter.ThreadRef {
	if store == nil {
		return nil
	}
	arts, err := store.ListArtifacts(acf.KindConversation)
	if err != nil {
		return nil
	}
	out := make(map[string]adapter.ThreadRef, len(arts))
	for _, art := range arts {
		for _, branchID := range openclawKnownBranches(store, art) {
			out[openclawSyncedSessionIDForBranch(art.ArtifactID, branchID)] = adapter.ThreadRef{
				ArtifactID: art.ArtifactID,
				BranchID:   branchID,
			}
		}
	}
	return out
}

func openclawSessionThreadRef(path string, store *acf.Store) (adapter.ThreadRef, bool) {
	hdr, ok := readOpenclawSessionHeader(path)
	if !ok || hdr.Aplexica != aplexicaImportMarker {
		return adapter.ThreadRef{}, false
	}
	if hdr.AplexicaThreadID != "" {
		return adapter.ThreadRef{
			ArtifactID: hdr.AplexicaThreadID,
			BranchID:   normalizeOpenClawBranchID(hdr.AplexicaBranchID),
		}, true
	}
	sessionID := hdr.ID
	if sessionID == "" {
		sessionID = strings.TrimSuffix(filepath.Base(path), ".jsonl")
	}
	ref := openclawSyncedThreadRefMap(store)[sessionID]
	return ref, ref.ArtifactID != ""
}

func openclawKnownBranches(store *acf.Store, art acf.Artifact) []string {
	seen := map[string]struct{}{acf.MainBranch: {}}
	branches, err := store.ListBranches(acf.KindConversation, art.ArtifactID, true)
	if err == nil {
		for _, branch := range branches {
			if norm, nerr := acf.NormalizeBranchName(branch.Name); nerr == nil {
				seen[norm] = struct{}{}
			}
		}
	}
	for _, branch := range art.MaterializedBranchByAgent {
		if norm, nerr := acf.NormalizeBranchName(branch); nerr == nil {
			seen[norm] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for branch := range seen {
		out = append(out, branch)
	}
	sort.Strings(out)
	return out
}

// ExportConversation replays the conversation artifact's event log and
// writes the result to destPath.
func (a *Adapter) ExportConversation(ctx context.Context, store *acf.Store, artifactID, destPath string) error {
	return adapter.ExportCanonicalConversation(ctx, store, artifactID, destPath, DecodeCanonical, conversationDecode)
}
