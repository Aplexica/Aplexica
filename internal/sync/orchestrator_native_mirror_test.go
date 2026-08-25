package syncd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter"
	"github.com/aplexica/aplexica/internal/adapter/claudecode"
	"github.com/aplexica/aplexica/internal/adapter/codex"
	"github.com/aplexica/aplexica/internal/retention"
	"github.com/aplexica/aplexica/internal/syncgate"
	"github.com/aplexica/aplexica/internal/syncrules"
	"github.com/stretchr/testify/require"
)

func TestOrchestrator_FansOutProjectMemoryToClaudeDesktopWorktree(t *testing.T) {
	home := realTempDir(t)
	projectDir := filepath.Join(home, "project")
	worktree := filepath.Join(projectDir, ".claude", "worktrees", "desktop-one")
	require.NoError(t, os.MkdirAll(filepath.Join(projectDir, ".git"), 0o755))
	require.NoError(t, os.MkdirAll(worktree, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(worktree, ".git"), []byte("gitdir: /tmp/git/worktrees/desktop-one\n"), 0o644))

	catalog := filepath.Join(home, "desktop-sessions")
	require.NoError(t, os.MkdirAll(catalog, 0o755))
	record := fmt.Sprintf(`{"sessionId":"local_aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee","cwd":%q,"originCwd":%q,"worktreePath":%q}`, worktree, projectDir, worktree)
	require.NoError(t, os.WriteFile(filepath.Join(catalog, "local_aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee.json"), []byte(record), 0o644))

	store := &acf.Store{Root: filepath.Join(home, "store")}
	require.NoError(t, store.Init())
	cc := claudecode.New()
	cc.HomeDir = home
	cc.DesktopSessionRoots = []string{catalog}
	cx := codex.New()
	cx.HomeDir = home

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	orch, err := NewOrchestrator(Config{
		Dir:              projectDir,
		Adapters:         []adapter.Adapter{cc, cx},
		Store:            store,
		RootsByAdapter:   map[string][]string{"codex": {projectDir}},
		QuietPeriod:      50 * time.Millisecond,
		GuardWindow:      2 * time.Second,
		MaxArtifactBytes: 1 << 20,
	})
	require.NoError(t, err)
	defer orch.Close()
	go orch.Run(ctx)
	time.Sleep(100 * time.Millisecond)

	body := []byte("# shared project instructions\n")
	require.NoError(t, os.WriteFile(filepath.Join(projectDir, "AGENTS.md"), body, 0o644))

	primaryClaude := filepath.Join(projectDir, "CLAUDE.md")
	desktopClaude := filepath.Join(worktree, "CLAUDE.md")
	require.Eventually(t, func() bool {
		primary, pErr := os.ReadFile(primaryClaude)
		desktop, dErr := os.ReadFile(desktopClaude)
		return pErr == nil && dErr == nil && string(primary) == string(body) && string(desktop) == string(body)
	}, 4*time.Second, 50*time.Millisecond,
		"project memory should reach both Claude's primary checkout and active Desktop worktree")
}

func TestOrchestrator_MirrorsClaudeSourceAcrossDesktopSurface(t *testing.T) {
	home := realTempDir(t)
	projectDir := filepath.Join(home, "project")
	worktree := filepath.Join(projectDir, ".claude", "worktrees", "desktop-one")
	require.NoError(t, os.MkdirAll(filepath.Join(projectDir, ".git"), 0o755))
	require.NoError(t, os.MkdirAll(worktree, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(worktree, ".git"), []byte("gitdir: /tmp/git/worktrees/desktop-one\n"), 0o644))

	catalog := filepath.Join(home, "desktop-sessions")
	require.NoError(t, os.MkdirAll(catalog, 0o755))
	record := fmt.Sprintf(`{"sessionId":"local_aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee","cwd":%q,"originCwd":%q}`, worktree, projectDir)
	require.NoError(t, os.WriteFile(filepath.Join(catalog, "local_aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee.json"), []byte(record), 0o644))

	store := &acf.Store{Root: filepath.Join(home, "store")}
	require.NoError(t, store.Init())
	cc := claudecode.New()
	cc.HomeDir = home
	cc.DesktopSessionRoots = []string{catalog}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	orch, err := NewOrchestrator(Config{
		Dir:              projectDir,
		Adapters:         []adapter.Adapter{cc},
		Store:            store,
		RootsByAdapter:   map[string][]string{"claude-code": {projectDir}},
		QuietPeriod:      50 * time.Millisecond,
		GuardWindow:      2 * time.Second,
		MaxArtifactBytes: 1 << 20,
	})
	require.NoError(t, err)
	defer orch.Close()
	go orch.Run(ctx)
	time.Sleep(100 * time.Millisecond)

	body := []byte("# authored from the shared Claude checkout\n")
	require.NoError(t, os.WriteFile(filepath.Join(projectDir, "CLAUDE.md"), body, 0o644))
	require.Eventually(t, func() bool {
		got, readErr := os.ReadFile(filepath.Join(worktree, "CLAUDE.md"))
		return readErr == nil && string(got) == string(body)
	}, 4*time.Second, 50*time.Millisecond,
		"one claude-code adapter should bridge its CLI/shared checkout and Desktop worktree surfaces")
}

func TestOrchestrator_NativeMirrorFirstContactPreservesLocalEdit(t *testing.T) {
	home := realTempDir(t)
	projectDir := filepath.Join(home, "project")
	worktree := filepath.Join(projectDir, ".claude", "worktrees", "desktop-one")
	require.NoError(t, os.MkdirAll(filepath.Join(projectDir, ".git"), 0o755))
	require.NoError(t, os.MkdirAll(worktree, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(worktree, ".git"), []byte("gitdir: /tmp/git/worktrees/desktop-one\n"), 0o644))

	localEdit := []byte("# instructions edited inside Desktop\n")
	desktopClaude := filepath.Join(worktree, "CLAUDE.md")
	require.NoError(t, os.WriteFile(desktopClaude, localEdit, 0o644))

	catalog := filepath.Join(home, "desktop-sessions")
	require.NoError(t, os.MkdirAll(catalog, 0o755))
	record := fmt.Sprintf(`{"sessionId":"local_aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee","cwd":%q,"originCwd":%q}`, worktree, projectDir)
	require.NoError(t, os.WriteFile(filepath.Join(catalog, "local_aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee.json"), []byte(record), 0o644))

	store := &acf.Store{Root: filepath.Join(home, "store")}
	require.NoError(t, store.Init())
	cc := claudecode.New()
	cc.HomeDir = home
	cc.DesktopSessionRoots = []string{catalog}
	cx := codex.New()
	cx.HomeDir = home

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	orch, err := NewOrchestrator(Config{
		Dir:              projectDir,
		Adapters:         []adapter.Adapter{cc, cx},
		Store:            store,
		RootsByAdapter:   map[string][]string{"codex": {projectDir}},
		QuietPeriod:      50 * time.Millisecond,
		GuardWindow:      2 * time.Second,
		MaxArtifactBytes: 1 << 20,
	})
	require.NoError(t, err)
	defer orch.Close()
	go orch.Run(ctx)
	time.Sleep(100 * time.Millisecond)

	canonical := []byte("# newly synchronized instructions\n")
	require.NoError(t, os.WriteFile(filepath.Join(projectDir, "AGENTS.md"), canonical, 0o644))
	require.Eventually(t, func() bool {
		got, readErr := os.ReadFile(filepath.Join(projectDir, "CLAUDE.md"))
		return readErr == nil && string(got) == string(canonical)
	}, 4*time.Second, 50*time.Millisecond)

	// Give the fan-out pass enough time to attempt the mirror. Its unknown
	// baseline must fail closed instead of treating first contact as consent to
	// overwrite a file that the Desktop session already changed.
	time.Sleep(200 * time.Millisecond)
	got, err := os.ReadFile(desktopClaude)
	require.NoError(t, err)
	require.Equal(t, localEdit, got)
}

