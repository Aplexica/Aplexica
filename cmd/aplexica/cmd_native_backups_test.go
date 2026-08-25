//go:build !windows

// The native-backup CLI exercises snapshot/restore round-trips, which
// reconstruct absolute paths from a single filesystem root; multi-volume
// Windows support requires separate multi-volume coverage. These round-trip
// tests are Unix-only.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/adapter"
	"github.com/aplexica/aplexica/internal/adapter/codex"
	"github.com/aplexica/aplexica/internal/nativebackup"
	"github.com/aplexica/aplexica/internal/sync"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// captureLog records the structured log calls so tests can assert on
// first-run-backup behavior without a real daemon logger.
type captureLog struct {
	infos  []string
	warns  []string
	errors []string
}

func (c *captureLog) Info(msg string, _ ...any)  { c.infos = append(c.infos, msg) }
func (c *captureLog) Warn(msg string, _ ...any)  { c.warns = append(c.warns, msg) }
func (c *captureLog) Error(msg string, _ ...any) { c.errors = append(c.errors, msg) }

func TestApplyRuntimeBackupBlocks_PreservesUnrelatedFailures(t *testing.T) {
	blocker := syncd.NewAdapterBlocker(map[string]string{
		"claude-code": "existing failure",
		"codex":       "old failure",
	})

	applyRuntimeBackupBlocks(blocker, "codex", map[string]string{"kilo": "new failure"})

	got := blocker.Snapshot()
	require.Equal(t, "existing failure", got["claude-code"])
	require.Equal(t, "new failure", got["kilo"])
	require.NotContains(t, got, "codex", "only the successfully rechecked adapter should clear")

	applyRuntimeBackupBlocks(blocker, "codex", map[string]string{"codex": "still failing"})
	require.Equal(t, "still failing", blocker.Snapshot()["codex"])
}

func TestStartNativeStartupSafety_BlocksSynchronouslyAndClearsPerAgent(t *testing.T) {
	home := t.TempDir()
	backupsRoot := filepath.Join(home, ".aplexica", "backups")
	alphaRoot := filepath.Join(home, ".alpha")
	zuluRoot := filepath.Join(home, ".zulu")
	for _, root := range []string{alphaRoot, zuluRoot} {
		require.NoError(t, os.MkdirAll(root, 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(root, "state.json"), []byte(root), 0o600))
	}
	agents := []nativebackup.AgentRoots{
		{Name: "alpha", Roots: []string{alphaRoot}},
		{Name: "zulu", Roots: []string{zuluRoot}},
	}
	mgr := newNativeBackupManager(backupsRoot, func() []nativebackup.AgentRoots { return agents })
	alphaStarted := make(chan struct{})
	alphaRelease := make(chan struct{})
	zuluStarted := make(chan struct{})
	zuluRelease := make(chan struct{})
	mgr.snapshotSafety = func(selected []nativebackup.AgentRoots, dest string) (nativebackup.Manifest, error) {
		switch selected[0].Name {
		case "alpha":
			close(alphaStarted)
			<-alphaRelease
		case "zulu":
			close(zuluStarted)
			<-zuluRelease
		}
		return nativebackup.SnapshotAuthenticated(selected, dest)
	}

	blocker := syncd.NewAdapterBlocker(nil)
	done := startNativeStartupSafety(mgr, nil, agents, blocker)
	for _, name := range []string{"alpha", "zulu"} {
		reason, blocked := blocker.Blocked(name)
		require.True(t, blocked, "%s must be gated before the helper returns", name)
		require.Equal(t, nativeSafetyVerificationPendingReason, reason)
	}
	select {
	case <-alphaStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("alpha safety snapshot did not start")
	}
	select {
	case <-done:
		t.Fatal("startup safety completed before the blocked snapshot was released")
	default:
	}

	close(alphaRelease)
	select {
	case <-zuluStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("zulu safety snapshot did not start")
	}
	_, alphaBlocked := blocker.Blocked("alpha")
	require.False(t, alphaBlocked, "alpha should clear as soon as its snapshot and checkpoint succeed")
	state, err := loadNativeBackupSafetyState(mgr.safetyPath())
	require.NoError(t, err)
	require.NotEmpty(t, state.Agents["alpha"].BackupID,
		"the durable safety checkpoint must precede clearing the live blocker")
	reason, zuluBlocked := blocker.Blocked("zulu")
	require.True(t, zuluBlocked)
	require.Equal(t, nativeSafetyVerificationPendingReason, reason)

	close(zuluRelease)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("startup safety did not finish")
	}
	require.Empty(t, blocker.Snapshot())
}

func TestStartNativeStartupSafety_FailureReplacesPendingBlock(t *testing.T) {
	home := t.TempDir()
	backupsRoot := filepath.Join(home, ".aplexica", "backups")
	root := filepath.Join(home, ".hermes")
	require.NoError(t, os.MkdirAll(root, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(root, "state.db"), []byte("state"), 0o600))
	agents := []nativebackup.AgentRoots{{Name: "hermes", Roots: []string{root}}}
	mgr := newNativeBackupManager(backupsRoot, func() []nativebackup.AgentRoots { return agents })
	started := make(chan struct{})
	release := make(chan struct{})
	mgr.snapshotSafety = func([]nativebackup.AgentRoots, string) (nativebackup.Manifest, error) {
		close(started)
		<-release
		return nativebackup.Manifest{}, errors.New("simulated safety snapshot failure")
	}

	blocker := syncd.NewAdapterBlocker(nil)
	done := startNativeStartupSafety(mgr, nil, agents, blocker)
	reason, blocked := blocker.Blocked("hermes")
	require.True(t, blocked)
	require.Equal(t, nativeSafetyVerificationPendingReason, reason)
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("safety snapshot did not start")
	}
	close(release)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("startup safety did not finish")
	}
	reason, blocked = blocker.Blocked("hermes")
	require.True(t, blocked)
	require.Equal(t, "simulated safety snapshot failure", reason)
	state, err := loadNativeBackupSafetyState(mgr.safetyPath())
	require.NoError(t, err)
	require.Equal(t, "simulated safety snapshot failure", state.Agents["hermes"].LastError)
}

func TestStartNativeStartupSafety_DoesNotClearSameSizeCorruptBackup(t *testing.T) {
	home := t.TempDir()
	backupsRoot := filepath.Join(home, ".aplexica", "backups")
	root := filepath.Join(home, ".hermes")
	require.NoError(t, os.MkdirAll(root, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(root, "state.db"), []byte("state"), 0o600))
	agent := nativebackup.AgentRoots{Name: "hermes", Roots: []string{root}}
	backupID := nativebackup.SnapshotPrefix + "hermes-corrupt"
	backupDir := filepath.Join(backupsRoot, backupID)
	man, err := nativebackup.SnapshotAuthenticated([]nativebackup.AgentRoots{agent}, backupDir)
	require.NoError(t, err)
	require.NotEmpty(t, man.Agents)
	require.NotEmpty(t, man.Agents[0].Roots)
	payload := filepath.Join(backupDir, filepath.FromSlash(man.Agents[0].Roots[0].Path))
	original, err := os.ReadFile(payload)
	require.NoError(t, err)
	require.NotEmpty(t, original)
	corrupt := append([]byte(nil), original...)
	corrupt[0] ^= 0xff
	require.NoError(t, os.WriteFile(payload, corrupt, 0o600))
	require.NoError(t, writeNativeBackupSafetyState(
		filepath.Join(backupsRoot, ".safety.json"),
		nativeBackupSafetyState{Agents: map[string]nativeBackupSafetyRecord{
			"hermes": {RootSignature: agentRootSignature(agent), BackupID: backupID},
		}},
	))

	mgr := newNativeBackupManager(backupsRoot, func() []nativebackup.AgentRoots {
		return []nativebackup.AgentRoots{agent}
	})
	replacementStarted := make(chan struct{})
	releaseFailure := make(chan struct{})
	mgr.snapshotSafety = func([]nativebackup.AgentRoots, string) (nativebackup.Manifest, error) {
		close(replacementStarted)
		<-releaseFailure
		return nativebackup.Manifest{}, errors.New("replacement unavailable")
	}
	blocker := syncd.NewAdapterBlocker(nil)
	done := startNativeStartupSafety(mgr, nil, []nativebackup.AgentRoots{agent}, blocker)
	select {
	case <-replacementStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("same-size corruption was not rejected in favor of a replacement")
	}
	reason, blocked := blocker.Blocked("hermes")
	require.True(t, blocked, "the corrupt payload must never clear the startup gate")
	require.Equal(t, nativeSafetyVerificationPendingReason, reason)
	close(releaseFailure)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("startup safety did not finish")
	}
	reason, blocked = blocker.Blocked("hermes")
	require.True(t, blocked)
	require.Equal(t, "replacement unavailable", reason)
}

func TestStartupSafetyCoversDiscovery_RequiresExactInitialTopology(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, ".codex")
	agent := nativebackup.AgentRoots{Name: "codex", Roots: []string{root}}
	signatures := nativeSafetyRootSignatures([]nativebackup.AgentRoots{agent})

	require.True(t, startupSafetyCoversDiscovery(signatures, "codex", adapter.Discovery{
		Installed:   true,
		GlobalRoots: []string{root},
	}))
	require.False(t, startupSafetyCoversDiscovery(signatures, "codex", adapter.Discovery{
		Installed:   true,
		GlobalRoots: []string{filepath.Join(home, ".codex-new")},
	}))
	require.False(t, startupSafetyCoversDiscovery(signatures, "claude-code", adapter.Discovery{
		Installed:   true,
		GlobalRoots: []string{filepath.Join(home, ".claude")},
	}))
	require.False(t, startupSafetyCoversDiscovery(signatures, "codex", adapter.Discovery{Installed: false}))
}

