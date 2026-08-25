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

func TestTranscodeToClaudeSession(t *testing.T) {
	base := time.Date(2026, 6, 1, 18, 57, 0, 0, time.UTC)
	turns := []acf.TextTurn{
		{Role: "user", Text: "what is the project name?"},
		{Role: "assistant", Text: "No project name is specified."},
	}
	out := transcodeToClaudeSession(turns, "019e84f9-b081-7c8a", "/Users/testuser", "codex", "rollout.jsonl", base)
	require.NotEmpty(t, out)
	for _, want := range []string{
		`"type":"custom-title"`,
		`"type":"ai-title"`, "↪ Codex: what is the project name?",
		`"type":"user"`, "what is the project name?",
		`"type":"assistant"`, "No project name is specified.",
		`"sessionId":"019e84f9-b081-7c8a"`,
	} {
		require.Contains(t, out, want)
	}
}

func TestTranscodeToClaudeSession_PrefersPortableNativeTitle(t *testing.T) {
	base := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	out := transcodeToClaudeSession(
		[]acf.TextTurn{{Role: "user", Text: "You should not use this as the subject."}},
		"019f6126-d3e2-7d00-a920-ba8403c67936",
		"/Users/exampleuser",
		"codex",
		"Find email context",
		base,
	)
	require.Contains(t, out, `"type":"custom-title"`)
	require.Contains(t, out, `"customTitle":"Find email context"`)
	require.Contains(t, out, `"aiTitle":"Find email context"`)
	require.NotContains(t, out, `"customTitle":"↪ Codex:`)
}

// Loop-safety: materialize → Claude's EncodeCanonical → ExtractTextTurns must
// reproduce the original turns, so re-materialization is inert.
func TestClaudeSession_RoundTripStable(t *testing.T) {
	turns := []acf.TextTurn{
		{Role: "user", Text: "what is my name?"},
		{Role: "assistant", Text: "Example User."},
		{Role: "user", Text: "what is 3+3?"},
		{Role: "assistant", Text: "6."},
	}
	out := transcodeToClaudeSession(turns, "tid-9", "/Users/testuser", "codex", "f", time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC))
	events, err := EncodeCanonical([]byte(out))
	require.NoError(t, err)
	require.Equal(t, turns, acf.ExtractTextTurns(events),
		"materialize → EncodeCanonical → ExtractTextTurns must reproduce the original turns (loop-safety)")
}

func TestClaudeSessionThreadID(t *testing.T) {
	out := transcodeToClaudeSession([]acf.TextTurn{{Role: "user", Text: "hi"}}, "thread-abc", "/Users/testuser", "codex", "f", time.Now())
	require.Equal(t, "thread-abc", claudeSessionThreadID([]byte(out)))
	ref, ok := claudeSessionThreadRef([]byte(out))
	require.True(t, ok)
	require.Equal(t, "thread-abc", ref.ArtifactID)
	require.Equal(t, acf.MainBranch, ref.BranchID)
	require.True(t, ref.GeneratedSnapshot)
	require.Equal(t, adapter.ConversationTurnsHash([]acf.TextTurn{{Role: "user", Text: "hi"}}), ref.MaterializedTurnsHash)
	require.Equal(t, 1, ref.MaterializedTurnCount)

	branchOut := transcodeToClaudeSessionWithThread(
		[]acf.TextTurn{{Role: "user", Text: "hi"}},
		"native-session-abc",
		"thread-abc",
		"review-branch",
		"/Users/testuser",
		"codex",
		"f",
		time.Now(),
	)
	ref, ok = claudeSessionThreadRef([]byte(branchOut))
	require.True(t, ok)
	require.Equal(t, "thread-abc", ref.ArtifactID)
	require.Equal(t, "review-branch", ref.BranchID)
	require.True(t, ref.GeneratedSnapshot)
	require.Contains(t, branchOut, `"customTitle":"[review-branch] f"`)
	require.Contains(t, branchOut, `"aiTitle":"[review-branch] f"`)
	generatedPath := filepath.Join(t.TempDir(), "generated.jsonl")
	require.NoError(t, os.WriteFile(generatedPath, []byte(branchOut), 0o600))
	marked, err := claudeSessionHasAplexicaThreadMarker(generatedPath)
	require.NoError(t, err)
	require.True(t, marked)
	nativePath := filepath.Join(t.TempDir(), "native.jsonl")
	require.NoError(t, os.WriteFile(nativePath, []byte(`{"type":"user","sessionId":"native"}`+"\n"+strings.Repeat("x", 8<<20)), 0o600))
	marked, err = claudeSessionHasAplexicaThreadMarker(nativePath)
	require.NoError(t, err)
	require.False(t, marked, "a huge native tail must not enter the whole-file merge probe")

	continued := branchOut + `{"type":"user","message":{"role":"user","content":"continued"},"sessionId":"native-session-abc"}` + "\n"
	ref, ok = claudeSessionThreadRef([]byte(continued))
	require.True(t, ok)
	require.False(t, ref.GeneratedSnapshot, "a Claude-authored continuation is not an unchanged generated mirror")
	require.Equal(t, 1, ref.MaterializedTurnCount, "native rows must not extend the stamped generated base")

	desktopMetadata := branchOut +
		`{"type":"ai-title","aiTitle":"Imported title","sessionId":"native-session-abc"}` + "\n" +
		`{"type":"mode","mode":"normal","sessionId":"native-session-abc"}` + "\n"
	ref, ok = claudeSessionThreadRef([]byte(desktopMetadata))
	require.True(t, ok)
	require.True(t, ref.GeneratedSnapshot, "Desktop import metadata is not a native conversation continuation")

	manualRename := branchOut + `{"type":"custom-title","customTitle":"My title","sessionId":"native-session-abc"}` + "\n"
	ref, ok = claudeSessionThreadRef([]byte(manualRename))
	require.True(t, ok)
	require.False(t, ref.GeneratedSnapshot, "an unstamped custom-title may be a manual user rename")

	// A native session with a random sessionId returns that id (which won't match an artifact).
	require.Equal(t, "rand-123", claudeSessionThreadID([]byte(`{"type":"user","message":{"role":"user","content":"hi"},"sessionId":"rand-123"}`)))
	ref, ok = claudeSessionThreadRef([]byte(`{"type":"user","message":{"role":"user","content":"hi"},"sessionId":"rand-123"}`))
	require.True(t, ok)
	require.Equal(t, "rand-123", ref.ArtifactID)
	require.Equal(t, acf.MainBranch, ref.BranchID)
	require.Equal(t, "", claudeSessionThreadID([]byte(`{"type":"mode"}`)))
}

func TestConversationSessionPath_ReusesExactOriginalNativeSessionWithoutWriting(t *testing.T) {
	home := t.TempDir()
	source := filepath.Join(home, ".claude", "projects", "-Users-exampleuser-project", "native-session.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(source), 0o755))
	original := strings.Join([]string{
		`{"type":"user","uuid":"native-user-1","parentUuid":null,"sessionId":"native-session","cwd":` + quotedClaudeJSON(home) + `,"message":{"role":"user","content":"what is capital of Poland?"}}`,
		`{"type":"assistant","uuid":"native-assistant-1","parentUuid":"native-user-1","sessionId":"native-session","cwd":` + quotedClaudeJSON(home) + `,"message":{"role":"assistant","content":[{"type":"text","text":"Warsaw."}],"model":"claude-opus-4-8"}}`,
	}, "\n") + "\n"
	require.NoError(t, os.WriteFile(source, []byte(original), 0o644))
	before, err := os.Stat(source)
	require.NoError(t, err)

	artifactID := acf.NewID()
	art := acf.Artifact{
		ArtifactID: artifactID,
		Kind:       acf.KindConversation,
		Scope:      acf.ScopeGlobal,
		Name:       "native-session.jsonl",
		SourcePath: source,
		UpdatedAt:  time.Date(2026, 7, 20, 15, 0, 0, 0, time.UTC),
	}
	head := canonicalConversationHead(t, artifactID,
		acf.TextTurn{Role: "user", Text: "what is capital of Poland?"},
		acf.TextTurn{Role: "assistant", Text: "Warsaw."},
	)

	a := &Adapter{HomeDir: home}
	path, ok, err := a.ConversationSessionPath(art, head, "codex")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, source, path)

	written, ok, err := a.MaterializeConversationSession(art, head, "codex")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, source, written)
	after, err := os.Stat(source)
	require.NoError(t, err)
	require.True(t, os.SameFile(before, after), "same-conversation delivery must preserve Claude's native inode")

	raw, err := os.ReadFile(source)
	require.NoError(t, err)
	require.Equal(t, original, string(raw), "an exact native source must be reused byte-for-byte")
	events, err := EncodeCanonical(raw)
	require.NoError(t, err)
	require.Equal(t, []acf.TextTurn{
		{Role: "user", Text: "what is capital of Poland?"},
		{Role: "assistant", Text: "Warsaw."},
	}, acf.ExtractTextTurns(events))
	require.Equal(t, "native-session", claudeSessionThreadID(raw), "the original Claude session identity must not change")
	requireClaudeParentChain(t, raw)

	synthetic := filepath.Join(home, ".claude", "projects", encodeProjectDir(home), artifactID+".jsonl")
	_, err = os.Stat(synthetic)
	require.ErrorIs(t, err, os.ErrNotExist, "a compatible origin session must not produce a duplicate /resume entry")
}

