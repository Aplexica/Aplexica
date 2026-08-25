package syncd

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter/codex"
	"github.com/aplexica/aplexica/internal/project"
	"github.com/stretchr/testify/require"
)

// TestOrchestrator_WatchFolder proves the runtime project-folder watch path:
// WatchFolder must (a) BACKFILL — import files already on disk in the folder
// the moment it's registered — and (b) LIVE-WATCH — pick up subsequent edits in
// that same folder, both flowing through the shared debouncer -> handleEvent
// pipeline so the folder's memory fans out to the same folder's sibling files
// (folder-local fan-out, identical to TestOrchestrator_Recursive_FansOutFromSubdir).
//
// Project scope is the load-bearing detail: the watched folder is `git init`ed
// so project.Detect reports VCS=git and the ad-hoc->global downgrade
// (DowngradeAdHocToGlobal, internal/adapter/opaque.go) does NOT fire. The
// imported AGENTS.md therefore stays ScopeProject and fans out to the FOLDER's
// own CLAUDE.md (codex/kilo/claude-code NativePath rooted at contextDir), rather
// than being downgraded to global and routed under ~/.codex or ~/.claude.
func TestOrchestrator_WatchFolder(t *testing.T) {
	root := realTempDir(t)

	// Primary watched dir (the orchestrator's cfg.Dir). Kept separate from the
	// project folder we register at runtime so its events don't intermingle.
	watched := filepath.Join(root, "proj")
	require.NoError(t, os.MkdirAll(watched, 0o755))

	// A SEPARATE project folder we'll begin watching at runtime via WatchFolder.
	// git init it so project.Detect sees VCS=git: this keeps imported artifacts
	// ScopeProject (no ad-hoc->global downgrade) so fan-out targets the folder's
	// own CLAUDE.md instead of the global ~/.codex root.
	extra := filepath.Join(root, "extra-proj")
	require.NoError(t, os.MkdirAll(extra, 0o755))
	require.NoError(t, exec.Command("git", "init", extra).Run())

	// BACKFILL setup: an AGENTS.md already on disk BEFORE WatchFolder is called.
	// WatchFolder's scan must import it and fan it out to extra/CLAUDE.md.
	require.NoError(t, os.WriteFile(filepath.Join(extra, "AGENTS.md"),
		[]byte("# proj memory\n"), 0o644))

	adapters, store, _ := buildAllThreeAdapters(t, root)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	orch, err := NewOrchestrator(Config{
		Dir:         watched,
		Adapters:    adapters,
		Store:       store,
		QuietPeriod: 100 * time.Millisecond,
		GuardWindow: 2 * time.Second,
	})
	require.NoError(t, err)
	defer orch.Close()

	go orch.Run(ctx)
	time.Sleep(150 * time.Millisecond)

	// Begin watching the project folder at runtime. This adds a watcher feeding
	// the shared debouncer AND backfills files already present (the AGENTS.md
	// above).
	require.NoError(t, orch.WatchFolder(ctx, extra))

	// BACKFILL assertion: the pre-existing AGENTS.md should import and fan out
	// to the folder's own CLAUDE.md (claude-code NativePath rooted at contextDir
	// = extra), proving the scan ran AND the artifact stayed project-scoped.
	claudePath := filepath.Join(extra, "CLAUDE.md")
	require.Eventually(t, func() bool {
		_, err := os.Stat(claudePath)
		return err == nil
	}, 4*time.Second, 100*time.Millisecond,
		"backfill: pre-existing extra/AGENTS.md must import + fan out to extra/CLAUDE.md")

	got, err := os.ReadFile(claudePath)
	require.NoError(t, err)
	require.Equal(t, "# proj memory\n", string(got),
		"backfilled CLAUDE.md must mirror the pre-existing AGENTS.md content")

	// LIVE-WATCH assertion: now that the watcher is running, a NEW edit to
	// extra/AGENTS.md must propagate to extra/CLAUDE.md — this can only work via
	// the live watcher (backfill already ran once, above).
	require.NoError(t, os.WriteFile(filepath.Join(extra, "AGENTS.md"),
		[]byte("# proj memory v2\n"), 0o644))

	require.Eventually(t, func() bool {
		b, err := os.ReadFile(claudePath)
		return err == nil && string(b) == "# proj memory v2\n"
	}, 4*time.Second, 100*time.Millisecond,
		"live watch: a subsequent edit to extra/AGENTS.md must reach extra/CLAUDE.md")
}

