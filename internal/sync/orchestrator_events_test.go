package syncd

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter"
	"github.com/aplexica/aplexica/internal/adapter/claudecode"
	"github.com/aplexica/aplexica/internal/secrets"
	"github.com/stretchr/testify/require"
)

type capturedEvent struct {
	kind string
	body map[string]any
}

type capturingPublisher struct {
	mu     sync.Mutex
	events []capturedEvent
}

func (p *capturingPublisher) Publish(kind string, body any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	m, _ := body.(map[string]any)
	p.events = append(p.events, capturedEvent{kind: kind, body: m})
}

func (p *capturingPublisher) byKind(k string) []capturedEvent {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []capturedEvent
	for _, e := range p.events {
		if e.kind == k {
			out = append(out, e)
		}
	}
	return out
}

// Importing an artifact must publish a meaningful `artifact.synced` live event
// carrying the agent + artifact name (so the Events feed reads "<agent> synced
// <name>"), NOT just the generic `agent.activity` ping that says nothing.
func TestOrchestrator_ImportPublishesArtifactSynced(t *testing.T) {
	tmp := realTempDir(t)
	store := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, store.Init())
	ss := &secrets.Store{Root: filepath.Join(tmp, "sec")}
	require.NoError(t, ss.Init())
	cc := claudecode.New()
	cc.HomeDir = tmp
	cc.SecretsStore = ss

	watchDir := filepath.Join(tmp, "proj")
	require.NoError(t, os.MkdirAll(watchDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(watchDir, "CLAUDE.md"), []byte("memory body"), 0o644))

	pub := &capturingPublisher{}
	orch, err := NewOrchestrator(Config{
		Dir:            watchDir,
		Adapters:       []adapter.Adapter{cc},
		Store:          store,
		EventPublisher: pub,
		QuietPeriod:    50 * time.Millisecond,
		GuardWindow:    time.Second,
	})
	require.NoError(t, err)
	defer orch.Close()

	require.NoError(t, orch.InitialScan(context.Background()))

	synced := pub.byKind("artifact.synced")
	require.NotEmpty(t, synced, "an import must publish an artifact.synced live event")
	got := synced[0]
	require.Equal(t, cc.Name(), got.body["source"], "artifact.synced must name the importing agent")
	require.Equal(t, "CLAUDE.md", got.body["name"], "artifact.synced must carry the artifact name")
	require.NotEmpty(t, got.body["artifactId"], "artifact.synced must carry the artifact id")
	require.Equal(t, "memory", got.body["kind"], "artifact.synced must carry the artifact kind")
	require.Equal(t, "synced", got.body["action"], "artifact.synced must carry a user-facing action key")
	require.Equal(t, redactPaths(filepath.Join(watchDir, "CLAUDE.md")), got.body["sourcePath"],
		"artifact.synced must carry the source path metadata")

	// The generic, label-less agent.activity ping should no longer flood the
	// feed — artifact.synced replaces it.
	require.Empty(t, pub.byKind("agent.activity"),
		"the generic agent.activity ping should be gone, replaced by artifact.synced")
}