func TestConversationSessionPath_RemoteArtifactNeverClaimsMatchingLocalClaudePath(t *testing.T) {
	home := t.TempDir()
	source := filepath.Join(home, ".claude", "projects", "-Users-exampleuser-project", "native-session.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(source), 0o755))
	require.NoError(t, os.WriteFile(source, []byte(
		`{"type":"user","uuid":"native-user-1","parentUuid":null,"sessionId":"native-session","message":{"role":"user","content":"same text"}}`+"\n",
	), 0o644))

	artifactID := acf.NewID()
	art := acf.Artifact{
		ArtifactID:           artifactID,
		Kind:                 acf.KindConversation,
		Scope:                acf.ScopeGlobal,
		Name:                 "native-session.jsonl",
		SourcePath:           source,
		RemoteOriginDeviceID: "another-device",
		UpdatedAt:            time.Date(2026, 7, 20, 15, 0, 0, 0, time.UTC),
	}
	head := canonicalConversationHead(t, artifactID, acf.TextTurn{Role: "user", Text: "same text"})
	a := &Adapter{HomeDir: home}

	path, ok, err := a.ConversationSessionPath(art, head, "claude-code")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, filepath.Join(home, ".claude", "projects", encodeProjectDir(home), artifactID+".jsonl"), path)
}

func TestLocalConversationSourcePathRejectsUnsafeTargets(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, ".claude", "projects")
	require.NoError(t, os.MkdirAll(root, 0o755))
	a := &Adapter{HomeDir: home}

	regular := filepath.Join(root, "project", "session.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(regular), 0o755))
	require.NoError(t, os.WriteFile(regular, []byte("{}\n"), 0o644))
	got, ok := a.localConversationSourcePath(regular)
	require.True(t, ok)
	require.Equal(t, regular, got)

	nonRegular := filepath.Join(root, "directory.jsonl")
	require.NoError(t, os.MkdirAll(nonRegular, 0o755))
	_, ok = a.localConversationSourcePath(nonRegular)
	require.False(t, ok, "directories and other non-regular targets must fail closed")

	finalLink := filepath.Join(root, "linked.jsonl")
	if err := os.Symlink(regular, finalLink); err == nil {
		_, ok = a.localConversationSourcePath(finalLink)
		require.False(t, ok, "a final-component symlink must never be appended through")
	}

	escape := t.TempDir()
	escapeFile := filepath.Join(escape, "escaped.jsonl")
	require.NoError(t, os.WriteFile(escapeFile, []byte("{}\n"), 0o644))
	componentLink := filepath.Join(root, "escape")
	if err := os.Symlink(escape, componentLink); err == nil {
		_, ok = a.localConversationSourcePath(filepath.Join(componentLink, "escaped.jsonl"))
		require.False(t, ok, "resolved targets outside the Claude projects root must fail closed")
	}

	inRootTarget := filepath.Join(root, "real-project")
	require.NoError(t, os.MkdirAll(inRootTarget, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(inRootTarget, "inside.jsonl"), []byte("{}\n"), 0o644))
	inRootLink := filepath.Join(root, "linked-project")
	if err := os.Symlink(inRootTarget, inRootLink); err == nil {
		_, ok = a.localConversationSourcePath(filepath.Join(inRootLink, "inside.jsonl"))
		require.False(t, ok, "directory symlinks must fail closed even when they resolve inside the projects root")
	}
}

func TestConversationSessionPath_LocallyAheadNativeSessionDeclinesWithoutClaimingSource(t *testing.T) {
	home := t.TempDir()
	source := filepath.Join(home, ".claude", "projects", "-Users-exampleuser-project", "native-session.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(source), 0o755))
	native := strings.Join([]string{
		`{"type":"user","uuid":"native-user-1","parentUuid":null,"sessionId":"native-session","message":{"role":"user","content":"q1"}}`,
		`{"type":"assistant","uuid":"native-assistant-1","parentUuid":"native-user-1","sessionId":"native-session","message":{"role":"assistant","content":[{"type":"text","text":"a1"}],"model":"claude-opus-4-8"}}`,
		`{"type":"user","uuid":"native-user-2","parentUuid":"native-assistant-1","sessionId":"native-session","message":{"role":"user","content":"unsynced local question"}}`,
	}, "\n") + "\n"
	require.NoError(t, os.WriteFile(source, []byte(native), 0o644))

	artifactID := acf.NewID()
	art := acf.Artifact{
		ArtifactID: artifactID, Kind: acf.KindConversation, Scope: acf.ScopeGlobal,
		Name: "native-session.jsonl", SourcePath: source,
		UpdatedAt: time.Date(2026, 7, 20, 15, 0, 0, 0, time.UTC),
	}
	head := canonicalConversationHead(t, artifactID,
		acf.TextTurn{Role: "user", Text: "q1"},
		acf.TextTurn{Role: "assistant", Text: "a1"},
	)
	a := &Adapter{HomeDir: home}
	path, ok, err := a.ConversationSessionPath(art, head, "codex")
	require.NoError(t, err)
	require.False(t, ok)
	require.Equal(t, source, path, "the stable retry path must be reported without being guard-marked")

	written, ok, err := a.MaterializeConversationSession(art, head, "codex")
	require.NoError(t, err)
	require.False(t, ok)
	require.Equal(t, source, written)
	nativeAfter, err := os.ReadFile(source)
	require.NoError(t, err)
	require.Equal(t, native, string(nativeAfter), "the locally-ahead source must remain untouched for watcher import")
}

func TestMaterializeConversationSession_NativeDeltaExtendsOriginalSessionOnly(t *testing.T) {
	home := t.TempDir()
	source := filepath.Join(home, ".claude", "projects", "-Users-exampleuser-project", "native-session.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(source), 0o755))
	original := strings.Join([]string{
		`{"type":"user","uuid":"native-user-1","parentUuid":null,"sessionId":"native-session","cwd":` + quotedClaudeJSON(home) + `,"message":{"role":"user","content":"q1"}}`,
		`{"type":"assistant","uuid":"native-assistant-1","parentUuid":"native-user-1","sessionId":"native-session","cwd":` + quotedClaudeJSON(home) + `,"message":{"role":"assistant","content":[{"type":"text","text":"a1"}],"model":"claude-opus-4-8"}}`,
	}, "\n") + "\n"
	require.NoError(t, os.WriteFile(source, []byte(original), 0o644))
	before, err := os.Stat(source)
	require.NoError(t, err)

	artifactID := acf.NewID()
	art := acf.Artifact{
		ArtifactID: artifactID, Kind: acf.KindConversation, Scope: acf.ScopeGlobal,
		Name: "native-session.jsonl", SourcePath: source,
		UpdatedAt: time.Date(2026, 7, 20, 15, 0, 0, 0, time.UTC),
	}
	canonical := []acf.TextTurn{
		{Role: "user", Text: "q1"},
		{Role: "assistant", Text: "a1"},
		{Role: "user", Text: "q2-from-codex"},
		{Role: "assistant", Text: "a2-from-codex"},
	}
	a := &Adapter{HomeDir: home}
	path, ok, err := a.ConversationSessionPath(art, canonicalConversationHead(t, artifactID, canonical...), "codex")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, source, path)

	written, ok, err := a.MaterializeConversationSession(art, canonicalConversationHead(t, artifactID, canonical...), "codex")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, source, written)

	nativeRaw, err := os.ReadFile(source)
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(string(nativeRaw), original), "the original native rows must remain byte-exact")
	after, err := os.Stat(source)
	require.NoError(t, err)
	require.True(t, os.SameFile(before, after), "native source inode must remain exact")

	nativeEvents, err := EncodeCanonical(nativeRaw)
	require.NoError(t, err)
	require.Equal(t, canonical, acf.ExtractTextTurns(nativeEvents))
	require.Equal(t, canonical, claudeLeafTextTurns(t, nativeRaw),
		"Claude's parentUuid leaf walk must see the complete canonical conversation")
	requireClaudeParentChain(t, nativeRaw)
	require.NotContains(t, string(nativeRaw), `"customTitle":"↪ Codex continuation`)
	require.NotContains(t, string(nativeRaw), `"aplexicaThreadId"`,
		"the original Claude source must remain path-keyed rather than becoming a generated mirror")
	entries, err := os.ReadDir(filepath.Dir(source))
	require.NoError(t, err)
	require.Len(t, entries, 1, "one canonical thread must remain one Claude conversation")

	canonical = append(canonical,
		acf.TextTurn{Role: "user", Text: "q3-from-codex"},
		acf.TextTurn{Role: "assistant", Text: "a3-from-codex"},
	)
	written, ok, err = a.MaterializeConversationSession(
		art,
		canonicalConversationHead(t, artifactID, canonical...),
		"codex",
	)
	require.NoError(t, err)
	require.True(t, ok, "later canonical growth must continue reusing the same native source")
	require.Equal(t, source, written)
	nativeRaw, err = os.ReadFile(source)
	require.NoError(t, err)
	require.Equal(t, canonical, claudeLeafTextTurns(t, nativeRaw))
	require.NotContains(t, string(nativeRaw), `"aplexicaThreadId"`)
	entries, err = os.ReadDir(filepath.Dir(source))
	require.NoError(t, err)
	require.Len(t, entries, 1)
}