func TestWaitForNativeStartupSafety_HoldsBypassUntilProofOrOverride(t *testing.T) {
	startupDone := make(chan struct{})
	blocker := syncd.NewAdapterBlocker(map[string]string{
		"hermes": nativeSafetyVerificationPendingReason,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan bool, 1)
	go func() {
		result <- waitForNativeStartupSafety(ctx, startupDone, blocker, "hermes")
	}()
	select {
	case <-result:
		t.Fatal("bypass loop started before startup verification completed")
	default:
	}
	close(startupDone)
	select {
	case <-result:
		t.Fatal("bypass loop started while the adapter remained safety-blocked")
	case <-time.After(300 * time.Millisecond):
	}
	blocker.Clear("hermes")
	select {
	case allowed := <-result:
		require.True(t, allowed)
	case <-time.After(time.Second):
		t.Fatal("bypass loop did not start after the safety block cleared")
	}
}

func TestWaitForNativeStartupSafety_CancellationStopsWait(t *testing.T) {
	startupDone := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.False(t, waitForNativeStartupSafety(ctx, startupDone, syncd.NewAdapterBlocker(nil), "hermes"))
}

func runNativeBackupsCmd(t *testing.T, args ...string) (string, error) {
	t.Helper()
	restoreNativeAgent = ""
	restoreNativeFrom = ""
	restoreNativeYes = false
	restoreNativeJSON = false
	backupsListJSON = false
	t.Cleanup(func() {
		restoreNativeAgent = ""
		restoreNativeFrom = ""
		restoreNativeYes = false
		restoreNativeJSON = false
		backupsListJSON = false
		restoreNativeRootsForTest = nil
	})
	return runRoot(t, args...)
}

// seedNativeSnapshot writes a fake agent native tree and snapshots it
// into backupsRoot under a pre-sync-* directory, returning its ID.
func seedNativeSnapshot(t *testing.T, home, backupsRoot string) string {
	t.Helper()
	root := filepath.Join(home, ".hermes")
	require.NoError(t, os.MkdirAll(root, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(root, "config.json"), []byte(`{"k":"v"}`), 0o600))

	dest := filepath.Join(backupsRoot, nativebackup.SnapshotPrefix+"2026-05-29T00-00-00Z")
	_, err := nativebackup.SnapshotAuthenticated(
		[]nativebackup.AgentRoots{{Name: "hermes", Roots: []string{root}}}, dest)
	require.NoError(t, err)
	restoreNativeRootsForTest = func() []nativebackup.AgentRoots {
		return []nativebackup.AgentRoots{{Name: "hermes", Roots: []string{root}}}
	}
	return filepath.Base(dest)
}

func TestBackupsList_Empty(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	out, err := runNativeBackupsCmd(t, "backups", "list")
	require.NoError(t, err, out)
	require.Contains(t, out, "no native snapshots")
}

func TestBackupsList_ShowsSnapshots(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	backupsRoot := filepath.Join(home, ".aplexica", "backups")
	id := seedNativeSnapshot(t, home, backupsRoot)

	out, err := runNativeBackupsCmd(t, "backups", "list")
	require.NoError(t, err, out)
	require.Contains(t, out, id)
	require.Contains(t, out, "pre-sync")
	require.Contains(t, out, "hermes")
}

func TestRestoreNative_RequiresConfirmation(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	backupsRoot := filepath.Join(home, ".aplexica", "backups")
	seedNativeSnapshot(t, home, backupsRoot)

	// No --yes and stdin says "no" → aborts without touching files.
	rootCmd.SetIn(strings.NewReader("no\n"))
	t.Cleanup(func() { rootCmd.SetIn(nil) })
	out, err := runNativeBackupsCmd(t, "restore-native")
	require.Error(t, err)
	require.Contains(t, err.Error(), "aborted")
	require.Contains(t, out, "OVERWRITING")
}

func TestRestoreNative_YesRoundTripsAndIsReversible(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	backupsRoot := filepath.Join(home, ".aplexica", "backups")
	id := seedNativeSnapshot(t, home, backupsRoot)

	// Mutate the live native file so the restore has work to do.
	nativeFile := filepath.Join(home, ".hermes", "config.json")
	require.NoError(t, os.WriteFile(nativeFile, []byte(`{"k":"MUTATED"}`), 0o600))

	out, err := runNativeBackupsCmd(t, "restore-native", "--from", id, "--yes")
	require.NoError(t, err, out)
	require.Contains(t, out, "restored")
	require.Contains(t, out, "pre-restore")

	// The native file is back to the snapshot's content.
	got, err := os.ReadFile(nativeFile)
	require.NoError(t, err)
	require.Equal(t, `{"k":"v"}`, string(got))

	// A reversible pre-restore snapshot now exists alongside the pre-sync.
	infos, err := nativebackup.List(backupsRoot)
	require.NoError(t, err)
	var sawPreRestore bool
	for _, bi := range infos {
		if bi.Kind == "pre-restore" {
			sawPreRestore = true
		}
	}
	require.True(t, sawPreRestore, "expected a pre-restore snapshot after restore")
}

func TestRestoreNative_DefaultsToLatestPreSync(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	backupsRoot := filepath.Join(home, ".aplexica", "backups")
	seedNativeSnapshot(t, home, backupsRoot)

	out, err := runNativeBackupsCmd(t, "restore-native", "--yes")
	require.NoError(t, err, out)
	require.Contains(t, out, "restored")
}

func TestRestoreNative_NoSnapshots(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	out, err := runNativeBackupsCmd(t, "restore-native", "--yes")
	require.Error(t, err)
	require.Contains(t, err.Error(), "no pre-sync snapshots")
	_ = out
}

// --- first-run hook ---

func TestFirstRunNativeBackup_WritesMarkerAndSkipsOnSecondCall(t *testing.T) {
	home := t.TempDir()
	backupsRoot := filepath.Join(home, ".aplexica", "backups")

	// One installed agent with a real native root.
	root := filepath.Join(home, ".hermes")
	require.NoError(t, os.MkdirAll(root, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(root, "config.json"), []byte(`{}`), 0o600))
	agents := []nativebackup.AgentRoots{{Name: "hermes", Roots: []string{root}}}

	lg1 := &captureLog{}
	runFirstRunNativeBackup(lg1, backupsRoot, agents)
	require.Contains(t, strings.Join(lg1.infos, "|"), "first-run native backup complete")

	// Marker written.
	_, err := os.Stat(initialDoneMarker(backupsRoot))
	require.NoError(t, err)

	// Exactly one pre-sync snapshot exists.
	infos, err := nativebackup.List(backupsRoot)
	require.NoError(t, err)
	preSyncCount := 0
	for _, bi := range infos {
		if bi.Kind == "pre-sync" {
			preSyncCount++
		}
	}
	require.Equal(t, 1, preSyncCount)

	// Second call is a no-op: marker present → skip, no new snapshot.
	lg2 := &captureLog{}
	runFirstRunNativeBackup(lg2, backupsRoot, agents)
	require.Contains(t, strings.Join(lg2.infos, "|"), "skipped")

	infos2, err := nativebackup.List(backupsRoot)
	require.NoError(t, err)
	preSyncCount2 := 0
	for _, bi := range infos2 {
		if bi.Kind == "pre-sync" {
			preSyncCount2++
		}
	}
	require.Equal(t, 1, preSyncCount2, "second run must not create another pre-sync snapshot")
}

func TestFirstRunNativeBackup_NoAgentsWritesMarker(t *testing.T) {
	home := t.TempDir()
	backupsRoot := filepath.Join(home, ".aplexica", "backups")

	lg := &captureLog{}
	runFirstRunNativeBackup(lg, backupsRoot, nil)
	require.Contains(t, strings.Join(lg.infos, "|"), "skipped")

	// Marker still written so we don't re-scan every start.
	_, err := os.Stat(initialDoneMarker(backupsRoot))
	require.NoError(t, err)
}

func TestAgentRootsFromDiscoveries_IncludesRecursiveAndCompactsRoots(t *testing.T) {
	disc := map[string]adapter.Discovery{
		"claude":   {Installed: true, GlobalRoots: []string{"/Users/x/.claude"}, RecursiveRoots: []string{"/Users/x/.claude/projects"}, MetadataRoots: []string{"/Users/x/Library/Application Support/Claude/claude-code-sessions"}},
		"codex":    {Installed: false, GlobalRoots: []string{"/Users/x/.codex"}}, // not installed → dropped
		"openclaw": {Installed: true, GlobalRoots: nil},                          // installed but rootless → dropped
		"kilo":     {Installed: true, GlobalRoots: []string{"/Users/x/.config/kilo"}, RecursiveRoots: []string{"/Users/x/.kilo"}},
	}
	roots := agentRootsFromDiscoveries(disc)
	require.Len(t, roots, 2)
	require.Equal(t, "claude", roots[0].Name)
	require.Equal(t, []string{"/Users/x/.claude"}, roots[0].Roots)
	require.NotContains(t, roots[0].Roots, "/Users/x/Library/Application Support/Claude/claude-code-sessions",
		"read-only app metadata must never enter native backup or restore inventories")
	require.Equal(t, "kilo", roots[1].Name)
	require.Equal(t, []string{"/Users/x/.config/kilo", "/Users/x/.kilo"}, roots[1].Roots)
}

func TestAgentRootsFromAdapters_PicksUpLateCodexInstall(t *testing.T) {
	home := t.TempDir()
	cli := filepath.Join(home, "bin", "codex")
	a := &codex.Adapter{
		HomeDir:                home,
		CLIExecutablePaths:     []string{cli},
		DesktopExecutablePaths: []string{},
		WorktreeRoots:          []string{},
	}
	require.Empty(t, agentRootsFromAdapters([]adapter.Adapter{a}))

	require.NoError(t, os.MkdirAll(filepath.Dir(cli), 0o755))
	require.NoError(t, os.WriteFile(cli, []byte("binary"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".codex", "memories"), 0o755))
	roots := agentRootsFromAdapters([]adapter.Adapter{a})
	require.Len(t, roots, 1)
	require.Equal(t, "codex", roots[0].Name)
	require.Equal(t, []string{filepath.Join(home, ".codex")}, roots[0].Roots)
	require.Contains(t, roots[0].ExcludePaths, filepath.Join(home, ".codex", "auth.json"))
	require.Contains(t, roots[0].ExcludePaths, filepath.Join(home, ".codex", "plugins", ".plugin-appserver"))
}

func TestAgentRootsFromDiscoveries_AppliesConservativeNativeBackupExclusions(t *testing.T) {
	home := t.TempDir()
	openClawRoot := filepath.Join(home, ".openclaw")
	workerCodexHome := filepath.Join(openClawRoot, "agents", "worker-1", "agent", "codex-home")
	require.NoError(t, os.MkdirAll(workerCodexHome, 0o700))
	require.NoError(t, os.MkdirAll(filepath.Join(home, "outside-agent", "agent", "codex-home"), 0o700))
	require.NoError(t, os.Symlink(filepath.Join(home, "outside-agent"), filepath.Join(openClawRoot, "agents", "linked-agent")))
	require.NoError(t, os.WriteFile(filepath.Join(openClawRoot, "agents", "not-an-agent"), []byte("user data"), 0o600))
	disc := map[string]adapter.Discovery{
		"claude-code": {Installed: true, GlobalRoots: []string{filepath.Join(home, ".claude")}},
		"codex": {
			Installed:      true,
			GlobalRoots:    []string{filepath.Join(home, ".codex")},
			RecursiveRoots: []string{filepath.Join(home, ".agents", "skills")},
		},
		"hermes":   {Installed: true, GlobalRoots: []string{filepath.Join(home, ".hermes")}},
		"kilo":     {Installed: true, GlobalRoots: []string{filepath.Join(home, ".local", "share", "kilo")}},
		"openclaw": {Installed: true, GlobalRoots: []string{openClawRoot}},
	}
	roots := agentRootsFromDiscoveries(disc)
	require.Len(t, roots, 5)
	byName := make(map[string]nativebackup.AgentRoots, len(roots))
	for _, root := range roots {
		byName[root.Name] = root
	}

	codex := byName["codex"]
	require.Contains(t, codex.ExcludePaths, filepath.Join(home, ".codex", "auth.json"))
	require.Contains(t, codex.ExcludePaths, filepath.Join(home, ".codex", "packages"))
	require.Contains(t, codex.ExcludePaths, filepath.Join(home, ".codex", "logs_2.sqlite"))
	require.Contains(t, codex.ExcludePaths, filepath.Join(home, ".codex", "computer-use", "Codex Computer Use.app"))
	for _, excluded := range codex.ExcludePaths {
		require.NotContains(t, excluded, filepath.Join(".agents", "skills"), "personal skills are user data")
	}

	claude := byName["claude-code"]
	require.Contains(t, claude.ExcludePaths, filepath.Join(home, ".claude", ".credentials.json"))
	require.Contains(t, claude.ExcludePaths, filepath.Join(home, ".claude", "daemon", "control.key"))
	require.Contains(t, claude.ExcludePaths, filepath.Join(home, ".claude", "security", "agent-sdk-venv"))

	hermes := byName["hermes"]
	require.Contains(t, hermes.ExcludePaths, filepath.Join(home, ".hermes", "auth.json"))
	require.Contains(t, hermes.ExcludePaths, filepath.Join(home, ".hermes", ".env"))
	require.Contains(t, hermes.ExcludePaths, filepath.Join(home, ".hermes", "state-snapshots"))
	require.Contains(t, hermes.ExcludePaths, filepath.Join(home, ".hermes", "backups"))
	require.Contains(t, hermes.ExcludePaths, filepath.Join(home, ".hermes", "node"))
	require.Contains(t, hermes.ExcludePaths, filepath.Join(home, ".hermes", "gateway.pid"))
	require.Contains(t, hermes.ExcludePaths, filepath.Join(home, ".hermes", "models.json"))
	require.Contains(t, hermes.ExcludePaths, filepath.Join(home, ".hermes", "hermes-agent", ".git"))
	require.Contains(t, hermes.ExcludePaths, filepath.Join(home, ".hermes", "hermes-agent", "apps", "desktop", "release"))
	require.NotContains(t, hermes.ExcludePaths, filepath.Join(home, ".hermes", "hermes-agent"),
		"the working tree can contain user modifications")
	for _, retained := range []string{"state.db", "response_store.db", "kanban.db", "sessions", "cron", "memories", "skills", "config.yaml"} {
		require.NotContains(t, hermes.ExcludePaths, filepath.Join(home, ".hermes", retained), retained+" is user state")
	}

	kilo := byName["kilo"]
	require.Contains(t, kilo.ExcludePaths, filepath.Join(home, ".local", "share", "kilo", "auth.json"))
	require.Contains(t, kilo.ExcludePaths, filepath.Join(home, ".local", "share", "kilo", "log"))

	openclaw := byName["openclaw"]
	require.Contains(t, openclaw.ExcludePaths, filepath.Join(home, ".openclaw", "npm"))
	require.Contains(t, openclaw.ExcludePaths, filepath.Join(home, ".openclaw", "identity"))
	require.Contains(t, openclaw.ExcludePaths, filepath.Join(home, ".openclaw", "devices"))
	require.NotContains(t, openclaw.ExcludePaths, filepath.Join(home, ".openclaw", "openclaw.json"),
		"mixed OpenClaw configuration must be preserved through typed redaction")
	require.Equal(t, []nativebackup.FileRedaction{{
		Path: filepath.Join(home, ".openclaw", "openclaw.json"),
		Kind: nativebackup.FileRedactionOpenClawConfig,
	}}, openclaw.RedactFiles)
	for _, codexHome := range []string{
		filepath.Join(openClawRoot, "agents", "main", "agent", "codex-home"),
		workerCodexHome,
	} {
		for _, relative := range []string{"auth.json", "cache", "packages", "plugins/cache", "plugins/.plugin-appserver", "logs_2.sqlite"} {
			require.Contains(t, openclaw.ExcludePaths, filepath.Join(codexHome, filepath.FromSlash(relative)))
		}
		require.NotContains(t, openclaw.ExcludePaths, filepath.Join(codexHome, "sessions"),
			"embedded Codex sessions are user history")
	}
	require.NotContains(t, openclaw.ExcludePaths,
		filepath.Join(openClawRoot, "agents", "linked-agent", "agent", "codex-home", "packages"),
		"symlinked agent directories must not be enumerated")
	for _, retained := range []string{"workspace", "memory", "state", "agents/main/sessions", "skills"} {
		require.NotContains(t, openclaw.ExcludePaths, filepath.Join(home, ".openclaw", filepath.FromSlash(retained)), retained+" is user state")
	}
}

func writeCodexNativeBackupSession(t *testing.T, root, name, metadata string) string {
	t.Helper()
	path := filepath.Join(root, "sessions", "2026", "07", "19", name)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte(metadata+"\n"+`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"keep marker-like prompt text"}]}}`+"\n"), 0o600))
	return path
}

