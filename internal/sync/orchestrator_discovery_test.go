package syncd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter"
	claudeadapter "github.com/aplexica/aplexica/internal/adapter/claudecode"
	codexadapter "github.com/aplexica/aplexica/internal/adapter/codex"
	"github.com/stretchr/testify/require"
)

func mustConversationEvents(t *testing.T, store *acf.Store, artifactID string) []acf.Event {
	t.Helper()
	events, err := store.ReadEvents(acf.KindConversation, artifactID)
	require.NoError(t, err)
	return events
}

// TestAdditionalRoots_ImportsFromNativeLocation is the Slice 2 regression
// test for FR-03.3 §4 ("the daemon watches the global paths permanently"):
// a memory file created under a discovered native root (AdditionalRoots),
// NOT the primary watched Dir, must still be imported into the canonical
// store via the same debounce + handleEvent pipeline.
func TestAdditionalRoots_ImportsFromNativeLocation(t *testing.T) {
	root := realTempDir(t)
	watched := filepath.Join(root, "proj")
	require.NoError(t, os.MkdirAll(watched, 0o755))
	native := filepath.Join(root, "native") // stands in for a discovered global root (e.g. ~/.claude)
	require.NoError(t, os.MkdirAll(native, 0o755))

	adapters, store, _ := buildAllThreeAdapters(t, root)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	orch, err := NewOrchestrator(Config{
		Dir:             watched,
		AdditionalRoots: []string{native},
		Adapters:        adapters,
		Store:           store,
		QuietPeriod:     100 * time.Millisecond,
		GuardWindow:     2 * time.Second,
	})
	require.NoError(t, err)
	defer orch.Close()

	go orch.Run(ctx)
	time.Sleep(150 * time.Millisecond)

	// Write a memory file into the ADDITIONAL (native) root — not the
	// primary watched dir. claudecode recognizes CLAUDE.md and imports it.
	require.NoError(t, os.WriteFile(filepath.Join(native, "CLAUDE.md"),
		[]byte("# from native home\n"), 0o644))

	require.Eventually(t, func() bool {
		mems, lerr := store.ListArtifacts(acf.KindMemory)
		return lerr == nil && len(mems) >= 1
	}, 3*time.Second, 100*time.Millisecond,
		"a file created under AdditionalRoots must be imported into the canonical store")
}

// TestInitialScan_WalksAdditionalRoots verifies the startup catch-up scan
// covers AdditionalRoots too: a file already present in a native root before
// the daemon starts is imported by InitialScan, not just by live events.
func TestInitialScan_WalksAdditionalRoots(t *testing.T) {
	root := realTempDir(t)
	watched := filepath.Join(root, "proj")
	require.NoError(t, os.MkdirAll(watched, 0o755))
	native := filepath.Join(root, "native")
	require.NoError(t, os.MkdirAll(native, 0o755))
	// Pre-existing file BEFORE the orchestrator starts.
	require.NoError(t, os.WriteFile(filepath.Join(native, "CLAUDE.md"),
		[]byte("# pre-existing native memory\n"), 0o644))

	adapters, store, _ := buildAllThreeAdapters(t, root)

	orch, err := NewOrchestrator(Config{
		Dir:             watched,
		AdditionalRoots: []string{native},
		Adapters:        adapters,
		Store:           store,
		QuietPeriod:     100 * time.Millisecond,
		GuardWindow:     2 * time.Second,
	})
	require.NoError(t, err)
	defer orch.Close()

	require.NoError(t, orch.InitialScan(context.Background()))

	mems, err := store.ListArtifacts(acf.KindMemory)
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(mems), 1,
		"InitialScan must import pre-existing files under AdditionalRoots")
}