func TestNativeSourceCompatibilityReusesIncrementalCacheForLargeSession(t *testing.T) {
	home := t.TempDir()
	sessionID := acf.NewID()
	source := filepath.Join(home, ".claude", "projects", "large-project", sessionID+".jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(source), 0o755))
	padding := strings.Repeat("x", 8<<20)
	raw := `{"type":"queue-operation","padding":"` + padding + `"}` + "\n" +
		`{"type":"user","uuid":"base-user","parentUuid":null,"sessionId":"` + sessionID + `","cwd":"/native/project","message":{"role":"user","content":"base question"}}` + "\n"
	require.NoError(t, os.WriteFile(source, []byte(raw), 0o644))

	artifactID := acf.NewID()
	art := acf.Artifact{ArtifactID: artifactID, Kind: acf.KindConversation, Scope: acf.ScopeGlobal, SourcePath: source}
	head := canonicalConversationHead(t, artifactID,
		acf.TextTurn{Role: "user", Text: "base question"},
		acf.TextTurn{Role: "assistant", Text: "remote answer"},
	)
	a := &Adapter{HomeDir: home}
	for range 3 {
		path, ok, err := a.ConversationSessionPath(art, head, "codex")
		require.NoError(t, err)
		require.True(t, ok)
		require.Equal(t, source, path)
	}
	written, ok, err := a.MaterializeConversationSession(art, head, "codex")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, source, written)
	writtenRaw, err := os.ReadFile(written)
	require.NoError(t, err)
	require.Contains(t, string(writtenRaw), `"cwd":"/native/project"`,
		"the appended delta should retain the original native project cwd")

	cache := a.conversationCache()
	require.Equal(t, uint64(1), cache.fullParses, "only the cold compatibility check may parse the full 8 MiB source")
	require.Equal(t, uint64(len(raw)), cache.fullReadBytes)
	require.Less(t, cache.tailReadBytes, uint64(4096), "warm checks may read only the tiny appended tail")
	require.Less(t, cache.probeReadBytes, uint64(64<<10), "warm validation must remain bounded independently of file size")
}

