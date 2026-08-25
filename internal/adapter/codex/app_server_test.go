package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/stretchr/testify/require"
)

func TestNew_WiresOptionalAppServerRegistration(t *testing.T) {
	a := New()
	require.NotNil(t, a.findAppServerExecutables)
	require.NotNil(t, a.registerAppServerThread)
}

func TestResumeCodexAppServerThread_Protocol(t *testing.T) {
	serverOutput := strings.NewReader(strings.Join([]string{
		`{"method":"server/ready","params":{}}`,
		`{"id":0,"result":{"userAgent":"codex"}}`,
		`{"method":"thread/status/changed","params":{"threadId":"thread-123"}}`,
		`{"id":1,"result":{"thread":{"id":"thread-123"}}}`,
		`{"method":"thread/name/updated","params":{"threadId":"thread-123","name":"Testing greeting"}}`,
		`{"id":2,"result":{}}`,
		"",
	}, "\n"))
	var clientOutput bytes.Buffer

	require.NoError(t, resumeCodexAppServerThread(serverOutput, &clientOutput, "thread-123", "/project", "Testing greeting"))

	lines := strings.Split(strings.TrimSpace(clientOutput.String()), "\n")
	require.Len(t, lines, 4)
	require.JSONEq(t, `{
		"method":"initialize",
		"id":0,
		"params":{"clientInfo":{"name":"aplexica","title":"Aplexica","version":"`+Version+`"}}
	}`, lines[0])
	require.JSONEq(t, `{"method":"initialized","params":{}}`, lines[1])
	require.JSONEq(t, `{"method":"thread/resume","id":1,"params":{"threadId":"thread-123","cwd":"/project"}}`, lines[2])
	require.JSONEq(t, `{"method":"thread/name/set","id":2,"params":{"threadId":"thread-123","name":"Testing greeting"}}`, lines[3])
	require.NotContains(t, clientOutput.String(), "jsonrpc", "Codex omits the JSON-RPC header on wire")
}

func TestResumeCodexAppServerThread_ReportsProtocolFailure(t *testing.T) {
	serverOutput := strings.NewReader(strings.Join([]string{
		`{"id":0,"result":{}}`,
		`{"id":1,"error":{"code":-32602,"message":"thread not found"}}`,
		"",
	}, "\n"))
	var clientOutput bytes.Buffer

	err := resumeCodexAppServerThread(serverOutput, &clientOutput, "missing-thread", "", "")
	require.Error(t, err)
	require.Contains(t, err.Error(), "thread/resume failed (-32602)")
	require.Contains(t, err.Error(), "thread not found")
}

func TestMaterializeConversationSession_AppRegistrationIdempotent(t *testing.T) {
	home := t.TempDir()
	art, head := codexMaterializeFixture(t)

	type registration struct {
		executable string
		codexHome  string
		threadID   string
		cwd        string
		title      string
	}
	var registrations []registration
	expectedRollout := filepath.Join(home, ".codex", "sessions", "2026", "07", "13",
		"rollout-2026-07-13T12-00-00-"+art.ArtifactID+".jsonl")
	a := &Adapter{
		HomeDir: home,
		findAppServerExecutables: func(gotHome string) []string {
			require.Equal(t, home, gotHome)
			return []string{"/fake/codex"}
		},
		registerAppServerThread: func(ctx context.Context, executable, codexHome, threadID, cwd, title string) error {
			_, hasDeadline := ctx.Deadline()
			require.True(t, hasDeadline)
			require.FileExists(t, expectedRollout, "registration must happen only after the rollout is durable")
			registrations = append(registrations, registration{executable, codexHome, threadID, cwd, title})
			return nil
		},
	}

	firstPath, supports, err := a.MaterializeConversationSession(art, head, "claude-code")
	require.NoError(t, err)
	require.True(t, supports)
	firstRollout, err := os.ReadFile(firstPath)
	require.NoError(t, err)

	secondPath, supports, err := a.MaterializeConversationSession(art, head, "claude-code")
	require.NoError(t, err)
	require.True(t, supports)
	secondRollout, err := os.ReadFile(secondPath)
	require.NoError(t, err)

	require.Equal(t, firstPath, secondPath, "deterministic rollout path is the idempotency key")
	require.Equal(t, firstRollout, secondRollout)
	require.Equal(t, []registration{
		{"/fake/codex", filepath.Join(home, ".codex"), art.ArtifactID, home, "Testing greeting"},
		{"/fake/codex", filepath.Join(home, ".codex"), art.ArtifactID, home, "Testing greeting"},
	}, registrations, "re-registration safely resumes the same deterministic thread")
}