// TestOrchestrator_WatchFolder_Idempotent proves WatchFolder's dedup guard:
// registering the same folder twice (the daemon boot-window seed-vs-onRegister
// race, or re-approving an already-registered project) must be a no-op the
// second time — no duplicate fsnotify watcher + goroutine leak.
func TestOrchestrator_WatchFolder_Idempotent(t *testing.T) {
	root := realTempDir(t)
	watched := filepath.Join(root, "proj")
	require.NoError(t, os.MkdirAll(watched, 0o755))

	dir := filepath.Join(root, "dup-proj")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, exec.Command("git", "init", dir).Run())

	adapters, store, _ := buildAllThreeAdapters(t, root)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	orch, err := NewOrchestrator(Config{
		Dir:         watched,
		Adapters:    adapters,
		Store:       store,
		QuietPeriod: 100 * time.Millisecond,
		GuardWindow: 2 * time.Second,
	})
	require.NoError(t, err)
	defer orch.Close()

	go orch.Run(ctx)
	time.Sleep(150 * time.Millisecond)

	require.NoError(t, orch.WatchFolder(ctx, dir))
	before := len(orch.extraWatchers)

	// Second registration of the same folder: idempotent no-op.
	require.NoError(t, orch.WatchFolder(ctx, dir))
	require.Equal(t, before, len(orch.extraWatchers),
		"re-registering the same folder must NOT add a second watcher")
}