func TestNativeSourceIncompleteTrailingRowFailsClosedWithoutCreatingAnotherSession(t *testing.T) {
	home := t.TempDir()
	sessionID := acf.NewID()
	source := filepath.Join(home, ".claude", "projects", "native-project", sessionID+".jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(source), 0o755))
	complete := `{"type":"user","uuid":"base-user","parentUuid":null,"sessionId":"` + sessionID + `","message":{"role":"user","content":"base question"}}` + "\n"
	partial := `{"type":"assistant","uuid":"partial-local","sessionId":"` + sessionID + `","message":{"role":"assistant","content":[{"type":"text","text":"unfinished`
	require.NoError(t, os.WriteFile(source, []byte(complete+partial), 0o644))

	artifactID := acf.NewID()
	art := acf.Artifact{ArtifactID: artifactID, Kind: acf.KindConversation, Scope: acf.ScopeGlobal, SourcePath: source}
	head := canonicalConversationHead(t, artifactID,
		acf.TextTurn{Role: "user", Text: "base question"},
		acf.TextTurn{Role: "assistant", Text: "remote answer"},
	)
	a := &Adapter{HomeDir: home}
	path, ok, err := a.ConversationSessionPath(art, head, "codex")
	require.NoError(t, err)
	require.False(t, ok)
	require.Equal(t, source, path, "a partial row keeps the stable retry path but must never be guard-marked")
	written, ok, err := a.MaterializeConversationSession(art, head, "codex")
	require.NoError(t, err)
	require.False(t, ok)
	require.Equal(t, source, written)
	raw, err := os.ReadFile(source)
	require.NoError(t, err)
	require.Equal(t, complete+partial, string(raw), "fail-closed recovery must not truncate or extend ambiguous native bytes")
}

func TestConversationSessionPath_DivergentLocalSourceDeclinesWithoutCreatingAnotherSession(t *testing.T) {
	home := t.TempDir()
	source := filepath.Join(home, ".claude", "projects", "-Users-exampleuser", "native-session.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(source), 0o755))
	require.NoError(t, os.WriteFile(source, []byte(`{"type":"user","message":{"role":"user","content":"live native turn"},"sessionId":"native-session"}`+"\n"), 0o644))

	artifactID := acf.NewID()
	art := acf.Artifact{
		ArtifactID: artifactID,
		Kind:       acf.KindConversation,
		Scope:      acf.ScopeGlobal,
		Name:       "native-session.jsonl",
		SourcePath: source,
		UpdatedAt:  time.Date(2026, 7, 1, 20, 57, 53, 0, time.UTC),
	}
	head := canonicalConversationHead(t, artifactID,
		acf.TextTurn{Role: "user", Text: "What is the distance to Moon?"},
		acf.TextTurn{Role: "assistant", Text: "384,400 km."},
		acf.TextTurn{Role: "user", Text: "What is the speed of Light?"},
		acf.TextTurn{Role: "assistant", Text: "299,792,458 meters per second."},
	)

	a := &Adapter{HomeDir: home}
	path, ok, err := a.ConversationSessionPath(art, head, "claude-code")
	require.NoError(t, err)
	require.False(t, ok)
	require.Equal(t, source, path)

	written, ok, err := a.MaterializeConversationSession(art, head, "claude-code")
	require.NoError(t, err)
	require.False(t, ok)
	require.Equal(t, source, written)

	original, err := os.ReadFile(source)
	require.NoError(t, err)
	require.Contains(t, string(original), "live native turn",
		"materialization must not overwrite or duplicate a divergent native session")
}

func TestMaterializeConversationSession_AppendsAnswerToStableSyntheticMirrorWithoutReplacingOpenInode(t *testing.T) {
	home := t.TempDir()
	artifactID := acf.NewID()
	base := time.Date(2026, 7, 18, 21, 48, 30, 0, time.UTC)
	art := acf.Artifact{
		ArtifactID: artifactID, Kind: acf.KindConversation, Scope: acf.ScopeGlobal,
		Name: "thread.jsonl", CreatedAt: base, UpdatedAt: base,
	}
	a := &Adapter{HomeDir: home}
	promptTurns := []acf.TextTurn{
		{Role: "user", Text: "what is capital of France?"},
		{Role: "assistant", Text: "Paris."},
		{Role: "user", Text: "how many people live in paris?"},
	}
	path, ok, err := a.MaterializeConversationSession(art, canonicalConversationHead(t, artifactID, promptTurns...), "codex")
	require.NoError(t, err)
	require.True(t, ok)
	before, err := os.Stat(path)
	require.NoError(t, err)
	openWriter, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	require.NoError(t, err)
	defer openWriter.Close()

	completeTurns := append(append([]acf.TextTurn(nil), promptTurns...),
		acf.TextTurn{Role: "assistant", Text: "About 2.1 million."})
	art.UpdatedAt = base.Add(13 * time.Second)
	completePath, ok, err := a.MaterializeConversationSession(art, canonicalConversationHead(t, artifactID, completeTurns...), "codex")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, path, completePath, "normal canonical growth should reuse the bounded stable mirror")
	after, err := os.Stat(path)
	require.NoError(t, err)
	require.True(t, os.SameFile(before, after), "answer materialization must preserve Claude's open inode")

	primaryBeforeLocalAppend, err := os.ReadFile(path)
	require.NoError(t, err)
	_, err = openWriter.WriteString(`{"type":"user","uuid":"native-user","parentUuid":"` + lastClaudeConversationalUUID(primaryBeforeLocalAppend) + `","sessionId":"` + artifactID + `","message":{"role":"user","content":"and the metro area?"}}` + "\n")
	require.NoError(t, err)
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	events, err := EncodeCanonical(raw)
	require.NoError(t, err)
	require.Equal(t, append(completeTurns, acf.TextTurn{Role: "user", Text: "and the metro area?"}),
		acf.ExtractTextTurns(events),
		"a later Claude append through the original descriptor must remain visible at the path")
	completeRaw, err := os.ReadFile(completePath)
	require.NoError(t, err)
	completeEvents, err := EncodeCanonical(completeRaw)
	require.NoError(t, err)
	require.Equal(t, append(completeTurns, acf.TextTurn{Role: "user", Text: "and the metro area?"}),
		acf.ExtractTextTurns(completeEvents))
}

func TestStableSyntheticMirrorStorageRemainsLinearAcrossTenThousandDeltas(t *testing.T) {
	home := t.TempDir()
	projectDir := filepath.Join(home, ".claude", "projects", encodeProjectDir(home))
	require.NoError(t, os.MkdirAll(projectDir, 0o755))
	artifactID := acf.NewID()
	stable := filepath.Join(projectDir, artifactID+".jsonl")
	base := time.Date(2026, 7, 20, 18, 0, 0, 0, time.UTC)
	planned := []acf.TextTurn{{Role: "user", Text: "base"}}
	var encoded bytes.Buffer
	encoded.WriteString(transcodeToClaudeSessionWithThread(
		planned, artifactID, artifactID, acf.MainBranch, home, "codex", "", base,
	))
	parentUUID := deterministicUUID(artifactID, 0)
	for index := 1; index <= 10_000; index++ {
		turn := acf.TextTurn{Role: "user", Text: "delta"}
		if index%2 == 1 {
			turn = acf.TextTurn{Role: "assistant", Text: "delta"}
		}
		planned = append(planned, turn)
		encoded.WriteString(transcodeClaudeTurnAppend(
			[]acf.TextTurn{turn}, artifactID, artifactID, acf.MainBranch,
			true, planned, nil, index, home, base, parentUUID, "delta",
		))
		parentUUID = deterministicUUID(artifactID, index)
	}
	created, err := writeClaudeSessionExclusive(stable, encoded.Bytes(), 0o644)
	require.NoError(t, err)
	require.True(t, created)
	entries, err := os.ReadDir(projectDir)
	require.NoError(t, err)
	require.Len(t, entries, 1,
		"ordinary canonical growth must stay on one stable path, not create one full snapshot per delta")
	info, err := os.Stat(stable)
	require.NoError(t, err)
	require.Equal(t, int64(encoded.Len()), info.Size())
	require.Less(t, info.Size(), int64(16<<20),
		"10,000 encoded deltas should consume one linear JSONL log, not quadratic full copies")
	matches, err := claudeSessionMatches(encoded.Bytes(), planned, artifactID, artifactID, acf.MainBranch)
	require.NoError(t, err)
	require.True(t, matches, "the final single-file leaf must remain resumable after 10,000 deltas")
}

func TestMaterializeConversationSession_RepeatedOrdinaryDeltasStayOnStableMirror(t *testing.T) {
	home := t.TempDir()
	artifactID := acf.NewID()
	base := time.Date(2026, 7, 20, 18, 0, 0, 0, time.UTC)
	art := acf.Artifact{
		ArtifactID: artifactID, Kind: acf.KindConversation, Scope: acf.ScopeGlobal,
		Name: "thread.jsonl", CreatedAt: base, UpdatedAt: base,
	}
	a := &Adapter{HomeDir: home}
	turns := []acf.TextTurn{{Role: "user", Text: "base"}}
	stable := filepath.Join(home, ".claude", "projects", encodeProjectDir(home), artifactID+".jsonl")
	for index := 0; index < 64; index++ {
		written, ok, err := a.MaterializeConversationSession(
			art, canonicalConversationHead(t, artifactID, turns...), "codex",
		)
		require.NoError(t, err)
		require.True(t, ok)
		require.Equal(t, stable, written)
		role := "assistant"
		if index%2 == 1 {
			role = "user"
		}
		turns = append(turns, acf.TextTurn{Role: role, Text: "delta"})
		art.UpdatedAt = art.UpdatedAt.Add(time.Second)
	}
	projectEntries, err := os.ReadDir(filepath.Dir(stable))
	require.NoError(t, err)
	require.Len(t, projectEntries, 1, "ordinary repeated writes must not create recovery snapshots")
	raw, err := os.ReadFile(stable)
	require.NoError(t, err)
	require.Equal(t, turns[:len(turns)-1], claudeLeafTextTurns(t, raw))
}

func TestMaterializeConversationSession_RepeatedClaudeContinuationsStayOnStableMirror(t *testing.T) {
	home := t.TempDir()
	artifactID := acf.NewID()
	base := time.Date(2026, 7, 20, 18, 0, 0, 0, time.UTC)
	art := acf.Artifact{
		ArtifactID: artifactID, Kind: acf.KindConversation, Scope: acf.ScopeGlobal,
		Name: "thread.jsonl", CreatedAt: base, UpdatedAt: base,
	}
	a := &Adapter{HomeDir: home}
	turns := []acf.TextTurn{{Role: "user", Text: "base prompt"}}
	stable, ok, err := a.MaterializeConversationSession(
		art, canonicalConversationHead(t, artifactID, turns...), "codex",
	)
	require.NoError(t, err)
	require.True(t, ok)
	stableInfo, err := os.Stat(stable)
	require.NoError(t, err)

	// More cycles than the recovery collision budget prove that ordinary
	// Claude-authored continuations do not consume one full recovery generation
	// per turn. Each remote prompt and native answer stays on the same inode.
	for index := 0; index < 40; index++ {
		answer := fmt.Sprintf("native answer %d", index)
		require.NoError(t, appendNativeClaudeAssistant(stable, artifactID, answer))
		turns = append(turns, acf.TextTurn{Role: "assistant", Text: answer})
		art.UpdatedAt = art.UpdatedAt.Add(time.Second)
		written, materialized, materializeErr := a.MaterializeConversationSession(
			art, canonicalConversationHead(t, artifactID, turns...), "claude-code",
		)
		require.NoError(t, materializeErr)
		require.True(t, materialized)
		require.Equal(t, stable, written)

		prompt := fmt.Sprintf("remote prompt %d", index+1)
		turns = append(turns, acf.TextTurn{Role: "user", Text: prompt})
		art.UpdatedAt = art.UpdatedAt.Add(time.Second)
		written, materialized, materializeErr = a.MaterializeConversationSession(
			art, canonicalConversationHead(t, artifactID, turns...), "codex",
		)
		require.NoError(t, materializeErr)
		require.True(t, materialized)
		require.Equal(t, stable, written)
	}

	stableAfter, err := os.Stat(stable)
	require.NoError(t, err)
	require.True(t, os.SameFile(stableInfo, stableAfter))
	entries, err := os.ReadDir(filepath.Dir(stable))
	require.NoError(t, err)
	require.Len(t, entries, 1, "native continuations must not allocate one recovery snapshot per turn")
	raw, err := os.ReadFile(stable)
	require.NoError(t, err)
	require.Equal(t, turns, claudeLeafTextTurns(t, raw))
	requireClaudeParentChain(t, raw)
}

func TestMaterializeConversationSession_RepeatedDesktopSyntheticRowsStayOnStableMirror(t *testing.T) {
	home := t.TempDir()
	artifactID := acf.NewID()
	base := time.Date(2026, 7, 20, 18, 0, 0, 0, time.UTC)
	art := acf.Artifact{
		ArtifactID: artifactID, Kind: acf.KindConversation, Scope: acf.ScopeGlobal,
		Name: "thread.jsonl", CreatedAt: base, UpdatedAt: base,
	}
	a := &Adapter{HomeDir: home}
	turns := []acf.TextTurn{{Role: "user", Text: "question 0"}}
	stable, ok, err := a.MaterializeConversationSession(
		art, canonicalConversationHead(t, artifactID, turns...), "codex",
	)
	require.NoError(t, err)
	require.True(t, ok)
	stableInfo, err := os.Stat(stable)
	require.NoError(t, err)

	for index := 0; index < 40; index++ {
		// Exercise both observed Desktop shapes: the original Aplexica
		// last-prompt still names the user, or a later bookkeeping row names the
		// reserved synthetic assistant. Neither row is a canonical answer.
		require.NoError(t, appendDesktopSyntheticAssistant(
			stable, artifactID, fmt.Sprintf("bookkeeping %d", index), index%2 == 1,
		))
		answer := fmt.Sprintf("real answer %d", index)
		turns = append(turns, acf.TextTurn{Role: "assistant", Text: answer})
		art.UpdatedAt = art.UpdatedAt.Add(time.Second)
		written, materialized, materializeErr := a.MaterializeConversationSession(
			art, canonicalConversationHead(t, artifactID, turns...), "codex",
		)
		require.NoError(t, materializeErr)
		require.True(t, materialized)
		require.Equal(t, stable, written)

		prompt := fmt.Sprintf("question %d", index+1)
		turns = append(turns, acf.TextTurn{Role: "user", Text: prompt})
		art.UpdatedAt = art.UpdatedAt.Add(time.Second)
		written, materialized, materializeErr = a.MaterializeConversationSession(
			art, canonicalConversationHead(t, artifactID, turns...), "codex",
		)
		require.NoError(t, materializeErr)
		require.True(t, materialized)
		require.Equal(t, stable, written)
	}

	stableAfter, err := os.Stat(stable)
	require.NoError(t, err)
	require.True(t, os.SameFile(stableInfo, stableAfter))
	entries, err := os.ReadDir(filepath.Dir(stable))
	require.NoError(t, err)
	require.Len(t, entries, 1, "Desktop bookkeeping must not allocate one recovery snapshot per answer")
	raw, err := os.ReadFile(stable)
	require.NoError(t, err)
	require.True(t, claudeRowsContainModel(raw, "<synthetic>"))
	require.Equal(t, turns, claudeLeafTextTurns(t, raw))
	requireClaudeParentChain(t, raw)
}

func TestClaudeSyntheticContinuation_RequiresExactStampedBaseHash(t *testing.T) {
	artifactID := acf.NewID()
	baseTurns := []acf.TextTurn{{Role: "user", Text: "base prompt"}}
	raw := []byte(transcodeToClaudeSessionWithThread(
		baseTurns, artifactID, artifactID, acf.MainBranch,
		"/Users/exampleuser", "codex", "", time.Date(2026, 7, 20, 18, 0, 0, 0, time.UTC),
	))
	continuation := []acf.TextTurn{{Role: "assistant", Text: "native answer"}}
	nativeAppendix, err := claudeAssistantAppendix(
		artifactID, deterministicUUID(artifactID, 0), "native answer", "claude-opus-4-8", true,
	)
	require.NoError(t, err)
	raw = append(raw, nativeAppendix...)
	planned := append(append([]acf.TextTurn(nil), baseTurns...), continuation...)

	matches, err := claudeSessionMatches(raw, planned, artifactID, artifactID, acf.MainBranch)
	require.NoError(t, err)
	require.True(t, matches)

	baseHash := adapter.ConversationTurnsHash(baseTurns)
	tampered := bytes.ReplaceAll(raw, []byte(baseHash), []byte(strings.Repeat("0", len(baseHash))))
	matches, err = claudeSessionMatches(tampered, planned, artifactID, artifactID, acf.MainBranch)
	require.NoError(t, err)
	require.False(t, matches, "an unstamped continuation cannot weaken the exact generated-base proof")
	_, appendable, err := claudeSessionAppendix(
		tampered, planned,
		artifactID, artifactID, acf.MainBranch, "/Users/exampleuser",
		time.Date(2026, 7, 20, 18, 0, 0, 0, time.UTC),
	)
	require.NoError(t, err)
	require.False(t, appendable, "an exact visible leaf still requires an authenticated generated base")
	_, appendable, err = claudeSessionAppendix(
		tampered,
		append(planned, acf.TextTurn{Role: "user", Text: "next prompt"}),
		artifactID, artifactID, acf.MainBranch, "/Users/exampleuser",
		time.Date(2026, 7, 20, 18, 0, 0, 0, time.UTC),
	)
	require.NoError(t, err)
	require.False(t, appendable)
}

func TestMaterializeConversationSession_LateSiblingRaceConvergesOnNextImportAndRematerialize(t *testing.T) {
	home := t.TempDir()
	artifactID := acf.NewID()
	base := time.Date(2026, 7, 18, 21, 48, 30, 0, time.UTC)
	art := acf.Artifact{
		ArtifactID: artifactID, Kind: acf.KindConversation, Scope: acf.ScopeGlobal,
		Name: "thread.jsonl", CreatedAt: base, UpdatedAt: base,
	}
	a := &Adapter{HomeDir: home}
	promptTurns := []acf.TextTurn{
		{Role: "user", Text: "what is capital of France?"},
		{Role: "assistant", Text: "Paris."},
		{Role: "user", Text: "how many people live in paris?"},
	}
	stable, ok, err := a.MaterializeConversationSession(
		art, canonicalConversationHead(t, artifactID, promptTurns...), "codex",
	)
	require.NoError(t, err)
	require.True(t, ok)
	promptRaw, err := os.ReadFile(stable)
	require.NoError(t, err)
	staleParent := lastClaudeConversationalUUID(promptRaw)

	completeTurns := append(append([]acf.TextTurn(nil), promptTurns...),
		acf.TextTurn{Role: "assistant", Text: "About 2.1 million."})
	art.UpdatedAt = base.Add(13 * time.Second)
	written, ok, err := a.MaterializeConversationSession(
		art, canonicalConversationHead(t, artifactID, completeTurns...), "codex",
	)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, stable, written)

	// This models Claude's unavoidable late write after Aplexica's post-fsync
	// validation: Claude still has the old leaf and publishes a sibling user
	// turn after the remote answer was appended.
	require.NoError(t, appendNativeClaudeUserAtParent(
		stable, artifactID, staleParent, "and the metro area?", true,
	))
	racedRaw, err := os.ReadFile(stable)
	require.NoError(t, err)
	racedVisible := claudeLeafTextTurns(t, racedRaw)
	require.GreaterOrEqual(t, len(racedVisible), len(promptTurns))
	require.Equal(t, promptTurns, racedVisible[:len(promptTurns)])
	require.NotEqual(t, completeTurns,
		racedVisible,
		"the fixture must reproduce Claude selecting one sibling leaf")

	// Seed the canonical store at the remote-answer head, then run the real
	// watcher dispatch against the raced mirror. Its thread-ref merge must import
	// both physical siblings before the following materialization linearizes the
	// full union.
	store := &acf.Store{Root: filepath.Join(home, "store")}
	require.NoError(t, store.Init())
	require.NoError(t, store.WriteArtifact(acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion,
		ArtifactID:       artifactID, Kind: acf.KindConversation, Scope: acf.ScopeGlobal,
		Name: "thread.jsonl", SourcePath: filepath.Join(home, "canonical", "thread.jsonl"),
		CreatedAt: base, UpdatedAt: base.Add(13 * time.Second),
	}))
	storeHead := canonicalConversationHead(t, artifactID, completeTurns...)
	storeHead.Type = acf.EventTypeCreate
	require.NoError(t, store.AppendEvent(acf.KindConversation, storeHead))
	a.CanonicalConversations = true
	ids, err := a.Import(t.Context(), store, stable)
	require.NoError(t, err)
	require.Equal(t, []string{artifactID}, ids)
	storeEvents, err := store.ReadEvents(acf.KindConversation, artifactID)
	require.NoError(t, err)
	materialized, materializedOK, err := acf.MaterializedConversationPayload(storeEvents)
	require.NoError(t, err)
	require.True(t, materializedOK)
	convergedTurns := acf.ExtractTextTurns(materialized.Events)
	require.Equal(t, append(completeTurns,
		acf.TextTurn{Role: "user", Text: "and the metro area?"}), convergedTurns)
	art.UpdatedAt = base.Add(15 * time.Second)
	recovery, ok, err := a.MaterializeConversationSession(
		art, canonicalConversationHead(t, artifactID, convergedTurns...), "codex",
	)
	require.NoError(t, err)
	require.False(t, ok)
	require.Equal(t, stable, recovery)
	stableAfter, err := os.ReadFile(stable)
	require.NoError(t, err)
	require.Equal(t, racedRaw, stableAfter, "eventual repair must not erase either raced branch")
	entries, err := os.ReadDir(filepath.Dir(stable))
	require.NoError(t, err)
	require.Len(t, entries, 1, "a sibling race must not multiply one thread into recovery conversations")
}

