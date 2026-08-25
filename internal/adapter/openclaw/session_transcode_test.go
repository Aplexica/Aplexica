package openclaw

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/stretchr/testify/require"
)

func testArtifact() acf.Artifact {
	return acf.Artifact{
		ArtifactID: "019eb7c7-870f-75cc-8dc2-6a108812d7f1",
		CreatedAt:  time.Unix(1781100000, 0).UTC(),
		UpdatedAt:  time.Unix(1781100060, 0).UTC(),
	}
}

func testTurns() []acf.TextTurn {
	return []acf.TextTurn{
		{Role: "user", Text: "# AGENTS.md instructions for /Users/testuser\ninjected"},
		{Role: "user", Text: "What is the capital of France?"},
		{Role: "assistant", Text: "Paris."},
	}
}

func TestBuildOpenclawSession_DeterministicAndMarked(t *testing.T) {
	art, turns := testArtifact(), testTurns()
	a := buildOpenclawSession(art, turns, "codex", "/Users/testuser/.openclaw/workspace")
	b := buildOpenclawSession(art, turns, "codex", "/Users/testuser/.openclaw/workspace")
	require.Equal(t, a, b, "same inputs must produce identical documents (idempotent re-materialization)")
	branch := buildOpenclawSessionForBranch(art, turns, "codex", "/Users/testuser/.openclaw/workspace", "review-branch")

	require.True(t, strings.HasPrefix(a.sessionID, syncedSessionIDPrefix),
		"sessionId (and therefore filename) must carry the echo-guard prefix")
	require.Len(t, a.sessionID, 36, "sessionId must keep the native 8-4-4-4-12 shape")
	require.Equal(t, openclawSyncedSessionID(art.ArtifactID), a.sessionID,
		"main branch keeps the legacy deterministic session id")
	require.Equal(t, openclawSyncedSessionIDForBranch(art.ArtifactID, "review-branch"), branch.sessionID)
	require.NotEqual(t, a.sessionID, branch.sessionID,
		"non-main branch must materialize into a distinct OpenClaw session")

	// Key slug: origin agent + first NON-INJECTED user message + seed suffix.
	require.True(t, strings.HasPrefix(a.keyName, "aplx-codex-what-is-the-capital-of-france"),
		"key %q must lead with origin agent and first real user message", a.keyName)
	require.True(t, strings.HasSuffix(a.keyName, "-8812d7f1"),
		"key %q must end with the seed TAIL (uuidv7 heads are timestamp-shared and collide)", a.keyName)
	require.True(t, strings.HasPrefix(branch.keyName, "aplx-review-branch-codex-what-is-the-capital"),
		"fork key %q must expose the selected branch", branch.keyName)

	lines := strings.Split(strings.TrimRight(string(a.transcript), "\n"), "\n")
	require.Len(t, lines, 4, "header + 3 messages")

	var hdr sessionHeaderLine
	require.NoError(t, json.Unmarshal([]byte(lines[0]), &hdr))
	require.Equal(t, "session", hdr.Type)
	require.Equal(t, sessionFormatVersion, hdr.Version)
	require.Equal(t, a.sessionID, hdr.ID)
	require.Equal(t, aplexicaImportMarker, hdr.Aplexica)
	require.Equal(t, art.ArtifactID, hdr.AplexicaThreadID)
	require.Equal(t, acf.MainBranch, hdr.AplexicaBranchID)

	// Role-specific bodies per the native format: user content is a plain
	// string, assistant content is a text-block array; messages chain by
	// parentId (first message's parentId is null).
	var first, second, third sessionMessageLine
	require.NoError(t, json.Unmarshal([]byte(lines[1]), &first))
	require.NoError(t, json.Unmarshal([]byte(lines[2]), &second))
	require.NoError(t, json.Unmarshal([]byte(lines[3]), &third))
	require.Nil(t, first.ParentID)
	require.Equal(t, first.ID, *second.ParentID)
	require.Equal(t, second.ID, *third.ParentID)
	_, userIsString := second.Message["content"].(string)
	require.True(t, userIsString, "user content must be a plain string")
	_, asstIsArray := third.Message["content"].([]any)
	require.True(t, asstIsArray, "assistant content must be a block array")
	require.Equal(t, "codex", third.Message["model"], "assistant messages carry the origin agent as model")

	require.Equal(t, a.sessionID, a.index.SessionID)
	require.Equal(t, "aplexica", a.index.ModelProvider)
	require.Equal(t, "codex", a.index.Model)
	require.Equal(t, art.UpdatedAt.UnixMilli(), a.index.SessionStartedAt,
		"timestamps come from the artifact's UpdatedAt so fresh syncs land inside the idle-reset window")
}