func TestOrchestrator_NewDesktopWorktreeReceivesExistingContext(t *testing.T) {
	home := realTempDir(t)
	projectDir := filepath.Join(home, "project")
	require.NoError(t, os.MkdirAll(filepath.Join(projectDir, ".git"), 0o755))
	catalog := filepath.Join(home, "desktop-sessions")
	require.NoError(t, os.MkdirAll(catalog, 0o755))

	store := &acf.Store{Root: filepath.Join(home, "store")}
	require.NoError(t, store.Init())
	cc := claudecode.New()
	cc.HomeDir = home
	cc.DesktopSessionRoots = []string{catalog}
	cx := codex.New()
	cx.HomeDir = home
	cx.WorktreeRoots = []string{}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	orch, err := NewOrchestrator(Config{
		Dir:              projectDir,
		Adapters:         []adapter.Adapter{cc, cx},
		Store:            store,
		RootsByAdapter:   map[string][]string{"codex": {projectDir}},
		QuietPeriod:      50 * time.Millisecond,
		GuardWindow:      2 * time.Second,
		MaxArtifactBytes: 1 << 20,
	})
	require.NoError(t, err)
	defer orch.Close()
	go orch.Run(ctx)
	time.Sleep(100 * time.Millisecond)

	body := []byte("# context that predates the Desktop task\n")
	require.NoError(t, os.WriteFile(filepath.Join(projectDir, "AGENTS.md"), body, 0o644))
	require.Eventually(t, func() bool {
		got, readErr := os.ReadFile(filepath.Join(projectDir, "CLAUDE.md"))
		return readErr == nil && string(got) == string(body)
	}, 4*time.Second, 50*time.Millisecond)

	// Establish the empty topology baseline, then simulate Desktop creating an
	// isolated worktree after the source artifact was already synchronized.
	orch.refreshNativeMirrorTopology(context.Background())
	worktree := filepath.Join(projectDir, ".claude", "worktrees", "desktop-later")
	require.NoError(t, os.MkdirAll(worktree, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(worktree, ".git"), []byte("gitdir: /tmp/git/worktrees/desktop-later\n"), 0o644))
	record := fmt.Sprintf(`{"sessionId":"local_aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee","cwd":%q,"originCwd":%q}`, worktree, projectDir)
	require.NoError(t, os.WriteFile(filepath.Join(catalog, "local_aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee.json"), []byte(record), 0o644))
	orch.refreshNativeMirrorTopology(context.Background())

	got, err := os.ReadFile(filepath.Join(worktree, "CLAUDE.md"))
	require.NoError(t, err)
	require.Equal(t, body, got, "topology refresh should seed a newly-created native-app worktree without another source edit")
}

func TestOrchestrator_NativeMirrorTopologyRefreshTargetsChangedAdapterOnly(t *testing.T) {
	home := realTempDir(t)
	projectDir := filepath.Join(home, "project")
	require.NoError(t, os.MkdirAll(filepath.Join(projectDir, ".git"), 0o755))
	catalog := filepath.Join(home, "desktop-sessions")
	require.NoError(t, os.MkdirAll(catalog, 0o755))

	store := &acf.Store{Root: filepath.Join(home, "store")}
	require.NoError(t, store.Init())
	cc := claudecode.New()
	cc.HomeDir = home
	cc.DesktopSessionRoots = []string{catalog}
	cx := codex.New()
	cx.HomeDir = home
	cx.WorktreeRoots = []string{}
	countedCodex := &exportCountingAdapter{Adapter: cx}

	orch, err := NewOrchestrator(Config{
		Dir:            projectDir,
		Adapters:       []adapter.Adapter{cc, countedCodex},
		Store:          store,
		RootsByAdapter: map[string][]string{"claude-code": {projectDir}},
	})
	require.NoError(t, err)
	defer orch.Close()

	body := []byte("# context for a later Desktop worktree\n")
	source := filepath.Join(projectDir, "CLAUDE.md")
	require.NoError(t, os.WriteFile(source, body, 0o644))
	require.True(t, orch.handleEvent(source))
	require.Equal(t, 1, countedCodex.exports)

	// The first topology observation establishes/refreshes Claude mirrors only.
	// It must not re-export to Codex when no worktree exists.
	orch.refreshNativeMirrorTopology(context.Background())
	require.Equal(t, 1, countedCodex.exports)

	worktree := filepath.Join(projectDir, ".claude", "worktrees", "desktop-later")
	require.NoError(t, os.MkdirAll(worktree, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(worktree, ".git"), []byte("gitdir: /tmp/git/worktrees/desktop-later\n"), 0o644))
	record := fmt.Sprintf(`{"sessionId":"local_aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee","cwd":%q,"originCwd":%q}`, worktree, projectDir)
	require.NoError(t, os.WriteFile(filepath.Join(catalog, "local_aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee.json"), []byte(record), 0o644))
	orch.refreshNativeMirrorTopology(context.Background())

	got, err := os.ReadFile(filepath.Join(worktree, "CLAUDE.md"))
	require.NoError(t, err)
	require.Equal(t, body, got)
	require.Equal(t, 1, countedCodex.exports,
		"a Claude worktree change must not touch an unrelated Codex destination")
}

