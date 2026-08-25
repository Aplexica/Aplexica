package claudecode

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

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
		Format:  "claude-code.session.jsonl",
		Content: string(content),
	})
}

// conversationEncodeFor is the path-aware encoder used by ImportConversation.
// In canonical mode it routes through the per-path incremental cache so an
// append-only session.jsonl is re-parsed only from where the last import
// stopped, not from byte 0. The produced payload is byte-identical to
// conversationEncode's — the cache is a pure CPU optimization. The legacy
// opaque path keeps the original whole-file encode (it stores raw content, so
// there is nothing to parse incrementally).
func (a *Adapter) conversationEncodeFor(path string, content []byte) (json.RawMessage, error) {
	if !a.CanonicalConversations {
		return a.conversationEncode(content)
	}
	events, err := a.conversationCache().encodeChecked(path, content)
	if err != nil {
		return nil, err
	}
	return acf.EncodePayload(acf.ConversationPayload{
		Format: acf.ConversationFormatV1,
		Events: events,
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

// ImportConversation reads a Claude Code session .jsonl file and writes one
// conversation artifact. The payload format depends on Adapter.CanonicalConversations:
// false (default) → legacy opaque; true → acf.conversation.v1.
func (a *Adapter) ImportConversation(ctx context.Context, store *acf.Store, nativePath string) ([]string, error) {
	var (
		ids []string
		err error
	)
	if a.CanonicalConversations {
		ids, err = adapter.ImportCanonicalConversationFile(ctx, store, a.opaqueParams(), nativePath, a.conversationCache().encodeFile)
	} else {
		ids, err = adapter.ImportOpaque(ctx, store, acf.KindConversation, a.opaqueParams(), nativePath, a.conversationEncode)
	}
	if err != nil {
		return nil, err
	}
	return a.applyConversationTitle(store, nativePath, ids)
}

// applyConversationTitle promotes Claude Code Desktop's title into the
// portable artifact name. The transcript remains the stable identity, so a
// Desktop rename updates presentation instead of creating another thread.
func (a *Adapter) applyConversationTitle(store *acf.Store, nativePath string, ids []string) ([]string, error) {
	cliSessionID := strings.TrimSuffix(filepath.Base(nativePath), filepath.Ext(nativePath))
	record, ok := a.desktopSessionForCLI(cliSessionID)
	if !ok {
		return ids, nil
	}
	title := adapter.ResolveConversationTitle(a.HomeDir, a.Name(), nativePath, record.Title)
	if title == "" {
		return ids, nil
	}
	for _, id := range ids {
		if _, err := adapter.PersistConversationTitle(store, id, title, a.deviceID(), a.Name(), a.Version()); err != nil {
			return nil, fmt.Errorf("claudecode: persist conversation title: %w", err)
		}
	}
	return ids, nil
}

// ExportConversation replays the conversation artifact's event log and writes the result.
func (a *Adapter) ExportConversation(ctx context.Context, store *acf.Store, artifactID, destPath string) error {
	return adapter.ExportCanonicalConversation(ctx, store, artifactID, destPath, DecodeCanonical, conversationDecode)
}
