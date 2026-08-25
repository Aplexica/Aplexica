package syncd

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter"
	"github.com/aplexica/aplexica/internal/adapter/claudecode"
	"github.com/stretchr/testify/require"
)

// forkedMirrorHarness drives the Stage-B forked-mirror repair through the REAL
// orchestrator funnel — writeConversationSession, the recursion guard, the
// dest-hash read-before-clobber gate — rather than against the adapter alone,
// because the properties under test (what the quarantine breaker is charged,
// what the canonical store holds afterwards) only exist at that level.
type forkedMirrorHarness struct {
	orch       *Orchestrator
	store      *acf.Store
	claude     *claudecode.Adapter
	tracker    *QuarantineTracker
	artifactID string
	mirror     string
	canonical  []acf.TextTurn
}

func newForkedMirrorHarness(t *testing.T, repair bool) *forkedMirrorHarness {
	t.Helper()
	root := realTempDir(t)
	store := &acf.Store{Root: filepath.Join(root, "store")}
	require.NoError(t, store.Init())

	claude := claudecode.New()
	claude.HomeDir = root
	claude.CanonicalConversations = true
	claude.RepairForkedMirrors = repair

	tracker := DefaultQuarantineTracker()
	orch, err := NewOrchestrator(Config{
		Dir:        root,
		Adapters:   []adapter.Adapter{claude},
		Store:      store,
		Quarantine: tracker,
		RootsByAdapter: map[string][]string{
			"claude-code": {filepath.Join(root, ".claude", "projects")},
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = orch.Close() })

	artifactID := acf.NewID()
	require.NoError(t, store.WriteArtifact(acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion,
		ArtifactID:       artifactID,
		Kind:             acf.KindConversation,
		Scope:            acf.ScopeGlobal,
		Name:             "rollout.jsonl",
		// A codex rollout, so the claude adapter can only ever reach its
		// deterministic synthetic mirror.
		SourcePath: filepath.Join(root, ".codex", "sessions", "rollout-2026-07-30T12-00-00-abc.jsonl"),
		CreatedAt:  time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC),
		UpdatedAt:  time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC),
	}))

	h := &forkedMirrorHarness{
		orch: orch, store: store, claude: claude, tracker: tracker, artifactID: artifactID,
	}

	first := []acf.TextTurn{
		{Role: "user", Text: "what is the capital of Canada?"},
		{Role: "assistant", Text: "Ottawa."},
	}
	appendConversationEvent(t, store, artifactID, acf.MainBranch, "", "codex", first)
	require.NoError(t, h.materialize(t))

	matches, err := filepath.Glob(filepath.Join(root, ".claude", "projects", "*", artifactID+".jsonl"))
	require.NoError(t, err)
	require.Len(t, matches, 1, "the first pass must publish exactly one synthetic mirror")
	h.mirror = matches[0]
	forkPoint := lastConversationalUUID(t, h.mirror)

	// Aplexica extends the mirror normally; these rows become the orphans.
	extended := append(append([]acf.TextTurn(nil), first...),
		acf.TextTurn{Role: "user", Text: "and of Poland?"},
		acf.TextTurn{Role: "assistant", Text: "Warsaw."},
	)
	h.appendCanonical(t, extended)
	require.NoError(t, h.materialize(t))

	// Claude Code appends its own child of the SAME node it still held as its
	// leaf, forking the graph and stranding Aplexica's two rows permanently.
	appendClaudeUserRowAt(t, h.mirror, artifactID, forkPoint, "and of Portugal?", true)

	// Pin the fixture to the physical signature it exists to reproduce: rows
	// on disk that the resume walk cannot reach. ResumableTextTurns is the
	// exported form of exactly that predicate.
	forkedRaw, err := os.ReadFile(h.mirror)
	require.NoError(t, err)
	_, err = claudecode.ResumableTextTurns(forkedRaw)
	require.Error(t, err, "the fixture must strand conversational rows off the resume leaf chain")

	h.canonical = append(append([]acf.TextTurn(nil), extended...),
		acf.TextTurn{Role: "user", Text: "and of Portugal?"},
		acf.TextTurn{Role: "assistant", Text: "Lisbon."},
	)
	h.appendCanonical(t, h.canonical)
	// The watcher import of Claude's own rows is what re-fingerprints the
	// destination; without it the orchestrator's read-before-clobber gate
	// declines every pass as a race before the adapter is ever consulted.
	orch.recordDestHash(h.mirror)
	return h
}