func TestOrchestrator_LateClaudeSurfaceActivatesWithoutWritingWhileAbsent(t *testing.T) {
	for _, surface := range []string{"cli", "desktop"} {
		t.Run(surface, func(t *testing.T) {
			home := realTempDir(t)
			projectDir := filepath.Join(home, "project")
			require.NoError(t, os.MkdirAll(filepath.Join(projectDir, ".git"), 0o755))
			// Make Codex (the source adapter) independently available.
			require.NoError(t, os.MkdirAll(filepath.Join(home, ".codex"), 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(home, ".codex", "auth.json"), []byte("{}"), 0o600))

			claudeCLI := filepath.Join(home, "bin", "claude")
			claudeApp := filepath.Join(home, "Applications", "Claude.app")
			cc := claudecode.New()
			cc.HomeDir = home
			cc.CLIExecutablePaths = []string{claudeCLI}
			cc.DesktopAppPaths = []string{claudeApp}
			cc.DesktopSessionRoots = []string{}
			cx := codex.New()
			cx.HomeDir = home
			cx.CLIExecutablePaths = []string{}
			cx.DesktopExecutablePaths = []string{}
			cx.WorktreeRoots = []string{}

			store := &acf.Store{Root: filepath.Join(home, "store")}
			require.NoError(t, store.Init())
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			orch, err := NewOrchestrator(Config{
				Dir:                     projectDir,
				Adapters:                []adapter.Adapter{cc, cx},
				DynamicAdapterDiscovery: true,
				Store:                   store,
				RootsByAdapter:          map[string][]string{"codex": {projectDir}},
				QuietPeriod:             50 * time.Millisecond,
				GuardWindow:             2 * time.Second,
				MaxArtifactBytes:        1 << 20,
			})
			require.NoError(t, err)
			defer orch.Close()
			go orch.Run(ctx)
			go orch.RunNativeLiveScan(ctx, 25*time.Millisecond)
			time.Sleep(100 * time.Millisecond)

			body := []byte("# available after install\n")
			require.NoError(t, os.WriteFile(filepath.Join(projectDir, "AGENTS.md"), body, 0o644))
			require.Eventually(t, func() bool {
				artifacts, listErr := store.ListArtifacts(acf.KindMemory)
				return listErr == nil && len(artifacts) == 1
			}, 4*time.Second, 50*time.Millisecond)
			_, err = os.Stat(filepath.Join(projectDir, "CLAUDE.md"))
			require.True(t, os.IsNotExist(err), "an absent Claude agent must not receive materialized files")

			if surface == "cli" {
				require.NoError(t, os.MkdirAll(filepath.Dir(claudeCLI), 0o755))
				require.NoError(t, os.WriteFile(claudeCLI, []byte("binary"), 0o755))
			} else {
				require.NoError(t, os.MkdirAll(claudeApp, 0o755))
			}
			require.Eventually(t, func() bool {
				got, readErr := os.ReadFile(filepath.Join(projectDir, "CLAUDE.md"))
				return readErr == nil && string(got) == string(body)
			}, 4*time.Second, 25*time.Millisecond)
		})
	}
}

func TestOrchestrator_LateCodexSurfaceActivatesWithoutWritingWhileAbsent(t *testing.T) {
	for _, surface := range []string{"cli", "desktop"} {
		t.Run(surface, func(t *testing.T) {
			home := realTempDir(t)
			projectDir := filepath.Join(home, "project")
			require.NoError(t, os.MkdirAll(filepath.Join(projectDir, ".git"), 0o755))

			// Make Claude (the source adapter) independently available.
			claudeCLI := filepath.Join(home, "bin", "claude")
			require.NoError(t, os.MkdirAll(filepath.Dir(claudeCLI), 0o755))
			require.NoError(t, os.WriteFile(claudeCLI, []byte("binary"), 0o755))

			codexCLI := filepath.Join(home, "bin", codexExecutableNameForTest())
			codexDesktop := filepath.Join(home, "Applications", "Codex.app", codexExecutableNameForTest())
			cc := claudecode.New()
			cc.HomeDir = home
			cc.CLIExecutablePaths = []string{claudeCLI}
			cc.DesktopAppPaths = []string{}
			cc.DesktopSessionRoots = []string{}
			cx := codex.New()
			cx.HomeDir = home
			cx.CLIExecutablePaths = []string{codexCLI}
			cx.DesktopExecutablePaths = []string{codexDesktop}
			cx.WorktreeRoots = []string{}

			store := &acf.Store{Root: filepath.Join(home, "store")}
			require.NoError(t, store.Init())
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			orch, err := NewOrchestrator(Config{
				Dir:                     projectDir,
				Adapters:                []adapter.Adapter{cc, cx},
				DynamicAdapterDiscovery: true,
				Store:                   store,
				RootsByAdapter:          map[string][]string{"claude-code": {projectDir}},
				QuietPeriod:             50 * time.Millisecond,
				GuardWindow:             2 * time.Second,
				MaxArtifactBytes:        1 << 20,
			})
			require.NoError(t, err)
			defer orch.Close()
			go orch.Run(ctx)
			go orch.RunNativeLiveScan(ctx, 25*time.Millisecond)
			time.Sleep(100 * time.Millisecond)

			body := []byte("# available after install\n")
			require.NoError(t, os.WriteFile(filepath.Join(projectDir, "CLAUDE.md"), body, 0o644))
			require.Eventually(t, func() bool {
				artifacts, listErr := store.ListArtifacts(acf.KindMemory)
				return listErr == nil && len(artifacts) == 1
			}, 4*time.Second, 50*time.Millisecond)
			_, err = os.Stat(filepath.Join(projectDir, "AGENTS.md"))
			require.True(t, os.IsNotExist(err), "an absent Codex agent must not receive materialized files")

			installPath := codexCLI
			if surface == "desktop" {
				installPath = codexDesktop
			}
			require.NoError(t, os.MkdirAll(filepath.Dir(installPath), 0o755))
			require.NoError(t, os.WriteFile(installPath, []byte("binary"), 0o755))
			require.Eventually(t, func() bool {
				got, readErr := os.ReadFile(filepath.Join(projectDir, "AGENTS.md"))
				return readErr == nil && string(got) == string(body)
			}, 4*time.Second, 25*time.Millisecond)
		})
	}
}

func codexExecutableNameForTest() string {
	if runtime.GOOS == "windows" {
		return "codex.exe"
	}
	return "codex"
}

func TestOrchestrator_LateCodexInstallRetriesUnchangedPreexistingMemory(t *testing.T) {
	home := realTempDir(t)
	projectDir := filepath.Join(home, "project")
	require.NoError(t, os.MkdirAll(projectDir, 0o755))
	codexRoot := filepath.Join(home, ".codex")
	memoryPath := filepath.Join(codexRoot, "memories", "personal.md")
	require.NoError(t, os.MkdirAll(filepath.Dir(memoryPath), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(codexRoot, "AGENTS.md"), []byte("# existing\n"), 0o644))
	require.NoError(t, os.WriteFile(memoryPath, []byte("remember this\n"), 0o644))

	cli := filepath.Join(home, "bin", codexExecutableNameForTest())
	cx := codex.New()
	cx.HomeDir = home
	cx.CLIExecutablePaths = []string{cli}
	cx.DesktopExecutablePaths = []string{}
	candidate := cx.CandidateDiscovery()
	roots := append(append([]string{}, candidate.GlobalRoots...), candidate.RecursiveRoots...)
	store := &acf.Store{Root: filepath.Join(home, "store")}
	require.NoError(t, store.Init())
	newOrchestrator := func() *Orchestrator {
		orch, err := NewOrchestrator(Config{
			Dir:                     projectDir,
			Adapters:                []adapter.Adapter{cx},
			Store:                   store,
			DynamicAdapterDiscovery: true,
			RootsByAdapter:          map[string][]string{cx.Name(): roots},
		})
		require.NoError(t, err)
		return orch
	}

	orch := newOrchestrator()
	require.False(t, orch.handleScanEvent(memoryPath))
	require.False(t, orch.scanCache.unchanged(memoryPath),
		"absence must not persist a fingerprint for native files that predate installation")
	require.NoError(t, orch.Close())

	orch = newOrchestrator()
	defer orch.Close()
	require.False(t, orch.scanCache.unchanged(memoryPath),
		"the deferred fingerprint must remain retryable across daemon restart")
	writeTestExecutable(t, cli)
	require.True(t, orch.handleScanEvent(memoryPath),
		"installing the CLI must import the unchanged preexisting memory")
	artifacts, err := store.ListArtifacts(acf.KindMemory)
	require.NoError(t, err)
	require.Len(t, artifacts, 1)
}