func TestMaterializeConversationSession_BranchesShorterRemoteStateWithoutOverwritingLocalSession(t *testing.T) {
	home := t.TempDir()
	artifactID := acf.NewID()
	source := filepath.Join(home, ".claude", "projects", "-Users-exampleuser", "native-session.jsonl")
	synthetic := filepath.Join(home, ".claude", "projects", encodeProjectDir(home), artifactID+".jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(synthetic), 0o755))
	existing := transcodeToClaudeSession([]acf.TextTurn{
		{Role: "user", Text: "hello"},
		{Role: "assistant", Text: "hi"},
		{Role: "user", Text: "newer question"},
		{Role: "assistant", Text: "newer answer"},
	}, artifactID, home, "claude-code", "native-session.jsonl", time.Date(2026, 7, 1, 20, 55, 0, 0, time.UTC))
	// Claude appends native rows without Aplexica's thread stamp. Even one such
	// row means this is no longer an untouched generated snapshot.
	existing += `{"type":"user","sessionId":"` + artifactID + `","message":{"role":"user","content":"native continuation"}}` + "\n"
	require.NoError(t, os.WriteFile(synthetic, []byte(existing), 0o644))

	art := acf.Artifact{
		ArtifactID: artifactID,
		Kind:       acf.KindConversation,
		Scope:      acf.ScopeGlobal,
		Name:       "native-session.jsonl",
		SourcePath: source,
		UpdatedAt:  time.Date(2026, 7, 1, 20, 57, 53, 0, time.UTC),
	}
	head := canonicalConversationHead(t, artifactID,
		acf.TextTurn{Role: "user", Text: "hello"},
		acf.TextTurn{Role: "assistant", Text: "older answer"},
	)

	a := &Adapter{HomeDir: home}
	written, ok, err := a.MaterializeConversationSession(art, head, "claude-code")
	require.NoError(t, err)
	require.False(t, ok)
	require.Equal(t, synthetic, written)

	data, err := os.ReadFile(synthetic)
	require.NoError(t, err)
	require.Contains(t, string(data), "newer question")
	require.Contains(t, string(data), "newer answer")
	require.Contains(t, string(data), "native continuation")

	entries, err := os.ReadDir(filepath.Dir(synthetic))
	require.NoError(t, err)
	require.Len(t, entries, 1)
}

func TestMaterializeConversationSession_PreservesNonPrefixStaleContinuation(t *testing.T) {
	home := t.TempDir()
	artifactID := acf.NewID()
	source := filepath.Join(home, ".claude", "projects", "-Users-exampleuser", "native-session.jsonl")
	synthetic := filepath.Join(home, ".claude", "projects", encodeProjectDir(home), artifactID+".jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(synthetic), 0o755))
	base := []acf.TextTurn{
		{Role: "user", Text: "q1"},
		{Role: "assistant", Text: "a1"},
	}
	staleNative := transcodeToClaudeSession(base, artifactID, home, "codex", "", time.Now())
	staleNative += `{"type":"user","uuid":"native-u","parentUuid":"missing-old-leaf","sessionId":"` + artifactID + `","message":{"role":"user","content":"q-from-stale-claude"}}` + "\n"
	staleNative += `{"type":"assistant","uuid":"native-a","parentUuid":"native-u","sessionId":"` + artifactID + `","message":{"role":"assistant","content":[{"type":"text","text":"a-from-stale-claude"}]}}` + "\n"
	require.NoError(t, os.WriteFile(synthetic, []byte(staleNative), 0o644))

	canonical := []acf.TextTurn{
		{Role: "user", Text: "q1"},
		{Role: "assistant", Text: "a1"},
		{Role: "user", Text: "q-from-codex"},
		{Role: "assistant", Text: "a-from-codex"},
		{Role: "user", Text: "q-from-stale-claude"},
		{Role: "assistant", Text: "a-from-stale-claude"},
	}
	art := acf.Artifact{
		ArtifactID: artifactID, Kind: acf.KindConversation, Scope: acf.ScopeGlobal,
		Name: "native-session.jsonl", SourcePath: source,
		UpdatedAt: time.Date(2026, 7, 17, 19, 0, 0, 0, time.UTC),
	}
	head := canonicalConversationHead(t, artifactID, canonical...)
	a := &Adapter{HomeDir: home}
	written, ok, err := a.MaterializeConversationSession(art, head, "claude-code")
	require.NoError(t, err)
	require.False(t, ok)
	require.Equal(t, synthetic, written)

	raw, err := os.ReadFile(synthetic)
	require.NoError(t, err)
	events, err := EncodeCanonical(raw)
	require.NoError(t, err)
	require.Equal(t, append(base,
		acf.TextTurn{Role: "user", Text: "q-from-stale-claude"},
		acf.TextTurn{Role: "assistant", Text: "a-from-stale-claude"},
	), acf.ExtractTextTurns(events))
	require.Contains(t, string(raw), "missing-old-leaf",
		"a non-prefix local continuation must never be atomically replaced while Claude may hold it open")
	require.NotContains(t, string(raw), "q-from-codex")

	entries, err := os.ReadDir(filepath.Dir(synthetic))
	require.NoError(t, err)
	require.Len(t, entries, 1, "a non-prefix continuation must not create another resume entry")
}

func TestMaterializeConversationSession_LocalSuffixBeforeAnswerCreatesDeterministicRemoteSession(t *testing.T) {
	home := t.TempDir()
	artifactID := acf.NewID()
	base := time.Date(2026, 7, 18, 21, 48, 30, 0, time.UTC)
	art := acf.Artifact{
		ArtifactID: artifactID, Kind: acf.KindConversation, Scope: acf.ScopeGlobal,
		Name: "thread.jsonl", CreatedAt: base, UpdatedAt: base,
	}
	a := &Adapter{HomeDir: home}
	promptTurns := []acf.TextTurn{
		{Role: "user", Text: "what is capital of France?"},
		{Role: "assistant", Text: "Paris."},
		{Role: "user", Text: "how many people live in paris?"},
	}
	primary, ok, err := a.MaterializeConversationSession(art, canonicalConversationHead(t, artifactID, promptTurns...), "codex")
	require.NoError(t, err)
	require.True(t, ok)
	before, err := os.Stat(primary)
	require.NoError(t, err)
	require.NoError(t, appendNativeClaudeUser(primary, artifactID, "and the metro area?"))

	completeTurns := append(append([]acf.TextTurn(nil), promptTurns...),
		acf.TextTurn{Role: "assistant", Text: "About 2.1 million."})
	art.UpdatedAt = base.Add(13 * time.Second)
	remote, ok, err := a.MaterializeConversationSession(art, canonicalConversationHead(t, artifactID, completeTurns...), "codex")
	require.NoError(t, err)
	require.False(t, ok)
	require.Equal(t, primary, remote)
	after, err := os.Stat(primary)
	require.NoError(t, err)
	require.True(t, os.SameFile(before, after), "the active Claude inode must never be replaced")

	primaryRaw, err := os.ReadFile(primary)
	require.NoError(t, err)
	primaryEvents, err := EncodeCanonical(primaryRaw)
	require.NoError(t, err)
	require.Equal(t, append(promptTurns, acf.TextTurn{Role: "user", Text: "and the metro area?"}),
		acf.ExtractTextTurns(primaryEvents))
	require.NotContains(t, string(primaryRaw), "About 2.1 million.",
		"the remote answer must not be appended after a newer local question")
	requireClaudeParentChain(t, primaryRaw)

	retry, ok, err := a.MaterializeConversationSession(art, canonicalConversationHead(t, artifactID, completeTurns...), "codex")
	require.NoError(t, err)
	require.False(t, ok)
	require.Equal(t, primary, retry)

	// The user keeps working in the preserved original session after the first
	// recovery generation exists. Its bytes therefore change again. That must
	// resolve inside the one stable conversation rather than keying another full
	// snapshot from the changed original bytes.
	//
	// Once canonical has absorbed BOTH local questions, the mirror's visible
	// turns are an ordered subsequence of the plan and the shipped
	// subsequence-proven rebuild converges it in place. That rebuild used to be
	// unreachable here: appendNativeClaudeUser writes its row without a new
	// last-prompt, and the walk refused to descend below the recorded leaf, so
	// the projection looked unspanned and every pass bailed one statement before
	// the loss proof — the state that re-queued forever.
	require.NoError(t, appendNativeClaudeUser(primary, artifactID, "continue in the original session"))
	laterTurns := append(append([]acf.TextTurn(nil), completeTurns...),
		acf.TextTurn{Role: "user", Text: "and the metro area?"},
		acf.TextTurn{Role: "user", Text: "continue in the original session"},
	)
	art.UpdatedAt = base.Add(20 * time.Second)
	later, ok, err := a.MaterializeConversationSession(
		art, canonicalConversationHead(t, artifactID, laterTurns...), "codex",
	)
	require.NoError(t, err)
	require.True(t, ok, "a mirror whose every turn canonical now holds must converge, not re-queue")
	require.Equal(t, primary, later)
	laterRaw, err := os.ReadFile(primary)
	require.NoError(t, err)
	laterEvents, err := EncodeCanonical(laterRaw)
	require.NoError(t, err)
	require.Equal(t, laterTurns, acf.ExtractTextTurns(laterEvents),
		"the rebuild must lose neither local question")
	entries, err := os.ReadDir(filepath.Dir(primary))
	require.NoError(t, err)
	require.Len(t, entries, 1,
		"divergence must preserve one stable conversation instead of creating recovery snapshots")
}

func TestMaterializeConversationSession_ExactPrewriteRacePreservesStableMirrorAndFallsBack(t *testing.T) {
	home := t.TempDir()
	artifactID := acf.NewID()
	base := time.Date(2026, 7, 18, 21, 48, 30, 0, time.UTC)
	art := acf.Artifact{
		ArtifactID: artifactID, Kind: acf.KindConversation, Scope: acf.ScopeGlobal,
		Name: "thread.jsonl", CreatedAt: base, UpdatedAt: base,
	}
	a := &Adapter{HomeDir: home}
	promptTurns := []acf.TextTurn{
		{Role: "user", Text: "what is capital of France?"},
		{Role: "assistant", Text: "Paris."},
		{Role: "user", Text: "how many people live in paris?"},
	}
	primary, ok, err := a.MaterializeConversationSession(art, canonicalConversationHead(t, artifactID, promptTurns...), "codex")
	require.NoError(t, err)
	require.True(t, ok)
	before, err := os.Stat(primary)
	require.NoError(t, err)

	completeTurns := append(append([]acf.TextTurn(nil), promptTurns...),
		acf.TextTurn{Role: "assistant", Text: "About 2.1 million."})
	art.UpdatedAt = base.Add(13 * time.Second)
	hookCalls := 0
	remote, ok, _, err := a.materializeConversationSession(
		art,
		canonicalConversationHead(t, artifactID, completeTurns...),
		"codex",
		func(path string) error {
			hookCalls++
			return appendNativeClaudeUser(path, artifactID, "and the metro area?")
		},
		nil,
	)
	require.NoError(t, err)
	require.False(t, ok)
	require.Equal(t, 1, hookCalls)
	require.Equal(t, primary, remote)
	after, err := os.Stat(primary)
	require.NoError(t, err)
	require.True(t, os.SameFile(before, after), "pre-write validation must preserve the active inode")

	primaryRaw, err := os.ReadFile(primary)
	require.NoError(t, err)
	primaryEvents, err := EncodeCanonical(primaryRaw)
	require.NoError(t, err)
	require.Equal(t, append(promptTurns, acf.TextTurn{Role: "user", Text: "and the metro area?"}),
		acf.ExtractTextTurns(primaryEvents))
	require.NotContains(t, string(primaryRaw), "About 2.1 million.")

	entries, err := os.ReadDir(filepath.Dir(primary))
	require.NoError(t, err)
	require.Len(t, entries, 1)
}

func TestMaterializeConversationSession_PostFsyncLeafValidationDetectsSiblingRace(t *testing.T) {
	home := t.TempDir()
	artifactID := acf.NewID()
	base := time.Date(2026, 7, 18, 21, 48, 30, 0, time.UTC)
	art := acf.Artifact{
		ArtifactID: artifactID, Kind: acf.KindConversation, Scope: acf.ScopeGlobal,
		Name: "thread.jsonl", CreatedAt: base, UpdatedAt: base,
	}
	a := &Adapter{HomeDir: home}
	promptTurns := []acf.TextTurn{
		{Role: "user", Text: "what is capital of France?"},
		{Role: "assistant", Text: "Paris."},
		{Role: "user", Text: "how many people live in paris?"},
	}
	stable, ok, err := a.MaterializeConversationSession(
		art, canonicalConversationHead(t, artifactID, promptTurns...), "codex",
	)
	require.NoError(t, err)
	require.True(t, ok)
	promptRaw, err := os.ReadFile(stable)
	require.NoError(t, err)
	staleParent := lastClaudeConversationalUUID(promptRaw)
	completeTurns := append(append([]acf.TextTurn(nil), promptTurns...),
		acf.TextTurn{Role: "assistant", Text: "About 2.1 million."})
	art.UpdatedAt = base.Add(13 * time.Second)
	afterCalls := 0
	remote, ok, _, err := a.materializeConversationSession(
		art,
		canonicalConversationHead(t, artifactID, completeTurns...),
		"codex",
		nil,
		func(path string) error {
			afterCalls++
			return appendNativeClaudeUserAtParent(
				path, artifactID, staleParent, "and the metro area?", true,
			)
		},
	)
	require.NoError(t, err)
	require.False(t, ok)
	require.Equal(t, 1, afterCalls)
	require.Equal(t, stable, remote,
		"post-fsync validation must reject a sibling parent graph without creating another session")
	stableRaw, err := os.ReadFile(stable)
	require.NoError(t, err)
	stableEvents, err := EncodeCanonical(stableRaw)
	require.NoError(t, err)
	require.Equal(t, append(completeTurns,
		acf.TextTurn{Role: "user", Text: "and the metro area?"}),
		acf.ExtractTextTurns(stableEvents),
		"post-fsync detection must preserve both physical sibling rows")
	entries, err := os.ReadDir(filepath.Dir(stable))
	require.NoError(t, err)
	require.Len(t, entries, 1)
}

func TestMaterializeConversationSession_ExactPrewriteValidationDetectsMiddleRewriteOutsideCacheProbes(t *testing.T) {
	home := t.TempDir()
	artifactID := acf.NewID()
	base := time.Date(2026, 7, 18, 21, 48, 30, 0, time.UTC)
	art := acf.Artifact{
		ArtifactID: artifactID, Kind: acf.KindConversation, Scope: acf.ScopeGlobal,
		Name: "thread.jsonl", CreatedAt: base, UpdatedAt: base,
	}
	a := &Adapter{HomeDir: home}
	promptTurns := []acf.TextTurn{
		{Role: "user", Text: "what is capital of France?"},
		{Role: "assistant", Text: "Paris."},
		{Role: "user", Text: "how many people live in paris?"},
	}
	stable, ok, err := a.MaterializeConversationSession(
		art, canonicalConversationHead(t, artifactID, promptTurns...), "codex",
	)
	require.NoError(t, err)
	require.True(t, ok)
	padding := strings.Repeat("a", 32<<10)
	metadata, err := json.Marshal(map[string]any{
		"type":              "queue-operation",
		"padding":           padding,
		"sessionId":         artifactID,
		"aplexicaThreadId":  artifactID,
		"aplexicaBranchId":  acf.MainBranch,
		"aplexicaTurnsHash": adapter.ConversationTurnsHash(promptTurns),
		"aplexicaTurnCount": len(promptTurns),
	})
	require.NoError(t, err)
	f, err := os.OpenFile(stable, os.O_WRONLY|os.O_APPEND, 0)
	require.NoError(t, err)
	_, err = f.Write(append(metadata, '\n'))
	require.NoError(t, err)
	require.NoError(t, f.Close())

	completeTurns := append(append([]acf.TextTurn(nil), promptTurns...),
		acf.TextTurn{Role: "assistant", Text: "About 2.1 million."})
	art.UpdatedAt = base.Add(13 * time.Second)
	hookCalls := 0
	remote, ok, _, err := a.materializeConversationSession(
		art,
		canonicalConversationHead(t, artifactID, completeTurns...),
		"codex",
		func(path string) error {
			hookCalls++
			raw, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			paddingStart := bytes.Index(raw, []byte(padding))
			require.GreaterOrEqual(t, paddingStart, 0)
			offset := paddingStart + len(padding)/2
			require.Greater(t, offset, convHeadSampleBytes)
			require.Less(t, offset+64, len(raw)-convTailSampleBytes)
			writer, openErr := os.OpenFile(path, os.O_WRONLY, 0)
			if openErr != nil {
				return openErr
			}
			_, writeErr := writer.WriteAt([]byte(strings.Repeat("b", 64)), int64(offset))
			if writeErr == nil {
				writeErr = writer.Sync()
			}
			closeErr := writer.Close()
			if writeErr != nil {
				return writeErr
			}
			return closeErr
		},
		nil,
	)
	require.NoError(t, err)
	require.False(t, ok)
	require.Equal(t, 1, hookCalls)
	require.Equal(t, stable, remote,
		"a same-size middle rewrite must fail exact append authority without creating another session")
	stableAfter, err := os.ReadFile(stable)
	require.NoError(t, err)
	require.Contains(t, string(stableAfter), strings.Repeat("b", 64))
	require.NotContains(t, string(stableAfter), "About 2.1 million.")
	entries, err := os.ReadDir(filepath.Dir(stable))
	require.NoError(t, err)
	require.Len(t, entries, 1)
}

func TestConversationSessionPath_FallsBackWhenSourcePathIsNotLocalClaudeProject(t *testing.T) {
	home := t.TempDir()
	artifactID := acf.NewID()
	art := acf.Artifact{
		ArtifactID: artifactID,
		Kind:       acf.KindConversation,
		Scope:      acf.ScopeGlobal,
		Name:       "remote-session.jsonl",
		SourcePath: filepath.Join(t.TempDir(), ".claude", "projects", "-Users-peer", "remote-session.jsonl"),
		UpdatedAt:  time.Date(2026, 7, 1, 20, 57, 53, 0, time.UTC),
	}
	head := canonicalConversationHead(t, artifactID,
		acf.TextTurn{Role: "user", Text: "What is the distance to Moon?"},
		acf.TextTurn{Role: "assistant", Text: "384,400 km."},
	)

	a := &Adapter{HomeDir: home}
	path, ok, err := a.ConversationSessionPath(art, head, "claude-code")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, filepath.Join(home, ".claude", "projects", encodeProjectDir(home), artifactID+".jsonl"), path)
}

func TestConversationSessionPath_NonMainBranchDoesNotOverwriteLocalSourcePath(t *testing.T) {
	home := t.TempDir()
	source := filepath.Join(home, ".claude", "projects", "-Users-exampleuser", "native-session.jsonl")
	artifactID := acf.NewID()
	art := acf.Artifact{
		ArtifactID: artifactID,
		Kind:       acf.KindConversation,
		Scope:      acf.ScopeGlobal,
		Name:       "native-session.jsonl",
		SourcePath: source,
		UpdatedAt:  time.Date(2026, 7, 1, 20, 57, 53, 0, time.UTC),
	}
	head := canonicalConversationHead(t, artifactID,
		acf.TextTurn{Role: "user", Text: "Try this risky path?"},
		acf.TextTurn{Role: "assistant", Text: "Use a branch instead."},
	)
	head.Branch = "review-branch"

	a := &Adapter{HomeDir: home}
	wantSessionID := claudeSyncedSessionID(artifactID, "review-branch")
	wantPath := filepath.Join(home, ".claude", "projects", encodeProjectDir(home), wantSessionID+".jsonl")

	path, ok, err := a.ConversationSessionPath(art, head, "claude-code")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, wantPath, path)

	written, ok, err := a.MaterializeConversationSession(art, head, "claude-code")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, wantPath, written)

	_, err = os.Stat(source)
	require.ErrorIs(t, err, os.ErrNotExist, "fork materialization must not overwrite the original native Claude session")

	data, err := os.ReadFile(wantPath)
	require.NoError(t, err)
	require.Contains(t, string(data), `"aplexicaThreadId":"`+artifactID+`"`)
	require.Contains(t, string(data), `"aplexicaBranchId":"review-branch"`)
	ref, ok := claudeSessionThreadRef(data)
	require.True(t, ok)
	require.Equal(t, artifactID, ref.ArtifactID)
	require.Equal(t, "review-branch", ref.BranchID)
	require.Equal(t, wantSessionID, claudeSessionThreadID(data))
}

