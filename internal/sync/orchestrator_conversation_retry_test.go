package syncd

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter"
	"github.com/aplexica/aplexica/internal/adapter/claudecode"
	"github.com/stretchr/testify/require"
)

func writeNativeClaudeConversation(t *testing.T, path, sessionID string, turns []acf.TextTurn) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	parent := ""
	rows := make([]map[string]any, 0, len(turns))
	for i, turn := range turns {
		uuid := "native-turn-" + string(rune('a'+i))
		row := map[string]any{
			"type":       turn.Role,
			"uuid":       uuid,
			"parentUuid": nil,
			"sessionId":  sessionID,
			"timestamp":  time.Date(2026, 7, 23, 13, 59, i, 0, time.UTC).Format(time.RFC3339Nano),
		}
		if parent != "" {
			row["parentUuid"] = parent
		}
		if turn.Role == "assistant" {
			row["message"] = map[string]any{
				"role":    "assistant",
				"model":   "claude-opus-4-8",
				"content": []any{map[string]any{"type": "text", "text": turn.Text}},
			}
		} else {
			row["message"] = map[string]any{"role": "user", "content": turn.Text}
		}
		rows = append(rows, row)
		parent = uuid
	}
	var raw []byte
	for _, row := range rows {
		encoded, err := json.Marshal(row)
		require.NoError(t, err)
		raw = append(raw, encoded...)
		raw = append(raw, '\n')
	}
	require.NoError(t, os.WriteFile(path, raw, 0o644))
}

func readClaudeTurns(t *testing.T, path string) []acf.TextTurn {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	turns, err := claudecode.ResumableTextTurns(raw)
	require.NoError(t, err)
	return turns
}

func TestConversationSessionMaterialization_RetriesAfterNoopClaudeImport(t *testing.T) {
	root := realTempDir(t)
	store := &acf.Store{Root: filepath.Join(root, "store")}
	require.NoError(t, store.Init())

	claude := claudecode.New()
	claude.HomeDir = root
	claude.CanonicalConversations = true
	codexSource := &fakeConvSource{name: "codex"}
	claudeProjects := filepath.Join(root, ".claude", "projects")
	orch, err := NewOrchestrator(Config{
		Dir:      root,
		Adapters: []adapter.Adapter{claude, codexSource},
		Store:    store,
		RootsByAdapter: map[string][]string{
			"claude-code": {claudeProjects},
		},
	})
	require.NoError(t, err)
	defer orch.Close()

	sessionID := "cdb0165b-12cd-4f34-9511-5934d9204b0a"
	source := filepath.Join(claudeProjects, "-Users-exampleuser", sessionID+".jsonl")
	first := []acf.TextTurn{
		{Role: "user", Text: "What is the capital of Canada?"},
		{Role: "assistant", Text: "Ottawa."},
	}
	writeNativeClaudeConversation(t, source, sessionID, first)
	require.True(t, orch.handleEvent(source))

	artifacts := conversationArtifactsForSource(t, store, source)
	require.Len(t, artifacts, 1)
	artifactID := artifacts[0].ArtifactID
	head, ok, err := store.LastEvent(acf.KindConversation, artifactID)
	require.NoError(t, err)
	require.True(t, ok)
	complete := append(append([]acf.TextTurn(nil), first...),
		acf.TextTurn{Role: "user", Text: "How many people live in Ottawa?"},
		acf.TextTurn{Role: "assistant", Text: "About one million people."},
	)
	appendConversationEvent(t, store, artifactID, acf.MainBranch, head.Hash, "codex", complete)

	// Claude writes a harmless bookkeeping row after Aplexica's imported
	// fingerprint. The next Codex fan-out must preserve it and queue the
	// canonical suffix rather than silently dropping the update.
	bookkeeping, err := json.Marshal(map[string]any{
		"type": "queue-operation", "sessionId": sessionID, "operation": "noop",
	})
	require.NoError(t, err)
	file, err := os.OpenFile(source, os.O_WRONLY|os.O_APPEND, 0)
	require.NoError(t, err)
	_, err = file.Write(append(bookkeeping, '\n'))
	require.NoError(t, err)
	require.NoError(t, file.Close())

	orch.fanOut(context.Background(), codexSource, []string{artifactID}, root,
		filepath.Join(root, ".codex", "sessions", "rollout.jsonl"), false, nil)
	require.Equal(t, first, readClaudeTurns(t, source),
		"the changed native file must remain untouched until its no-op import lands")

	queues, err := loadDeferredMaterializationQueues(store.Root)
	require.NoError(t, err)
	require.Contains(t, queues, "claude-code")
	require.Contains(t, queues["claude-code"].entries, artifactID,
		"the skipped canonical head must be durable before fan-out returns")
	journal, err := os.ReadFile(filepath.Join(store.Root, deferredMaterializationDirtyName))
	require.NoError(t, err)
	require.NotContains(t, string(journal), "How many people live in Ottawa?",
		"the retry journal must contain routing metadata only, never conversation text")
	require.Contains(t, string(journal), `"originAgent":"codex"`)

	// This import commits no new canonical event: the appended row has no
	// visible turn. It must still refresh the destination fingerprint and wake
	// the pending projection.
	require.True(t, orch.handleEvent(source))
	require.Eventually(t, func() bool {
		return acf.TextTurnsEqual(readClaudeTurns(t, source), complete)
	}, 3*time.Second, 20*time.Millisecond)

	entries, err := os.ReadDir(filepath.Dir(source))
	require.NoError(t, err)
	require.Len(t, entries, 1, "retry must extend the original Claude session, never create a continuation")
	require.Eventually(t, func() bool {
		loaded, loadErr := loadDeferredMaterializationQueues(store.Root)
		return loadErr == nil && len(loaded) == 0
	}, 3*time.Second, 20*time.Millisecond)
}