// TestInitialScan_WalksMetadataRoots is the Desktop-title regression guard.
// Claude Desktop can write its catalog record after the transcript's final
// append, so startup must treat that read-only app metadata as an import
// trigger and resolve it back to the real CLI transcript.
func TestInitialScan_WalksMetadataRoots(t *testing.T) {
	root := realTempDir(t)
	watched := filepath.Join(root, "watched")
	require.NoError(t, os.MkdirAll(watched, 0o755))

	const cliSessionID = "a5b71172-2a33-4ff3-abb2-22c32758d73d"
	cwd := filepath.Join(root, "project", ".claude", "worktrees", "testing-greeting")
	encodedCWD := strings.NewReplacer(
		"/", "-", "\\", "-", ":", "-", ".", "-", "_", "-", " ", "-",
	).Replace(cwd)
	transcript := filepath.Join(root, ".claude", "projects", encodedCWD, cliSessionID+".jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(transcript), 0o755))
	require.NoError(t, os.WriteFile(transcript, []byte(
		fmt.Sprintf(`{"type":"user","sessionId":%q,"cwd":%q,"message":{"role":"user","content":"Just Testing, say Hi!"}}`, cliSessionID, cwd)+"\n",
	), 0o600))

	catalog := filepath.Join(root, "claude-desktop-catalog")
	record := filepath.Join(catalog, "project", "local_testing-greeting.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(record), 0o755))
	require.NoError(t, os.WriteFile(record, []byte(
		fmt.Sprintf(`{"sessionId":"local_testing-greeting","cliSessionId":%q,"cwd":%q,"title":"Testing greeting"}`, cliSessionID, cwd),
	), 0o600))

	store := &acf.Store{Root: filepath.Join(root, "store")}
	require.NoError(t, store.Init())
	claude := claudeadapter.New()
	claude.HomeDir = root
	claude.DesktopSessionRoots = []string{catalog}

	orch, err := NewOrchestrator(Config{
		Dir:           watched,
		MetadataRoots: []string{catalog},
		RootsByAdapter: map[string][]string{
			claude.Name(): {catalog},
		},
		Adapters: []adapter.Adapter{claude},
		Store:    store,
	})
	require.NoError(t, err)
	defer orch.Close()

	require.NoError(t, orch.InitialScan(context.Background()))
	conversations, err := store.ListArtifacts(acf.KindConversation)
	require.NoError(t, err)
	require.Len(t, conversations, 1)
	require.Equal(t, "Testing greeting", conversations[0].Name)
	require.Equal(t, transcript, conversations[0].SourcePath,
		"Desktop catalog metadata must only trigger import of the native transcript")
}

// TestLiveScan_WalksRecursiveRoots verifies the daemon's runtime catch-up scan
// covers recursive native roots too. It intentionally starts only the scan loop,
// not orch.Run, so the assertion does not depend on OS watcher delivery.
func TestLiveScan_WalksRecursiveRoots(t *testing.T) {
	root := realTempDir(t)
	watched := filepath.Join(root, "proj")
	require.NoError(t, os.MkdirAll(watched, 0o755))
	projectsRoot := filepath.Join(root, ".claude", "projects")
	sessionDir := filepath.Join(projectsRoot, "-tmp-proj")
	require.NoError(t, os.MkdirAll(sessionDir, 0o755))

	adapters, store, _ := buildAllThreeAdapters(t, root)

	orch, err := NewOrchestrator(Config{
		Dir:            watched,
		RecursiveRoots: []string{projectsRoot},
		Adapters:       adapters,
		Store:          store,
		QuietPeriod:    100 * time.Millisecond,
		GuardWindow:    2 * time.Second,
	})
	require.NoError(t, err)
	defer orch.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go orch.runLiveScan(ctx, 50*time.Millisecond)

	jsonlPath := filepath.Join(sessionDir, "session-live-scan.jsonl")
	require.NoError(t, os.WriteFile(jsonlPath,
		[]byte(`{"type":"summary","leafUuid":"abc","sessionId":"session-live-scan","summary":"live scan"}`+"\n"), 0o644))

	require.Eventually(t, func() bool {
		convos, lerr := store.ListArtifacts(acf.KindConversation)
		return lerr == nil && len(convos) == 1
	}, 3*time.Second, 50*time.Millisecond,
		"live scan must import files missed by watcher events under RecursiveRoots")
}

func TestNativeLiveScan_ImportsFlatAndRecursiveAgentRoots(t *testing.T) {
	root := realTempDir(t)
	watched := filepath.Join(root, "proj")
	require.NoError(t, os.MkdirAll(watched, 0o755))
	codexRoot := filepath.Join(root, ".codex")
	sessionsRoot := filepath.Join(codexRoot, "sessions")
	// The recent-day scanner only visits now±1 day partitions, so the
	// session dir must be derived from the current date — a hardcoded
	// date silently falls out of the scan window as the calendar moves.
	now := time.Now()
	sessionDir := filepath.Join(sessionsRoot, now.Format("2006"), now.Format("01"), now.Format("02"))
	require.NoError(t, os.MkdirAll(sessionDir, 0o755))

	adapters, store, _ := buildAllThreeAdapters(t, root)
	for _, ad := range adapters {
		if cx, ok := ad.(*codexadapter.Adapter); ok {
			cx.CanonicalConversations = true
		}
	}

	orch, err := NewOrchestrator(Config{
		Dir:             watched,
		AdditionalRoots: []string{codexRoot},
		RecursiveRoots:  []string{sessionsRoot},
		RootsByAdapter: map[string][]string{
			"codex": {codexRoot, sessionsRoot},
		},
		Adapters:    adapters,
		Store:       store,
		QuietPeriod: 50 * time.Millisecond,
		GuardWindow: 2 * time.Second,
	})
	require.NoError(t, err)
	defer orch.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go orch.RunNativeLiveScan(ctx, 50*time.Millisecond)

	require.NoError(t, os.WriteFile(filepath.Join(codexRoot, "AGENTS.md"),
		[]byte("# native flat memory\n"), 0o644))

	seed, err := os.ReadFile(filepath.Join("..", "adapter", "codex", "testdata", "session-tiny.jsonl"))
	require.NoError(t, err)
	jsonlPath := filepath.Join(sessionDir, "rollout-native-live-scan.jsonl")
	require.NoError(t, os.WriteFile(jsonlPath, seed, 0o644))

	require.Eventually(t, func() bool {
		mems, merr := store.ListArtifacts(acf.KindMemory)
		convos, cerr := store.ListArtifacts(acf.KindConversation)
		return merr == nil && cerr == nil && len(mems) == 1 && len(convos) == 1 && convos[0].SourcePath == jsonlPath
	}, 3*time.Second, 50*time.Millisecond,
		"native live scan must catch both flat native roots and nested session roots without watcher delivery")
}

func TestRecentCodexSessionDayScan_ImportsDatePartitionedRollout(t *testing.T) {
	root := realTempDir(t)
	watched := filepath.Join(root, "proj")
	require.NoError(t, os.MkdirAll(watched, 0o755))
	sessionsRoot := filepath.Join(root, ".codex", "sessions")
	sessionTime := time.Now().Add(2 * time.Second)
	sessionDir := filepath.Join(sessionsRoot, sessionTime.Format("2006"), sessionTime.Format("01"), sessionTime.Format("02"))
	require.NoError(t, os.MkdirAll(sessionDir, 0o755))

	adapters, store, _ := buildAllThreeAdapters(t, root)
	for _, ad := range adapters {
		if cx, ok := ad.(*codexadapter.Adapter); ok {
			cx.CanonicalConversations = true
		}
	}

	orch, err := NewOrchestrator(Config{
		Dir:            watched,
		RecursiveRoots: []string{sessionsRoot},
		RootsByAdapter: map[string][]string{
			"codex": {filepath.Join(root, ".codex"), sessionsRoot},
		},
		Adapters:    adapters,
		Store:       store,
		QuietPeriod: 50 * time.Millisecond,
		GuardWindow: 2 * time.Second,
	})
	require.NoError(t, err)
	defer orch.Close()

	seed, err := os.ReadFile(filepath.Join("..", "adapter", "codex", "testdata", "session-tiny.jsonl"))
	require.NoError(t, err)
	jsonlPath := filepath.Join(sessionDir, "rollout-recent-day-scan.jsonl")
	require.NoError(t, os.WriteFile(jsonlPath, seed, 0o644))

	n := orch.scanRecentCodexSessionDays(sessionsRoot, sessionTime)
	require.Equal(t, 1, n)

	convos, err := store.ListArtifacts(acf.KindConversation)
	require.NoError(t, err)
	require.Len(t, convos, 1)
	require.Equal(t, jsonlPath, convos[0].SourcePath)
}

func TestRecentClaudeSessionScan_ImportsRecentlyModifiedSession(t *testing.T) {
	root := realTempDir(t)
	watched := filepath.Join(root, "proj")
	require.NoError(t, os.MkdirAll(watched, 0o755))
	claudeRoot := filepath.Join(root, ".claude")
	projectsRoot := filepath.Join(claudeRoot, "projects")
	sessionDir := filepath.Join(projectsRoot, "-Users-exampleuser")
	require.NoError(t, os.MkdirAll(sessionDir, 0o755))

	adapters, store, _ := buildAllThreeAdapters(t, root)

	orch, err := NewOrchestrator(Config{
		Dir:            watched,
		RecursiveRoots: []string{projectsRoot},
		RootsByAdapter: map[string][]string{
			"claude-code": {claudeRoot, projectsRoot},
		},
		Adapters:                  adapters,
		Store:                     store,
		QuietPeriod:               50 * time.Millisecond,
		GuardWindow:               2 * time.Second,
		RecentClaudeSessionWindow: time.Minute,
	})
	require.NoError(t, err)
	defer orch.Close()

	seed, err := os.ReadFile(filepath.Join("..", "adapter", "claudecode", "testdata", "session-tiny.jsonl"))
	require.NoError(t, err)
	jsonlPath := filepath.Join(sessionDir, "claude-recent-session-scan.jsonl")

	orch.markClaudeHotSession(jsonlPath)
	require.Equal(t, 0, orch.ScanRecentClaudeSessions(context.Background()),
		"a predicted hot session path must survive until the materializer creates it")

	require.NoError(t, os.WriteFile(jsonlPath, seed, 0o644))
	readyAt := time.Now().Add(-time.Second)
	require.NoError(t, os.Chtimes(jsonlPath, readyAt, readyAt))

	n := orch.ScanRecentClaudeSessions(context.Background())
	require.Equal(t, 1, n)

	convos, err := store.ListArtifacts(acf.KindConversation)
	require.NoError(t, err)
	require.Len(t, convos, 1)
	require.Equal(t, jsonlPath, convos[0].SourcePath)
}

func TestRecentCodexSessionDayScan_ImportsCompleteRowsWhileFileIsActive(t *testing.T) {
	root := realTempDir(t)
	watched := filepath.Join(root, "proj")
	require.NoError(t, os.MkdirAll(watched, 0o755))
	sessionsRoot := filepath.Join(root, ".codex", "sessions")
	sessionTime := time.Now()
	sessionDir := filepath.Join(sessionsRoot, sessionTime.Format("2006"), sessionTime.Format("01"), sessionTime.Format("02"))
	require.NoError(t, os.MkdirAll(sessionDir, 0o755))

	adapters, store, _ := buildAllThreeAdapters(t, root)
	for _, ad := range adapters {
		if cx, ok := ad.(*codexadapter.Adapter); ok {
			cx.CanonicalConversations = true
		}
	}

	orch, err := NewOrchestrator(Config{
		Dir:            watched,
		RecursiveRoots: []string{sessionsRoot},
		RootsByAdapter: map[string][]string{
			"codex": {filepath.Join(root, ".codex"), sessionsRoot},
		},
		Adapters:    adapters,
		Store:       store,
		QuietPeriod: time.Second,
		GuardWindow: 2 * time.Second,
	})
	require.NoError(t, err)
	defer orch.Close()

	seed, err := os.ReadFile(filepath.Join("..", "adapter", "codex", "testdata", "session-tiny.jsonl"))
	require.NoError(t, err)
	jsonlPath := filepath.Join(sessionDir, "rollout-recent-day-quiet.jsonl")
	require.NoError(t, os.WriteFile(jsonlPath, seed, 0o644))
	require.NoError(t, os.Chtimes(jsonlPath, sessionTime, sessionTime))

	require.Equal(t, 1, orch.scanRecentCodexSessionDays(sessionsRoot, sessionTime),
		"the hot scanner must import complete JSON rows without waiting for file-wide quiet")
	require.True(t, orch.scanCache.unchanged(jsonlPath))

	convos, err := store.ListArtifacts(acf.KindConversation)
	require.NoError(t, err)
	require.Len(t, convos, 1)
	require.Equal(t, jsonlPath, convos[0].SourcePath)
}

func TestRecentCodexSessionDayScan_StreamsPromptThenAssistantAnswer(t *testing.T) {
	root := realTempDir(t)
	watched := filepath.Join(root, "proj")
	require.NoError(t, os.MkdirAll(watched, 0o755))
	sessionsRoot := filepath.Join(root, ".codex", "sessions")
	sessionTime := time.Now().Add(-2 * time.Second)
	sessionDir := filepath.Join(sessionsRoot, sessionTime.Format("2006"), sessionTime.Format("01"), sessionTime.Format("02"))
	require.NoError(t, os.MkdirAll(sessionDir, 0o755))

	adapters, store, _ := buildAllThreeAdapters(t, root)
	for _, ad := range adapters {
		if cx, ok := ad.(*codexadapter.Adapter); ok {
			cx.CanonicalConversations = true
		}
	}

	orch, err := NewOrchestrator(Config{
		Dir:            watched,
		RecursiveRoots: []string{sessionsRoot},
		RootsByAdapter: map[string][]string{
			"codex": {filepath.Join(root, ".codex"), sessionsRoot},
		},
		Adapters:    adapters,
		Store:       store,
		QuietPeriod: 50 * time.Millisecond,
		GuardWindow: 2 * time.Second,
	})
	require.NoError(t, err)
	defer orch.Close()

	jsonlPath := filepath.Join(sessionDir, "rollout-recent-day-partial.jsonl")
	partial := strings.Join([]string{
		`{"timestamp":"2026-06-30T00:00:00Z","type":"session_meta","payload":{"id":"partial"}}`,
		`{"timestamp":"2026-06-30T00:00:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}}`,
		"",
	}, "\n")
	require.NoError(t, os.WriteFile(jsonlPath, []byte(partial), 0o644))
	require.NoError(t, os.Chtimes(jsonlPath, sessionTime, sessionTime))

	require.Equal(t, 1, orch.scanRecentCodexSessionDays(sessionsRoot, time.Now()),
		"a complete user row must synchronize before the assistant answer")
	require.True(t, orch.scanCache.unchanged(jsonlPath))
	convos, err := store.ListArtifacts(acf.KindConversation)
	require.NoError(t, err)
	require.Len(t, convos, 1)
	first, ok, err := acf.MaterializedConversationPayload(mustConversationEvents(t, store, convos[0].ArtifactID))
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, []acf.TextTurn{{Role: "user", Text: "hello"}}, acf.ExtractTextTurns(first.Events))

	complete := partial + `{"timestamp":"2026-06-30T00:00:02Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}]}}` + "\n"
	require.NoError(t, os.WriteFile(jsonlPath, []byte(complete), 0o644))
	require.NoError(t, os.Chtimes(jsonlPath, sessionTime.Add(time.Second), sessionTime.Add(time.Second)))

	require.Equal(t, 1, orch.scanRecentCodexSessionDays(sessionsRoot, time.Now()))
	require.Equal(t, jsonlPath, convos[0].SourcePath)
	second, ok, err := acf.MaterializedConversationPayload(mustConversationEvents(t, store, convos[0].ArtifactID))
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, []acf.TextTurn{{Role: "user", Text: "hello"}, {Role: "assistant", Text: "hi"}},
		acf.ExtractTextTurns(second.Events))
}