func (h *forkedMirrorHarness) appendCanonical(t *testing.T, turns []acf.TextTurn) {
	t.Helper()
	head, ok, err := h.store.LastEvent(acf.KindConversation, h.artifactID)
	require.NoError(t, err)
	require.True(t, ok)
	appendConversationEvent(t, h.store, h.artifactID, acf.MainBranch, head.Hash, "codex", turns)
}

func (h *forkedMirrorHarness) materialize(t *testing.T) error {
	t.Helper()
	art, err := h.store.ReadArtifact(acf.KindConversation, h.artifactID)
	require.NoError(t, err)
	head, ok, err := h.store.LastEvent(acf.KindConversation, h.artifactID)
	require.NoError(t, err)
	require.True(t, ok)
	return h.orch.writeConversationSession(convSessionPlan{
		st:          h.claude,
		name:        "claude-code",
		art:         art,
		branch:      acf.MainBranch,
		sourceAgent: "codex",
	}, head)
}

// storeFingerprint is every observable the canonical store exposes for this
// artifact. The repair must not move any of it.
func (h *forkedMirrorHarness) storeFingerprint(t *testing.T) (acf.Artifact, acf.Event, int64) {
	t.Helper()
	art, err := h.store.ReadArtifact(acf.KindConversation, h.artifactID)
	require.NoError(t, err)
	head, ok, err := h.store.LastEvent(acf.KindConversation, h.artifactID)
	require.NoError(t, err)
	require.True(t, ok)
	size, err := h.store.EventLogSize(acf.KindConversation, h.artifactID)
	require.NoError(t, err)
	return art, head, size
}

func lastConversationalUUID(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	last := ""
	for _, line := range splitJSONLines(raw) {
		var row struct {
			Type string `json:"type"`
			UUID string `json:"uuid"`
		}
		if json.Unmarshal(line, &row) == nil &&
			(row.Type == "user" || row.Type == "assistant") && row.UUID != "" {
			last = row.UUID
		}
	}
	require.NotEmpty(t, last)
	return last
}

func splitJSONLines(raw []byte) [][]byte {
	var out [][]byte
	start := 0
	for i := 0; i <= len(raw); i++ {
		if i == len(raw) || raw[i] == '\n' {
			if line := raw[start:i]; len(line) > 0 {
				out = append(out, line)
			}
			start = i + 1
		}
	}
	return out
}

func appendClaudeUserRowAt(t *testing.T, path, sessionID, parentUUID, text string, updateLeaf bool) {
	t.Helper()
	appendClaudeRow(t, path, map[string]any{
		"type":       "user",
		"uuid":       "native-" + text,
		"parentUuid": parentUUID,
		"sessionId":  sessionID,
		"message":    map[string]any{"role": "user", "content": text},
	})
	if updateLeaf {
		appendClaudeRow(t, path, map[string]any{
			"type": "last-prompt", "lastPrompt": text,
			"leafUuid": "native-" + text, "sessionId": sessionID,
		})
	}
}

func appendClaudeRow(t *testing.T, path string, row map[string]any) {
	t.Helper()
	encoded, err := json.Marshal(row)
	require.NoError(t, err)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	require.NoError(t, err)
	_, err = f.Write(append(encoded, '\n'))
	require.NoError(t, err)
	require.NoError(t, f.Sync())
	require.NoError(t, f.Close())
}