func TestEncodeProjectDir(t *testing.T) {
	require.Equal(t, "-Users-testuser", encodeProjectDir("/Users/testuser"))
	// Windows paths: separators AND the drive colon must flatten, or the
	// encoded name carries an illegal `C:` segment + embedded separators
	// (mkdir ~/.claude/projects/C: fails on Windows).
	require.Equal(t, "C--Users-testuser-proj", encodeProjectDir(`C:\Users\testuser\proj`))
}

func TestTranscode_NoTurns_Empty(t *testing.T) {
	require.Equal(t, "", transcodeToClaudeSession(nil, "x", "/Users/testuser", "codex", "f", time.Now()))
	_ = strings.TrimSpace
}

// A materialized assistant row must carry a PRESENT, recognized model id — not
// the bogus "aplexica-synced" (which Claude Code rejects with a "could not be
// restored" warning) and NOT omitted: Claude Code's /resume calls .includes() on
// message.model, so a missing model crashes resume ("undefined is not an object
// (evaluating 'e.includes')").
func TestTranscode_AssistantRow_ValidModel(t *testing.T) {
	turns := []acf.TextTurn{
		{Role: "user", Text: "what is 9+9?"},
		{Role: "assistant", Text: "18."},
	}
	out := transcodeToClaudeSession(turns, "tid-model", "/Users/testuser", "claude-code", "f", time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC))
	require.NotEmpty(t, out)
	require.NotContains(t, out, "aplexica-synced",
		"must not write the bogus model id that triggers the restore warning")
	sawAssistant := false
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if !strings.Contains(line, `"type":"assistant"`) {
			continue
		}
		sawAssistant = true
		var row map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &row))
		msg, _ := row["message"].(map[string]any)
		model, ok := msg["model"].(string)
		require.True(t, ok && model != "",
			"assistant message.model must be a present non-empty string (omitting it crashes /resume)")
		require.NotEqual(t, "aplexica-synced", model)
	}
	require.True(t, sawAssistant, "expected at least one assistant row")
}