func nativeBackupManifestContainsTarget(man nativebackup.Manifest, target string) bool {
	want := filepath.ToSlash(strings.TrimPrefix(filepath.Clean(target), string(filepath.Separator)))
	for _, agent := range man.Agents {
		for _, entry := range agent.Roots {
			if strings.HasSuffix(entry.Path, want) {
				return true
			}
		}
	}
	return false
}

func TestNativeBackupCodexSubagentExclusionPreservesUserSessionsAndSanitizesHistory(t *testing.T) {
	home := t.TempDir()
	codexRoot := filepath.Join(home, ".codex")
	internalThreadSource := writeCodexNativeBackupSession(t, codexRoot, "rollout-internal-thread-source.jsonl",
		`{"type":"session_meta","payload":{"thread_source":"subagent","source":{"subagent":{"agent_path":"reviewer"}}}}`)
	internalSourceObject := writeCodexNativeBackupSession(t, codexRoot, "rollout-internal-source-object.jsonl",
		`{"type":"session_meta","payload":{"source":{"subagent":{"thread_spawn":{"parent_thread_id":"parent"}}}}}`)
	mainSession := writeCodexNativeBackupSession(t, codexRoot, "rollout-main.jsonl",
		`{"type":"session_meta","payload":{"thread_source":"cli","source":"cli"}}`)
	automationSession := writeCodexNativeBackupSession(t, codexRoot, "rollout-automation.jsonl",
		`{"type":"session_meta","payload":{"thread_source":"exec","source":"exec"}}`)

	staticExcluded := nativeBackupExcludePaths("codex", []string{codexRoot})
	require.NotContains(t, staticExcluded, internalThreadSource,
		"routine discovery/status must not scan rollout content")
	policy := nativebackup.AgentRoots{Name: "codex", Roots: []string{codexRoot}, ExcludePaths: staticExcluded}
	policies := withNativeBackupContentExclusions([]nativebackup.AgentRoots{policy})
	require.Len(t, policies, 1)
	policy = policies[0]
	require.Contains(t, policy.ExcludePaths, internalThreadSource)
	require.Contains(t, policy.ExcludePaths, internalSourceObject)
	require.NotContains(t, policy.ExcludePaths, mainSession)
	require.NotContains(t, policy.ExcludePaths, automationSession,
		"prompt text and non-subagent execution modes are user-owned history")

	newSnapshot := filepath.Join(home, ".aplexica", "backups", nativebackup.ManualPrefix+"new-policy")
	newManifest, err := nativebackup.SnapshotAuthenticated([]nativebackup.AgentRoots{policy}, newSnapshot)
	require.NoError(t, err)
	require.False(t, nativeBackupManifestContainsTarget(newManifest, internalThreadSource))
	require.False(t, nativeBackupManifestContainsTarget(newManifest, internalSourceObject))
	require.True(t, nativeBackupManifestContainsTarget(newManifest, mainSession))
	require.True(t, nativeBackupManifestContainsTarget(newManifest, automationSession))

	// Model a signed snapshot created before the content-aware policy existed.
	// Background maintenance must rebuild it without the explicitly internal
	// rollouts while retaining both kinds of user-owned session.
	oldSnapshot := filepath.Join(home, ".aplexica", "backups", nativebackup.ManualPrefix+"old-policy")
	oldManifest, err := nativebackup.SnapshotAuthenticated(
		[]nativebackup.AgentRoots{{Name: "codex", Roots: []string{codexRoot}}}, oldSnapshot)
	require.NoError(t, err)
	require.True(t, nativeBackupManifestContainsTarget(oldManifest, internalThreadSource))
	require.True(t, nativeBackupManifestContainsTarget(oldManifest, internalSourceObject))

	mgr := newNativeBackupManager(filepath.Dir(oldSnapshot), func() []nativebackup.AgentRoots {
		return []nativebackup.AgentRoots{policy}
	})
	mgr.SweepNativeBackupHistory(&captureLog{})

	sanitized, err := nativebackup.ReadManifest(oldSnapshot)
	require.NoError(t, err)
	require.NoError(t, nativebackup.VerifyDefaultManifest(sanitized, oldSnapshot))
	require.False(t, nativeBackupManifestContainsTarget(sanitized, internalThreadSource))
	require.False(t, nativeBackupManifestContainsTarget(sanitized, internalSourceObject))
	require.True(t, nativeBackupManifestContainsTarget(sanitized, mainSession))
	require.True(t, nativeBackupManifestContainsTarget(sanitized, automationSession))
}

func TestNativeBackupCodexPackagesAreExcludedFromSnapshotRestoreAndSanitizedHistory(t *testing.T) {
	home := t.TempDir()
	codexRoot := filepath.Join(home, ".codex")
	packagePath := filepath.Join(codexRoot, "packages", "standalone", "releases", "0.144.6-aarch64-apple-darwin", "bin", "codex")
	require.NoError(t, os.MkdirAll(filepath.Dir(packagePath), 0o700))
	require.NoError(t, os.WriteFile(packagePath, []byte("downloaded-runtime"), 0o700))
	mainSession := writeCodexNativeBackupSession(t, codexRoot, "rollout-main-packages.jsonl",
		`{"type":"session_meta","payload":{"thread_source":"cli","source":"cli"}}`)
	configPath := filepath.Join(codexRoot, "config.toml")
	require.NoError(t, os.WriteFile(configPath, []byte("model = \"saved\"\n"), 0o600))
	wantSession, err := os.ReadFile(mainSession)
	require.NoError(t, err)
	wantConfig, err := os.ReadFile(configPath)
	require.NoError(t, err)

	staticExcluded := nativeBackupExcludePaths("codex", []string{codexRoot})
	require.Contains(t, staticExcluded, filepath.Join(codexRoot, "packages"))
	policy := nativebackup.AgentRoots{Name: "codex", Roots: []string{codexRoot}, ExcludePaths: staticExcluded}
	policy = withNativeBackupContentExclusions([]nativebackup.AgentRoots{policy})[0]
	backupsRoot := filepath.Join(home, ".aplexica", "backups")

	newSnapshot := filepath.Join(backupsRoot, nativebackup.ManualPrefix+"packages-new-policy")
	newManifest, err := nativebackup.SnapshotAuthenticated([]nativebackup.AgentRoots{policy}, newSnapshot)
	require.NoError(t, err)
	require.False(t, nativeBackupManifestContainsTarget(newManifest, packagePath),
		"downloaded Codex packages must not be copied into new backups")
	require.True(t, nativeBackupManifestContainsTarget(newManifest, mainSession))
	require.True(t, nativeBackupManifestContainsTarget(newManifest, configPath))

	// Model an authenticated snapshot made before packages became rebuildable
	// backup state. Restore must honor today's policy even before background
	// sanitization has rewritten that historical snapshot.
	oldSnapshot := filepath.Join(backupsRoot, nativebackup.ManualPrefix+"packages-old-policy")
	oldManifest, err := nativebackup.SnapshotAuthenticated(
		[]nativebackup.AgentRoots{{Name: "codex", Roots: []string{codexRoot}}}, oldSnapshot)
	require.NoError(t, err)
	require.True(t, nativeBackupManifestContainsTarget(oldManifest, packagePath))
	require.True(t, nativeBackupManifestContainsTarget(oldManifest, mainSession))
	require.True(t, nativeBackupManifestContainsTarget(oldManifest, configPath))

	require.NoError(t, os.WriteFile(packagePath, []byte("current-machine-runtime"), 0o700))
	require.NoError(t, os.WriteFile(mainSession, []byte("current-session"), 0o600))
	require.NoError(t, os.WriteFile(configPath, []byte("model = \"current\"\n"), 0o600))
	_, err = nativebackup.RestoreWithOptions(context.Background(), oldSnapshot, nativebackup.NativeRestoreOptions{
		Agent:             "codex",
		CurrentAgentRoots: []nativebackup.AgentRoots{policy},
		Coordinator: nativebackup.LocalRestoreCoordinator{
			LockPath: filepath.Join(home, ".aplexica", "state", "native-restore.lock"),
		},
	})
	require.NoError(t, err)
	got, err := os.ReadFile(packagePath)
	require.NoError(t, err)
	require.Equal(t, "current-machine-runtime", string(got),
		"restore must not overwrite the destination machine's downloaded runtime")
	got, err = os.ReadFile(mainSession)
	require.NoError(t, err)
	require.Equal(t, wantSession, got)
	got, err = os.ReadFile(configPath)
	require.NoError(t, err)
	require.Equal(t, wantConfig, got)

	result, err := nativebackup.SanitizeSnapshotContext(context.Background(), oldSnapshot, nativebackup.SanitizeOptions{
		CurrentAgentRoots: []nativebackup.AgentRoots{policy},
	})
	require.NoError(t, err)
	require.Equal(t, nativebackup.SanitizeComplete, result.Status)
	require.Equal(t, 1, result.RemovedFiles)
	require.Equal(t, int64(len("downloaded-runtime")), result.RemovedBytes)
	sanitized, err := nativebackup.ReadManifest(oldSnapshot)
	require.NoError(t, err)
	require.NoError(t, nativebackup.VerifyDefaultManifest(sanitized, oldSnapshot))
	require.False(t, nativeBackupManifestContainsTarget(sanitized, packagePath))
	require.True(t, nativeBackupManifestContainsTarget(sanitized, mainSession))
	require.True(t, nativeBackupManifestContainsTarget(sanitized, configPath))

	mirroredRoot := filepath.Clean(codexRoot)
	mirroredRoot = strings.TrimPrefix(mirroredRoot, filepath.VolumeName(mirroredRoot))
	mirroredRoot = strings.TrimPrefix(mirroredRoot, string(filepath.Separator))
	mirroredRoot = filepath.Join(oldSnapshot, "codex", mirroredRoot)
	require.NoDirExists(t, filepath.Join(mirroredRoot, "packages"))
	got, err = os.ReadFile(filepath.Join(mirroredRoot, "sessions", "2026", "07", "19", filepath.Base(mainSession)))
	require.NoError(t, err)
	require.Equal(t, wantSession, got)
	got, err = os.ReadFile(filepath.Join(mirroredRoot, "config.toml"))
	require.NoError(t, err)
	require.Equal(t, wantConfig, got)
}

type openClawEmbeddedCodexFixture struct {
	agentRoot    string
	machineFiles map[string]string
	userFiles    map[string]string
}

func writeOpenClawEmbeddedCodexFixture(t *testing.T, openClawRoot, agentID string) openClawEmbeddedCodexFixture {
	t.Helper()
	codexHome := filepath.Join(openClawRoot, "agents", agentID, "agent", "codex-home")
	machineFiles := map[string]string{
		filepath.Join(codexHome, "auth.json"):                                                     "machine-auth-" + agentID,
		filepath.Join(codexHome, "cache", "models.json"):                                          "machine-cache-" + agentID,
		filepath.Join(codexHome, "packages", "standalone", "releases", "0.144.6", "bin", "codex"): "machine-package-" + agentID,
		filepath.Join(codexHome, "plugins", "cache", "catalog.json"):                              "machine-plugin-cache-" + agentID,
		filepath.Join(codexHome, "plugins", ".plugin-appserver", "runtime"):                       "machine-plugin-runtime-" + agentID,
		filepath.Join(codexHome, "logs_2.sqlite"):                                                 "machine-log-cache-" + agentID,
	}
	userFiles := map[string]string{
		filepath.Join(codexHome, "sessions", "2026", "07", "20", "rollout-user.jsonl"): "user-session-" + agentID,
		filepath.Join(codexHome, "config.toml"):                                        "model = \"saved-" + agentID + "\"\n",
		filepath.Join(openClawRoot, "agents", agentID, "workspace", "project.md"):      "user-workspace-" + agentID,
	}
	for path, value := range machineFiles {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
		require.NoError(t, os.WriteFile(path, []byte(value), 0o600))
	}
	for path, value := range userFiles {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
		require.NoError(t, os.WriteFile(path, []byte(value), 0o600))
	}
	return openClawEmbeddedCodexFixture{
		agentRoot:    filepath.Join(openClawRoot, "agents", agentID),
		machineFiles: machineFiles,
		userFiles:    userFiles,
	}
}

func nativeBackupMirroredTarget(snapshot, agent, target string) string {
	clean := filepath.Clean(target)
	clean = strings.TrimPrefix(clean, filepath.VolumeName(clean))
	clean = strings.TrimPrefix(clean, string(filepath.Separator))
	return filepath.Join(snapshot, agent, clean)
}

func requireOpenClawEmbeddedCodexManifestPolicy(
	t *testing.T,
	man nativebackup.Manifest,
	fixtures []openClawEmbeddedCodexFixture,
) {
	t.Helper()
	for _, fixture := range fixtures {
		for path := range fixture.machineFiles {
			require.False(t, nativeBackupManifestContainsTarget(man, path), path)
		}
		for path := range fixture.userFiles {
			require.True(t, nativeBackupManifestContainsTarget(man, path), path)
		}
	}
}

