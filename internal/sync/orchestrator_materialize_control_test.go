package syncd

import (
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter"
	"github.com/stretchr/testify/require"
)

type recordingConversationTarget struct {
	fakeConvSource
	dest string

	mu     sync.Mutex
	branch string
	turns  []acf.TextTurn
}

func (r *recordingConversationTarget) MaterializeConversationSession(_ acf.Artifact, head acf.Event, _ string) (string, bool, error) {
	payload, err := acf.DecodeConversationPayload(head)
	if err != nil {
		return "", false, err
	}
	r.mu.Lock()
	r.branch = head.Branch
	r.turns = acf.ExtractTextTurns(payload.Events)
	r.mu.Unlock()
	return r.dest, true, nil
}

func (r *recordingConversationTarget) snapshot() (string, []acf.TextTurn) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.branch, append([]acf.TextTurn(nil), r.turns...)
}

func TestMaterializeConversationBranch_WritesSelectedBranchAndPointer(t *testing.T) {
	root := realTempDir(t)
	store := &acf.Store{Root: filepath.Join(root, "store")}
	require.NoError(t, store.Init())

	artifactID := acf.NewID()
	require.NoError(t, store.WriteArtifact(acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion,
		ArtifactID:       artifactID,
		Kind:             acf.KindConversation,
		Scope:            acf.ScopeGlobal,
		Name:             "conversation",
		CreatedAt:        time.Date(2026, 7, 6, 10, 0, 0, 0, time.UTC),
		UpdatedAt:        time.Date(2026, 7, 6, 10, 0, 0, 0, time.UTC),
	}))
	appendConversationEvent(t, store, artifactID, acf.MainBranch, "", "",
		[]acf.TextTurn{{Role: "user", Text: "main q"}, {Role: "assistant", Text: "main a"}})
	events, err := store.ReadEvents(acf.KindConversation, artifactID)
	require.NoError(t, err)
	mainHead := events[len(events)-1]

	require.NoError(t, store.AppendEvent(acf.KindConversation, acf.Event{
		EventID:          acf.NewID(),
		ArtifactID:       artifactID,
		Type:             acf.EventTypeForkOuter,
		Timestamp:        time.Date(2026, 7, 6, 10, 1, 0, 0, time.UTC),
		ParentHash:       mainHead.Hash,
		Branch:           "review",
		ForkSourceBranch: acf.MainBranch,
		ForkFromEventID:  mainHead.EventID,
		Provenance:       acf.Provenance{SourceAgent: "aplexica-cli"},
	}))
	events, err = store.ReadEvents(acf.KindConversation, artifactID)
	require.NoError(t, err)
	forkHead := events[len(events)-1]
	appendConversationEvent(t, store, artifactID, "review", forkHead.Hash, "claude-code",
		[]acf.TextTurn{
			{Role: "user", Text: "main q"},
			{Role: "assistant", Text: "main a"},
			{Role: "user", Text: "branch q"},
			{Role: "assistant", Text: "branch a"},
		})

	src := &fakeConvSource{name: "claude-code"}
	tgt := &recordingConversationTarget{
		fakeConvSource: fakeConvSource{name: "codex"},
		dest:           filepath.Join(root, "codex-session.jsonl"),
	}
	orch, err := NewOrchestrator(Config{
		Dir:      root,
		Adapters: []adapter.Adapter{src, tgt},
		Store:    store,
	})
	require.NoError(t, err)
	defer orch.Close()

	path, materialized, err := orch.MaterializeConversationBranch(artifactID, "codex", "review")
	require.NoError(t, err)
	require.True(t, materialized)
	require.Equal(t, tgt.dest, path)
	branch, turns := tgt.snapshot()
	require.Equal(t, "review", branch)
	require.Equal(t, []acf.TextTurn{
		{Role: "user", Text: "main q"},
		{Role: "assistant", Text: "main a"},
		{Role: "user", Text: "branch q"},
		{Role: "assistant", Text: "branch a"},
	}, turns)

	art, err := store.ReadArtifact(acf.KindConversation, artifactID)
	require.NoError(t, err)
	require.Equal(t, "review", art.MaterializedBranchByAgent["codex"])
	require.Contains(t, art.SyncedAgents, "codex")
}

func appendConversationEvent(
	t *testing.T,
	store *acf.Store,
	artifactID string,
	branch string,
	parentHash string,
	sourceAgent string,
	turns []acf.TextTurn,
) {
	t.Helper()
	events := make([]acf.ConversationEvent, 0, len(turns))
	for i, turn := range turns {
		events = append(events, acf.ConversationEvent{
			Type:      acf.EventTypeTurn,
			Timestamp: time.Date(2026, 7, 6, 10, 0, i, 0, time.UTC),
			Role:      turn.Role,
			Content:   []acf.ContentBlock{{Type: "text", Text: turn.Text}},
		})
	}
	payload, err := acf.EncodePayload(acf.ConversationPayload{
		Format: acf.ConversationFormatV1,
		Events: events,
	})
	require.NoError(t, err)
	if sourceAgent == "" {
		sourceAgent = "claude-code"
	}
	require.NoError(t, store.AppendEvent(acf.KindConversation, acf.Event{
		EventID:    acf.NewID(),
		ArtifactID: artifactID,
		Type:       acf.EventTypeUpdate,
		Timestamp:  time.Date(2026, 7, 6, 10, 0, len(turns), 0, time.UTC),
		ParentHash: parentHash,
		Branch:     branch,
		Provenance: acf.Provenance{SourceAgent: sourceAgent},
		Payload:    payload,
	}))
}

var _ adapter.ConversationSessionTarget = (*recordingConversationTarget)(nil)
var _ adapter.Adapter = (*recordingConversationTarget)(nil)
