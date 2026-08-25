package syncd

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter"
	"github.com/aplexica/aplexica/internal/adapter/claudecode"
	"github.com/stretchr/testify/require"
)

// The same settled path can reach handleEvent from several concurrent
// pipelines at once: the watcher debouncer settle, the native live scan
// (scanNativeRoots), and the recent-session hot scanners. Without serialization,
// a freshly created Claude Code session can be imported twice — producing two
// artifacts for one native session — and its assistant append can be encoded
// twice (two identical update events), so every synced device rendered the
// answer twice. handleEvent must serialize per path so the losing pipeline
// re-reads the scan cache/artifact head AFTER the winner committed and no-ops.

const raceSessionUserLine = `{"type":"user","uuid":"u-1","timestamp":"2026-01-02T03:04:05.000Z","sessionId":"sess-race","message":{"role":"user","content":"How many moons does Mercury have?"}}
`

// Claude Code ≥2.1.204 splits one assistant API message across two JSONL
// records sharing message.id: a thinking-only record, then the text record.
const raceSessionAssistantLines = `{"type":"assistant","uuid":"a-1","parentUuid":"u-1","timestamp":"2026-01-02T03:04:06.000Z","sessionId":"sess-race","message":{"id":"msg_01","role":"assistant","content":[{"type":"thinking","thinking":"","signature":"sig"}]}}
{"type":"assistant","uuid":"a-2","parentUuid":"a-1","timestamp":"2026-01-02T03:04:07.000Z","sessionId":"sess-race","message":{"id":"msg_01","role":"assistant","content":[{"type":"text","text":"Mercury has zero moons."}]}}
`

func conversationArtifactsForSource(t *testing.T, store *acf.Store, src string) []acf.Artifact {
	t.Helper()
	all, err := store.ListArtifacts(acf.KindConversation)
	require.NoError(t, err)
	var out []acf.Artifact
	for _, a := range all {
		if a.SourcePath == src {
			out = append(out, a)
		}
	}
	return out
}

func stormHandleEvent(t *testing.T, orch *Orchestrator, path string, n int) {
	t.Helper()
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			orch.handleEvent(path)
		}()
	}
	close(start)
	wg.Wait()
}

func TestHandleEvent_ConcurrentSamePathImportsOnce(t *testing.T) {
	tmp := realTempDir(t)
	store := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, store.Init())

	cc := claudecode.New()
	cc.HomeDir = tmp
	cc.CanonicalConversations = true

	orch, err := NewOrchestrator(Config{
		Dir:         tmp,
		Adapters:    []adapter.Adapter{cc},
		Store:       store,
		QuietPeriod: 10 * time.Millisecond,
		GuardWindow: time.Second,
	})
	require.NoError(t, err)
	defer orch.Close()

	sess := filepath.Join(tmp, ".claude", "projects", "-proj", "sess-race.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(sess), 0o755))
	require.NoError(t, os.WriteFile(sess, []byte(raceSessionUserLine), 0o644))

	// Wave 1: a brand-new session file processed by N pipelines at once must
	// create exactly ONE artifact.
	stormHandleEvent(t, orch, sess, 8)
	arts := conversationArtifactsForSource(t, store, sess)
	require.Len(t, arts, 1,
		"concurrent first-import of one native session must create exactly one artifact")

	// Wave 2: the assistant answer is appended and the grown file is again
	// processed concurrently. The thread must gain the assistant turn ONCE.
	require.NoError(t, os.WriteFile(sess, []byte(raceSessionUserLine+raceSessionAssistantLines), 0o644))
	stormHandleEvent(t, orch, sess, 8)

	arts = conversationArtifactsForSource(t, store, sess)
	require.Len(t, arts, 1, "the append must not mint a second artifact")

	events, err := store.ReadEvents(acf.KindConversation, arts[0].ArtifactID)
	require.NoError(t, err)
	payload, ok, err := acf.MaterializedConversationPayload(events)
	require.NoError(t, err)
	require.True(t, ok)

	var assistantTexts []string
	for _, ev := range payload.Events {
		if ev.Type != acf.EventTypeTurn || ev.Role != "assistant" {
			continue
		}
		for _, b := range ev.Content {
			if b.Type == "text" && b.Text != "" {
				assistantTexts = append(assistantTexts, b.Text)
			}
		}
	}
	require.Equal(t, []string{"Mercury has zero moons."}, assistantTexts,
		"the assistant answer must appear exactly once in the canonical thread")
}