func TestNativeBackupOpenClawEmbeddedCodexHomesSnapshotRestoreAndSanitize(t *testing.T) {
	home := t.TempDir()
	openClawRoot := filepath.Join(home, ".openclaw")
	fixtures := []openClawEmbeddedCodexFixture{
		writeOpenClawEmbeddedCodexFixture(t, openClawRoot, "main"),
		writeOpenClawEmbeddedCodexFixture(t, openClawRoot, "research-worker"),
	}
	policy := nativebackup.AgentRoots{
		Name:         "openclaw",
		Roots:        []string{openClawRoot},
		ExcludePaths: nativeBackupExcludePaths("openclaw", []string{openClawRoot}),
		RedactFiles:  nativeBackupRedactFiles("openclaw", []string{openClawRoot}),
	}
	backupsRoot := filepath.Join(home, ".aplexica", "backups")

	newSnapshot := filepath.Join(backupsRoot, nativebackup.ManualPrefix+"openclaw-embedded-new-policy")
	newManifest, err := nativebackup.SnapshotAuthenticated([]nativebackup.AgentRoots{policy}, newSnapshot)
	require.NoError(t, err)
	requireOpenClawEmbeddedCodexManifestPolicy(t, newManifest, fixtures)

	oldSnapshot := filepath.Join(backupsRoot, nativebackup.ManualPrefix+"openclaw-embedded-old-policy")
	oldManifest, err := nativebackup.SnapshotAuthenticated(
		[]nativebackup.AgentRoots{{Name: "openclaw", Roots: []string{openClawRoot}}}, oldSnapshot)
	require.NoError(t, err)
	for _, fixture := range fixtures {
		for path := range fixture.machineFiles {
			require.True(t, nativeBackupManifestContainsTarget(oldManifest, path), path)
		}
		for path := range fixture.userFiles {
			require.True(t, nativeBackupManifestContainsTarget(oldManifest, path), path)
		}
	}

	currentMachine := make(map[string]string)
	for _, fixture := range fixtures {
		for path := range fixture.machineFiles {
			currentMachine[path] = "current-machine-" + filepath.Base(path)
			require.NoError(t, os.WriteFile(path, []byte(currentMachine[path]), 0o600))
		}
		for path := range fixture.userFiles {
			require.NoError(t, os.WriteFile(path, []byte("current-user"), 0o600))
		}
	}
	_, err = nativebackup.RestoreWithOptions(context.Background(), oldSnapshot, nativebackup.NativeRestoreOptions{
		Agent:             "openclaw",
		CurrentAgentRoots: []nativebackup.AgentRoots{policy},
		ExcludeTarget:     nativeBackupDynamicTargetExcluded,
		Coordinator: nativebackup.LocalRestoreCoordinator{
			LockPath: filepath.Join(home, ".aplexica", "state", "native-restore.lock"),
		},
	})
	require.NoError(t, err)
	for _, fixture := range fixtures {
		for path := range fixture.machineFiles {
			got, readErr := os.ReadFile(path)
			require.NoError(t, readErr)
			require.Equal(t, currentMachine[path], string(got), path)
		}
		for path, want := range fixture.userFiles {
			got, readErr := os.ReadFile(path)
			require.NoError(t, readErr)
			require.Equal(t, want, string(got), path)
		}
	}

	result, err := nativebackup.SanitizeSnapshotContext(context.Background(), oldSnapshot, nativebackup.SanitizeOptions{
		CurrentAgentRoots: []nativebackup.AgentRoots{policy},
		ExcludeTarget:     nativeBackupDynamicTargetExcluded,
	})
	require.NoError(t, err)
	require.Equal(t, nativebackup.SanitizeComplete, result.Status)
	require.Equal(t, 12, result.RemovedFiles)
	sanitized, err := nativebackup.ReadManifest(oldSnapshot)
	require.NoError(t, err)
	require.NoError(t, nativebackup.VerifyDefaultManifest(sanitized, oldSnapshot))
	requireOpenClawEmbeddedCodexManifestPolicy(t, sanitized, fixtures)
	for _, fixture := range fixtures {
		for path := range fixture.machineFiles {
			require.NoFileExists(t, nativeBackupMirroredTarget(oldSnapshot, "openclaw", path), path)
			got, readErr := os.ReadFile(path)
			require.NoError(t, readErr)
			require.Equal(t, currentMachine[path], string(got), "sanitization must not edit live machine state")
		}
		for path, want := range fixture.userFiles {
			got, readErr := os.ReadFile(nativeBackupMirroredTarget(oldSnapshot, "openclaw", path))
			require.NoError(t, readErr)
			require.Equal(t, want, string(got), path)
		}
	}
}

func TestNativeBackupOpenClawEmbeddedCodexRestoreExcludesDisappearedAgentMachineState(t *testing.T) {
	home := t.TempDir()
	openClawRoot := filepath.Join(home, ".openclaw")
	fixture := writeOpenClawEmbeddedCodexFixture(t, openClawRoot, "disappeared-agent")
	backupsRoot := filepath.Join(home, ".aplexica", "backups")
	snapshot := filepath.Join(backupsRoot, nativebackup.ManualPrefix+"openclaw-disappeared-restore")
	_, err := nativebackup.SnapshotAuthenticated(
		[]nativebackup.AgentRoots{{Name: "openclaw", Roots: []string{openClawRoot}}}, snapshot)
	require.NoError(t, err)
	require.NoError(t, os.RemoveAll(fixture.agentRoot))

	// The current exact-path policy cannot enumerate an agent that no longer
	// exists. The authenticated component policy must still exclude its stale
	// machine state while allowing user sessions/config/workspace to return.
	policy := nativebackup.AgentRoots{
		Name:         "openclaw",
		Roots:        []string{openClawRoot},
		ExcludePaths: nativeBackupExcludePaths("openclaw", []string{openClawRoot}),
		RedactFiles:  nativeBackupRedactFiles("openclaw", []string{openClawRoot}),
	}
	_, err = nativebackup.RestoreWithOptions(context.Background(), snapshot, nativebackup.NativeRestoreOptions{
		Agent:             "openclaw",
		CurrentAgentRoots: []nativebackup.AgentRoots{policy},
		ExcludeTarget:     nativeBackupDynamicTargetExcluded,
		Coordinator: nativebackup.LocalRestoreCoordinator{
			LockPath: filepath.Join(home, ".aplexica", "state", "native-restore.lock"),
		},
	})
	require.NoError(t, err)
	for path := range fixture.machineFiles {
		require.NoFileExists(t, path, path)
	}
	for path, want := range fixture.userFiles {
		got, readErr := os.ReadFile(path)
		require.NoError(t, readErr)
		require.Equal(t, want, string(got), path)
	}
}

func TestNativeBackupDynamicTargetExclusionUsesExactOpenClawComponents(t *testing.T) {
	tests := []struct {
		name  string
		agent string
		path  string
		want  bool
	}{
		{name: "package", agent: "openclaw", path: "agents/worker/agent/codex-home/packages/standalone/bin/codex", want: true},
		{name: "credential", agent: "openclaw", path: "agents/retired/agent/codex-home/auth.json", want: true},
		{name: "plugin runtime", agent: "openclaw", path: "agents/worker/agent/codex-home/plugins/.plugin-appserver/runtime", want: true},
		{name: "session", agent: "openclaw", path: "agents/worker/agent/codex-home/sessions/rollout.jsonl"},
		{name: "config", agent: "openclaw", path: "agents/worker/agent/codex-home/config.toml"},
		{name: "workspace", agent: "openclaw", path: "agents/worker/workspace/project.md"},
		{name: "lookalike home", agent: "openclaw", path: "agents/worker/agent/codex-home-packages/auth.json"},
		{name: "other adapter", agent: "codex", path: "agents/worker/agent/codex-home/packages/bin/codex"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			relative := filepath.FromSlash(tc.path)
			require.Equal(t, tc.want, nativeBackupDynamicTargetExcluded(nativebackup.NativeTarget{
				Agent: tc.agent, Root: t.TempDir(), RelativePath: relative,
			}))
		})
	}
}

func TestNativeBackupOpenClawEmbeddedCodexHomesKnownPolicyWithoutDiscovery(t *testing.T) {
	home := t.TempDir()
	openClawRoot := filepath.Join(home, ".openclaw")
	fixtures := []openClawEmbeddedCodexFixture{
		writeOpenClawEmbeddedCodexFixture(t, openClawRoot, "main"),
		writeOpenClawEmbeddedCodexFixture(t, openClawRoot, "non-main-agent"),
	}
	backupsRoot := filepath.Join(home, ".aplexica", "backups")
	snapshot := filepath.Join(backupsRoot, nativebackup.ManualPrefix+"openclaw-known-policy")
	_, err := nativebackup.SnapshotAuthenticated(
		[]nativebackup.AgentRoots{{Name: "openclaw", Roots: []string{openClawRoot}}}, snapshot)
	require.NoError(t, err)
	require.NoError(t, os.RemoveAll(fixtures[1].agentRoot))

	// Runtime discovery can be empty while an authenticated historical manifest
	// still names the OpenClaw source root, and the non-main agent may itself be
	// gone. The component policy must still sanitize its machine state while the
	// known-agent exact-path fallback covers the conventional/current homes.
	mgr := newNativeBackupManager(backupsRoot, func() []nativebackup.AgentRoots { return nil })
	mgr.SweepNativeBackupHistory(&captureLog{})

	man, err := nativebackup.ReadManifest(snapshot)
	require.NoError(t, err)
	require.NoError(t, nativebackup.VerifyDefaultManifest(man, snapshot))
	requireOpenClawEmbeddedCodexManifestPolicy(t, man, fixtures)
	for _, fixture := range fixtures {
		for path := range fixture.machineFiles {
			require.NoFileExists(t, nativeBackupMirroredTarget(snapshot, "openclaw", path), path)
		}
		for path, want := range fixture.userFiles {
			got, readErr := os.ReadFile(nativeBackupMirroredTarget(snapshot, "openclaw", path))
			require.NoError(t, readErr)
			require.Equal(t, want, string(got), path)
		}
	}
}

func TestAgentRootSignature_IgnoresBackupExclusionPolicy(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".codex")
	base := nativebackup.AgentRoots{Name: "codex", Roots: []string{root}}
	withPolicy := base
	withPolicy.ExcludePaths = []string{filepath.Join(root, "cache"), filepath.Join(root, "auth.json")}
	withPolicy.RedactFiles = []nativebackup.FileRedaction{{
		Path: filepath.Join(root, "config.json"),
		Kind: nativebackup.FileRedactionOpenClawConfig,
	}}
	require.Equal(t, agentRootSignature(base), agentRootSignature(withPolicy),
		"backup policy changes must not force another multi-gigabyte safety baseline")
}

func TestSelectAgentRoots_PreservesBackupExclusionPolicy(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".codex")
	policy := nativebackup.AgentRoots{
		Name:         "codex",
		Roots:        []string{root},
		ExcludePaths: []string{filepath.Join(root, "auth.json")},
	}
	selected, err := selectAgentRoots([]nativebackup.AgentRoots{policy}, []string{"codex"})
	require.NoError(t, err)
	require.Equal(t, []nativebackup.AgentRoots{policy}, selected)
}

func TestNativeBackupManager_StartupSafetyBacksUpOnlyMissingAgents(t *testing.T) {
	home := t.TempDir()
	backupsRoot := filepath.Join(home, ".aplexica", "backups")
	hermesRoot := filepath.Join(home, ".hermes")
	kiloRoot := filepath.Join(home, ".config", "kilo")
	require.NoError(t, os.MkdirAll(hermesRoot, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(hermesRoot, "state.db"), []byte("old"), 0o600))
	require.NoError(t, os.MkdirAll(kiloRoot, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(kiloRoot, "AGENTS.md"), []byte("kilo"), 0o600))
	partial := filepath.Join(backupsRoot, nativebackup.SnapshotPrefix+"codex-2026-05-01T00-00-00Z")
	require.NoError(t, os.MkdirAll(partial, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(partial, "partial.jsonl"), []byte("incomplete"), 0o600))

	_, err := nativebackup.Snapshot(
		[]nativebackup.AgentRoots{{Name: "hermes", Roots: []string{hermesRoot}}},
		filepath.Join(backupsRoot, nativebackup.SnapshotPrefix+"2026-05-29T00-00-00Z"),
	)
	require.NoError(t, err)

	mgr := newNativeBackupManager(backupsRoot, func() []nativebackup.AgentRoots {
		return []nativebackup.AgentRoots{
			{Name: "hermes", Roots: []string{hermesRoot}},
			{Name: "kilo", Roots: []string{kiloRoot}},
		}
	})
	blocked := mgr.EnsureStartupSafety(&captureLog{})
	require.Empty(t, blocked)
	require.Empty(t, mgr.EnsureStartupSafety(&captureLog{}),
		"a complete legacy safety snapshot remains valid after file verification")
	mgr.SweepNativeBackupHistory(&captureLog{})
	require.NoDirExists(t, partial, "manifestless snapshots are unusable and must be reclaimed")

	status, err := mgr.Status(nil)
	require.NoError(t, err)
	require.Len(t, status.Safety, 2)
	require.Equal(t, "protected", status.Safety[0].State)
	require.Equal(t, "hermes", status.Safety[0].Agent)
	require.Equal(t, "protected", status.Safety[1].State)
	require.Equal(t, "kilo", status.Safety[1].Agent)

	infos, err := nativebackup.List(backupsRoot)
	require.NoError(t, err)
	var kiloBackups int
	for _, info := range infos {
		if len(info.Agents) == 1 && info.Agents[0] == "kilo" {
			kiloBackups++
		}
	}
	require.Equal(t, 1, kiloBackups)
}

