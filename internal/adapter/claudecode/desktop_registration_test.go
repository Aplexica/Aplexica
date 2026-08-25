package claudecode

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/stretchr/testify/require"
)

func TestNew_WiresNonActivatingDesktopCatalogUpsert(t *testing.T) {
	a := New()
	require.NotNil(t, a.upsertDesktopSession)
}

func TestMaterializeConversationSession_UpsertsDesktopAfterDurableWrite(t *testing.T) {
	home := t.TempDir()
	app := filepath.Join(home, "Applications", "Claude.app")
	require.NoError(t, os.MkdirAll(app, 0o755))
	art, head := claudeDesktopMaterializeFixture(t)
	art.Name = "Fix synchronized conversation subjects"
	dest := filepath.Join(home, ".claude", "projects", encodeProjectDir(home), art.ArtifactID+".jsonl")
	var requests []desktopSessionUpsert
	a := &Adapter{
		HomeDir:             home,
		CLIExecutablePaths:  []string{},
		DesktopAppPaths:     []string{app},
		DesktopSessionRoots: []string{},
		upsertDesktopSession: func(ctx context.Context, request desktopSessionUpsert) error {
			_, hasDeadline := ctx.Deadline()
			require.True(t, hasDeadline)
			require.FileExists(t, dest, "Desktop catalog update must follow the durable CLI transcript write")
			requests = append(requests, request)
			return nil
		},
	}

	for range 2 {
		path, supported, err := a.MaterializeConversationSession(art, head, "codex")
		require.NoError(t, err)
		require.True(t, supported)
		require.Equal(t, dest, path)
	}
	require.Len(t, requests, 2)
	for _, request := range requests {
		require.Equal(t, art.ArtifactID, request.SessionID)
		require.Equal(t, art.Name, request.Title)
		require.Equal(t, home, request.CWD)
		require.Equal(t, art.UpdatedAt, request.Activity)
	}
}