func TestMaterializeConversationSession_NamesForkWithBranch(t *testing.T) {
	home := t.TempDir()
	art, head := codexMaterializeFixture(t)
	head.Branch = "Test2"
	var threadID, title string
	a := &Adapter{
		HomeDir:            home,
		CLIExecutablePaths: []string{},
		findAppServerExecutables: func(string) []string {
			return []string{"/fake/codex"}
		},
		registerAppServerThread: func(_ context.Context, _, _, gotThreadID, _, gotTitle string) error {
			threadID = gotThreadID
			title = gotTitle
			return nil
		},
	}

	_, supports, err := a.MaterializeConversationSession(art, head, "claude-code")
	require.NoError(t, err)
	require.True(t, supports)
	require.Equal(t, codexSessionID(art.ArtifactID, "test2"), threadID)
	require.Equal(t, "[test2] Testing greeting", title)
}

func TestMaterializeConversationSession_PreservesClaudeProjectCWD(t *testing.T) {
	home := t.TempDir()
	project := filepath.Join(home, "code", "aplexica")
	worktree := filepath.Join(project, ".claude", "worktrees", "testing-greeting")
	require.NoError(t, os.MkdirAll(worktree, 0o755))
	source := filepath.Join(home, ".claude", "projects", "-encoded", "source.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(source), 0o755))
	sourceRow, err := json.Marshal(map[string]any{"type": "user", "cwd": worktree})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(source, append(sourceRow, '\n'), 0o600))

	art, head := codexMaterializeFixture(t)
	art.SourcePath = source
	var registeredCWD string
	a := &Adapter{
		HomeDir: home,
		findAppServerExecutables: func(string) []string {
			return []string{"/fake/codex"}
		},
		registerAppServerThread: func(_ context.Context, _, _, _, cwd, _ string) error {
			registeredCWD = cwd
			return nil
		},
	}

	path, supports, err := a.MaterializeConversationSession(art, head, "claude-code")
	require.NoError(t, err)
	require.True(t, supports)
	require.Equal(t, project, registeredCWD)
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	firstLine, _, _ := strings.Cut(string(raw), "\n")
	var meta struct {
		Payload struct {
			CWD string `json:"cwd"`
		} `json:"payload"`
	}
	require.NoError(t, json.Unmarshal([]byte(firstLine), &meta))
	require.Equal(t, project, meta.Payload.CWD)
}

func TestMaterializeConversationSession_NoAppServerPreservesCLIOnlySuccess(t *testing.T) {
	home := t.TempDir()
	art, head := codexMaterializeFixture(t)
	registrarCalled := false
	a := &Adapter{
		HomeDir: home,
		findAppServerExecutables: func(string) []string {
			return nil
		},
		registerAppServerThread: func(context.Context, string, string, string, string, string) error {
			registrarCalled = true
			return nil
		},
	}

	path, supports, err := a.MaterializeConversationSession(art, head, "claude-code")
	require.NoError(t, err)
	require.True(t, supports)
	require.False(t, registrarCalled)
	_, err = os.Stat(path)
	require.NoError(t, err, "the CLI rollout remains the successful fallback")
}

func TestMaterializeConversationSession_AppServerFailureIsBestEffort(t *testing.T) {
	home := t.TempDir()
	art, head := codexMaterializeFixture(t)
	a := &Adapter{
		HomeDir: home,
		findAppServerExecutables: func(string) []string {
			return []string{"/fake/codex"}
		},
		registerAppServerThread: func(context.Context, string, string, string, string, string) error {
			return errors.New("app-server unavailable")
		},
	}

	path, supports, err := a.MaterializeConversationSession(art, head, "claude-code")
	require.NoError(t, err)
	require.True(t, supports)
	rollout, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Contains(t, string(rollout), art.ArtifactID)
}