func TestRecentCodexSessionDayScan_DropsCommentaryWhileStreamingLatestPrompt(t *testing.T) {
	root := realTempDir(t)
	sessionsRoot := filepath.Join(root, ".codex", "sessions")
	now := time.Now()
	sessionDir := filepath.Join(sessionsRoot, now.Format("2006"), now.Format("01"), now.Format("02"))
	require.NoError(t, os.MkdirAll(sessionDir, 0o755))
	path := filepath.Join(sessionDir, "rollout-pending-followup.jsonl")
	partial := strings.Join([]string{
		`{"timestamp":"2026-07-18T20:10:54Z","type":"session_meta","payload":{"id":"thread"}}`,
		`{"timestamp":"2026-07-18T20:10:55Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"first question"}]}}`,
		`{"timestamp":"2026-07-18T20:10:56Z","type":"response_item","payload":{"type":"message","role":"assistant","phase":"final_answer","content":[{"type":"output_text","text":"first answer"}]}}`,
		`{"timestamp":"2026-07-18T20:11:59Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"second question"}]}}`,
		`{"timestamp":"2026-07-18T20:12:02Z","type":"response_item","payload":{"type":"message","role":"assistant","phase":"commentary","content":[{"type":"output_text","text":"still working"}]}}`,
	}, "\n") + "\n"
	require.NoError(t, os.WriteFile(path, []byte(partial), 0o644))
	stable := now.Add(-time.Second)
	require.NoError(t, os.Chtimes(path, stable, stable))

	adapters, store, _ := buildAllThreeAdapters(t, root)
	for _, ad := range adapters {
		if cx, ok := ad.(*codexadapter.Adapter); ok {
			cx.CanonicalConversations = true
		}
	}
	orch, err := NewOrchestrator(Config{
		Dir: root, RecursiveRoots: []string{sessionsRoot},
		RootsByAdapter: map[string][]string{"codex": {filepath.Join(root, ".codex"), sessionsRoot}},
		Adapters:       adapters, Store: store, QuietPeriod: 10 * time.Millisecond, GuardWindow: time.Second,
	})
	require.NoError(t, err)
	defer orch.Close()

	require.Equal(t, 1, orch.scanRecentCodexSessionDays(sessionsRoot, now))
	conversations, err := store.ListArtifacts(acf.KindConversation)
	require.NoError(t, err)
	require.Len(t, conversations, 1)
	pendingPayload, ok, err := acf.MaterializedConversationPayload(
		mustConversationEvents(t, store, conversations[0].ArtifactID),
	)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, []acf.TextTurn{
		{Role: "user", Text: "first question"},
		{Role: "assistant", Text: "first answer"},
		{Role: "user", Text: "second question"},
	}, acf.ExtractTextTurns(pendingPayload.Events), "commentary must not become an assistant answer")

	complete := partial + `{"timestamp":"2026-07-18T20:12:06Z","type":"response_item","payload":{"type":"message","role":"assistant","phase":"final_answer","content":[{"type":"output_text","text":"second answer"}]}}` + "\n"
	require.NoError(t, os.WriteFile(path, []byte(complete), 0o644))
	require.NoError(t, os.Chtimes(path, stable.Add(time.Millisecond), stable.Add(time.Millisecond)))
	require.Equal(t, 1, orch.scanRecentCodexSessionDays(sessionsRoot, now))
}