// Design rule 3: a repair pass must stay under the failure budget of the
// systems it drives. The quarantine breaker is 3 failures / 10 minutes per
// adapter and blocks ALL materialization once tripped, live sync included.
//
// The arithmetic: the breaker is fed from exactly one call site, the Export
// loop in fanOut. The conversation-session pass reaches the adapter through
// writeConversationSession, which records an adapter error string for status
// and never calls Quarantine.RecordFailure — so a repair that refuses, or one
// that errors, contributes ZERO failures per ten minutes. 0 < 3.
func TestForkedMirrorRepair_ChargesNothingToTheQuarantineBreaker(t *testing.T) {
	h := newForkedMirrorHarness(t, true)
	// Strand a row the canonical plan cannot reproduce, so the loss proof
	// refuses on every pass and the repair can never converge.
	appendClaudeRow(t, h.mirror, map[string]any{
		"type": "user", "uuid": "native-cmd", "parentUuid": lastConversationalUUID(t, h.mirror),
		"sessionId": h.artifactID,
		"message":   map[string]any{"role": "user", "content": "<command-name>/model</command-name>"},
	})
	h.orch.recordDestHash(h.mirror)

	before, err := os.ReadFile(h.mirror)
	require.NoError(t, err)
	beforeArt, beforeHead, beforeSize := h.storeFingerprint(t)

	// Ten passes is more than three times the breaker's threshold inside one
	// window, which is the shape that would trip it if this path fed it.
	for i := 0; i < 10; i++ {
		err := h.materialize(t)
		require.Error(t, err, "an unrepairable fork must keep declining")
		require.True(t, errors.Is(err, ErrInboundNativeMaterialization))
	}

	now := time.Now()
	require.False(t, h.tracker.IsQuarantined("claude-code", now),
		"the repair pass must never be able to quarantine the adapter")
	require.Empty(t, h.tracker.Snapshot(now))

	after, err := os.ReadFile(h.mirror)
	require.NoError(t, err)
	require.Equal(t, before, after, "a refused repair must not touch a byte")
	afterArt, afterHead, afterSize := h.storeFingerprint(t)
	require.Equal(t, beforeArt, afterArt)
	require.Equal(t, beforeHead, afterHead)
	require.Equal(t, beforeSize, afterSize)
}

// The successful repair is device-local by construction: it rewrites one
// projection and must leave the canonical store — the thing the whole fleet
// shares — bit-for-bit identical. Canonical dedupe needs a fleet flag-day and
// is deliberately not attempted here.
func TestForkedMirrorRepair_NeverMutatesTheCanonicalStore(t *testing.T) {
	h := newForkedMirrorHarness(t, true)
	beforeArt, beforeHead, beforeSize := h.storeFingerprint(t)

	require.NoError(t, h.materialize(t), "the repairable fork must converge")
	turns := readClaudeTurns(t, h.mirror)
	require.Equal(t, h.canonical, turns,
		"the resume walk must reconstruct the whole canonical thread")

	afterArt, afterHead, afterSize := h.storeFingerprint(t)
	require.Equal(t, beforeArt, afterArt)
	require.Equal(t, beforeHead, afterHead)
	require.Equal(t, beforeSize, afterSize)

	mirrors, err := filepath.Glob(filepath.Join(filepath.Dir(h.mirror), "*.jsonl"))
	require.NoError(t, err)
	require.Len(t, mirrors, 1, "the repair must never create a second session for the thread")
}

// The same fork, with the flag off, is the shipped behaviour: it declines
// forever and nothing on disk moves.
func TestForkedMirrorRepair_DisabledByDefaultLeavesTheLivelockIntact(t *testing.T) {
	h := newForkedMirrorHarness(t, false)
	before, err := os.ReadFile(h.mirror)
	require.NoError(t, err)

	for i := 0; i < 3; i++ {
		err := h.materialize(t)
		require.Error(t, err)
		require.True(t, errors.Is(err, ErrInboundNativeMaterialization))
		var decline *ConversationDeclineError
		require.True(t, errors.As(err, &decline))
		require.Equal(t, adapter.SessionDeclineForkedMirror, decline.Reason,
			"the disabled path must still report the fork honestly")
	}
	after, err := os.ReadFile(h.mirror)
	require.NoError(t, err)
	require.Equal(t, before, after)
}