func TestRuntimeDiscoveryTokenIncludesAdapterReadiness(t *testing.T) {
	base := adapter.Discovery{
		Installed:      true,
		ActiveSurfaces: []adapter.Surface{adapter.SurfaceDesktop},
		GlobalRoots:    []string{"/shared/codex"},
	}
	ready := base
	ready.RuntimeToken = "/runtime/codex"

	require.NotEqual(t, runtimeDiscoveryToken(base), runtimeDiscoveryToken(ready),
		"an app-server helper arriving after Desktop installation must trigger catch-up")
}

func writeTestExecutable(t *testing.T, path string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte("binary"), 0o755))
}

type lateInstallConversationTarget struct {
	*fakeConvTarget
	installed                    bool
	runtimeToken                 string
	discoverErr                  error
	activationSeen               bool
	materializedBeforeActivation bool
}

type lateInstallArtifactTarget struct {
	fakeConvSource
	installed                bool
	exports                  int
	activationSeen           bool
	exportedBeforeActivation bool
}

func (f *lateInstallArtifactTarget) CandidateDiscovery() adapter.Discovery {
	return adapter.Discovery{}
}

func (f *lateInstallArtifactTarget) Discover() (adapter.Discovery, error) {
	return adapter.Discovery{Installed: f.installed}, nil
}

func (f *lateInstallArtifactTarget) NativePath(art acf.Artifact, _ string) (string, bool, error) {
	return filepath.Join(os.TempDir(), "late-install-artifact-target", art.ArtifactID+".md"), true, nil
}

func (f *lateInstallArtifactTarget) HandlesFormat(acf.Kind, string) bool { return true }

func (f *lateInstallArtifactTarget) Export(context.Context, *acf.Store, string, string) error {
	if !f.activationSeen {
		f.exportedBeforeActivation = true
	}
	f.exports++
	return nil
}

func seedMemoryWithOrigin(t *testing.T, store *acf.Store, sourceAgent, deviceID, name, sourcePath string) {
	t.Helper()
	id := acf.NewID()
	now := time.Now().UTC()
	remoteOriginDeviceID := ""
	remoteSourceAgent := ""
	if name == "" && sourcePath == "" && deviceID != "" {
		remoteOriginDeviceID = deviceID
		remoteSourceAgent = sourceAgent
	}
	require.NoError(t, store.WriteArtifact(acf.Artifact{
		AcfSchemaVersion:     acf.SchemaVersion,
		ArtifactID:           id,
		Kind:                 acf.KindMemory,
		Scope:                acf.ScopeGlobal,
		Name:                 name,
		SourcePath:           sourcePath,
		CreatedAt:            now,
		UpdatedAt:            now,
		RemoteOriginDeviceID: remoteOriginDeviceID,
		RemoteSourceAgent:    remoteSourceAgent,
	}))
	payload, err := json.Marshal(acf.MemoryPayload{Format: "markdown", Content: "remember this"})
	require.NoError(t, err)
	require.NoError(t, store.AppendEvent(acf.KindMemory, acf.Event{
		EventID:    acf.NewID(),
		ArtifactID: id,
		Type:       acf.EventTypeCreate,
		Timestamp:  now,
		Provenance: acf.Provenance{DeviceID: deviceID, SourceAgent: sourceAgent},
		Payload:    payload,
	}))
}

func (f *lateInstallConversationTarget) CandidateDiscovery() adapter.Discovery {
	return adapter.Discovery{}
}

func (f *lateInstallConversationTarget) Discover() (adapter.Discovery, error) {
	if f.discoverErr != nil {
		return adapter.Discovery{}, f.discoverErr
	}
	return adapter.Discovery{Installed: f.installed, RuntimeToken: f.runtimeToken}, nil
}

func (f *lateInstallConversationTarget) MaterializeConversationSession(art acf.Artifact, head acf.Event, source string) (string, bool, error) {
	if !f.activationSeen {
		f.materializedBeforeActivation = true
	}
	return f.fakeConvTarget.MaterializeConversationSession(art, head, source)
}

func TestOrchestrator_StartupDiscoveryBaselineDoesNotRefanUnchangedAdapter(t *testing.T) {
	home := realTempDir(t)
	store := &acf.Store{Root: filepath.Join(home, "store")}
	require.NoError(t, store.Init())
	seedMemoryWithOrigin(t, store, "codex", "local-device", "startup-memory", "")
	seedInboundConversations(t, store, "codex", "peer-device", 1)

	target := &lateInstallArtifactTarget{
		fakeConvSource: fakeConvSource{name: "codex"},
		installed:      true,
	}
	activationCalls := 0
	orch, err := NewOrchestrator(Config{
		Dir:                     home,
		Adapters:                []adapter.Adapter{target},
		DynamicAdapterDiscovery: true,
		RuntimeAdapterActivated: func(string, adapter.Discovery) { activationCalls++ },
		Store:                   store,
		LocalDeviceID:           "local-device",
	})
	require.NoError(t, err)
	defer orch.Close()

	orch.refreshRuntimeAdapterDiscovery(context.Background())
	require.Equal(t, 1, activationCalls,
		"runtime safety must still be established on the first live scan")
	require.Zero(t, target.exports,
		"an adapter already installed at startup must not receive a redundant full-store re-fan")
}