func TestNativeBackupManager_StartupSafetyPrunesCrashResidueBeforeEveryAllocation(t *testing.T) {
	home := t.TempDir()
	backupsRoot := filepath.Join(home, ".aplexica", "backups")
	agentRoot := filepath.Join(home, ".codex")
	require.NoError(t, os.MkdirAll(agentRoot, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(agentRoot, "state.json"), []byte("state"), 0o600))
	agent := nativebackup.AgentRoots{Name: "codex", Roots: []string{agentRoot}}
	oldPartials := []string{
		filepath.Join(backupsRoot, nativebackup.SnapshotPrefix+"codex-2000-01-01T00-00-00Z"),
		filepath.Join(backupsRoot, nativebackup.SnapshotPrefix+"codex-2000-01-01T00-00-01Z"),
	}
	for _, partial := range oldPartials {
		require.NoError(t, os.MkdirAll(partial, 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(partial, "large-partial.bin"), []byte("crash residue"), 0o600))
	}
	require.NoError(t, writeNativeBackupSafetyState(
		filepath.Join(backupsRoot, ".safety.json"),
		nativeBackupSafetyState{Agents: map[string]nativeBackupSafetyRecord{
			agent.Name: {RootSignature: agentRootSignature(agent), BackupID: filepath.Base(oldPartials[0])},
		}},
	))

	previousPartials := append([]string(nil), oldPartials...)
	for attempt := 0; attempt < 2; attempt++ {
		mgr := newNativeBackupManager(backupsRoot, func() []nativebackup.AgentRoots {
			return []nativebackup.AgentRoots{agent}
		})
		var allocated string
		mgr.snapshotSafety = func(_ []nativebackup.AgentRoots, dest string) (nativebackup.Manifest, error) {
			for _, previous := range previousPartials {
				require.NoDirExists(t, previous,
					"all prior manifestless allocations must be reclaimed before the next full copy starts")
			}
			allocated = dest
			require.NoError(t, os.MkdirAll(dest, 0o700))
			require.NoError(t, os.WriteFile(filepath.Join(dest, "new-partial.bin"), []byte("current failed allocation"), 0o600))
			return nativebackup.Manifest{}, errors.New("simulated process death after allocation")
		}

		blocked := mgr.EnsureStartupSafety(&captureLog{})
		require.Equal(t, "simulated process death after allocation", blocked[agent.Name])
		require.NotEmpty(t, allocated)
		entries, err := os.ReadDir(backupsRoot)
		require.NoError(t, err)
		var partials []string
		for _, entry := range entries {
			kind, ok := nativebackup.SnapshotKindFromID(entry.Name())
			if ok && kind == "pre-sync" && entry.IsDir() {
				partials = append(partials, filepath.Join(backupsRoot, entry.Name()))
			}
		}
		require.Equal(t, []string{allocated}, partials,
			"a crash loop may leave only the current failed allocation, never N+1 historical copies")
		previousPartials = partials
	}
}

func TestNativeBackupManager_StartupSafetyFailsClosedWhenCrashResidueCannotBeRemoved(t *testing.T) {
	tests := []struct {
		name       string
		remove     func(string) error
		wantReason string
	}{
		{
			name: "Windows sharing violation",
			remove: func(path string) error {
				return &os.PathError{Op: "RemoveAll", Path: path, Err: errors.New("the process cannot access the file because it is being used by another process")}
			},
			wantReason: "being used by another process",
		},
		{
			name:       "removal success cannot be verified",
			remove:     func(string) error { return nil },
			wantReason: "path still exists",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			backupsRoot := filepath.Join(home, ".aplexica", "backups")
			agentRoot := filepath.Join(home, ".codex")
			require.NoError(t, os.MkdirAll(agentRoot, 0o700))
			require.NoError(t, os.WriteFile(filepath.Join(agentRoot, "state.json"), []byte("state"), 0o600))
			partial := filepath.Join(backupsRoot, nativebackup.SnapshotPrefix+"codex-2000-01-01T00-00-00Z")
			require.NoError(t, os.MkdirAll(partial, 0o700))
			require.NoError(t, os.WriteFile(filepath.Join(partial, "large-partial.bin"), []byte("crash residue"), 0o600))
			agent := nativebackup.AgentRoots{Name: "codex", Roots: []string{agentRoot}}
			mgr := newNativeBackupManager(backupsRoot, func() []nativebackup.AgentRoots {
				return []nativebackup.AgentRoots{agent}
			})
			removeCalls := 0
			mgr.removeIncompletePreSync = func(path string) error {
				removeCalls++
				require.Equal(t, partial, path)
				return tt.remove(path)
			}
			snapshotCalls := 0
			mgr.snapshotSafety = func(_ []nativebackup.AgentRoots, _ string) (nativebackup.Manifest, error) {
				snapshotCalls++
				return nativebackup.Manifest{}, nil
			}

			blocked := mgr.EnsureStartupSafety(&captureLog{})
			require.Contains(t, blocked[agent.Name], tt.wantReason)
			require.Zero(t, snapshotCalls, "a failed cleanup proof must block before another full snapshot starts")
			require.GreaterOrEqual(t, removeCalls, 2, "cleanup is retried at the exact allocation boundary")
			require.DirExists(t, partial)
			state, err := loadNativeBackupSafetyState(mgr.safetyPath())
			require.NoError(t, err)
			require.Contains(t, state.Agents[agent.Name].LastError, tt.wantReason)
		})
	}
}

func TestNativeBackupManager_ColonNamedCodexSafetySnapshotIsReusedAfterRestart(t *testing.T) {
	home := t.TempDir()
	backupsRoot := filepath.Join(home, ".aplexica", "backups")
	codexRoot := filepath.Join(home, ".codex")
	sessionDir := filepath.Join(codexRoot, "sessions", "2026", "06", "30")
	require.NoError(t, os.MkdirAll(sessionDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(sessionDir,
		"rollout-2026-06-30T18:16:48.3NZ-ae04c012.jsonl"), []byte("conversation"), 0o600))
	agents := []nativebackup.AgentRoots{{Name: "codex", Roots: []string{codexRoot}}}

	first := newNativeBackupManager(backupsRoot, func() []nativebackup.AgentRoots { return agents })
	require.Empty(t, first.EnsureStartupSafety(&captureLog{}))
	firstStatus, err := first.Status(nil)
	require.NoError(t, err)
	require.Len(t, firstStatus.Safety, 1)
	firstBackupID := firstStatus.Safety[0].BackupID
	require.NotEmpty(t, firstBackupID)

	// Construct a new manager to model a real daemon restart. The authenticated
	// snapshot must pass its full file/inventory proof instead of being replaced.
	second := newNativeBackupManager(backupsRoot, func() []nativebackup.AgentRoots { return agents })
	snapshotCalls := 0
	second.snapshotSafety = func(_ []nativebackup.AgentRoots, _ string) (nativebackup.Manifest, error) {
		snapshotCalls++
		return nativebackup.Manifest{}, errors.New("unexpected replacement snapshot")
	}
	require.Empty(t, second.EnsureStartupSafety(&captureLog{}))
	require.Zero(t, snapshotCalls, "restart must not even attempt a replacement copy")
	secondStatus, err := second.Status(nil)
	require.NoError(t, err)
	require.Len(t, secondStatus.Safety, 1)
	require.Equal(t, firstBackupID, secondStatus.Safety[0].BackupID)
	infos, err := nativebackup.List(backupsRoot)
	require.NoError(t, err)
	count := 0
	for _, info := range infos {
		if info.Kind == "pre-sync" && len(info.Agents) == 1 && info.Agents[0] == "codex" {
			count++
		}
	}
	require.Equal(t, 1, count, "restart must reuse the colon-named Codex safety snapshot")
}

func TestNativeBackupManager_StartupSafetyCheckpointsBeforeLaterAgentFailure(t *testing.T) {
	home := t.TempDir()
	backupsRoot := filepath.Join(home, ".aplexica", "backups")
	alphaRoot := filepath.Join(home, ".alpha")
	zuluRoot := filepath.Join(home, ".zulu")
	for _, root := range []string{alphaRoot, zuluRoot} {
		require.NoError(t, os.MkdirAll(root, 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(root, "state.json"), []byte(root), 0o600))
	}
	agents := []nativebackup.AgentRoots{
		{Name: "alpha", Roots: []string{alphaRoot}},
		{Name: "zulu", Roots: []string{zuluRoot}},
	}
	mgr := newNativeBackupManager(backupsRoot, func() []nativebackup.AgentRoots { return agents })
	firstCheckpointVisible := false
	mgr.snapshotSafety = func(selected []nativebackup.AgentRoots, dest string) (nativebackup.Manifest, error) {
		if selected[0].Name == "alpha" {
			return nativebackup.SnapshotAuthenticated(selected, dest)
		}
		state, err := loadNativeBackupSafetyState(mgr.safetyPath())
		if err == nil {
			rec := state.Agents["alpha"]
			firstCheckpointVisible = rec.BackupID != "" && rec.RootSignature == agentRootSignature(agents[0])
		}
		return nativebackup.Manifest{}, errors.New("simulated later-agent snapshot failure")
	}

	blocked := mgr.EnsureStartupSafety(&captureLog{})
	require.True(t, firstCheckpointVisible,
		"the first agent's safety record must be durable before the second snapshot starts")
	require.Contains(t, blocked, "zulu")
	state, err := loadNativeBackupSafetyState(mgr.safetyPath())
	require.NoError(t, err)
	require.NotEmpty(t, state.Agents["alpha"].BackupID)
	require.Contains(t, state.Agents["zulu"].LastError, "simulated later-agent")
}

func TestNativeBackupManager_StartupSafetyCheckpointSurvivesInterruption(t *testing.T) {
	home := t.TempDir()
	backupsRoot := filepath.Join(home, ".aplexica", "backups")
	alphaRoot := filepath.Join(home, ".alpha")
	zuluRoot := filepath.Join(home, ".zulu")
	for _, root := range []string{alphaRoot, zuluRoot} {
		require.NoError(t, os.MkdirAll(root, 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(root, "state.json"), []byte(root), 0o600))
	}
	agents := []nativebackup.AgentRoots{
		{Name: "alpha", Roots: []string{alphaRoot}},
		{Name: "zulu", Roots: []string{zuluRoot}},
	}
	mgr := newNativeBackupManager(backupsRoot, func() []nativebackup.AgentRoots { return agents })
	mgr.snapshotSafety = func(selected []nativebackup.AgentRoots, dest string) (nativebackup.Manifest, error) {
		if selected[0].Name == "zulu" {
			panic("simulated daemon interruption")
		}
		return nativebackup.SnapshotAuthenticated(selected, dest)
	}
	interrupted := false
	func() {
		defer func() {
			interrupted = recover() != nil
		}()
		mgr.EnsureStartupSafety(&captureLog{})
	}()
	require.True(t, interrupted)

	state, err := loadNativeBackupSafetyState(mgr.safetyPath())
	require.NoError(t, err)
	require.NotEmpty(t, state.Agents["alpha"].BackupID,
		"a process interruption after alpha must not lose alpha's completed checkpoint")
	require.Empty(t, state.Agents["zulu"].BackupID)

	restarted := newNativeBackupManager(backupsRoot, func() []nativebackup.AgentRoots { return agents })
	require.Empty(t, restarted.EnsureStartupSafety(&captureLog{}))
	infos, err := nativebackup.List(backupsRoot)
	require.NoError(t, err)
	counts := map[string]int{}
	for _, info := range infos {
		if info.Kind == "pre-sync" && len(info.Agents) == 1 {
			counts[info.Agents[0]]++
		}
	}
	require.Equal(t, 1, counts["alpha"], "restart must reuse alpha's checkpoint instead of copying it again")
	require.Equal(t, 1, counts["zulu"])
}