func appendNativeClaudeAssistant(path, sessionID, text string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	appendix, err := claudeAssistantAppendix(
		sessionID, lastClaudeConversationalUUID(raw), text, "claude-opus-4-8", true,
	)
	if err != nil {
		return err
	}
	return appendClaudeTestRows(path, appendix)
}

func claudeRowsContainModel(raw []byte, model string) bool {
	for _, line := range bytes.Split(raw, []byte("\n")) {
		var row struct {
			Message *struct {
				Model string `json:"model"`
			} `json:"message"`
		}
		if json.Unmarshal(bytes.TrimSpace(line), &row) == nil && row.Message != nil && row.Message.Model == model {
			return true
		}
	}
	return false
}

func appendDesktopSyntheticAssistant(path, sessionID, text string, updateLeaf bool) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	appendix, err := claudeAssistantAppendix(
		sessionID, lastClaudeConversationalUUID(raw), text, "<synthetic>", updateLeaf,
	)
	if err != nil {
		return err
	}
	return appendClaudeTestRows(path, appendix)
}

func claudeAssistantAppendix(sessionID, parentUUID, text, model string, updateLeaf bool) ([]byte, error) {
	uuid := deterministicUUID(sessionID+":native-assistant:"+model+":"+text, 0)
	row := map[string]any{
		"type":       "assistant",
		"uuid":       uuid,
		"parentUuid": parentOrNil(parentUUID),
		"sessionId":  sessionID,
		"message": map[string]any{
			"role": "assistant", "model": model,
			"content": []any{map[string]any{"type": "text", "text": text}},
		},
	}
	encoded, err := json.Marshal(row)
	if err != nil {
		return nil, err
	}
	appendix := append(encoded, '\n')
	if updateLeaf {
		lastPrompt, marshalErr := json.Marshal(map[string]any{
			"type": "last-prompt", "lastPrompt": text, "leafUuid": uuid, "sessionId": sessionID,
		})
		if marshalErr != nil {
			return nil, marshalErr
		}
		appendix = append(appendix, lastPrompt...)
		appendix = append(appendix, '\n')
	}
	return appendix, nil
}