func TestOrchestrator_LateSurfaceBackfillsSameAgentConversationOnlyIntoChangedTarget(t *testing.T) {
	home := realTempDir(t)
	store := &acf.Store{Root: filepath.Join(home, "store")}
	require.NoError(t, store.Init())
	seedInboundConversations(t, store, "codex", "peer-device", 3)

	target := &lateInstallConversationTarget{
		fakeConvTarget: &fakeConvTarget{fakeConvSource: fakeConvSource{name: "codex"}},
	}
	sibling := &fakeConvTarget{fakeConvSource: fakeConvSource{name: "claude-code"}}
	orch, err := NewOrchestrator(Config{
		Dir:                     home,
		Adapters:                []adapter.Adapter{target, sibling},
		DynamicAdapterDiscovery: true,
		RuntimeAdapterActivated: func(name string, _ adapter.Discovery) {
			if name == target.Name() {
				target.activationSeen = true
			}
		},
		Store:         store,
		LocalDeviceID: "local-device",
	})
	require.NoError(t, err)
	defer orch.Close()
	orch.SetConvBackfill(map[string]int{"codex": 2})
	require.Zero(t, target.materialized())

	target.installed = true
	orch.refreshRuntimeAdapterDiscovery(context.Background())
	require.True(t, target.activationSeen)
	require.False(t, target.materializedBeforeActivation,
		"the native safety activation hook must finish before materialization")
	require.Equal(t, 2, target.materialized(),
		"a late local surface should receive bounded same-agent cloud history")
	require.Zero(t, sibling.materialized(),
		"late activation must not replay conversations into an unchanged adapter")
}

func TestOrchestrator_RuntimeHelperArrivalBackfillsRemoteButNotLocalSameAgentConversation(t *testing.T) {
	home := realTempDir(t)
	store := &acf.Store{Root: filepath.Join(home, "store")}
	require.NoError(t, store.Init())
	seedInboundConversations(t, store, "codex", "peer-device", 1)
	seedConversationsFromDevice(t, store, "codex", "local-device", 1)
	artifacts, err := store.ListArtifacts(acf.KindConversation)
	require.NoError(t, err)
	for _, art := range artifacts {
		events, readErr := store.ReadEvents(art.Kind, art.ArtifactID)
		require.NoError(t, readErr)
		if events[len(events)-1].Provenance.DeviceID != "local-device" {
			continue
		}
		art.SourcePath = filepath.Join(home, ".codex", "sessions", "local.jsonl")
		art.UpdatedAt = art.UpdatedAt.Add(time.Hour) // newest, but ineligible
		require.NoError(t, store.WriteArtifact(art))
	}

	target := &lateInstallConversationTarget{
		fakeConvTarget: &fakeConvTarget{fakeConvSource: fakeConvSource{name: "codex"}},
		installed:      true,
		runtimeToken:   "desktop-package",
	}
	orch, err := NewOrchestrator(Config{
		Dir:                     home,
		Adapters:                []adapter.Adapter{target},
		DynamicAdapterDiscovery: true,
		Store:                   store,
		LocalDeviceID:           "local-device",
	})
	require.NoError(t, err)
	defer orch.Close()
	orch.SetConvBackfill(map[string]int{"codex": 1})

	target.runtimeToken = "desktop-package+app-server-helper"
	orch.refreshRuntimeAdapterDiscovery(context.Background())
	require.Equal(t, 1, target.materialized(),
		"the newest local session must be skipped without consuming the cap, leaving room for older remote history")
}

func TestOrchestrator_RuntimeBackfillFindsPeerHistoryBehindLocalMergeAndSnapshot(t *testing.T) {
	home := realTempDir(t)
	store := &acf.Store{Root: filepath.Join(home, "store")}
	require.NoError(t, store.Init())
	seedInboundConversations(t, store, "codex", "peer-device", 2)
	artifacts, err := store.ListArtifacts(acf.KindConversation)
	require.NoError(t, err)
	require.Len(t, artifacts, 2)

	mergeEvents, err := store.ReadEvents(acf.KindConversation, artifacts[0].ArtifactID)
	require.NoError(t, err)
	mergeHead := mergeEvents[len(mergeEvents)-1]
	require.NoError(t, store.AppendEvent(acf.KindConversation, acf.Event{
		EventID:    acf.NewID(),
		ArtifactID: artifacts[0].ArtifactID,
		Type:       acf.EventTypeUpdate,
		Timestamp:  mergeHead.Timestamp.Add(time.Hour),
		ParentHash: mergeHead.Hash,
		Provenance: acf.Provenance{DeviceID: "local-device", SourceAgent: "codex"},
		Payload:    mergeHead.Payload,
	}))

	_, err = retention.CreateSnapshot(context.Background(), store, acf.KindConversation, artifacts[1].ArtifactID)
	require.NoError(t, err)
	moved, _, err := retention.PruneArtifact(context.Background(), store, acf.KindConversation, artifacts[1].ArtifactID, time.Time{})
	require.NoError(t, err)
	require.Positive(t, moved)
	_, err = retention.CreateSnapshot(context.Background(), store, acf.KindConversation, artifacts[1].ArtifactID)
	require.NoError(t, err)
	_, deleted, err := retention.PruneArtifact(context.Background(), store, acf.KindConversation, artifacts[1].ArtifactID, time.Now().Add(time.Hour))
	require.NoError(t, err)
	require.Positive(t, deleted, "the compacted peer-provenance layer must be grace-deleted for this regression")
	remaining, err := store.ReadEventsIncludingCompacted(acf.KindConversation, artifacts[1].ArtifactID)
	require.NoError(t, err)
	require.Len(t, remaining, 1, "only the self-contained empty-provenance snapshot should survive")

	target := &lateInstallConversationTarget{
		fakeConvTarget: &fakeConvTarget{fakeConvSource: fakeConvSource{name: "codex"}},
		installed:      true,
		runtimeToken:   "desktop-package",
	}
	orch, err := NewOrchestrator(Config{
		Dir:                     home,
		Adapters:                []adapter.Adapter{target},
		DynamicAdapterDiscovery: true,
		Store:                   store,
		LocalDeviceID:           "local-device",
	})
	require.NoError(t, err)
	defer orch.Close()

	target.runtimeToken = "desktop-package+app-server-helper"
	orch.refreshRuntimeAdapterDiscovery(context.Background())
	require.Equal(t, 2, target.materialized(),
		"local merge and empty-provenance snapshot tails must not hide source-less peer history")
}

func TestOrchestrator_RuntimeBackfillRejectsLegacyLocalConversationWithoutSourcePath(t *testing.T) {
	home := realTempDir(t)
	store := &acf.Store{Root: filepath.Join(home, "store")}
	require.NoError(t, store.Init())
	seedConversationsFromDevice(t, store, "codex", "old-local-hostname", 1)

	target := &lateInstallConversationTarget{
		fakeConvTarget: &fakeConvTarget{fakeConvSource: fakeConvSource{name: "codex"}},
		installed:      true,
		runtimeToken:   "cli",
	}
	sibling := &fakeConvTarget{fakeConvSource: fakeConvSource{name: "claude-code"}}
	orch, err := NewOrchestrator(Config{
		Dir:                     home,
		Adapters:                []adapter.Adapter{target, sibling},
		DynamicAdapterDiscovery: true,
		Store:                   store,
		LocalDeviceID:           "new-cloud-device-id",
	})
	require.NoError(t, err)
	defer orch.Close()

	target.runtimeToken = "cli+desktop"
	orch.refreshRuntimeAdapterDiscovery(context.Background())
	require.Zero(t, target.materialized(),
		"a named legacy native artifact must not be mistaken for an inbound source-less shell")
}