func TestNativeBackupManager_StartupSafetyAdoptsValidAuthenticatedReplacement(t *testing.T) {
	home := t.TempDir()
	backupsRoot := filepath.Join(home, ".aplexica", "backups")
	root := filepath.Join(home, ".hermes")
	require.NoError(t, os.MkdirAll(root, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(root, "state.db"), []byte("state"), 0o600))
	agent := nativebackup.AgentRoots{Name: "hermes", Roots: []string{root}}

	damagedID := nativebackup.SnapshotPrefix + "hermes-2026-07-18T01-00-00Z"
	damagedDir := filepath.Join(backupsRoot, damagedID)
	damagedManifest, err := nativebackup.SnapshotAuthenticated([]nativebackup.AgentRoots{agent}, damagedDir)
	require.NoError(t, err)
	time.Sleep(time.Millisecond)
	replacementID := nativebackup.SnapshotPrefix + "hermes-2026-07-18T02-00-00Z"
	replacementDir := filepath.Join(backupsRoot, replacementID)
	_, err = nativebackup.SnapshotAuthenticated([]nativebackup.AgentRoots{agent}, replacementDir)
	require.NoError(t, err)
	require.NotEmpty(t, damagedManifest.Agents[0].Roots)
	require.NoError(t, os.Remove(filepath.Join(damagedDir, filepath.FromSlash(damagedManifest.Agents[0].Roots[0].Path))))
	require.NoError(t, writeNativeBackupSafetyState(
		filepath.Join(backupsRoot, ".safety.json"),
		nativeBackupSafetyState{Agents: map[string]nativeBackupSafetyRecord{
			"hermes": {RootSignature: agentRootSignature(agent), BackupID: damagedID},
		}},
	))

	mgr := newNativeBackupManager(backupsRoot, func() []nativebackup.AgentRoots { return []nativebackup.AgentRoots{agent} })
	snapshotCalls := 0
	mgr.snapshotSafety = func(selected []nativebackup.AgentRoots, dest string) (nativebackup.Manifest, error) {
		snapshotCalls++
		return nativebackup.SnapshotAuthenticated(selected, dest)
	}
	require.Empty(t, mgr.EnsureStartupSafety(&captureLog{}))
	require.Zero(t, snapshotCalls, "a fully validated replacement must be adopted without another full copy")
	state, err := loadNativeBackupSafetyState(mgr.safetyPath())
	require.NoError(t, err)
	require.Equal(t, replacementID, state.Agents["hermes"].BackupID)
}

// fifoPayloadProbe replaces a snapshot payload with a FIFO and polls for a
// reader. A full payload verifier blocks while opening the FIFO until this
// probe supplies a writer, giving the test an exact signal that payload bytes
// were touched without relying on elapsed-time assertions.
type fifoPayloadProbe struct {
	stop   chan struct{}
	done   chan error
	opened chan struct{}
}

func startFIFOPayloadProbe(path string) *fifoPayloadProbe {
	p := &fifoPayloadProbe{
		stop:   make(chan struct{}),
		done:   make(chan error, 1),
		opened: make(chan struct{}, 1),
	}
	go func() {
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for {
			fd, err := unix.Open(path, unix.O_WRONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
			if err == nil {
				p.opened <- struct{}{}
				p.done <- unix.Close(fd)
				return
			}
			if errors.Is(err, unix.ENOENT) {
				// Pre-allocation cleanup may remove an untrusted/incomplete tree
				// before this sentinel is stopped. Removal proves the FIFO was not
				// opened and is therefore a successful negative observation.
				p.done <- nil
				return
			}
			if !errors.Is(err, unix.ENXIO) {
				p.done <- err
				return
			}
			select {
			case <-p.stop:
				p.done <- nil
				return
			case <-ticker.C:
			}
		}
	}()
	return p
}

func (p *fifoPayloadProbe) stopAndReport(t *testing.T) bool {
	t.Helper()
	close(p.stop)
	require.NoError(t, <-p.done)
	select {
	case <-p.opened:
		return true
	default:
		return false
	}
}

func replaceSnapshotPayloadWithFIFO(t *testing.T, backupDir string, man nativebackup.Manifest) string {
	t.Helper()
	require.Len(t, man.Agents, 1)
	require.Len(t, man.Agents[0].Roots, 1)
	path := filepath.Join(backupDir, filepath.FromSlash(man.Agents[0].Roots[0].Path))
	require.NoError(t, os.Remove(path))
	require.NoError(t, unix.Mkfifo(path, 0o600))
	return path
}

func writeManagerTestManifest(t *testing.T, backupDir string, man nativebackup.Manifest) {
	t.Helper()
	data, err := json.Marshal(man)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(backupDir, nativebackup.ManifestName), data, 0o600))
}

func TestNativeBackupManager_StartupSafetyDoesNotOpenUnrelatedAuthenticatedPayload(t *testing.T) {
	home := t.TempDir()
	backupsRoot := filepath.Join(home, ".aplexica", "backups")
	hermesRoot := filepath.Join(home, ".hermes")
	codexRoot := filepath.Join(home, ".codex")
	for _, root := range []string{hermesRoot, codexRoot} {
		require.NoError(t, os.MkdirAll(root, 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(root, "state.bin"), []byte("state"), 0o600))
	}
	hermes := nativebackup.AgentRoots{Name: "hermes", Roots: []string{hermesRoot}}
	goodID := nativebackup.SnapshotPrefix + "hermes-good"
	_, err := nativebackup.SnapshotAuthenticated([]nativebackup.AgentRoots{hermes}, filepath.Join(backupsRoot, goodID))
	require.NoError(t, err)

	// Make a newer, correctly authenticated Codex manifest declare an 8 GiB
	// payload, then replace that payload with a FIFO open sentinel. Startup for
	// Hermes must authenticate and reject the signed agent metadata without ever
	// opening this unrelated object.
	badID := nativebackup.SnapshotPrefix + "codex-unrelated"
	badDir := filepath.Join(backupsRoot, badID)
	bad, err := nativebackup.SnapshotAuthenticated(
		[]nativebackup.AgentRoots{{Name: "codex", Roots: []string{codexRoot}}}, badDir)
	require.NoError(t, err)
	bad.CreatedAt = time.Now().UTC().Add(time.Hour)
	bad.Agents[0].Roots[0].Bytes = 8 << 30
	bad.Agents[0].Roots[0].SHA256 = strings.Repeat("0", 64)
	require.NoError(t, nativebackup.SignDefaultManifest(&bad, badDir))
	writeManagerTestManifest(t, badDir, bad)
	probe := startFIFOPayloadProbe(replaceSnapshotPayloadWithFIFO(t, badDir, bad))

	mgr := newNativeBackupManager(backupsRoot, func() []nativebackup.AgentRoots {
		return []nativebackup.AgentRoots{hermes}
	})
	snapshotCalls := 0
	mgr.snapshotSafety = func(_ []nativebackup.AgentRoots, _ string) (nativebackup.Manifest, error) {
		snapshotCalls++
		return nativebackup.Manifest{}, errors.New("unexpected replacement snapshot")
	}
	require.Empty(t, mgr.EnsureStartupSafety(&captureLog{}))
	require.False(t, probe.stopAndReport(t), "unrelated authenticated payload must never be opened")
	require.Zero(t, snapshotCalls)
	state, err := loadNativeBackupSafetyState(mgr.safetyPath())
	require.NoError(t, err)
	require.Equal(t, goodID, state.Agents["hermes"].BackupID)
}

func TestNativeBackupManager_StartupSafetyRejectsUntrustedCandidateBeforePayloadOpen(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(t *testing.T, backupDir string, man nativebackup.Manifest)
	}{
		{
			name: "tampered signed metadata",
			mutate: func(t *testing.T, backupDir string, man nativebackup.Manifest) {
				t.Helper()
				// Keep exact agent/root metadata so inspecting it before authentication
				// would classify this as relevant, but invalidate the HMAC.
				man.AplexicaVersion += "-tampered"
				writeManagerTestManifest(t, backupDir, man)
			},
		},
		{
			name: "malformed manifest",
			mutate: func(t *testing.T, backupDir string, _ nativebackup.Manifest) {
				t.Helper()
				require.NoError(t, os.WriteFile(
					filepath.Join(backupDir, nativebackup.ManifestName),
					[]byte(`{"schemaVersion":2,"agents":[`), 0o600))
			},
		},
		{
			name: "v2 schema downgrade marker",
			mutate: func(t *testing.T, backupDir string, man nativebackup.Manifest) {
				t.Helper()
				// A failed v2 authentication must not fall back to unsigned legacy
				// compatibility merely because schemaVersion was changed to zero.
				man.SchemaVersion = 0
				writeManagerTestManifest(t, backupDir, man)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			backupsRoot := filepath.Join(home, ".aplexica", "backups")
			root := filepath.Join(home, ".hermes")
			require.NoError(t, os.MkdirAll(root, 0o700))
			require.NoError(t, os.WriteFile(filepath.Join(root, "state.db"), []byte("state"), 0o600))
			agent := nativebackup.AgentRoots{Name: "hermes", Roots: []string{root}}
			goodID := nativebackup.SnapshotPrefix + "hermes-good"
			_, err := nativebackup.SnapshotAuthenticated([]nativebackup.AgentRoots{agent}, filepath.Join(backupsRoot, goodID))
			require.NoError(t, err)

			badID := nativebackup.SnapshotPrefix + "hermes-untrusted"
			badDir := filepath.Join(backupsRoot, badID)
			bad, err := nativebackup.SnapshotAuthenticated([]nativebackup.AgentRoots{agent}, badDir)
			require.NoError(t, err)
			bad.CreatedAt = time.Now().UTC().Add(time.Hour)
			require.NoError(t, nativebackup.SignDefaultManifest(&bad, badDir))
			writeManagerTestManifest(t, badDir, bad)
			probe := startFIFOPayloadProbe(replaceSnapshotPayloadWithFIFO(t, badDir, bad))
			tt.mutate(t, badDir, bad)
			// Keep malformed candidates first even though List must fall back to the
			// directory timestamp when their manifest cannot be decoded.
			future := time.Now().Add(2 * time.Hour)
			require.NoError(t, os.Chtimes(badDir, future, future))

			mgr := newNativeBackupManager(backupsRoot, func() []nativebackup.AgentRoots {
				return []nativebackup.AgentRoots{agent}
			})
			snapshotCalls := 0
			mgr.snapshotSafety = func(_ []nativebackup.AgentRoots, _ string) (nativebackup.Manifest, error) {
				snapshotCalls++
				return nativebackup.Manifest{}, errors.New("unexpected replacement snapshot")
			}
			require.Empty(t, mgr.EnsureStartupSafety(&captureLog{}))
			require.False(t, probe.stopAndReport(t), "untrusted candidate payload must never be opened")
			require.Zero(t, snapshotCalls)
			state, err := loadNativeBackupSafetyState(mgr.safetyPath())
			require.NoError(t, err)
			require.Equal(t, goodID, state.Agents["hermes"].BackupID)
		})
	}
}

func TestNativeBackupManager_StartupSafetyManifestFIFOCannotBlockCandidateEnumeration(t *testing.T) {
	home := t.TempDir()
	backupsRoot := filepath.Join(home, ".aplexica", "backups")
	root := filepath.Join(home, ".hermes")
	require.NoError(t, os.MkdirAll(root, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(root, "state.db"), []byte("state"), 0o600))
	agent := nativebackup.AgentRoots{Name: "hermes", Roots: []string{root}}
	goodID := nativebackup.SnapshotPrefix + "hermes-good"
	_, err := nativebackup.SnapshotAuthenticated([]nativebackup.AgentRoots{agent}, filepath.Join(backupsRoot, goodID))
	require.NoError(t, err)

	badDir := filepath.Join(backupsRoot, nativebackup.SnapshotPrefix+"manifest-fifo")
	require.NoError(t, os.MkdirAll(badDir, 0o700))
	require.NoError(t, unix.Mkfifo(filepath.Join(badDir, nativebackup.ManifestName), 0o600))

	mgr := newNativeBackupManager(backupsRoot, func() []nativebackup.AgentRoots { return []nativebackup.AgentRoots{agent} })
	mgr.snapshotSafety = func(_ []nativebackup.AgentRoots, _ string) (nativebackup.Manifest, error) {
		return nativebackup.Manifest{}, errors.New("unexpected replacement snapshot")
	}
	done := make(chan map[string]string, 1)
	go func() { done <- mgr.EnsureStartupSafety(&captureLog{}) }()
	select {
	case blocked := <-done:
		require.Empty(t, blocked)
	case <-time.After(2 * time.Second):
		// Unblock the exact regression (an ordinary blocking FIFO read) before
		// failing so the manager goroutine can release its locks cleanly.
		fifo := filepath.Join(badDir, nativebackup.ManifestName)
		if fd, openErr := unix.Open(fifo, unix.O_WRONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0); openErr == nil {
			_ = unix.Close(fd)
		}
		select {
		case <-done:
		case <-time.After(time.Second):
		}
		t.Fatal("startup candidate enumeration blocked on a manifest FIFO")
	}
	state, err := loadNativeBackupSafetyState(mgr.safetyPath())
	require.NoError(t, err)
	require.Equal(t, goodID, state.Agents["hermes"].BackupID)
}

func TestNativeBackupManager_StartupSafetyAdoptsOrphanAfterCheckpointedFailure(t *testing.T) {
	home := t.TempDir()
	backupsRoot := filepath.Join(home, ".aplexica", "backups")
	root := filepath.Join(home, ".hermes")
	require.NoError(t, os.MkdirAll(root, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(root, "state.db"), []byte("state"), 0o600))
	agent := nativebackup.AgentRoots{Name: "hermes", Roots: []string{root}}

	orphanID := nativebackup.SnapshotPrefix + "hermes-2000-01-01T00-00-00Z"
	_, err := nativebackup.SnapshotAuthenticated(
		[]nativebackup.AgentRoots{agent}, filepath.Join(backupsRoot, orphanID))
	require.NoError(t, err)
	require.NoError(t, writeNativeBackupSafetyState(
		filepath.Join(backupsRoot, ".safety.json"),
		nativeBackupSafetyState{Agents: map[string]nativeBackupSafetyRecord{
			"hermes": {
				RootSignature: agentRootSignature(agent),
				LastError:     "prior retry failed before manifest commit",
				LastFailureAt: time.Now().UTC(),
			},
		}},
	))

	mgr := newNativeBackupManager(backupsRoot, func() []nativebackup.AgentRoots { return []nativebackup.AgentRoots{agent} })
	snapshotCalls := 0
	mgr.snapshotSafety = func(selected []nativebackup.AgentRoots, dest string) (nativebackup.Manifest, error) {
		snapshotCalls++
		return nativebackup.SnapshotAuthenticated(selected, dest)
	}
	require.Empty(t, mgr.EnsureStartupSafety(&captureLog{}))
	require.Zero(t, snapshotCalls,
		"a current-signature failed record must adopt a completed authenticated retry")
	state, err := loadNativeBackupSafetyState(mgr.safetyPath())
	require.NoError(t, err)
	require.Equal(t, orphanID, state.Agents["hermes"].BackupID)
	require.Empty(t, state.Agents["hermes"].LastError)
	require.True(t, state.Agents["hermes"].LastFailureAt.IsZero())
}