func appendClaudeTestRows(path string, appendix []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return err
	}
	if _, err = f.Write(appendix); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func appendNativeClaudeUser(path, sessionID, text string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return appendNativeClaudeUserAtParent(path, sessionID, lastClaudeConversationalUUID(raw), text, false)
}

func appendNativeClaudeUserAtParent(path, sessionID, parentUUID, text string, updateLeaf bool) error {
	uuid := deterministicUUID(sessionID+":native:"+text, 0)
	row := map[string]any{
		"type":       "user",
		"uuid":       uuid,
		"parentUuid": parentOrNil(parentUUID),
		"sessionId":  sessionID,
		"message":    map[string]any{"role": "user", "content": text},
	}
	encoded, err := json.Marshal(row)
	if err != nil {
		return err
	}
	appendix := append(encoded, '\n')
	if updateLeaf {
		lastPrompt, marshalErr := json.Marshal(map[string]any{
			"type": "last-prompt", "lastPrompt": text, "leafUuid": uuid, "sessionId": sessionID,
		})
		if marshalErr != nil {
			return marshalErr
		}
		appendix = append(appendix, lastPrompt...)
		appendix = append(appendix, '\n')
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return err
	}
	if _, err = f.Write(appendix); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func lastClaudeConversationalUUID(raw []byte) string {
	last := ""
	for _, line := range bytes.Split(raw, []byte("\n")) {
		var row struct {
			Type string `json:"type"`
			UUID string `json:"uuid"`
		}
		if json.Unmarshal(bytes.TrimSpace(line), &row) == nil &&
			(row.Type == "user" || row.Type == "assistant") && row.UUID != "" {
			last = row.UUID
		}
	}
	return last
}

func claudeLeafTextTurns(t *testing.T, raw []byte) []acf.TextTurn {
	t.Helper()
	turns, err := claudeVisibleLeafTextTurns(raw)
	require.NoError(t, err)
	return turns
}

func requireClaudeParentChain(t *testing.T, raw []byte) {
	t.Helper()
	seen := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var row struct {
			Type       string  `json:"type"`
			UUID       string  `json:"uuid"`
			ParentUUID *string `json:"parentUuid"`
		}
		require.NoError(t, json.Unmarshal([]byte(line), &row))
		if row.Type != "user" && row.Type != "assistant" {
			continue
		}
		require.NotEmpty(t, row.UUID)
		require.False(t, seen[row.UUID], "turn UUID must not be duplicated: %s", row.UUID)
		if row.ParentUUID != nil {
			require.True(t, seen[*row.ParentUUID],
				"parentUuid must reference an earlier turn in the same session: %s", *row.ParentUUID)
		}
		seen[row.UUID] = true
	}
}

func canonicalConversationHead(t *testing.T, artifactID string, turns ...acf.TextTurn) acf.Event {
	t.Helper()
	base := time.Date(2026, 7, 1, 20, 55, 0, 0, time.UTC)
	events := make([]acf.ConversationEvent, 0, len(turns))
	for i, turn := range turns {
		events = append(events, acf.ConversationEvent{
			Type:      acf.EventTypeTurn,
			Timestamp: base.Add(time.Duration(i) * time.Second),
			Role:      turn.Role,
			Content:   []acf.ContentBlock{{Type: "text", Text: turn.Text}},
		})
	}
	payload, err := acf.EncodePayload(acf.ConversationPayload{
		Format: acf.ConversationFormatV1,
		Events: events,
	})
	require.NoError(t, err)
	return acf.Event{
		EventID:    acf.NewID(),
		ArtifactID: artifactID,
		Type:       acf.EventTypeUpdate,
		Timestamp:  base.Add(time.Duration(len(turns)) * time.Second),
		Payload:    payload,
	}
}

// TestMaterializeConversationSession_RebuildsMirrorThatCanonicalReordered
// reproduces an out-of-order mirror regression.
//
// The mirror fell behind, the user continued inside that stale view, and the
// continuation was linearized into canonical AFTER the remote turns it had
// missed. From that point the mirror is a SUBSEQUENCE of canonical, never a
// prefix, so the prefix-gated append declined on every inbound event and the
// artifact re-entered the deferral queue forever — the user's transcript froze
// at three exchanges while the canonical chain stayed perfectly healthy.
func TestMaterializeConversationSession_RebuildsMirrorThatCanonicalReordered(t *testing.T) {
	home := t.TempDir()
	artifactID := acf.NewID()
	base := time.Date(2026, 7, 27, 13, 12, 0, 0, time.UTC)
	art := acf.Artifact{
		ArtifactID: artifactID, Kind: acf.KindConversation, Scope: acf.ScopeGlobal,
		Name: "thread.jsonl", CreatedAt: base, UpdatedAt: base,
	}
	a := &Adapter{HomeDir: home}

	// The mirror as the user saw it: it never received the "population of
	// Germany" exchange, and then took their own next question.
	stale := []acf.TextTurn{
		{Role: "user", Text: "What is the capital of Germany?"},
		{Role: "assistant", Text: "Berlin."},
		{Role: "user", Text: "How many people live in Berlin?"},
		{Role: "assistant", Text: "3,700,577."},
		{Role: "user", Text: "What is the second biggest city in Germany?"},
		{Role: "assistant", Text: "Hamburg."},
	}
	path, ok, err := a.MaterializeConversationSession(
		art, canonicalConversationHead(t, artifactID, stale...), "claude-code")
	require.NoError(t, err)
	require.True(t, ok)

	// Canonical, having linearized the continuation after the missed exchange.
	canonical := []acf.TextTurn{
		{Role: "user", Text: "What is the capital of Germany?"},
		{Role: "assistant", Text: "Berlin."},
		{Role: "user", Text: "How many people live in Berlin?"},
		{Role: "assistant", Text: "3,700,577."},
		{Role: "user", Text: "How many people live in Germany?"},
		{Role: "assistant", Text: "83,467,117."},
		{Role: "user", Text: "What is the second biggest city in Germany?"},
		{Role: "assistant", Text: "Hamburg."},
	}
	require.False(t, claudeTextTurnsPrefix(stale, canonical),
		"precondition: the mirror is NOT a prefix, which is what used to deadlock it")
	require.True(t, claudeTextTurnsSubsequence(stale, canonical),
		"precondition: canonical only inserted turns, so a rebuild loses nothing")

	art.UpdatedAt = base.Add(10 * time.Minute)
	rebuiltPath, ok, err := a.MaterializeConversationSession(
		art, canonicalConversationHead(t, artifactID, canonical...), "claude-code")
	require.NoError(t, err)
	require.True(t, ok, "a diverged mirror holding nothing canonical lacks must be rebuilt, not deferred forever")
	require.Equal(t, path, rebuiltPath, "the rebuild must stay on the one stable pathname")

	raw, err := os.ReadFile(rebuiltPath)
	require.NoError(t, err)
	events, err := EncodeCanonical(raw)
	require.NoError(t, err)
	require.Equal(t, canonical, acf.ExtractTextTurns(events),
		"the mirror must now render the full canonical thread in canonical order")
}

// TestMaterializeConversationSession_DoesNotRebuildMirrorHoldingUnimportedTurns
// is the safety half: a mirror carrying a turn canonical has never seen holds
// an unimported continuation, so it must still be left alone for its watcher
// import — rebuilding would destroy the user's words.
func TestMaterializeConversationSession_DoesNotRebuildMirrorHoldingUnimportedTurns(t *testing.T) {
	home := t.TempDir()
	artifactID := acf.NewID()
	base := time.Date(2026, 7, 27, 13, 12, 0, 0, time.UTC)
	art := acf.Artifact{
		ArtifactID: artifactID, Kind: acf.KindConversation, Scope: acf.ScopeGlobal,
		Name: "thread.jsonl", CreatedAt: base, UpdatedAt: base,
	}
	a := &Adapter{HomeDir: home}

	seeded := []acf.TextTurn{
		{Role: "user", Text: "q1"},
		{Role: "assistant", Text: "a1"},
		{Role: "user", Text: "typed here, not yet imported"},
	}
	path, ok, err := a.MaterializeConversationSession(
		art, canonicalConversationHead(t, artifactID, seeded...), "claude-code")
	require.NoError(t, err)
	require.True(t, ok)
	beforeRaw, err := os.ReadFile(path)
	require.NoError(t, err)

	// Canonical went a different way and never saw the local turn.
	canonical := []acf.TextTurn{
		{Role: "user", Text: "q1"},
		{Role: "assistant", Text: "a1"},
		{Role: "user", Text: "asked on another device"},
		{Role: "assistant", Text: "answered there"},
	}
	require.False(t, claudeTextTurnsSubsequence(seeded, canonical))

	art.UpdatedAt = base.Add(time.Minute)
	_, ok, err = a.MaterializeConversationSession(
		art, canonicalConversationHead(t, artifactID, canonical...), "claude-code")
	require.NoError(t, err)
	require.False(t, ok, "a mirror with unimported turns must still defer")

	afterRaw, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, beforeRaw, afterRaw, "the unimported continuation must survive byte-for-byte")
}