// TestOrchestrator_FolderLocal_FanOutRestrictedToAgents proves that a
// registered "local" project with a bounded Entry.Agents set fans out ONLY to
// the named agents. Both folders below are git-init'd (project-scoped, no
// ad-hoc->global downgrade) and watched via WatchFolder, with the full V1
// adapter set installed and the rules engine disabled (allows all). The ONLY
// variable is each folder's Entry.Agents.
//
// The signal is claude-code's native CLAUDE.md. When the source memory file is
// AGENTS.md, codex is the primary importer and the AGENTS.md-family targets
// (codex/kilo/hermes/openclaw) all dedup back onto the on-disk AGENTS.md — so
// the single DISTINCT fan-out file is claude-code's CLAUDE.md. Whether it
// appears is therefore a clean proxy for "did claude-code receive the fan-out":
//
//   - included/ : Agents=[codex, claude-code]  -> CLAUDE.md MUST appear.
//   - excluded/ : Agents=[codex]               -> CLAUDE.md MUST NOT appear,
//     even though claude-code is installed and rules allow it — the
//     folder-local scope gate suppresses it.
//
// Identical setup, opposite outcome, driven solely by Entry.Agents: that is the
// restriction.
func TestOrchestrator_FolderLocal_FanOutRestrictedToAgents(t *testing.T) {
	root := realTempDir(t)

	watched := filepath.Join(root, "proj")
	require.NoError(t, os.MkdirAll(watched, 0o755))

	// Two separate git-init'd project folders watched at runtime. Both stay
	// ScopeProject so fan-out targets each folder's own CLAUDE.md.
	included := filepath.Join(root, "included-proj")
	excluded := filepath.Join(root, "excluded-proj")
	require.NoError(t, os.MkdirAll(included, 0o755))
	require.NoError(t, os.MkdirAll(excluded, 0o755))
	require.NoError(t, exec.Command("git", "init", included).Run())
	require.NoError(t, exec.Command("git", "init", excluded).Run())

	infoIncl, err := project.Detect(included)
	require.NoError(t, err)
	infoExcl, err := project.Detect(excluded)
	require.NoError(t, err)

	reg, err := project.NewRegistry(filepath.Join(root, "projects.json"))
	require.NoError(t, err)
	// included/: claude-code IS in the agent set -> must receive CLAUDE.md.
	require.NoError(t, reg.AddOrUpdate(project.Entry{
		ID:     infoIncl.ID,
		Path:   included,
		VCS:    "git",
		Scope:  "local",
		Agents: []string{"codex", "claude-code"},
	}))
	// excluded/: claude-code is NOT in the agent set -> must NOT receive it,
	// even though it's installed and rules allow it.
	require.NoError(t, reg.AddOrUpdate(project.Entry{
		ID:     infoExcl.ID,
		Path:   excluded,
		VCS:    "git",
		Scope:  "local",
		Agents: []string{"codex"},
	}))

	adapters, store, _ := buildAllFiveAdapters(t, root)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	orch, err := NewOrchestrator(Config{
		Dir:             watched,
		Adapters:        adapters,
		Store:           store,
		ProjectRegistry: reg,
		QuietPeriod:     100 * time.Millisecond,
		GuardWindow:     2 * time.Second,
	})
	require.NoError(t, err)
	defer orch.Close()

	go orch.Run(ctx)
	time.Sleep(150 * time.Millisecond)

	require.NoError(t, orch.WatchFolder(ctx, included))
	require.NoError(t, orch.WatchFolder(ctx, excluded))

	// codex (alphabetically first AGENTS.md owner) is primary in both folders.
	require.NoError(t, os.WriteFile(filepath.Join(included, "AGENTS.md"),
		[]byte("# included memory\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(excluded, "AGENTS.md"),
		[]byte("# excluded memory\n"), 0o644))

	// POSITIVE: claude-code is in included/'s Agents -> CLAUDE.md fans out.
	inclClaude := filepath.Join(included, "CLAUDE.md")
	require.Eventually(t, func() bool {
		_, err := os.Stat(inclClaude)
		return err == nil
	}, 4*time.Second, 100*time.Millisecond,
		"claude-code IS in included/'s Entry.Agents -> included/CLAUDE.md must be fanned out")

	// NEGATIVE: claude-code is NOT in excluded/'s Agents -> CLAUDE.md must NOT
	// appear. Give the fan-out a fair window AFTER the positive case landed
	// (both folders are processed by the same pipeline) before asserting absence.
	time.Sleep(500 * time.Millisecond)
	require.NoFileExists(t, filepath.Join(excluded, "CLAUDE.md"),
		"claude-code is NOT in excluded/'s Entry.Agents -> excluded/CLAUDE.md must NOT be fanned out")
}

// TestOrchestrator_FanOut_ProjectScopeUsesProjectPath is a regression test for
// the contextDir bug: fanOut used the TRIGGERING file's directory as the
// contextDir for every artifact, so a project-scoped artifact triggered by an
// edit FAR from its project folder (e.g. Claude's auto-memory at
// ~/.claude/projects/<cwd>/memory/*.md, whose project is <P>/CLAUDE.md) fanned
// out into the trigger's directory instead of the project folder.
//
// We construct exactly that shape: a project-scoped memory artifact whose
// Project.Path is the registered project folder X, but whose SourcePath/trigger
// directory is an unrelated folder Y. Calling fanOut with contextDir = Y must
// land claude-code's CLAUDE.md under X (Project.Path), NOT under Y.
func TestOrchestrator_FanOut_ProjectScopeUsesProjectPath(t *testing.T) {
	root := realTempDir(t)
	watched := filepath.Join(root, "proj")
	require.NoError(t, os.MkdirAll(watched, 0o755))

	// X: the registered project folder (where fan-out MUST land).
	projDir := filepath.Join(root, "proj-x")
	require.NoError(t, os.MkdirAll(projDir, 0o755))
	require.NoError(t, exec.Command("git", "init", projDir).Run())

	// Y: a DIFFERENT directory simulating the auto-memory trigger location,
	// far from the project folder (where the buggy code would have landed it).
	triggerDir := filepath.Join(root, "elsewhere", "memory")
	require.NoError(t, os.MkdirAll(triggerDir, 0o755))

	adapters, store, _ := buildAllFiveAdapters(t, root)

	info, err := project.Detect(projDir)
	require.NoError(t, err)
	reg, err := project.NewRegistry(filepath.Join(root, "projects.json"))
	require.NoError(t, err)
	require.NoError(t, reg.AddOrUpdate(project.Entry{
		ID: info.ID, Path: projDir, VCS: "git", Scope: "local",
	}))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	orch, err := NewOrchestrator(Config{
		Dir:             watched,
		Adapters:        adapters,
		Store:           store,
		ProjectRegistry: reg,
		QuietPeriod:     100 * time.Millisecond,
		GuardWindow:     2 * time.Second,
	})
	require.NoError(t, err)
	defer orch.Close()

	// Seed a real memory artifact via the codex importer (gives a valid event
	// log + payload format so fanOut's ReadEvents/HandlesFormat path is live),
	// then patch it into the bug shape: project-scoped, Project.Path = X,
	// SourcePath/trigger = Y.
	cx := codex.New()
	cx.HomeDir = root
	triggerFile := filepath.Join(triggerDir, "AGENTS.md")
	require.NoError(t, os.WriteFile(triggerFile, []byte("# auto-memory\n"), 0o644))
	ids, err := cx.ImportMemory(ctx, store, triggerFile)
	require.NoError(t, err)
	require.Len(t, ids, 1)

	art, err := store.ReadArtifact(acf.KindMemory, ids[0])
	require.NoError(t, err)
	art.Scope = acf.ScopeProject
	art.SourcePath = triggerFile
	art.Project = &project.ProjectInfo{ID: info.ID, Path: projDir, VCS: "git"}
	require.NoError(t, store.WriteArtifact(art))

	// Fan out with contextDir = the TRIGGER directory (Y). The fix must override
	// this per-artifact with art.Project.Path (X) for project scope.
	orch.fanOut(ctx, cx, ids, triggerDir, triggerFile, false, nil)

	// claude-code's native CLAUDE.md must land in the PROJECT folder (X)...
	require.FileExists(t, filepath.Join(projDir, "CLAUDE.md"),
		"project-scoped fan-out must target art.Project.Path (X), not the trigger dir")
	// ...and must NOT land in the trigger directory (Y) — the buggy behavior.
	require.NoFileExists(t, filepath.Join(triggerDir, "CLAUDE.md"),
		"fan-out must NOT use the triggering file's directory for a project-scoped artifact")
}