type declineConversationTarget struct {
	fakeConvSource
	dest string

	mu       sync.Mutex
	attempts map[string]int
	decline  map[string]bool
}

func (d *declineConversationTarget) MaterializeConversationSession(
	art acf.Artifact,
	_ acf.Event,
	_ string,
) (string, bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.attempts[art.ArtifactID]++
	if d.decline[art.ArtifactID] {
		return filepath.Join(d.dest, art.ArtifactID+".jsonl"), false, nil
	}
	return filepath.Join(d.dest, art.ArtifactID+".jsonl"), true, nil
}

func (d *declineConversationTarget) count(artifactID string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.attempts[artifactID]
}

func TestDeferredConversationMaterialization_DoesNotLetOneDeclineStarveAnother(t *testing.T) {
	root := realTempDir(t)
	store := &acf.Store{Root: filepath.Join(root, "store")}
	require.NoError(t, store.Init())
	source := &fakeConvSource{name: "codex"}
	target := &declineConversationTarget{
		fakeConvSource: fakeConvSource{name: "claude-code"},
		dest:           filepath.Join(root, "native"),
		attempts:       map[string]int{},
		decline:        map[string]bool{},
	}
	seedConversations(t, store, "codex", 2)
	conversations, err := store.ListArtifacts(acf.KindConversation)
	require.NoError(t, err)
	require.Len(t, conversations, 2)
	blockedID := conversations[0].ArtifactID
	healthyID := conversations[1].ArtifactID
	target.decline[blockedID] = true

	orch, err := NewOrchestrator(Config{
		Dir: root, Adapters: []adapter.Adapter{source, target}, Store: store,
	})
	require.NoError(t, err)
	defer orch.Close()

	orch.deferMaterialization("claude-code", blockedID, "codex", false, false, true)
	orch.deferMaterialization("claude-code", healthyID, "codex", false, false, true)
	require.Eventually(t, func() bool {
		return target.count(healthyID) >= 1
	}, 3*time.Second, 20*time.Millisecond,
		"a permanently open conversation must not block a different pending conversation")
	require.GreaterOrEqual(t, target.count(blockedID), 1)
}