func TestMaterializeConversationSession_NativeSourcePropagatesNativeCWDToDesktop(t *testing.T) {
	home := t.TempDir()
	nativeCWD := filepath.Join(home, "worktrees", "actual-project")
	app := filepath.Join(home, "Applications", "Claude.app")
	require.NoError(t, os.MkdirAll(app, 0o755))
	sessionID := acf.NewID()
	source := filepath.Join(home, ".claude", "projects", "native-project", sessionID+".jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(source), 0o755))
	native := `{"type":"user","uuid":"native-user","parentUuid":null,"sessionId":"` + sessionID + `","cwd":` + quotedClaudeJSON(nativeCWD) + `,"message":{"role":"user","content":"native question"}}` + "\n"
	require.NoError(t, os.WriteFile(source, []byte(native), 0o644))

	artifactID := acf.NewID()
	activity := time.Date(2026, 7, 20, 18, 0, 0, 0, time.UTC)
	art := acf.Artifact{
		ArtifactID: artifactID, Kind: acf.KindConversation, Scope: acf.ScopeGlobal,
		SourcePath: source, UpdatedAt: activity,
	}
	head := canonicalConversationHead(t, artifactID,
		acf.TextTurn{Role: "user", Text: "native question"},
		acf.TextTurn{Role: "assistant", Text: "remote answer"},
	)
	var got desktopSessionUpsert
	a := &Adapter{
		HomeDir: home, CLIExecutablePaths: []string{}, DesktopAppPaths: []string{app}, DesktopSessionRoots: []string{},
		upsertDesktopSession: func(_ context.Context, request desktopSessionUpsert) error {
			got = request
			return nil
		},
	}

	written, supported, err := a.MaterializeConversationSession(art, head, "codex")
	require.NoError(t, err)
	require.True(t, supported)
	require.Equal(t, source, written)
	require.Equal(t, sessionID, got.SessionID)
	require.Equal(t, nativeCWD, got.CWD, "Desktop must open the native project, not the user home")
	require.Equal(t, activity, got.Activity)
	nativeAfter, err := os.ReadFile(source)
	require.NoError(t, err)
	require.Contains(t, string(nativeAfter), "remote answer")
}

func TestMaterializeConversationSession_CLIOnlyDoesNotWriteDesktopCatalog(t *testing.T) {
	home := t.TempDir()
	art, head := claudeDesktopMaterializeFixture(t)
	called := false
	a := &Adapter{
		HomeDir:             home,
		CLIExecutablePaths:  []string{filepath.Join(home, "bin", "claude")},
		DesktopAppPaths:     []string{},
		DesktopSessionRoots: []string{},
		upsertDesktopSession: func(context.Context, desktopSessionUpsert) error {
			called = true
			return nil
		},
	}

	path, supported, err := a.MaterializeConversationSession(art, head, "codex")
	require.NoError(t, err)
	require.True(t, supported)
	require.False(t, called)
	require.FileExists(t, path, "CLI transcript materialization remains the fallback")
}

func TestMaterializeConversationSession_DesktopCatalogFailureIsBestEffort(t *testing.T) {
	home := t.TempDir()
	app := filepath.Join(home, "Applications", "Claude.app")
	require.NoError(t, os.MkdirAll(app, 0o755))
	art, head := claudeDesktopMaterializeFixture(t)
	a := &Adapter{
		HomeDir:             home,
		CLIExecutablePaths:  []string{},
		DesktopAppPaths:     []string{app},
		DesktopSessionRoots: []string{},
		upsertDesktopSession: func(context.Context, desktopSessionUpsert) error {
			return errors.New("Desktop unavailable")
		},
	}

	path, supported, err := a.MaterializeConversationSession(art, head, "codex")
	require.NoError(t, err)
	require.True(t, supported)
	require.FileExists(t, path)
}

func TestUpsertClaudeDesktopSession_CreatesOneDeterministicTitledRecord(t *testing.T) {
	home := t.TempDir()
	catalogLeaf := filepath.Join(home, "catalog", "account", "workspace")
	require.NoError(t, os.MkdirAll(catalogLeaf, 0o755))
	template := filepath.Join(catalogLeaf, "local_native.json")
	require.NoError(t, os.WriteFile(template, []byte(`{
		"sessionId":"local_native",
		"cliSessionId":"native",
		"cwd":"/tmp/project",
		"createdAt":1000,
		"lastActivityAt":1000,
		"permissionMode":"default",
		"alwaysAllowedReasons":[],
		"privateAppField":{"doNotCopy":true}
	}`), 0o600))

	id := "019f6126-d3e2-7d00-a920-ba8403c67936"
	activity := time.Date(2026, 7, 15, 16, 0, 0, 0, time.UTC)
	a := &Adapter{HomeDir: home, DesktopSessionRoots: []string{filepath.Join(home, "catalog")}}
	require.NoError(t, a.upsertClaudeDesktopSession(context.Background(), desktopSessionUpsert{
		SessionID: id,
		Title:     "Actual Codex subject",
		CWD:       home,
		Activity:  activity,
	}))

	target := filepath.Join(catalogLeaf, "local_"+id+".json")
	raw, err := os.ReadFile(target)
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, json.Unmarshal(raw, &got))
	require.Equal(t, "local_"+id, got["sessionId"])
	require.Equal(t, id, got["cliSessionId"])
	require.Equal(t, home, got["cwd"])
	require.Equal(t, home, got["originCwd"])
	require.Equal(t, "Actual Codex subject", got["title"])
	require.Equal(t, "auto", got["titleSource"])
	require.Equal(t, float64(activity.UnixMilli()), got["createdAt"])
	require.Equal(t, float64(activity.UnixMilli()), got["lastActivityAt"])
	require.Equal(t, float64(0), got["lastFocusedAt"], "sync must not claim the app was focused")
	require.Equal(t, "default", got["permissionMode"])
	require.NotContains(t, got, "privateAppField")

	entries, err := os.ReadDir(catalogLeaf)
	require.NoError(t, err)
	require.Len(t, entries, 2, "one sync creates exactly one deterministic record")
}