func TestOrchestrator_RuntimeBackfillDoesNotInventSourceForGraceDeletedHistory(t *testing.T) {
	home := realTempDir(t)
	store := &acf.Store{Root: filepath.Join(home, "store")}
	require.NoError(t, store.Init())
	seedInboundConversations(t, store, "codex", "peer-device", 1)
	artifacts, err := store.ListArtifacts(acf.KindConversation)
	require.NoError(t, err)
	require.Len(t, artifacts, 1)
	id := artifacts[0].ArtifactID
	legacy := artifacts[0]
	legacy.RemoteOriginDeviceID = ""
	legacy.RemoteSourceAgent = ""
	require.NoError(t, store.WriteArtifact(legacy))

	_, err = retention.CreateSnapshot(context.Background(), store, acf.KindConversation, id)
	require.NoError(t, err)
	_, _, err = retention.PruneArtifact(context.Background(), store, acf.KindConversation, id, time.Time{})
	require.NoError(t, err)
	_, err = retention.CreateSnapshot(context.Background(), store, acf.KindConversation, id)
	require.NoError(t, err)
	_, deleted, err := retention.PruneArtifact(context.Background(), store, acf.KindConversation, id, time.Now().Add(time.Hour))
	require.NoError(t, err)
	require.Positive(t, deleted)

	eng, err := syncrules.New([]syncrules.Rule{{
		Name: "only-known-codex-source",
		Match: syncrules.MatchSpec{
			AgentSource: []string{"codex"},
			Type:        []string{"conversation"},
		},
		Route: syncrules.RouteSpec{Agents: []string{"codex"}},
	}})
	require.NoError(t, err)
	target := &lateInstallConversationTarget{
		fakeConvTarget: &fakeConvTarget{fakeConvSource: fakeConvSource{name: "codex"}},
		installed:      true,
		runtimeToken:   "cli",
	}
	orch, err := NewOrchestrator(Config{
		Dir:                     home,
		Adapters:                []adapter.Adapter{target},
		DynamicAdapterDiscovery: true,
		Store:                   store,
		LocalDeviceID:           "local-device",
		RulesEngine:             eng,
	})
	require.NoError(t, err)
	defer orch.Close()

	target.runtimeToken = "cli+desktop"
	orch.refreshRuntimeAdapterDiscovery(context.Background())
	require.Zero(t, target.materialized(),
		"a pruned source must remain unknown instead of borrowing the fallback adapter name for source-specific rules")
}

func TestOrchestrator_RuntimeBackfillDoesNotDuplicateLocalConversationAfterProvenanceGraceDeletion(t *testing.T) {
	home := realTempDir(t)
	store := &acf.Store{Root: filepath.Join(home, "store")}
	require.NoError(t, store.Init())
	seedConversationsFromDevice(t, store, "codex", "local-device", 1)
	artifacts, err := store.ListArtifacts(acf.KindConversation)
	require.NoError(t, err)
	require.Len(t, artifacts, 1)
	art := artifacts[0]
	art.SourcePath = filepath.Join(home, ".codex", "sessions", "local.jsonl")
	require.NoError(t, store.WriteArtifact(art))

	_, err = retention.CreateSnapshot(context.Background(), store, acf.KindConversation, art.ArtifactID)
	require.NoError(t, err)
	_, _, err = retention.PruneArtifact(context.Background(), store, acf.KindConversation, art.ArtifactID, time.Time{})
	require.NoError(t, err)
	_, err = retention.CreateSnapshot(context.Background(), store, acf.KindConversation, art.ArtifactID)
	require.NoError(t, err)
	_, deleted, err := retention.PruneArtifact(context.Background(), store, acf.KindConversation, art.ArtifactID, time.Now().Add(time.Hour))
	require.NoError(t, err)
	require.Positive(t, deleted)
	remaining, err := store.ReadEventsIncludingCompacted(acf.KindConversation, art.ArtifactID)
	require.NoError(t, err)
	require.Len(t, remaining, 1)
	require.Empty(t, remaining[0].Provenance.SourceAgent)

	target := &lateInstallConversationTarget{
		fakeConvTarget: &fakeConvTarget{fakeConvSource: fakeConvSource{name: "codex"}},
		installed:      true,
		runtimeToken:   "cli",
	}
	sibling := &fakeConvTarget{fakeConvSource: fakeConvSource{name: "claude-code"}}
	orch, err := NewOrchestrator(Config{
		Dir:                     home,
		Adapters:                []adapter.Adapter{target, sibling},
		DynamicAdapterDiscovery: true,
		Store:                   store,
		LocalDeviceID:           "local-device",
	})
	require.NoError(t, err)
	defer orch.Close()

	target.runtimeToken = "cli+desktop"
	orch.refreshRuntimeAdapterDiscovery(context.Background())
	require.Zero(t, target.materialized(),
		"a helper arriving after retention must not synthesize a second local Codex session")
	require.Zero(t, sibling.materialized(),
		"targeted Codex readiness must not touch the unchanged Claude adapter")
}

func TestOrchestrator_LateSurfaceRecoversAfterInitialDiscoveryError(t *testing.T) {
	home := realTempDir(t)
	store := &acf.Store{Root: filepath.Join(home, "store")}
	require.NoError(t, store.Init())
	seedInboundConversations(t, store, "codex", "peer-device", 1)
	target := &lateInstallConversationTarget{
		fakeConvTarget: &fakeConvTarget{fakeConvSource: fakeConvSource{name: "codex"}},
		discoverErr:    errors.New("temporary probe failure"),
	}
	orch, err := NewOrchestrator(Config{
		Dir:                     home,
		Adapters:                []adapter.Adapter{target},
		DynamicAdapterDiscovery: true,
		RuntimeAdapterActivated: func(string, adapter.Discovery) { target.activationSeen = true },
		Store:                   store,
		LocalDeviceID:           "local-device",
	})
	require.NoError(t, err)
	defer orch.Close()

	target.discoverErr = nil
	target.installed = true
	orch.refreshRuntimeAdapterDiscovery(context.Background())
	require.True(t, target.activationSeen)
	require.Equal(t, 1, target.materialized(),
		"the first successful probe must catch up even when startup discovery had no token")
}

func TestOrchestrator_LateSurfaceStillHonorsDifferentSourceGate(t *testing.T) {
	home := realTempDir(t)
	store := &acf.Store{Root: filepath.Join(home, "store")}
	require.NoError(t, store.Init())
	seedConversations(t, store, "claude-code", 1)
	source := &fakeConvSource{name: "claude-code"}
	target := &lateInstallConversationTarget{
		fakeConvTarget: &fakeConvTarget{fakeConvSource: fakeConvSource{name: "codex"}},
	}
	orch, err := NewOrchestrator(Config{
		Dir:                     home,
		Adapters:                []adapter.Adapter{source, target},
		DynamicAdapterDiscovery: true,
		RuntimeAdapterActivated: func(string, adapter.Discovery) { target.activationSeen = true },
		SyncGate: syncgate.New(syncgate.Config{Agents: map[string]bool{
			"claude-code": false,
			"codex":       true,
		}}),
		Store: store,
	})
	require.NoError(t, err)
	defer orch.Close()

	target.installed = true
	orch.refreshRuntimeAdapterDiscovery(context.Background())
	require.True(t, target.activationSeen)
	require.Zero(t, target.materialized(),
		"same-device source gating must remain in force for a different source adapter")
}