func TestRecentCodexSessionDayScan_ImportsChangedRolloutAboveReadinessPreparseBound(t *testing.T) {
	root := realTempDir(t)
	watched := filepath.Join(root, "proj")
	require.NoError(t, os.MkdirAll(watched, 0o755))
	sessionsRoot := filepath.Join(root, ".codex", "sessions")
	sessionTime := time.Now().Add(-2 * time.Second)
	sessionDir := filepath.Join(sessionsRoot, sessionTime.Format("2006"), sessionTime.Format("01"), sessionTime.Format("02"))
	require.NoError(t, os.MkdirAll(sessionDir, 0o755))

	adapters, store, _ := buildAllThreeAdapters(t, root)
	for _, ad := range adapters {
		if cx, ok := ad.(*codexadapter.Adapter); ok {
			cx.CanonicalConversations = true
		}
	}

	orch, err := NewOrchestrator(Config{
		Dir:            watched,
		RecursiveRoots: []string{sessionsRoot},
		RootsByAdapter: map[string][]string{
			"codex": {filepath.Join(root, ".codex"), sessionsRoot},
		},
		Adapters:    adapters,
		Store:       store,
		QuietPeriod: 50 * time.Millisecond,
		GuardWindow: 2 * time.Second,
	})
	require.NoError(t, err)
	defer orch.Close()

	hugePath := filepath.Join(sessionDir, "rollout-large-live-worklog.jsonl")
	var large strings.Builder
	large.WriteString(`{"timestamp":"2026-07-18T20:10:54Z","type":"session_meta","payload":{"id":"large-native"}}` + "\n")
	large.WriteString(`{"timestamp":"2026-07-18T20:10:55Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"large session question"}]}}` + "\n")
	padding := `{"timestamp":"2026-07-18T20:10:56Z","type":"ignored","payload":{}}` + "\n"
	for large.Len() <= 2*1024*1024 {
		large.WriteString(padding)
	}
	require.NoError(t, os.WriteFile(hugePath, []byte(large.String()), 0o644))
	require.NoError(t, os.Chtimes(hugePath, sessionTime, sessionTime))

	require.Equal(t, 1, orch.scanRecentCodexSessionDays(sessionsRoot, time.Now()))
	convos, err := store.ListArtifacts(acf.KindConversation)
	require.NoError(t, err)
	require.Len(t, convos, 1)
	payload, ok, err := acf.MaterializedConversationPayload(mustConversationEvents(t, store, convos[0].ArtifactID))
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, []acf.TextTurn{{Role: "user", Text: "large session question"}}, acf.ExtractTextTurns(payload.Events))
}

