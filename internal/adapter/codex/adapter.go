// Package codex implements the Aplexica adapter for the Codex agent.
// See ADR-0017 (ACF as hub format) — codex is the second adapter alongside
// claudecode, validating that the canonical format generalizes across agents.
package codex

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter"
	"github.com/aplexica/aplexica/internal/project"
	"github.com/aplexica/aplexica/internal/secrets"
)

// Version 0.9.3 (2026-07): streams generated-session prompts and final answers
// as separate portable updates while excluding injected harness/tool content.
// 0.9.2 prevents source-rollout inode replacement. 0.8.0 turns metadata-only Codex title renames into durable
// conversation updates so they fan out to other agents. 0.7.0 preserves foreign
// conversation titles and project cwd when registering synchronized threads with
// Codex Desktop. 0.6.0 preserves Codex Desktop thread names as the
// portable conversation artifact name so every target can render the same
// subject. 0.5.0 spans the Codex CLI and native desktop app,
// registering synchronized rollouts through app-server and using Codex's
// current shared $HOME/.agents/skills layout. 0.4.0 materialized canonical
// foreign conversations as native Codex rollout sessions.
// 0.3.0 (2026-06): captures session transcripts under
// ~/.codex/sessions/ (recursive root) as canonical conversations, and
// materializes cross-agent conversation transcripts (ConversationDocTarget).
// 0.2.0 folded the managed ~/.codex/memories/*.md layer into AGENTS.md.
const Version = "0.9.3"

// Adapter is the Codex adapter. HomeDir defaults to the user's home directory.
// SecretsStore is the parallel secrets store used by tool-kind operations
// (not yet implemented for Codex in V0.1.4); memory-kind operations don't
// touch it.
type Adapter struct {
	HomeDir  string
	DeviceID string
	// deviceIDMu guards DeviceID after construction: pairing rotates the
	// cloud device id at runtime (SetDeviceID) while imports read it.
	deviceIDMu   sync.RWMutex
	SecretsStore *secrets.Store

	// WorktreeRoots overrides the Codex Desktop managed-worktree roots. nil
	// selects the supported default at $CODEX_HOME/worktrees (with CODEX_HOME
	// currently rooted at <HomeDir>/.codex); an empty slice disables worktree
	// discovery. This seam also keeps tests independent of the real Codex app.
	WorktreeRoots []string

	// CLIExecutablePaths and DesktopExecutablePaths override independent
	// surface probes. nil uses platform defaults; a non-nil empty slice disables
	// that surface probe for deterministic CLI-only/Desktop-only tests.
	CLIExecutablePaths     []string
	DesktopExecutablePaths []string

	// The Codex desktop app indexes resumable threads in state that only Codex
	// itself should mutate. New wires these private seams to the supported
	// app-server protocol; tests can replace them without launching Codex.
	findAppServerExecutables func(homeDir string) []string
	registerAppServerThread  func(ctx context.Context, executable, codexHome, threadID, cwd, title string) error

	// CanonicalConversations — same semantics as claudecode v0.15.0.
	// When true, ImportConversation produces acf.conversation.v1 payloads
	// (structured event log) via EncodeCanonical. When false (default),
	// imports stay in the legacy opaque codex.session.jsonl format.
	// Export always reads both transparently.
	CanonicalConversations bool

	// Registry, when set, lets the shared import pipeline keep files under a
	// registered local project at project scope (skipping the ad-hoc→global
	// downgrade). nil = today's behavior. Wired by the daemon.
	Registry *project.Registry

	// convCache keeps active append-only rollouts incremental. It is lazily
	// allocated because most short-lived adapter uses never import a session.
	convCacheOnce sync.Once
	convCache     *convEncodeCache
}

func (a *Adapter) conversationCache() *convEncodeCache {
	a.convCacheOnce.Do(func() {
		a.convCache = newConvEncodeCache(defaultConvCacheMaxEntries, defaultConvCacheMaxBytes)
	})
	return a.convCache
}

func New() *Adapter {
	home, _ := os.UserHomeDir()
	host, _ := os.Hostname()
	var ss *secrets.Store
	if home != "" {
		ss = &secrets.Store{Root: filepath.Join(home, ".aplexica", "secrets")}
		_ = ss.Init() // best-effort; CLI callers explicitly re-init with their override
	}
	a := &Adapter{
		HomeDir:                 home,
		DeviceID:                host,
		SecretsStore:            ss,
		registerAppServerThread: registerCodexAppServerThread,
	}
	a.findAppServerExecutables = func(string) []string { return a.codexDesktopExecutableCandidates() }
	return a
}