func TestUpsertClaudeDesktopSession_UpdatesSameRecordAndPreservesUnknownFields(t *testing.T) {
	home := t.TempDir()
	catalog := filepath.Join(home, "catalog")
	require.NoError(t, os.MkdirAll(catalog, 0o755))
	id := "019f6126-d3e2-7d00-a920-ba8403c67936"
	path := filepath.Join(catalog, "local_"+id+".json")
	original := `{
		"sessionId":"local_` + id + `",
		"cliSessionId":"` + id + `",
		"cwd":"/old",
		"originCwd":"/old",
		"createdAt":1000,
		"lastActivityAt":1000,
		"lastFocusedAt":777,
		"isArchived":false,
		"title":"Old automatic title",
		"titleSource":"auto",
		"unknown":{"keep":true}
	}`
	require.NoError(t, os.WriteFile(path, []byte(original), 0o600))
	a := &Adapter{HomeDir: home, DesktopSessionRoots: []string{catalog}}
	require.NoError(t, a.upsertClaudeDesktopSession(context.Background(), desktopSessionUpsert{
		SessionID: id,
		Title:     "Updated real subject",
		CWD:       home,
		Activity:  time.UnixMilli(2000),
	}))

	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, json.Unmarshal(raw, &got))
	require.Equal(t, "Updated real subject", got["title"])
	require.Equal(t, float64(1000), got["createdAt"])
	require.Equal(t, float64(2000), got["lastActivityAt"])
	require.Equal(t, float64(777), got["lastFocusedAt"])
	require.Equal(t, map[string]any{"keep": true}, got["unknown"])
	entries, err := os.ReadDir(catalog)
	require.NoError(t, err)
	require.Len(t, entries, 1, "updates must not create another conversation")
}

func TestUpsertClaudeDesktopSession_PreservesUserRename(t *testing.T) {
	home := t.TempDir()
	catalog := filepath.Join(home, "catalog")
	require.NoError(t, os.MkdirAll(catalog, 0o755))
	id := "019f6126-d3e2-7d00-a920-ba8403c67936"
	path := filepath.Join(catalog, "local_"+id+".json")
	original := `{"sessionId":"local_` + id + `","cliSessionId":"` + id + `","createdAt":1000,"lastActivityAt":1000,"title":"My title","titleSource":"user"}`
	require.NoError(t, os.WriteFile(path, []byte(original), 0o600))
	a := &Adapter{HomeDir: home, DesktopSessionRoots: []string{catalog}}
	require.NoError(t, a.upsertClaudeDesktopSession(context.Background(), desktopSessionUpsert{
		SessionID: id,
		Title:     "Codex title",
		CWD:       home,
		Activity:  time.UnixMilli(2000),
	}))
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	var got desktopSessionRecord
	require.NoError(t, json.Unmarshal(raw, &got))
	require.Equal(t, "My title", got.Title)
	require.Equal(t, "user", got.TitleSource)
}

func claudeDesktopMaterializeFixture(t *testing.T) (acf.Artifact, acf.Event) {
	t.Helper()
	artifactID := acf.NewID()
	created := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	art := acf.Artifact{
		ArtifactID: artifactID,
		Kind:       acf.KindConversation,
		Scope:      acf.ScopeGlobal,
		CreatedAt:  created,
		UpdatedAt:  created,
	}
	head := canonicalConversationHead(t, artifactID,
		acf.TextTurn{Role: "user", Text: "Can Claude Desktop see this?"},
		acf.TextTurn{Role: "assistant", Text: "Yes."},
	)
	head.Timestamp = created
	return art, head
}