func TestNativeBackupManager_StartupSafetyRejectsSameSizeCorruptOrphan(t *testing.T) {
	home := t.TempDir()
	backupsRoot := filepath.Join(home, ".aplexica", "backups")
	root := filepath.Join(home, ".hermes")
	require.NoError(t, os.MkdirAll(root, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(root, "state.db"), []byte("state"), 0o600))
	agent := nativebackup.AgentRoots{Name: "hermes", Roots: []string{root}}

	corruptID := nativebackup.SnapshotPrefix + "hermes-2000-01-01T00-00-00Z"
	corruptDir := filepath.Join(backupsRoot, corruptID)
	man, err := nativebackup.SnapshotAuthenticated([]nativebackup.AgentRoots{agent}, corruptDir)
	require.NoError(t, err)
	require.NotEmpty(t, man.Agents)
	require.NotEmpty(t, man.Agents[0].Roots)
	copyPath := filepath.Join(corruptDir, filepath.FromSlash(man.Agents[0].Roots[0].Path))
	original, err := os.ReadFile(copyPath)
	require.NoError(t, err)
	require.NotEmpty(t, original)
	corrupt := append([]byte{}, original...)
	corrupt[0] ^= 0xff
	require.NoError(t, os.WriteFile(copyPath, corrupt, 0o600))
	require.NoError(t, writeNativeBackupSafetyState(
		filepath.Join(backupsRoot, ".safety.json"),
		nativeBackupSafetyState{Agents: map[string]nativeBackupSafetyRecord{
			"hermes": {
				RootSignature: agentRootSignature(agent),
				LastError:     "prior retry failed before manifest commit",
			},
		}},
	))

	mgr := newNativeBackupManager(backupsRoot, func() []nativebackup.AgentRoots { return []nativebackup.AgentRoots{agent} })
	snapshotCalls := 0
	mgr.snapshotSafety = func(selected []nativebackup.AgentRoots, dest string) (nativebackup.Manifest, error) {
		snapshotCalls++
		return nativebackup.SnapshotAuthenticated(selected, dest)
	}
	require.Empty(t, mgr.EnsureStartupSafety(&captureLog{}))
	require.Equal(t, 1, snapshotCalls,
		"same-size content corruption must reject the orphan and take a replacement")
	state, err := loadNativeBackupSafetyState(mgr.safetyPath())
	require.NoError(t, err)
	require.NotEqual(t, corruptID, state.Agents["hermes"].BackupID)
}

func TestNativeBackupManager_StartupSafetyRejectsSameSizeCorruptReferencedBackup(t *testing.T) {
	home := t.TempDir()
	backupsRoot := filepath.Join(home, ".aplexica", "backups")
	root := filepath.Join(home, ".hermes")
	require.NoError(t, os.MkdirAll(root, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(root, "state.db"), []byte("state"), 0o600))
	agent := nativebackup.AgentRoots{Name: "hermes", Roots: []string{root}}

	corruptID := nativebackup.SnapshotPrefix + "hermes-2000-01-01T00-00-00Z"
	corruptDir := filepath.Join(backupsRoot, corruptID)
	man, err := nativebackup.SnapshotAuthenticated([]nativebackup.AgentRoots{agent}, corruptDir)
	require.NoError(t, err)
	require.NotEmpty(t, man.Agents)
	require.NotEmpty(t, man.Agents[0].Roots)
	copyPath := filepath.Join(corruptDir, filepath.FromSlash(man.Agents[0].Roots[0].Path))
	original, err := os.ReadFile(copyPath)
	require.NoError(t, err)
	require.NotEmpty(t, original)
	corrupt := append([]byte{}, original...)
	corrupt[0] ^= 0xff
	require.NoError(t, os.WriteFile(copyPath, corrupt, 0o600))
	require.NoError(t, writeNativeBackupSafetyState(
		filepath.Join(backupsRoot, ".safety.json"),
		nativeBackupSafetyState{Agents: map[string]nativeBackupSafetyRecord{
			"hermes": {
				RootSignature: agentRootSignature(agent),
				BackupID:      corruptID,
			},
		}},
	))

	mgr := newNativeBackupManager(backupsRoot, func() []nativebackup.AgentRoots { return []nativebackup.AgentRoots{agent} })
	snapshotCalls := 0
	mgr.snapshotSafety = func(selected []nativebackup.AgentRoots, dest string) (nativebackup.Manifest, error) {
		snapshotCalls++
		return nativebackup.SnapshotAuthenticated(selected, dest)
	}
	require.Empty(t, mgr.EnsureStartupSafety(&captureLog{}))
	require.Equal(t, 1, snapshotCalls,
		"same-size content corruption must invalidate an explicitly referenced safety snapshot")
	state, err := loadNativeBackupSafetyState(mgr.safetyPath())
	require.NoError(t, err)
	require.NotEqual(t, corruptID, state.Agents["hermes"].BackupID)
}

func TestNativeBackupManager_StartupSafetyPrunesSupersededPreSync(t *testing.T) {
	home := t.TempDir()
	backupsRoot := filepath.Join(home, ".aplexica", "backups")
	hermesRoot := filepath.Join(home, ".hermes")
	require.NoError(t, os.MkdirAll(hermesRoot, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(hermesRoot, "state.db"), []byte("state"), 0o600))

	oldDir := filepath.Join(backupsRoot, nativebackup.SnapshotPrefix+"hermes-2026-05-01T00-00-00Z")
	middleDir := filepath.Join(backupsRoot, nativebackup.SnapshotPrefix+"hermes-2026-05-01T12-00-00Z")
	newDir := filepath.Join(backupsRoot, nativebackup.SnapshotPrefix+"hermes-2026-05-02T00-00-00Z")
	_, err := nativebackup.SnapshotAuthenticated([]nativebackup.AgentRoots{{Name: "hermes", Roots: []string{hermesRoot}}}, oldDir)
	require.NoError(t, err)
	time.Sleep(time.Millisecond)
	_, err = nativebackup.SnapshotAuthenticated([]nativebackup.AgentRoots{{Name: "hermes", Roots: []string{hermesRoot}}}, middleDir)
	require.NoError(t, err)
	time.Sleep(time.Millisecond)
	_, err = nativebackup.SnapshotAuthenticated([]nativebackup.AgentRoots{{Name: "hermes", Roots: []string{hermesRoot}}}, newDir)
	require.NoError(t, err)
	// Candidate ordering must use authenticated CreatedAt, not attacker-influenced
	// directory metadata gathered before manifest authentication.
	future := time.Now().Add(24 * time.Hour)
	require.NoError(t, os.Chtimes(oldDir, future, future))

	mgr := newNativeBackupManager(backupsRoot, func() []nativebackup.AgentRoots {
		return []nativebackup.AgentRoots{{Name: "hermes", Roots: []string{hermesRoot}}}
	})
	require.Empty(t, mgr.EnsureStartupSafety(&captureLog{}))
	mgr.SweepNativeBackupHistory(&captureLog{})

	status, err := mgr.Status(nil)
	require.NoError(t, err)
	require.Len(t, status.Safety, 1)
	require.Equal(t, filepath.Base(newDir), status.Safety[0].BackupID)
	require.DirExists(t, oldDir, "the original pre-Aplexica rollback baseline must survive")
	require.NoDirExists(t, middleDir, "redundant intermediate safety snapshots should be reclaimed")
	require.DirExists(t, newDir)
}

func TestNativeBackupManager_StartupSafetyReplacesDamagedReferencedBackup(t *testing.T) {
	home := t.TempDir()
	backupsRoot := filepath.Join(home, ".aplexica", "backups")
	hermesRoot := filepath.Join(home, ".hermes")
	require.NoError(t, os.MkdirAll(hermesRoot, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(hermesRoot, "state.db"), []byte("state"), 0o600))
	agent := nativebackup.AgentRoots{Name: "hermes", Roots: []string{hermesRoot}}

	oldID := nativebackup.SnapshotPrefix + "hermes-2026-05-01T00-00-00Z"
	oldDir := filepath.Join(backupsRoot, oldID)
	_, err := nativebackup.SnapshotAuthenticated([]nativebackup.AgentRoots{agent}, oldDir)
	require.NoError(t, err)
	oldManifest, err := nativebackup.ReadManifest(oldDir)
	require.NoError(t, err)
	require.NotEmpty(t, oldManifest.Agents)
	require.NotEmpty(t, oldManifest.Agents[0].Roots)
	require.NoError(t, writeNativeBackupSafetyState(
		filepath.Join(backupsRoot, ".safety.json"),
		nativeBackupSafetyState{Agents: map[string]nativeBackupSafetyRecord{
			"hermes": {RootSignature: agentRootSignature(agent), BackupID: oldID},
		}},
	))
	require.NoError(t, os.Remove(filepath.Join(oldDir, filepath.FromSlash(oldManifest.Agents[0].Roots[0].Path))))

	// A corrupt non-safety recovery snapshot is user-visible history. Startup
	// cleanup must not silently delete it merely because its manifest is bad.
	manualDir := filepath.Join(backupsRoot, nativebackup.ManualPrefix+"2026-05-01T00-00-00Z")
	require.NoError(t, os.MkdirAll(manualDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(manualDir, "partial"), []byte("keep"), 0o600))

	mgr := newNativeBackupManager(backupsRoot, func() []nativebackup.AgentRoots {
		return []nativebackup.AgentRoots{agent}
	})
	require.Empty(t, mgr.EnsureStartupSafety(&captureLog{}))
	mgr.SweepNativeBackupHistory(&captureLog{})

	status, err := mgr.Status(nil)
	require.NoError(t, err)
	require.Len(t, status.Safety, 1)
	require.Equal(t, "protected", status.Safety[0].State)
	require.NotEqual(t, oldID, status.Safety[0].BackupID,
		"a damaged referenced backup must never satisfy the safety gate")
	require.DirExists(t, oldDir, "the original baseline remains available for partial forensic recovery")
	require.DirExists(t, manualDir, "startup cleanup must preserve non-safety recovery history")
}