func TestProjectionRepairMigration_ExtendsRecentOriginalClaudeSession(t *testing.T) {
	root := realTempDir(t)
	store := &acf.Store{Root: filepath.Join(root, "store")}
	require.NoError(t, store.Init())
	claudeProjects := filepath.Join(root, ".claude", "projects")
	sessionID := "local-claude-session"
	sourcePath := filepath.Join(claudeProjects, "-Users-exampleuser", sessionID+".jsonl")
	first := []acf.TextTurn{{Role: "user", Text: "q1"}, {Role: "assistant", Text: "a1"}}
	complete := append(append([]acf.TextTurn(nil), first...),
		acf.TextTurn{Role: "user", Text: "q2"},
		acf.TextTurn{Role: "assistant", Text: "a2"},
	)
	writeNativeClaudeConversation(t, sourcePath, sessionID, first)

	artifactID := acf.NewID()
	now := time.Now().UTC()
	require.NoError(t, store.WriteArtifact(acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion,
		ArtifactID:       artifactID,
		Kind:             acf.KindConversation,
		Scope:            acf.ScopeGlobal,
		Name:             filepath.Base(sourcePath),
		SourcePath:       sourcePath,
		CreatedAt:        now,
		UpdatedAt:        now,
	}))
	appendConversationEvent(t, store, artifactID, acf.MainBranch, "", "claude-code", first)
	head, ok, err := store.LastEvent(acf.KindConversation, artifactID)
	require.NoError(t, err)
	require.True(t, ok)
	appendConversationEvent(t, store, artifactID, acf.MainBranch, head.Hash, "codex", complete)

	claude := claudecode.New()
	claude.HomeDir = root
	codexSource := &fakeConvSource{name: "codex"}
	orch, err := NewOrchestrator(Config{
		Dir: root, Adapters: []adapter.Adapter{claude, codexSource}, Store: store,
		RootsByAdapter: map[string][]string{"claude-code": {claudeProjects}},
	})
	require.NoError(t, err)
	defer orch.Close()

	require.Eventually(t, func() bool {
		return acf.TextTurnsEqual(readClaudeTurns(t, sourcePath), complete)
	}, 3*time.Second, 20*time.Millisecond,
		"the v2 migration must repair recent conversations stranded by the old silent decline")
	entries, err := os.ReadDir(filepath.Dir(sourcePath))
	require.NoError(t, err)
	require.Len(t, entries, 1)
}

func seedRemoteConversationProjectionFixture(
	t *testing.T,
	store *acf.Store,
	remoteDeviceID string,
) (acf.Artifact, acf.Event) {
	t.Helper()
	artifactID := acf.NewID()
	now := time.Now().UTC()
	art := acf.Artifact{
		AcfSchemaVersion:     acf.SchemaVersion,
		ArtifactID:           artifactID,
		Kind:                 acf.KindConversation,
		Scope:                acf.ScopeGlobal,
		CreatedAt:            now,
		UpdatedAt:            now,
		RemoteOriginDeviceID: remoteDeviceID,
	}
	require.NoError(t, store.WriteArtifact(art))
	appendConversationEvent(t, store, artifactID, acf.MainBranch, "", "codex", []acf.TextTurn{
		{Role: "user", Text: "remote question"},
		{Role: "assistant", Text: "remote answer"},
	})
	head, ok, err := conversationHeadForBranch(store, artifactID, acf.MainBranch)
	require.NoError(t, err)
	require.True(t, ok)
	art, err = store.ReadArtifact(acf.KindConversation, artifactID)
	require.NoError(t, err)
	return art, head
}

func TestMissingRemoteConversationProjection_RepairsOnlyAbsentSession(t *testing.T) {
	root := realTempDir(t)
	store := &acf.Store{Root: filepath.Join(root, "store")}
	require.NoError(t, store.Init())
	art, head := seedRemoteConversationProjectionFixture(t, store, "peer-device")

	claude := claudecode.New()
	claude.HomeDir = root
	dest, supported, err := claude.ConversationSessionPath(art, head, "codex")
	require.NoError(t, err)
	require.True(t, supported)
	require.NoFileExists(t, dest)

	orch, err := NewOrchestrator(Config{
		Dir:           root,
		Adapters:      []adapter.Adapter{claude},
		Store:         store,
		LocalDeviceID: "receiving-device",
	})
	require.NoError(t, err)
	defer orch.Close()

	// Direct invocation keeps this invariant covered on the non-Windows CI
	// jobs; the production startup hook itself is gated by the build-tagged
	// platform constant below.
	orch.seedMissingRemoteConversationProjections()
	require.Eventually(t, func() bool {
		_, statErr := os.Lstat(dest)
		return statErr == nil
	}, 3*time.Second, 20*time.Millisecond)
	require.Equal(t,
		[]acf.TextTurn{{Role: "user", Text: "remote question"}, {Role: "assistant", Text: "remote answer"}},
		readClaudeTurns(t, dest),
	)
}