// TestReopenWatchersBeforeRun_KeepsRecursiveRootEvents covers the daemon boot
// path: watchers are constructed, InitialScan may run for a while, then the
// daemon reopens sources immediately before Run. A file created after that
// reopen under a recursive native root must still flow through the watcher.
func TestReopenWatchersBeforeRun_KeepsRecursiveRootEvents(t *testing.T) {
	root := realTempDir(t)
	watched := filepath.Join(root, "proj")
	require.NoError(t, os.MkdirAll(watched, 0o755))
	projectsRoot := filepath.Join(root, ".claude", "projects")
	sessionDir := filepath.Join(projectsRoot, "-tmp-proj")
	require.NoError(t, os.MkdirAll(sessionDir, 0o755))

	adapters, store, _ := buildAllThreeAdapters(t, root)

	orch, err := NewOrchestrator(Config{
		Dir:            watched,
		RecursiveRoots: []string{projectsRoot},
		Adapters:       adapters,
		Store:          store,
		QuietPeriod:    50 * time.Millisecond,
		GuardWindow:    2 * time.Second,
	})
	require.NoError(t, err)
	defer orch.Close()
	require.NoError(t, orch.ReopenWatchersBeforeRun())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go orch.Run(ctx)

	jsonlPath := filepath.Join(sessionDir, "session-reopened-watch.jsonl")
	require.NoError(t, os.WriteFile(jsonlPath,
		[]byte(`{"type":"summary","leafUuid":"abc","sessionId":"session-reopened-watch","summary":"reopened watcher"}`+"\n"), 0o644))

	require.Eventually(t, func() bool {
		convos, lerr := store.ListArtifacts(acf.KindConversation)
		return lerr == nil && len(convos) == 1
	}, 5*time.Second, 50*time.Millisecond,
		"reopened watchers must keep recursive native roots active")
}