func TestOrchestrator_LateSurfaceRemoteDifferentSourceBypassesLocalSourceGate(t *testing.T) {
	home := realTempDir(t)
	store := &acf.Store{Root: filepath.Join(home, "store")}
	require.NoError(t, store.Init())
	seedInboundConversations(t, store, "hermes", "peer-device", 1)
	target := &lateInstallConversationTarget{
		fakeConvTarget: &fakeConvTarget{fakeConvSource: fakeConvSource{name: "codex"}},
	}
	orch, err := NewOrchestrator(Config{
		Dir:                     home,
		Adapters:                []adapter.Adapter{target},
		DynamicAdapterDiscovery: true,
		SyncGate: syncgate.New(syncgate.Config{Agents: map[string]bool{
			"hermes": false,
			"codex":  true,
		}}),
		Store:         store,
		LocalDeviceID: "local-device",
	})
	require.NoError(t, err)
	defer orch.Close()

	target.installed = true
	orch.refreshRuntimeAdapterDiscovery(context.Background())
	require.Equal(t, 1, target.materialized(),
		"a peer-authored conversation must reach a newly installed different harness even when its remote source is disabled locally")
}

func TestOrchestrator_LateInstallRemoteMemoryBypassesLocalSourceGate(t *testing.T) {
	home := realTempDir(t)
	store := &acf.Store{Root: filepath.Join(home, "store")}
	require.NoError(t, store.Init())
	seedMemoryWithOrigin(t, store, "hermes", "peer-device", "", "")
	target := &lateInstallArtifactTarget{fakeConvSource: fakeConvSource{name: "codex"}}
	orch, err := NewOrchestrator(Config{
		Dir:                     home,
		Adapters:                []adapter.Adapter{target},
		DynamicAdapterDiscovery: true,
		SyncGate: syncgate.New(syncgate.Config{Agents: map[string]bool{
			"hermes": false,
			"codex":  true,
		}}),
		Store:         store,
		LocalDeviceID: "local-device",
	})
	require.NoError(t, err)
	defer orch.Close()

	target.installed = true
	orch.refreshRuntimeAdapterDiscovery(context.Background())
	require.Equal(t, 1, target.exports,
		"a peer-authored memory must catch up to a newly installed different harness without requiring its remote source locally")
}

func TestOrchestrator_OfflineInstallCatchesUpRemoteMemoryOnStartup(t *testing.T) {
	home := realTempDir(t)
	store := &acf.Store{Root: filepath.Join(home, "store")}
	require.NoError(t, store.Init())
	seedMemoryWithOrigin(t, store, "hermes", "peer-device", "", "")
	require.NoError(t, writeRuntimeDiscoveryCache(store.Root, map[string]runtimeDiscoveryState{
		"codex": {Token: runtimeDiscoveryToken(adapter.Discovery{}), Installed: false},
	}))
	target := &lateInstallArtifactTarget{
		fakeConvSource: fakeConvSource{name: "codex"},
		installed:      true,
	}
	orch, err := NewOrchestrator(Config{
		Dir:                     home,
		Adapters:                []adapter.Adapter{target},
		DynamicAdapterDiscovery: true,
		RuntimeAdapterActivated: func(string, adapter.Discovery) { target.activationSeen = true },
		Store:                   store,
		LocalDeviceID:           "local-device",
	})
	require.NoError(t, err)
	defer orch.Close()

	orch.refreshRuntimeAdapterDiscovery(context.Background())
	require.True(t, target.activationSeen)
	require.False(t, target.exportedBeforeActivation)
	require.Equal(t, 1, target.exports,
		"an install completed while the daemon was stopped must catch up canonical memory on the first runtime poll")
}

func TestOrchestrator_OfflineInstallCatchesUpRemoteConversationOnStartup(t *testing.T) {
	home := realTempDir(t)
	store := &acf.Store{Root: filepath.Join(home, "store")}
	require.NoError(t, store.Init())
	seedInboundConversations(t, store, "hermes", "peer-device", 1)
	require.NoError(t, writeRuntimeDiscoveryCache(store.Root, map[string]runtimeDiscoveryState{
		"codex": {Token: runtimeDiscoveryToken(adapter.Discovery{}), Installed: false},
	}))
	target := &lateInstallConversationTarget{
		fakeConvTarget: &fakeConvTarget{fakeConvSource: fakeConvSource{name: "codex"}},
		installed:      true,
	}
	orch, err := NewOrchestrator(Config{
		Dir:                     home,
		Adapters:                []adapter.Adapter{target},
		DynamicAdapterDiscovery: true,
		RuntimeAdapterActivated: func(string, adapter.Discovery) { target.activationSeen = true },
		Store:                   store,
		LocalDeviceID:           "local-device",
	})
	require.NoError(t, err)
	defer orch.Close()

	orch.refreshRuntimeAdapterDiscovery(context.Background())
	require.True(t, target.activationSeen)
	require.False(t, target.materializedBeforeActivation)
	require.Equal(t, 1, target.materialized(),
		"an install completed while the daemon was stopped must catch up bounded peer conversations on the first runtime poll")
}

func TestOrchestrator_LateInstallLocalMemoryHonorsDifferentSourceGate(t *testing.T) {
	home := realTempDir(t)
	store := &acf.Store{Root: filepath.Join(home, "store")}
	require.NoError(t, store.Init())
	seedMemoryWithOrigin(t, store, "hermes", "local-device", "local memory", filepath.Join(home, "hermes-memory.md"))
	target := &lateInstallArtifactTarget{fakeConvSource: fakeConvSource{name: "codex"}}
	orch, err := NewOrchestrator(Config{
		Dir:                     home,
		Adapters:                []adapter.Adapter{target},
		DynamicAdapterDiscovery: true,
		SyncGate: syncgate.New(syncgate.Config{Agents: map[string]bool{
			"hermes": false,
			"codex":  true,
		}}),
		Store:         store,
		LocalDeviceID: "local-device",
	})
	require.NoError(t, err)
	defer orch.Close()

	target.installed = true
	orch.refreshRuntimeAdapterDiscovery(context.Background())
	require.Zero(t, target.exports,
		"a local artifact must retain ordinary source gating during late-target catch-up")
}