func TestMissingRemoteConversationProjection_DoesNotTouchExistingSession(t *testing.T) {
	root := realTempDir(t)
	store := &acf.Store{Root: filepath.Join(root, "store")}
	require.NoError(t, store.Init())
	art, head := seedRemoteConversationProjectionFixture(t, store, "peer-device")

	claude := claudecode.New()
	claude.HomeDir = root
	dest, materialized, err := claude.MaterializeConversationSession(art, head, "codex")
	require.NoError(t, err)
	require.True(t, materialized)
	before, err := os.Stat(dest)
	require.NoError(t, err)
	beforeBytes, err := os.ReadFile(dest)
	require.NoError(t, err)

	orch, err := NewOrchestrator(Config{
		Dir:           root,
		Adapters:      []adapter.Adapter{claude},
		Store:         store,
		LocalDeviceID: "receiving-device",
	})
	require.NoError(t, err)
	defer orch.Close()
	orch.seedMissingRemoteConversationProjections()

	time.Sleep(100 * time.Millisecond)
	after, err := os.Stat(dest)
	require.NoError(t, err)
	afterBytes, err := os.ReadFile(dest)
	require.NoError(t, err)
	require.True(t, os.SameFile(before, after))
	require.Equal(t, before.ModTime(), after.ModTime())
	require.Equal(t, beforeBytes, afterBytes)
}

func TestMissingRemoteConversationProjection_SkipsLocallyOwnedConversation(t *testing.T) {
	root := realTempDir(t)
	store := &acf.Store{Root: filepath.Join(root, "store")}
	require.NoError(t, store.Init())
	art, head := seedRemoteConversationProjectionFixture(t, store, "")

	claude := claudecode.New()
	claude.HomeDir = root
	dest, supported, err := claude.ConversationSessionPath(art, head, "codex")
	require.NoError(t, err)
	require.True(t, supported)

	orch, err := NewOrchestrator(Config{
		Dir:           root,
		Adapters:      []adapter.Adapter{claude},
		Store:         store,
		LocalDeviceID: "receiving-device",
	})
	require.NoError(t, err)
	defer orch.Close()
	orch.seedMissingRemoteConversationProjections()

	time.Sleep(100 * time.Millisecond)
	require.NoFileExists(t, dest)
}

func TestMissingRemoteConversationProjection_DoesNotLetExistingRetriesStarveNewRepair(t *testing.T) {
	root := realTempDir(t)
	store := &acf.Store{Root: filepath.Join(root, "store")}
	require.NoError(t, store.Init())
	target, head := seedRemoteConversationProjectionFixture(t, store, "peer-device")

	claude := claudecode.New()
	claude.HomeDir = root
	dest, supported, err := claude.ConversationSessionPath(target, head, "codex")
	require.NoError(t, err)
	require.True(t, supported)
	require.NoFileExists(t, dest)

	blockedIDs := make([]string, 0, DefaultConvBackfill)
	for range DefaultConvBackfill {
		art, _ := seedRemoteConversationProjectionFixture(t, store, "peer-device")
		blockedIDs = append(blockedIDs, art.ArtifactID)
	}

	orch, err := NewOrchestrator(Config{
		Dir:      root,
		Adapters: []adapter.Adapter{claude},
		Store:    store,
		// Suppress the automatic Windows startup hook until the pre-existing
		// retry queue below is installed; the test invokes the same scan
		// directly after switching to the receiving-device identity.
		LocalDeviceID: "peer-device",
	})
	require.NoError(t, err)
	defer orch.Close()
	orch.cfg.LocalDeviceID = "receiving-device"

	// Model the live Windows state: the normal four-item backfill allowance is
	// already occupied by old entries that keep retrying. Keep the queue marked
	// as draining so this assertion observes startup seeding without racing the
	// background worker.
	queue := newDeferredMaterializationQueue()
	queue.draining = true
	for _, artifactID := range blockedIDs {
		queue.generation++
		queue.ids = append(queue.ids, artifactID)
		queue.entries[artifactID] = deferredMaterializationEntry{
			version:        queue.generation,
			includePrimary: true,
		}
	}
	orch.deferredMaterializeMu.Lock()
	orch.deferredMaterialize["claude-code"] = queue
	orch.deferredMaterializeMu.Unlock()

	orch.seedMissingRemoteConversationProjections()

	orch.deferredMaterializeMu.Lock()
	_, queued := queue.entries[target.ArtifactID]
	queue.draining = false
	orch.deferredMaterializeMu.Unlock()
	require.True(t, queued, "an existing retry queue must not consume the bounded startup scan")
}

func TestMissingRemoteConversationProjection_StartupHookIsWindowsOnly(t *testing.T) {
	require.Equal(t, runtime.GOOS == "windows", repairMissingRemoteConversationProjectionsAtStartup)
}

var _ adapter.ConversationSessionTarget = (*declineConversationTarget)(nil)