func TestNativeBackupManager_BackgroundMaintenanceSanitizesV2AndSkipsLegacy(t *testing.T) {
	home := t.TempDir()
	backupsRoot := filepath.Join(home, ".aplexica", "backups")
	hermesRoot := filepath.Join(home, ".hermes")
	for rel, value := range map[string]string{
		"state.db":      "conversation-state",
		"memories/note": "user-memory",
		"cache/model":   "rebuildable-cache",
		".env":          "TELEGRAM_BOT_TOKEN=secret",
	} {
		path := filepath.Join(hermesRoot, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
		require.NoError(t, os.WriteFile(path, []byte(value), 0o600))
	}
	oldRoots := []nativebackup.AgentRoots{{Name: "hermes", Roots: []string{hermesRoot}}}
	v2Dir := filepath.Join(backupsRoot, nativebackup.ManualPrefix+"v2-old-policy")
	_, err := nativebackup.SnapshotAuthenticated(oldRoots, v2Dir)
	require.NoError(t, err)
	legacyDir := filepath.Join(backupsRoot, nativebackup.ManualPrefix+"legacy-old-policy")
	_, err = nativebackup.Snapshot(oldRoots, legacyDir)
	require.NoError(t, err)
	legacyManifestBefore, err := os.ReadFile(filepath.Join(legacyDir, nativebackup.ManifestName))
	require.NoError(t, err)

	policy := nativebackup.AgentRoots{
		Name:         "hermes",
		Roots:        []string{hermesRoot},
		ExcludePaths: nativeBackupExcludePaths("hermes", []string{hermesRoot}),
	}
	mgr := newNativeBackupManager(backupsRoot, func() []nativebackup.AgentRoots { return []nativebackup.AgentRoots{policy} })
	log := &captureLog{}
	mgr.SweepNativeBackupHistory(log)

	mirroredRoot := filepath.Clean(hermesRoot)
	mirroredRoot = strings.TrimPrefix(mirroredRoot, filepath.VolumeName(mirroredRoot))
	mirroredRoot = strings.TrimPrefix(mirroredRoot, string(filepath.Separator))
	mirroredV2 := filepath.Join(v2Dir, "hermes", mirroredRoot)
	for _, excluded := range []string{"cache/model", ".env"} {
		_, err := os.Stat(filepath.Join(mirroredV2, filepath.FromSlash(excluded)))
		require.ErrorIs(t, err, os.ErrNotExist)
	}
	for rel, want := range map[string]string{"state.db": "conversation-state", "memories/note": "user-memory"} {
		got, err := os.ReadFile(filepath.Join(mirroredV2, filepath.FromSlash(rel)))
		require.NoError(t, err)
		require.Equal(t, want, string(got))
	}
	v2Manifest, err := nativebackup.ReadManifest(v2Dir)
	require.NoError(t, err)
	require.NoError(t, nativebackup.VerifyDefaultManifest(v2Manifest, v2Dir))
	legacyManifestAfter, err := os.ReadFile(filepath.Join(legacyDir, nativebackup.ManifestName))
	require.NoError(t, err)
	require.Equal(t, legacyManifestBefore, legacyManifestAfter, "unsigned legacy data must remain byte-for-byte unchanged")
	require.Contains(t, strings.Join(log.infos, "|"), "native backup sanitizer: maintenance complete")
}

func TestNativeBackupManager_BackgroundMaintenanceSanitizesKnownAgentWithoutDiscovery(t *testing.T) {
	home := t.TempDir()
	backupsRoot := filepath.Join(home, ".aplexica", "backups")
	hermesRoot := filepath.Join(home, ".hermes")
	for rel, value := range map[string]string{
		"state.db": "conversation-state",
		".env":     "TELEGRAM_BOT_TOKEN=secret",
	} {
		path := filepath.Join(hermesRoot, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
		require.NoError(t, os.WriteFile(path, []byte(value), 0o600))
	}
	snapshotDir := filepath.Join(backupsRoot, nativebackup.ManualPrefix+"dormant-hermes")
	_, err := nativebackup.SnapshotAuthenticated(
		[]nativebackup.AgentRoots{{Name: "hermes", Roots: []string{hermesRoot}}}, snapshotDir)
	require.NoError(t, err)

	// Discovery may be empty after Hermes is uninstalled. Its authenticated
	// snapshot still carries the original root, so the built-in agent policy
	// must continue to remove credentials and rebuildable state.
	mgr := newNativeBackupManager(backupsRoot, func() []nativebackup.AgentRoots { return nil })
	mgr.SweepNativeBackupHistory(&captureLog{})

	mirroredRoot := filepath.Clean(hermesRoot)
	mirroredRoot = strings.TrimPrefix(mirroredRoot, filepath.VolumeName(mirroredRoot))
	mirroredRoot = strings.TrimPrefix(mirroredRoot, string(filepath.Separator))
	mirrored := filepath.Join(snapshotDir, "hermes", mirroredRoot)
	require.NoFileExists(t, filepath.Join(mirrored, ".env"))
	got, err := os.ReadFile(filepath.Join(mirrored, "state.db"))
	require.NoError(t, err)
	require.Equal(t, "conversation-state", string(got))
	man, err := nativebackup.ReadManifest(snapshotDir)
	require.NoError(t, err)
	require.NoError(t, nativebackup.VerifyDefaultManifest(man, snapshotDir))
}

func TestNativeBackupManager_BackgroundMaintenanceSerializesUnderOpMu(t *testing.T) {
	backupsRoot := filepath.Join(t.TempDir(), "backups")
	mgr := newNativeBackupManager(backupsRoot, func() []nativebackup.AgentRoots { return nil })
	mgr.opMu.Lock()
	entered := make(chan struct{})
	done := make(chan struct{})
	go func() {
		close(entered)
		mgr.SweepNativeBackupHistory(nil)
		close(done)
	}()
	<-entered
	select {
	case <-done:
		t.Fatal("maintenance escaped opMu serialization")
	default:
	}
	mgr.opMu.Unlock()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("maintenance did not resume after opMu release")
	}
}

func TestNativeBackupManager_BackgroundMaintenanceDefersNewSanitizerWhenRecoveryIncomplete(t *testing.T) {
	for _, tc := range []struct {
		name     string
		recovery nativebackup.SanitizeRecoveryResult
		err      error
	}{
		{name: "pending cleanup", recovery: nativebackup.SanitizeRecoveryResult{Pending: 1}},
		{name: "recovery error", err: errors.New("sharing violation removing rollback tree")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backupsRoot := filepath.Join(t.TempDir(), "backups")
			require.NoError(t, os.MkdirAll(filepath.Join(backupsRoot, nativebackup.ManualPrefix+"candidate"), 0o700))
			mgr := newNativeBackupManager(backupsRoot, func() []nativebackup.AgentRoots { return nil })
			mgr.recoverSanitizeTransactions = func(context.Context, string, string, bool) (nativebackup.SanitizeRecoveryResult, error) {
				return tc.recovery, tc.err
			}
			sanitizeCalls := 0
			mgr.sanitizeSnapshot = func(context.Context, string, nativebackup.SanitizeOptions) (nativebackup.SanitizeResult, error) {
				sanitizeCalls++
				return nativebackup.SanitizeResult{Status: nativebackup.SanitizeUnchanged}, nil
			}

			mgr.SweepNativeBackupHistory(&captureLog{})

			require.Zero(t, sanitizeCalls,
				"an unresolved transaction must not create another full hidden rebuild on restart")
		})
	}
}

func TestNativeBackupManager_SaveRetentionSerializesUnderOpMu(t *testing.T) {
	backupsRoot := filepath.Join(t.TempDir(), "backups")
	mgr := newNativeBackupManager(backupsRoot, func() []nativebackup.AgentRoots { return nil })
	mgr.opMu.Lock()
	entered := make(chan struct{})
	done := make(chan struct{})
	go func() {
		close(entered)
		_, _ = mgr.SaveRetention(nativebackup.RetentionConfig{})
		close(done)
	}()
	<-entered
	select {
	case <-done:
		t.Fatal("retention escaped opMu serialization")
	default:
	}
	mgr.opMu.Unlock()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("retention did not resume after opMu release")
	}
}

func TestNativeBackupManager_StartupSafetyAcceptsManualSafetyReference(t *testing.T) {
	home := t.TempDir()
	backupsRoot := filepath.Join(home, ".aplexica", "backups")
	hermesRoot := filepath.Join(home, ".hermes")
	require.NoError(t, os.MkdirAll(hermesRoot, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(hermesRoot, "state.db"), []byte("state"), 0o600))
	mgr := newNativeBackupManager(backupsRoot, func() []nativebackup.AgentRoots {
		return []nativebackup.AgentRoots{{Name: "hermes", Roots: []string{hermesRoot}}}
	})

	manual, err := mgr.Create("manual", []string{"hermes"})
	require.NoError(t, err)
	require.Empty(t, mgr.EnsureStartupSafety(&captureLog{}))

	status, err := mgr.Status(nil)
	require.NoError(t, err)
	require.Len(t, status.Safety, 1)
	require.Equal(t, manual.ID, status.Safety[0].BackupID)
	infos, err := nativebackup.List(backupsRoot)
	require.NoError(t, err)
	for _, info := range infos {
		require.NotEqual(t, "pre-sync", info.Kind,
			"a valid manual safety reference must not trigger a redundant startup snapshot")
	}
}

func TestNativeBackupManager_OverrideClearsBlockedState(t *testing.T) {
	home := t.TempDir()
	backupsRoot := filepath.Join(home, "backups-blocked-by-file")
	require.NoError(t, os.WriteFile(backupsRoot, []byte("not a dir"), 0o600))
	kiloRoot := filepath.Join(home, ".config", "kilo")
	require.NoError(t, os.MkdirAll(kiloRoot, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(kiloRoot, "AGENTS.md"), []byte("kilo"), 0o600))

	mgr := newNativeBackupManager(backupsRoot, func() []nativebackup.AgentRoots {
		return []nativebackup.AgentRoots{{Name: "kilo", Roots: []string{kiloRoot}}}
	})
	blocked := mgr.EnsureStartupSafety(&captureLog{})
	require.Contains(t, blocked, "kilo")

	require.NoError(t, os.Remove(backupsRoot))
	require.NoError(t, os.MkdirAll(backupsRoot, 0o700))
	st, err := mgr.Override("kilo")
	require.NoError(t, err)
	require.Equal(t, "overridden", st.State)
	require.True(t, st.Override)

	status, err := mgr.Status(nil)
	require.NoError(t, err)
	require.Equal(t, "overridden", status.Safety[0].State)
}

func TestNativeBackupManager_SaveSchedulePreservesLastRunAt(t *testing.T) {
	home := t.TempDir()
	backupsRoot := filepath.Join(home, ".aplexica", "backups")
	mgr := newNativeBackupManager(backupsRoot, func() []nativebackup.AgentRoots { return nil })

	lastRun := time.Date(2026, 7, 5, 18, 0, 0, 0, time.UTC)
	require.NoError(t, writeNativeBackupSchedule(mgr.schedulePath(), nativebackup.ScheduleConfig{
		Enabled:         true,
		IntervalMinutes: 60,
		Destination:     "local",
		LastRunAt:       lastRun,
	}))

	saved, err := mgr.SaveSchedule(nativebackup.ScheduleConfig{
		Enabled:         true,
		IntervalMinutes: 1440,
		Destination:     "cloud",
	})
	require.NoError(t, err)
	require.Equal(t, lastRun, saved.LastRunAt)
	require.False(t, saved.NextRunAt.IsZero())

	reloaded, err := mgr.LoadSchedule()
	require.NoError(t, err)
	require.Equal(t, lastRun, reloaded.LastRunAt)
	require.Equal(t, "cloud", reloaded.Destination)
}

func TestNativeBackupManager_RetainsManualHistoryPerAgent(t *testing.T) {
	home := t.TempDir()
	backupsRoot := filepath.Join(home, ".aplexica", "backups")
	claudeRoot := filepath.Join(home, ".claude")
	codexRoot := filepath.Join(home, ".codex")
	require.NoError(t, os.MkdirAll(claudeRoot, 0o700))
	require.NoError(t, os.MkdirAll(codexRoot, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(claudeRoot, "CLAUDE.md"), []byte("initial"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(codexRoot, "AGENTS.md"), []byte("initial"), 0o600))

	_, err := nativebackup.Snapshot(
		[]nativebackup.AgentRoots{{Name: "claude", Roots: []string{claudeRoot}}},
		filepath.Join(backupsRoot, nativebackup.SnapshotPrefix+"claude-2026-05-29T00-00-00Z"),
	)
	require.NoError(t, err)

	mgr := newNativeBackupManager(backupsRoot, func() []nativebackup.AgentRoots {
		return []nativebackup.AgentRoots{
			{Name: "claude", Roots: []string{claudeRoot}},
			{Name: "codex", Roots: []string{codexRoot}},
		}
	})
	_, err = mgr.SaveRetention(nativebackup.RetentionConfig{
		PerAgent: map[string]int{"claude": 1, "codex": 2},
	})
	require.NoError(t, err)

	for i := 0; i < 3; i++ {
		require.NoError(t, os.WriteFile(filepath.Join(claudeRoot, "CLAUDE.md"), []byte("claude"), 0o600))
		_, err := mgr.Create("manual", []string{"claude"})
		require.NoError(t, err)
	}
	for i := 0; i < 3; i++ {
		require.NoError(t, os.WriteFile(filepath.Join(codexRoot, "AGENTS.md"), []byte("codex"), 0o600))
		_, err := mgr.Create("manual", []string{"codex"})
		require.NoError(t, err)
	}

	infos, err := nativebackup.List(backupsRoot)
	require.NoError(t, err)
	counts := map[string]int{}
	var safetyCount int
	for _, info := range infos {
		if info.Kind == "pre-sync" {
			safetyCount++
			continue
		}
		if info.Kind != "manual" || len(info.Agents) != 1 {
			continue
		}
		counts[info.Agents[0]]++
	}
	require.Equal(t, 1, safetyCount, "pre-Aplexica safety snapshots must not be pruned by manual retention")
	require.Equal(t, 1, counts["claude"])
	require.Equal(t, 2, counts["codex"])
}

func TestNativeBackupManager_DeleteRemovesSnapshotAndClearsSafetyReference(t *testing.T) {
	home := t.TempDir()
	backupsRoot := filepath.Join(home, ".aplexica", "backups")
	kiloRoot := filepath.Join(home, ".config", "kilo")
	require.NoError(t, os.MkdirAll(kiloRoot, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(kiloRoot, "AGENTS.md"), []byte("kilo"), 0o600))

	mgr := newNativeBackupManager(backupsRoot, func() []nativebackup.AgentRoots {
		return []nativebackup.AgentRoots{{Name: "kilo", Roots: []string{kiloRoot}}}
	})
	info, err := mgr.Create("manual", []string{"kilo"})
	require.NoError(t, err)

	status, err := mgr.Status(nil)
	require.NoError(t, err)
	require.Equal(t, "protected", status.Safety[0].State)
	require.Equal(t, info.ID, status.Safety[0].BackupID)

	deleted, err := mgr.Delete(info.ID)
	require.NoError(t, err)
	require.Equal(t, info.ID, deleted.ID)
	require.NoDirExists(t, filepath.Join(backupsRoot, info.ID))

	status, err = mgr.Status(nil)
	require.NoError(t, err)
	require.Equal(t, "backup_required", status.Safety[0].State)
	require.Empty(t, status.Safety[0].BackupID)
}
