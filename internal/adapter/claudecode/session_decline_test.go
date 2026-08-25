package claudecode

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter"
	"github.com/stretchr/testify/require"
)

// writeNativeClaudeTranscript writes a pristine native transcript: one
// parentUuid chain, no Aplexica thread stamp, and a sessionId equal to the file
// name, which is what claudeNativeSourceSessionPlan requires before it will
// treat the path as the original session.
func writeNativeClaudeTranscript(t *testing.T, path, sessionID, cwd string, turns []acf.TextTurn) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	var out bytes.Buffer
	parent := ""
	for i, turn := range turns {
		uuid := fmt.Sprintf("native-%s-%d", turn.Role, i)
		row := map[string]any{
			"type":       turn.Role,
			"uuid":       uuid,
			"parentUuid": nil,
			"sessionId":  sessionID,
			"cwd":        cwd,
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
		encoded, err := json.Marshal(row)
		require.NoError(t, err)
		out.Write(encoded)
		out.WriteByte('\n')
		parent = uuid
	}
	require.NoError(t, os.WriteFile(path, out.Bytes(), 0o644))
}

// firstClaudeUserUUID is the fork point used by the mechanism-A fixture: the
// earliest node that already has a child, so a second child of it strands
// everything after it on a dead sibling branch.
func firstClaudeUserUUID(raw []byte) string {
	for _, line := range bytes.Split(raw, []byte("\n")) {
		var row struct {
			Type string `json:"type"`
			UUID string `json:"uuid"`
		}
		if json.Unmarshal(bytes.TrimSpace(line), &row) == nil && row.Type == "user" && row.UUID != "" {
			return row.UUID
		}
	}
	return ""
}

func nativeDeclineArtifact(t *testing.T, source string) acf.Artifact {
	t.Helper()
	return acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion,
		ArtifactID:       acf.NewID(),
		Kind:             acf.KindConversation,
		Scope:            acf.ScopeGlobal,
		Name:             filepath.Base(source),
		SourcePath:       source,
		CreatedAt:        time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC),
		UpdatedAt:        time.Date(2026, 7, 30, 12, 5, 0, 0, time.UTC),
	}
}

// A genuinely diverged native session must classify differently from one that
// is merely ahead: the first can terminate, the second never may.
func TestClaudeNativeSourceSessionPlan_SplitsAheadFromDiverged(t *testing.T) {
	home := t.TempDir()
	source := filepath.Join(home, ".claude", "projects", "-Users-exampleuser-project", "native-diverge.jsonl")
	existing := []acf.TextTurn{
		{Role: "user", Text: "what is the capital of Canada?"},
		{Role: "assistant", Text: "Ottawa."},
		{Role: "user", Text: "and of Poland?"},
		{Role: "assistant", Text: "Warsaw."},
	}
	writeNativeClaudeTranscript(t, source, "native-diverge", home, existing)
	a := &Adapter{HomeDir: home}

	_, _, relation, reason := a.claudeNativeSourceSessionPlan(source, existing)
	require.Equal(t, claudeNativeSessionExact, relation)
	require.Equal(t, adapter.SessionDeclineUnspecified, reason)

	appendable := append(append([]acf.TextTurn(nil), existing...),
		acf.TextTurn{Role: "user", Text: "and of Spain?"},
	)
	_, _, relation, reason = a.claudeNativeSourceSessionPlan(source, appendable)
	require.Equal(t, claudeNativeSessionAppendable, relation)
	require.Equal(t, adapter.SessionDeclineUnspecified, reason)

	// Canonical is a strict prefix of the native file: the agent is genuinely
	// ahead, and its pending import is the only authority that may move
	// canonical forward.
	_, _, relation, reason = a.claudeNativeSourceSessionPlan(source, existing[:2])
	require.Equal(t, claudeNativeSessionAheadOrDiverged, relation)
	require.Equal(t, adapter.SessionDeclineNativeAhead, reason)

	// Each side now holds a turn the other lacks. No append converges that.
	diverged := append(append([]acf.TextTurn(nil), existing[:2]...),
		acf.TextTurn{Role: "user", Text: "and of Portugal?"},
		acf.TextTurn{Role: "assistant", Text: "Lisbon."},
	)
	_, _, relation, reason = a.claudeNativeSourceSessionPlan(source, diverged)
	require.Equal(t, claudeNativeSessionDiverged, relation)
	require.Equal(t, adapter.SessionDeclineDiverged, reason)
}

