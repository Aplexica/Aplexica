package syncd

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter"
	"github.com/aplexica/aplexica/internal/adapter/hermes"
	"github.com/aplexica/aplexica/internal/hermesdb"
	"github.com/stretchr/testify/require"
)

type recordingHermesExporter struct {
	*hermes.Adapter
	causedBy map[string]string
}

func (r *recordingHermesExporter) Export(
	ctx context.Context,
	store *acf.Store,
	artifactID string,
	destPath string,
) error {
	r.causedBy[artifactID] = adapter.CausedByFromContext(ctx)
	return r.Adapter.Export(ctx, store, artifactID, destPath)
}

// Hermes stores every session in one SQLite database, so it cannot implement
// ConversationSessionTarget's one-session-path contract. Canonical fan-out must
// nevertheless use its ordinary Export path immediately; relying only on the
// independent five-second hermeswatch poll adds up to one full poll interval to
// every Codex/Claude -> Hermes turn.
func TestFanOut_CanonicalConversationMaterializesHermesImmediately(t *testing.T) {
	root := realTempDir(t)
	store := &acf.Store{Root: filepath.Join(root, "store")}
	require.NoError(t, store.Init())

	dbPath := filepath.Join(root, ".hermes", "state.db")
	require.NoError(t, os.MkdirAll(filepath.Dir(dbPath), 0o755))
	db, err := hermesdb.InitTestDB(dbPath)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	source := &fakeConvSource{name: "codex"}
	h := &recordingHermesExporter{Adapter: hermes.New(), causedBy: map[string]string{}}
	h.HomeDir = root
	orch, err := NewOrchestrator(Config{
		Dir:      root,
		Adapters: []adapter.Adapter{source, h},
		Store:    store,
	})
	require.NoError(t, err)
	defer orch.Close()

	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	type wantSession struct {
		prompt     string
		answer     string
		sourceHash string
	}
	wants := map[string]wantSession{}
	artifactIDs := make([]string, 0, 2)
	for i, turns := range []struct {
		prompt string
		answer string
	}{
		{prompt: "What is 19 multiplied by 23?", answer: "437"},
		{prompt: "What is 7 plus 8?", answer: "15"},
	} {
		createdAt := now.Add(time.Duration(i) * time.Minute)
		artifactID := acf.NewID()
		require.NoError(t, store.WriteArtifact(acf.Artifact{
			AcfSchemaVersion: acf.SchemaVersion,
			ArtifactID:       artifactID,
			Kind:             acf.KindConversation,
			Scope:            acf.ScopeGlobal,
			Name:             "fresh Codex turn",
			CreatedAt:        createdAt,
			UpdatedAt:        createdAt,
		}))
		payload, encodeErr := acf.EncodePayload(acf.ConversationPayload{
			Format: acf.ConversationFormatV1,
			Events: []acf.ConversationEvent{
				{
					Type:      acf.EventTypeTurn,
					Role:      "user",
					Timestamp: createdAt,
					Content:   []acf.ContentBlock{{Type: "text", Text: turns.prompt}},
				},
				{
					Type:      acf.EventTypeTurn,
					Role:      "assistant",
					Timestamp: createdAt.Add(time.Second),
					Content:   []acf.ContentBlock{{Type: "text", Text: turns.answer}},
				},
			},
		})
		require.NoError(t, encodeErr)
		require.NoError(t, store.AppendEvent(acf.KindConversation, acf.Event{
			EventID:    acf.NewID(),
			ArtifactID: artifactID,
			Type:       acf.EventTypeCreate,
			Timestamp:  createdAt,
			Provenance: acf.Provenance{DeviceID: "origin", SourceAgent: "codex"},
			Payload:    payload,
		}))
		head, found, headErr := store.LastEvent(acf.KindConversation, artifactID)
		require.NoError(t, headErr)
		require.True(t, found)
		artifactIDs = append(artifactIDs, artifactID)
		wants[artifactID] = wantSession{turns.prompt, turns.answer, head.Hash}
	}

	// fanOut is synchronous. Both rows share one state.db but must exist when it
	// returns, without collapsing to one path-keyed plan and without waiting for
	// a hermeswatch TickInbound call.
	orch.fanOut(context.Background(), source, artifactIDs, root, "", false, nil)

	sessions, err := hermesdb.ListSessions(dbPath, 0)
	require.NoError(t, err)
	require.Len(t, sessions, 2)
	for _, session := range sessions {
		want, ok := wants[session.Session.ID]
		require.True(t, ok, "unexpected Hermes session %s", session.Session.ID)
		require.Equal(t, hermesdb.AplexicaCanonicalImportSource, session.Session.Source)
		require.Len(t, session.Messages, 2)
		require.Equal(t, "user", session.Messages[0].Role)
		require.Equal(t, want.prompt, *session.Messages[0].Content)
		require.Equal(t, "assistant", session.Messages[1].Role)
		require.Equal(t, want.answer, *session.Messages[1].Content)
		require.Equal(t, want.sourceHash, h.causedBy[session.Session.ID],
			"each shared-DB export must retain its own CausedBy head hash")
	}
}