func (a *Adapter) Name() string    { return "codex" }
func (a *Adapter) Version() string { return Version }

// Discover reports codex installed when <HomeDir>/.codex exists and shows an
// install signal: a codex-authored file (auth/config/version — files Aplexica's
// materializers never write) or a resolvable codex executable. Marker files
// are the reliable signal because the daemon's launch context (launchd /
// scheduled task) has a minimal PATH that misses user-level installs. When the
// managed memory layer (<HomeDir>/.codex/memories) is present it is added as a
// second watched root so its *.md files are seen even by a non-recursive
// watcher — the AGENTS.md root alone never observes that subdirectory.
func (a *Adapter) Discover() (adapter.Discovery, error) {
	d := a.surfaceDiscovery()
	if d.Installed {
		if fi, err := os.Stat(a.memoriesDir()); err == nil && fi.IsDir() {
			d.GlobalRoots = append(d.GlobalRoots, a.memoriesDir())
		}
		// Session transcripts live under ~/.codex/sessions/<YYYY>/<MM>/<DD>/
		// rollout-*.jsonl — too deep for the flat watcher, so watch it
		// recursively when present.
		if fi, err := os.Stat(a.sessionsDir()); err == nil && fi.IsDir() {
			d.RecursiveRoots = append(d.RecursiveRoots, a.sessionsDir())
		}
		// Current Codex surfaces discover personal skills under
		// $HOME/.agents/skills. Keep the legacy ~/.codex/skills root as a
		// read-only compatibility source for older Codex releases/migrations.
		for _, skills := range []string{a.userSkillsDir(), filepath.Join(a.HomeDir, ".codex", "skills")} {
			if fi, err := os.Stat(skills); err == nil && fi.IsDir() {
				d.RecursiveRoots = append(d.RecursiveRoots, skills)
			}
		}
		// Desktop-managed worktrees are additional consumers of project state,
		// not global import roots. Report the surface for diagnostics without
		// watching or backing up the app-owned worktree hierarchy itself.
		if a.hasManagedWorktreeRoot() {
			d.Detail += "; Codex Desktop worktrees present"
		}
	}
	return d, nil
}

func (a *Adapter) userSkillsDir() string {
	return filepath.Join(a.HomeDir, ".agents", "skills")
}

// inferScope returns ScopeGlobal when the path is under <HomeDir>/.codex/,
// otherwise ScopeProject. ScopeNamespace is reserved for V2 team features.
func (a *Adapter) inferScope(absPath string) acf.Scope {
	if a.HomeDir == "" {
		return acf.ScopeProject
	}
	globalRoot := filepath.Join(a.HomeDir, ".codex") + string(filepath.Separator)
	if strings.HasPrefix(absPath, globalRoot) {
		return acf.ScopeGlobal
	}
	userSkillsRoot := a.userSkillsDir() + string(filepath.Separator)
	if strings.HasPrefix(absPath, userSkillsRoot) {
		return acf.ScopeGlobal
	}
	return acf.ScopeProject
}

// opaqueParams bundles the per-adapter values used by memory Import calls.
// Tool/skill/conversation will follow the same pattern as they're added.
func (a *Adapter) opaqueParams() adapter.OpaqueParams {
	return adapter.OpaqueParams{
		DeviceID:       a.deviceID(),
		SourceAgent:    a.Name(),
		AdapterVersion: a.Version(),
		InferScope:     a.inferScope,
		InferProject:   adapter.DefaultInferProject,
		Registry:       a.Registry,
	}
}

// SetDeviceID replaces the provenance device identity at runtime. Pairing —
// and re-pairing, which rotates the cloud device id — happens while imports
// are running; every internal read goes through deviceID so the swap cannot
// race them.
func (a *Adapter) SetDeviceID(id string) {
	a.deviceIDMu.Lock()
	a.DeviceID = id
	a.deviceIDMu.Unlock()
}

func (a *Adapter) deviceID() string {
	a.deviceIDMu.RLock()
	defer a.deviceIDMu.RUnlock()
	return a.DeviceID
}