// Mechanism B: a native-origin session that diverged declines at BOTH the path
// planner and the writer, and both must name the same reason without touching
// the file.
func TestClaudeConversationSessionDecline_ReportsNativeDivergence(t *testing.T) {
	home := t.TempDir()
	source := filepath.Join(home, ".claude", "projects", "-Users-exampleuser-project", "native-report.jsonl")
	existing := []acf.TextTurn{
		{Role: "user", Text: "what is the capital of Canada?"},
		{Role: "assistant", Text: "Ottawa."},
		{Role: "user", Text: "and of Poland?"},
		{Role: "assistant", Text: "Warsaw."},
	}
	writeNativeClaudeTranscript(t, source, "native-report", home, existing)
	art := nativeDeclineArtifact(t, source)
	head := canonicalConversationHead(t, art.ArtifactID,
		existing[0], existing[1],
		acf.TextTurn{Role: "user", Text: "and of Portugal?"},
		acf.TextTurn{Role: "assistant", Text: "Lisbon."},
	)
	a := &Adapter{HomeDir: home}

	path, supported, reason, err := a.ConversationSessionPathReason(art, head, "codex")
	require.NoError(t, err)
	require.False(t, supported)
	require.Equal(t, source, path)
	require.Equal(t, adapter.SessionDeclineDiverged, reason)

	// The compatibility entry point must behave exactly as it did before.
	legacyPath, legacySupported, legacyErr := a.ConversationSessionPath(art, head, "codex")
	require.NoError(t, legacyErr)
	require.False(t, legacySupported)
	require.Equal(t, source, legacyPath)

	before, err := os.ReadFile(source)
	require.NoError(t, err)
	written, ok, reason, err := a.MaterializeConversationSessionReason(art, head, "codex")
	require.NoError(t, err)
	require.False(t, ok)
	require.Equal(t, source, written)
	require.Equal(t, adapter.SessionDeclineDiverged, reason)
	after, err := os.ReadFile(source)
	require.NoError(t, err)
	require.Equal(t, before, after, "a decline must never mutate the native transcript")
}

// A native session that is merely ahead must report native_ahead, not
// divergence — routing it to a terminal class is what would give the entry
// that most needs a human a false explanation.
func TestClaudeConversationSessionDecline_ReportsNativeAhead(t *testing.T) {
	home := t.TempDir()
	source := filepath.Join(home, ".claude", "projects", "-Users-exampleuser-project", "native-ahead.jsonl")
	existing := []acf.TextTurn{
		{Role: "user", Text: "what is the capital of Canada?"},
		{Role: "assistant", Text: "Ottawa."},
		{Role: "user", Text: "and of Poland?"},
		{Role: "assistant", Text: "Warsaw."},
	}
	writeNativeClaudeTranscript(t, source, "native-ahead", home, existing)
	art := nativeDeclineArtifact(t, source)
	head := canonicalConversationHead(t, art.ArtifactID, existing[0], existing[1])
	a := &Adapter{HomeDir: home}

	_, supported, reason, err := a.ConversationSessionPathReason(art, head, "codex")
	require.NoError(t, err)
	require.False(t, supported)
	require.Equal(t, adapter.SessionDeclineNativeAhead, reason)
}