func TestRun_ImportsCodexSessionCreatedUnderRecursiveRoot(t *testing.T) {
	root := realTempDir(t)
	watched := filepath.Join(root, "proj")
	require.NoError(t, os.MkdirAll(watched, 0o755))
	sessionsRoot := filepath.Join(root, ".codex", "sessions")
	// Current-date partition: keeps the recent-day scan fallback live (the
	// scanner only visits now±1 day dirs), so the test cannot rot as the
	// calendar moves past a hardcoded date.
	now := time.Now()
	sessionDir := filepath.Join(sessionsRoot, now.Format("2006"), now.Format("01"), now.Format("02"))
	require.NoError(t, os.MkdirAll(sessionDir, 0o755))

	adapters, store, _ := buildAllThreeAdapters(t, root)
	for _, ad := range adapters {
		if cx, ok := ad.(*codexadapter.Adapter); ok {
			cx.CanonicalConversations = true
		}
	}

	orch, err := NewOrchestrator(Config{
		Dir:            watched,
		RecursiveRoots: []string{sessionsRoot},
		RootsByAdapter: map[string][]string{
			"codex": {filepath.Join(root, ".codex"), sessionsRoot},
		},
		Adapters:    adapters,
		Store:       store,
		QuietPeriod: 50 * time.Millisecond,
		GuardWindow: 2 * time.Second,
	})
	require.NoError(t, err)
	defer orch.Close()
	require.NoError(t, orch.ReopenWatchersBeforeRun())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go orch.Run(ctx)

	seed, err := os.ReadFile(filepath.Join("..", "adapter", "codex", "testdata", "session-tiny.jsonl"))
	require.NoError(t, err)
	jsonlPath := filepath.Join(sessionDir, "rollout-live-codex.jsonl")
	require.NoError(t, os.WriteFile(jsonlPath, seed, 0o644))

	require.Eventually(t, func() bool {
		convos, lerr := store.ListArtifacts(acf.KindConversation)
		return lerr == nil && len(convos) == 1 && convos[0].SourcePath == jsonlPath
	}, 5*time.Second, 50*time.Millisecond,
		"a Codex rollout created after Run starts under ~/.codex/sessions must import live")
}

// Long agentic sessions routinely exceed 8MB; they must stay on the 500ms hot
// path (the incremental encode cache makes their appends cheap to import).
func TestRecentClaudeSessionCandidates_IncludesLargeSessions(t *testing.T) {
	root := realTempDir(t)
	adapters, store, _ := buildAllThreeAdapters(t, root)
	orch, err := NewOrchestrator(Config{
		Dir:                       root,
		Adapters:                  adapters,
		Store:                     store,
		RecentClaudeSessionWindow: 15 * time.Minute,
	})
	require.NoError(t, err)
	defer orch.Close()

	dir := filepath.Join(root, ".claude", "projects", "-Users-x")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	p := filepath.Join(dir, "big.jsonl")
	require.NoError(t, os.WriteFile(p, make([]byte, 9<<20), 0o644)) // 9MB
	old := time.Now().Add(-2 * time.Second)
	require.NoError(t, os.Chtimes(p, old, old))

	orch.markClaudeHotSession(p)
	orch.recentClaudeScanMu.Lock()
	files := orch.recentClaudeSessionCandidatesLocked(time.Now())
	orch.recentClaudeScanMu.Unlock()
	require.Len(t, files, 1, "a 9MB hot session must remain a scan candidate")
}