func TestMaterializeConversationSession_TriesNextAppServerCandidate(t *testing.T) {
	home := t.TempDir()
	art, head := codexMaterializeFixture(t)
	var tried []string
	a := &Adapter{
		HomeDir: home,
		findAppServerExecutables: func(string) []string {
			return []string{"/stale/codex", "/usable/codex", "/unused/codex"}
		},
		registerAppServerThread: func(_ context.Context, executable, _, _, _, _ string) error {
			tried = append(tried, executable)
			if executable == "/usable/codex" {
				return nil
			}
			return errors.New("unsupported app-server")
		},
	}

	_, supports, err := a.MaterializeConversationSession(art, head, "claude-code")
	require.NoError(t, err)
	require.True(t, supports)
	require.Equal(t, []string{"/stale/codex", "/usable/codex"}, tried)
}

func TestEnvWithValue_ReplacesExistingCodexHome(t *testing.T) {
	got := envWithValue([]string{"A=1", "CODEX_HOME=/old", "b=2"}, "CODEX_HOME", "/new")
	require.Equal(t, []string{"A=1", "b=2", "CODEX_HOME=/new"}, got)
}

func TestCodexWindowsAppServerCandidates_FindsStoreRuntimeCopies(t *testing.T) {
	localAppData := t.TempDir()
	direct := filepath.Join(localAppData, "OpenAI", "Codex", "bin", "codex.exe")
	hashed := filepath.Join(localAppData, "Packages", codexWindowsPackageFamily,
		"LocalCache", "Local", "OpenAI", "Codex", "bin", "runtime-hash", "codex.exe")
	for _, path := range []string{direct, hashed} {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte("binary"), 0o755))
	}

	require.Equal(t, []string{direct, hashed}, codexWindowsAppServerCandidates(localAppData))
}

func TestCodexWindowsDesktopInstallPresent_DetectsMSIXPackageWithoutHelper(t *testing.T) {
	localAppData := t.TempDir()
	require.False(t, codexWindowsDesktopInstallPresent(localAppData))
	require.NoError(t, os.MkdirAll(filepath.Join(localAppData, "Packages", codexWindowsPackageFamily), 0o755))
	require.True(t, codexWindowsDesktopInstallPresent(localAppData))
}

func TestCodexWindowsDesktopProbe_DoesNotClaimStandaloneCLI(t *testing.T) {
	localAppData := t.TempDir()
	standalone := filepath.Join(localAppData, "Programs", "OpenAI", "Codex", "bin", "codex.exe")
	require.NoError(t, os.MkdirAll(filepath.Dir(standalone), 0o755))
	require.NoError(t, os.WriteFile(standalone, []byte("binary"), 0o755))

	require.Empty(t, codexWindowsAppServerCandidates(localAppData))
	require.False(t, codexWindowsDesktopInstallPresent(localAppData),
		"the standalone CLI install must not activate the Desktop surface")
}

func codexMaterializeFixture(t *testing.T) (acf.Artifact, acf.Event) {
	t.Helper()
	payload, err := acf.EncodePayload(acf.ConversationPayload{
		Format: acf.ConversationFormatV1,
		Events: []acf.ConversationEvent{
			{Type: acf.EventTypeTurn, Role: "user", Content: []acf.ContentBlock{{Type: "text", Text: "What is the capital of France?"}}},
			{Type: acf.EventTypeTurn, Role: "assistant", Content: []acf.ContentBlock{{Type: "text", Text: "Paris."}}},
		},
	})
	require.NoError(t, err)
	created := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	return acf.Artifact{
		ArtifactID: "019eb7c7-870f-75cc-8dc2-6a108812d7f1",
		Kind:       acf.KindConversation,
		Scope:      acf.ScopeGlobal,
		Name:       "Testing greeting",
		CreatedAt:  created,
		UpdatedAt:  created,
	}, acf.Event{Payload: payload, Timestamp: created}
}