func TestMaterializeConversationSession_WritesTranscriptAndIndex(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".openclaw", "agents", "main", "sessions")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	// Pre-existing index with a native entry carrying rich fields the adapter
	// doesn't model — they must survive the upsert byte-for-byte.
	native := `{"agent:main:main":{"sessionId":"11111111","sessionFile":"x.jsonl","skillsSnapshot":{"skills":[{"name":"weather"}]},"customUnknownField":42}}`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "sessions.json"), []byte(native), 0o600))

	payload, err := acf.EncodePayload(acf.ConversationPayload{
		Format: acf.ConversationFormatV1,
		Events: []acf.ConversationEvent{
			{Type: acf.EventTypeTurn, Role: "user", Content: []acf.ContentBlock{{Type: "text", Text: "What is the capital of France?"}}},
			{Type: acf.EventTypeTurn, Role: "assistant", Content: []acf.ContentBlock{{Type: "text", Text: "Paris."}}},
		},
	})
	require.NoError(t, err)

	a := &Adapter{HomeDir: home}
	path, supports, err := a.MaterializeConversationSession(testArtifact(), acf.Event{Payload: payload, Branch: "review-branch"}, "codex")
	require.NoError(t, err)
	require.True(t, supports)
	require.True(t, strings.HasPrefix(filepath.Base(path), syncedSessionIDPrefix))
	require.Equal(t, dir, filepath.Dir(path), "transcript must land in the main agent's session store")

	transcript, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Contains(t, string(transcript), "What is the capital of France?")
	require.Contains(t, string(transcript), `"aplexicaBranchId":"review-branch"`)
	ref, ok := openclawSessionThreadRef(path, nil)
	require.True(t, ok)
	require.Equal(t, testArtifact().ArtifactID, ref.ArtifactID)
	require.Equal(t, "review-branch", ref.BranchID)

	var idx map[string]json.RawMessage
	b, err := os.ReadFile(filepath.Join(dir, "sessions.json"))
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(b, &idx))
	require.Len(t, idx, 2, "native entry + materialized entry")
	require.Contains(t, string(idx["agent:main:main"]), `"customUnknownField": 42`,
		"unknown native fields must survive the upsert")
	require.Contains(t, string(idx["agent:main:main"]), "skillsSnapshot")

	var entry sessionIndexEntry
	found := false
	for k, v := range idx {
		if strings.HasPrefix(k, "agent:main:aplx-review-branch-codex-") {
			require.NoError(t, json.Unmarshal(v, &entry))
			found = true
		}
	}
	require.True(t, found, "materialized entry must be keyed agent:main:aplx-<slug>; got keys %v", keysOf(idx))
	require.Equal(t, path, entry.SessionFile)
	require.Equal(t, "codex", entry.Model)

	// Re-materialization must overwrite in place, not accumulate.
	path2, supports2, err := a.MaterializeConversationSession(testArtifact(), acf.Event{Payload: payload, Branch: "review-branch"}, "codex")
	require.NoError(t, err)
	require.True(t, supports2)
	require.Equal(t, path, path2)
	b, _ = os.ReadFile(filepath.Join(dir, "sessions.json"))
	idx = map[string]json.RawMessage{}
	require.NoError(t, json.Unmarshal(b, &idx))
	require.Len(t, idx, 2, "re-materialization must not add a second entry")
}

// Reproduces a lost-update race: a startup scan materializes many conversations
// concurrently, and unserialized sessions.json read-modify-write cycles can
// clobber one another. With sessionIndexMu every entry must survive.
func TestMaterializeConversationSession_ConcurrentUpsertsAllSurvive(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".openclaw", "agents", "main", "sessions")
	require.NoError(t, os.MkdirAll(dir, 0o700))

	payload, err := acf.EncodePayload(acf.ConversationPayload{
		Format: acf.ConversationFormatV1,
		Events: []acf.ConversationEvent{
			{Type: acf.EventTypeTurn, Role: "user", Content: []acf.ContentBlock{{Type: "text", Text: "hello"}}},
		},
	})
	require.NoError(t, err)

	a := &Adapter{HomeDir: home}
	const n = 32
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Same head (like real UUIDv7 artifacts created in one window);
			// distinct middle so transcript FILES differ (Windows denies the
			// concurrent rename-over of a single shared file); distinct tail
			// so index KEYS differ.
			art := acf.Artifact{
				ArtifactID: fmt.Sprintf("019eb7c7-aaaa-%04d-cccc-dddd%08d", i, i),
				UpdatedAt:  time.Unix(1781100000, 0).UTC(),
			}
			_, supports, err := a.MaterializeConversationSession(art, acf.Event{Payload: payload}, "codex")
			require.NoError(t, err)
			require.True(t, supports)
		}(i)
	}
	wg.Wait()

	b, err := os.ReadFile(filepath.Join(dir, "sessions.json"))
	require.NoError(t, err)
	var idx map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(b, &idx))
	require.Len(t, idx, n, "every concurrent materialization's index entry must survive")
}

func TestMaterializeConversationSession_NoStore_OptsOut(t *testing.T) {
	a := &Adapter{HomeDir: t.TempDir()} // no .openclaw/agents at all
	_, supports, err := a.MaterializeConversationSession(testArtifact(), acf.Event{}, "codex")
	require.NoError(t, err)
	require.False(t, supports, "no agents dir means OpenClaw was never initialized — opt out")
}

func TestImportConversation_SkipsMaterializedEcho(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".openclaw", "agents", "main", "sessions")
	require.NoError(t, os.MkdirAll(dir, 0o700))

	doc := buildOpenclawSession(testArtifact(), testTurns(), "codex", "/ws")
	path := filepath.Join(dir, doc.sessionID+".jsonl")
	require.NoError(t, os.WriteFile(path, doc.transcript, 0o600))
	require.True(t, sessionFileIsCanonicalImport(path))

	a := &Adapter{HomeDir: home}
	ids, err := a.ImportConversation(context.Background(), nil, path)
	require.NoError(t, err)
	require.Empty(t, ids, "materialized transcripts must not round-trip into the store")

	// A native transcript (no marker) is NOT skipped by the guard.
	nativePath := filepath.Join(dir, "native.jsonl")
	require.NoError(t, os.WriteFile(nativePath,
		[]byte(`{"type":"session","version":3,"id":"x","timestamp":"2026-06-10T12:00:00.000Z","cwd":"/ws"}`+"\n"), 0o600))
	require.False(t, sessionFileIsCanonicalImport(nativePath))
}

func keysOf(m map[string]json.RawMessage) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