func TestOrchestrator_LateInstallLocalMemoryAllowsEnabledAbsentSource(t *testing.T) {
	home := realTempDir(t)
	store := &acf.Store{Root: filepath.Join(home, "store")}
	require.NoError(t, store.Init())
	seedMemoryWithOrigin(t, store, "hermes", "local-device", "local memory", filepath.Join(home, "hermes-memory.md"))
	target := &lateInstallArtifactTarget{fakeConvSource: fakeConvSource{name: "codex"}}
	orch, err := NewOrchestrator(Config{
		Dir:                     home,
		Adapters:                []adapter.Adapter{target},
		DynamicAdapterDiscovery: true,
		SyncGate: syncgate.New(syncgate.Config{Agents: map[string]bool{
			"hermes": true,
			"codex":  true,
		}}),
		Store:         store,
		LocalDeviceID: "local-device",
	})
	require.NoError(t, err)
	defer orch.Close()

	target.installed = true
	orch.refreshRuntimeAdapterDiscovery(context.Background())
	require.Equal(t, 1, target.exports,
		"an enabled local source need not be installed for its artifact to reach a different late target")
}

func TestOrchestrator_LateSurfaceLocalConversationAllowsEnabledAbsentSource(t *testing.T) {
	home := realTempDir(t)
	store := &acf.Store{Root: filepath.Join(home, "store")}
	require.NoError(t, store.Init())
	seedConversationsFromDevice(t, store, "hermes", "local-device", 1)
	target := &lateInstallConversationTarget{
		fakeConvTarget: &fakeConvTarget{fakeConvSource: fakeConvSource{name: "codex"}},
	}
	orch, err := NewOrchestrator(Config{
		Dir:                     home,
		Adapters:                []adapter.Adapter{target},
		DynamicAdapterDiscovery: true,
		SyncGate: syncgate.New(syncgate.Config{Agents: map[string]bool{
			"hermes": true,
			"codex":  true,
		}}),
		Store:         store,
		LocalDeviceID: "local-device",
	})
	require.NoError(t, err)
	defer orch.Close()

	target.installed = true
	orch.refreshRuntimeAdapterDiscovery(context.Background())
	require.Equal(t, 1, target.materialized(),
		"an enabled local source need not be installed for its conversation to reach a different late target")
}

func TestOrchestrator_LateInstallMemoryRecoversCompactedSourceForRules(t *testing.T) {
	home := realTempDir(t)
	store := &acf.Store{Root: filepath.Join(home, "store")}
	require.NoError(t, store.Init())
	seedMemoryWithOrigin(t, store, "hermes", "peer-device", "", "")
	artifacts, err := store.ListArtifacts(acf.KindMemory)
	require.NoError(t, err)
	require.Len(t, artifacts, 1)
	id := artifacts[0].ArtifactID
	_, err = retention.CreateSnapshot(context.Background(), store, acf.KindMemory, id)
	require.NoError(t, err)
	moved, _, err := retention.PruneArtifact(context.Background(), store, acf.KindMemory, id, time.Time{})
	require.NoError(t, err)
	require.Positive(t, moved)

	eng, err := syncrules.New([]syncrules.Rule{{
		Name: "route-hermes-memory",
		Match: syncrules.MatchSpec{
			AgentSource: []string{"hermes"},
			Type:        []string{"memory"},
		},
		Route: syncrules.RouteSpec{Agents: []string{"codex"}},
	}})
	require.NoError(t, err)
	target := &lateInstallArtifactTarget{fakeConvSource: fakeConvSource{name: "codex"}}
	orch, err := NewOrchestrator(Config{
		Dir:                     home,
		Adapters:                []adapter.Adapter{target},
		DynamicAdapterDiscovery: true,
		Store:                   store,
		LocalDeviceID:           "local-device",
		RulesEngine:             eng,
	})
	require.NoError(t, err)
	defer orch.Close()

	target.installed = true
	orch.refreshRuntimeAdapterDiscovery(context.Background())
	require.Equal(t, 1, target.exports,
		"source-specific routing must use compacted provenance while it remains inside the retention grace period")
}

func TestOrchestrator_LateInstallMemoryUsesInboundShellAfterProvenanceGraceDeletion(t *testing.T) {
	home := realTempDir(t)
	store := &acf.Store{Root: filepath.Join(home, "store")}
	require.NoError(t, store.Init())
	seedMemoryWithOrigin(t, store, "hermes", "peer-device", "", "")
	artifacts, err := store.ListArtifacts(acf.KindMemory)
	require.NoError(t, err)
	require.Len(t, artifacts, 1)
	id := artifacts[0].ArtifactID

	_, err = retention.CreateSnapshot(context.Background(), store, acf.KindMemory, id)
	require.NoError(t, err)
	_, _, err = retention.PruneArtifact(context.Background(), store, acf.KindMemory, id, time.Time{})
	require.NoError(t, err)
	_, err = retention.CreateSnapshot(context.Background(), store, acf.KindMemory, id)
	require.NoError(t, err)
	_, deleted, err := retention.PruneArtifact(context.Background(), store, acf.KindMemory, id, time.Now().Add(time.Hour))
	require.NoError(t, err)
	require.Positive(t, deleted)
	remaining, err := store.ReadEventsIncludingCompacted(acf.KindMemory, id)
	require.NoError(t, err)
	require.Len(t, remaining, 1)
	require.Empty(t, remaining[0].Provenance.DeviceID)

	target := &lateInstallArtifactTarget{fakeConvSource: fakeConvSource{name: "codex"}}
	orch, err := NewOrchestrator(Config{
		Dir:                     home,
		Adapters:                []adapter.Adapter{target},
		DynamicAdapterDiscovery: true,
		Store:                   store,
		LocalDeviceID:           "local-device",
	})
	require.NoError(t, err)
	defer orch.Close()

	target.installed = true
	orch.refreshRuntimeAdapterDiscovery(context.Background())
	require.Equal(t, 1, target.exports,
		"a retained inbound shell must remain catch-up eligible after compacted peer provenance expires")
}

func TestOrchestrator_MaterializeInboundPreservesAbsentRemoteSourceForRules(t *testing.T) {
	home := realTempDir(t)
	store := &acf.Store{Root: filepath.Join(home, "store")}
	require.NoError(t, store.Init())
	seedMemoryWithOrigin(t, store, "hermes", "peer-device", "", "")
	artifacts, err := store.ListArtifacts(acf.KindMemory)
	require.NoError(t, err)
	require.Len(t, artifacts, 1)

	eng, err := syncrules.New([]syncrules.Rule{{
		Name: "route-live-hermes-memory",
		Match: syncrules.MatchSpec{
			AgentSource: []string{"hermes"},
			Type:        []string{"memory"},
		},
		Route: syncrules.RouteSpec{Agents: []string{"codex"}},
	}})
	require.NoError(t, err)
	target := &lateInstallArtifactTarget{fakeConvSource: fakeConvSource{name: "codex"}}
	orch, err := NewOrchestrator(Config{
		Dir:         home,
		Adapters:    []adapter.Adapter{target},
		Store:       store,
		RulesEngine: eng,
	})
	require.NoError(t, err)
	defer orch.Close()

	orch.materializeInbound(artifacts[0].ArtifactID)
	require.Equal(t, 1, target.exports,
		"live inbound routing must retain the remote source name when that adapter is absent locally")
}