// Mechanism A: Claude Code appends its own child of a node Aplexica already
// extended, stranding Aplexica's rows on a dead sibling branch. Every repair
// door fails closed on the resulting node-count mismatch; the reason must say
// so rather than looking like an ordinary divergence.
func TestMaterializeConversationSessionReason_ForkedSyntheticMirror(t *testing.T) {
	home := t.TempDir()
	a := &Adapter{HomeDir: home}
	art := acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion,
		ArtifactID:       acf.NewID(),
		Kind:             acf.KindConversation,
		Scope:            acf.ScopeGlobal,
		// A codex rollout: localConversationSourcePath rejects it, so this
		// artifact can only ever reach the deterministic synthetic mirror.
		SourcePath: filepath.Join(home, ".codex", "sessions", "rollout-2026-07-30T12-00-00-abc.jsonl"),
		CreatedAt:  time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC),
		UpdatedAt:  time.Date(2026, 7, 30, 12, 5, 0, 0, time.UTC),
	}
	first := []acf.TextTurn{
		{Role: "user", Text: "what is the capital of Canada?"},
		{Role: "assistant", Text: "Ottawa."},
	}
	dest, ok, reason, err := a.MaterializeConversationSessionReason(
		art, canonicalConversationHead(t, art.ArtifactID, first...), "codex")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, adapter.SessionDeclineUnspecified, reason)

	raw, err := os.ReadFile(dest)
	require.NoError(t, err)
	forkPoint := firstClaudeUserUUID(raw)
	require.NotEmpty(t, forkPoint)
	sessionID := strings.TrimSuffix(filepath.Base(dest), filepath.Ext(dest))
	require.NoError(t, appendNativeClaudeUserAtParent(dest, sessionID, forkPoint, "and of Poland?", true))

	complete := append(append([]acf.TextTurn(nil), first...),
		acf.TextTurn{Role: "user", Text: "and of Poland?"},
		acf.TextTurn{Role: "assistant", Text: "Warsaw."},
	)
	before, err := os.ReadFile(dest)
	require.NoError(t, err)
	// Pin the fixture to the physical signature it is meant to reproduce, so a
	// future change cannot make this test pass through a different decline.
	projection, err := parseClaudeVisibleLeaf(before)
	require.NoError(t, err)
	require.NotEqual(t, projection.nodeCount, len(projection.turns),
		"the fixture must strand rows off the resume leaf chain")
	_, ok, reason, err = a.MaterializeConversationSessionReason(
		art, canonicalConversationHead(t, art.ArtifactID, complete...), "codex")
	require.NoError(t, err)
	require.False(t, ok)
	require.Equal(t, adapter.SessionDeclineForkedMirror, reason)
	after, err := os.ReadFile(dest)
	require.NoError(t, err)
	require.Equal(t, before, after, "Stage A never rewrites a forked mirror")
}

// An unreadable or half-written native transcript is a race, not a structural
// fault; an Aplexica-stamped file at a path the artifact calls its pristine
// native source is the opposite.
func TestClaudeNativeSourceSessionPlan_ClassifiesInvalidSnapshots(t *testing.T) {
	home := t.TempDir()
	turns := []acf.TextTurn{
		{Role: "user", Text: "what is the capital of Canada?"},
		{Role: "assistant", Text: "Ottawa."},
	}

	missing := filepath.Join(home, ".claude", "projects", "-Users-exampleuser-project", "gone.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(missing), 0o755))
	a := &Adapter{HomeDir: home}
	_, _, relation, reason := a.claudeNativeSourceSessionPlan(missing, turns)
	require.Equal(t, claudeNativeSessionInvalid, relation)
	require.Equal(t, adapter.SessionDeclineRace, reason)

	renamed := filepath.Join(home, ".claude", "projects", "-Users-exampleuser-project", "not-the-session-id.jsonl")
	writeNativeClaudeTranscript(t, renamed, "some-other-session", home, turns)
	_, _, relation, reason = a.claudeNativeSourceSessionPlan(renamed, turns)
	require.Equal(t, claudeNativeSessionInvalid, relation)
	require.Equal(t, adapter.SessionDeclineGraphMalformed, reason)
}
