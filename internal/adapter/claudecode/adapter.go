package claudecode

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

// Version 0.13.0 (2026-07): updates Claude Desktop's catalog in place without
// launching/focusing the app, and preserves canonical thread identity when a
// catalog record changes. 0.12.0 watches Claude Desktop's catalog so
// metadata-only title assignment and renames trigger normal conversation fan-out.
// 0.11.0 preserves Claude Code Desktop titles as portable
// conversation artifact names. 0.10.0 carries source-native conversation titles into
// Claude CLI custom-title records and the Desktop catalog. 0.9.0 registers materialized CLI transcripts with Claude
// Code Desktop through its claude://resume deep link, so synced conversations
// appear in the app without writing app-owned catalog records.
// 0.8.0 (2026-07): treats Claude Code CLI and Claude Code Desktop as
// two surfaces of the same adapter. Desktop uses the same ~/.claude engine
// state, while its session catalog is used to normalize automatic
// worktree sessions back to their originating project. See desktop.go.
// 0.7.0 (2026-06): TYPE-AWARE per-project auto-memory routing. Each
// ~/.claude/projects/<cwd>/memory/*.md topic carries a metadata.type of "user"
// or "project". type:user bodies fold into the single GLOBAL ~/.claude/CLAUDE.md
// artifact (gathered across every project's memory dir); type:project bodies
// fold into the cwd's REGISTERED project memory (<P>/CLAUDE.md, project-scoped —
// never global). The MEMORY.md index is always skipped. See globalmemory.go.
// 0.6.0 folded the auto-memory layer (all types) into the single CLAUDE.md-keyed
// global-memory artifact so memories added in Claude Code fan out to the other
// agents attributed to claude-code (not mis-attributed to hermes on the
// MEMORY.md basename collision).
// 0.5.0 materialized foreign conversations as native ~/.claude/projects/ sessions
// (adapter.ConversationSessionTarget), superseding the 0.3.0 markdown transcript.
const Version = "0.14.2"

// Adapter is the Claude Code adapter. HomeDir defaults to the user's home
// directory; tests may override it. SecretsStore is the parallel secrets
// store used by tool-kind operations.
type Adapter struct {
	HomeDir  string
	DeviceID string
	// deviceIDMu guards DeviceID after construction: pairing rotates the
	// cloud device id at runtime (SetDeviceID) while imports read it.
	deviceIDMu   sync.RWMutex
	SecretsStore *secrets.Store

	// DesktopSessionRoots optionally overrides the platform-default Claude
	// Code Desktop session-catalog locations. nil selects the native defaults;
	// a non-nil empty slice disables Desktop catalog discovery. It exists as a
	// test/configuration seam because the app-owned catalog is outside
	// ~/.claude and differs between macOS and Windows.
	DesktopSessionRoots []string

	// CLIExecutablePaths and DesktopAppPaths override surface probes. nil uses
	// platform defaults; a non-nil empty slice disables that probe. They keep
	// CLI-only, Desktop-only, and late-install transitions deterministic in
	// tests without coupling them to the developer machine.
	CLIExecutablePaths []string
	DesktopAppPaths    []string

	// upsertDesktopSession writes the one deterministic catalog record for a
	// durable synthetic transcript. It never launches or activates Desktop.
	// The function is injectable for tests; nil preserves CLI-only sync.
	upsertDesktopSession  func(ctx context.Context, request desktopSessionUpsert) error
	desktopRegistrationMu sync.Mutex

	// CanonicalConversations, when true, makes ImportConversation produce
	// acf.conversation.v1 payloads (structured event list) instead of the
	// legacy opaque "claude-code.session.jsonl" format. Default false for
	// backward compatibility with v0.1.2 — v0.14.x conversation artifacts.
	// Added in v0.15.0; the CLI exposes it via `aplexica import --canonical`.
	CanonicalConversations bool

	// RepairForkedMirrors authorizes the ONE repair Aplexica is otherwise not
	// allowed to attempt: rewriting a deterministic SYNTHETIC conversation
	// mirror whose parentUuid graph forked, so that Claude Code's resume walk
	// can reach the whole canonical thread again.
	//
	// The condition it repairs is permanent on disk and blocks every other
	// door: Claude Code appended its own child of a node Aplexica had already
	// extended, stranding Aplexica's rows on a dead sibling branch, and the
	// resulting whole-file/leaf-chain node-count mismatch fails closed in
	// claudeSessionMatches, claudeSessionAppendix and rebuildDivergedClaudeMirror
	// alike. The artifact then re-enters the deferral queue on every inbound
	// event, forever.
	//
	// DEFAULT FALSE, and the false path is a strict no-op: every predicate,
	// every write primitive and every byte on disk is exactly what it was
	// before the repair existed. It is off by default because it authorizes a
	// whole-file rewrite of a file a third-party process co-owns, and mirrors can
	// hold turns canonical lacks. Enabling it never lowers the loss proof: a
	// mirror is rebuilt only
	// when EVERY conversational row in it is provably reproducible from the
	// canonical plan (claudeMirrorRowsContained).
	//
	// Never applies to a native-origin session, never touches the canonical
	// store, never creates a second session for a thread. The daemon wires it
	// from the `sync.repairForkedMirrors` config key; changing it requires a
	// daemon restart.
	RepairForkedMirrors bool

	// Registry, when set, lets the shared import pipeline keep files under a
	// registered local project at project scope (skipping the ad-hoc→global
	// downgrade). nil = today's behavior. Wired by the daemon.
	Registry *project.Registry

	// convCache makes canonical conversation encoding incremental (parse only
	// the appended tail on each watcher settle instead of the whole file).
	// Lazily created per long-lived adapter instance; see convcache.go.
	convCacheOnce sync.Once
	convCache     *convEncodeCache

	// Desktop's catalog can contain hundreds of large JSON records (connector
	// schemas included). Cache the small decoded projection by a metadata
	// fingerprint so fanout bursts do not re-decode unchanged records while a
	// newly-created record becomes visible immediately.
	desktopCacheMu      sync.Mutex
	desktopCacheKey     string
	desktopCacheRecords []desktopSessionRecord

	// nativeRepairRefusals remembers which native transcripts the diverged
	// rebuild has already proven it cannot repair, keyed by pathname and valid
	// only for the exact (size, mtime, plan) it was computed from. The rebuild
	// pays a whole-file read and a per-row canonical encode before it can
	// discover it will refuse, and some transcripts can never satisfy
	// the loss proof, so without this a whole-store fan-out would re-pay that
	// cost for every one of them on every pass. See nativeRepairVerdict.
	nativeRepairMu       sync.Mutex
	nativeRepairRefusals map[string]nativeRepairVerdict
}

// conversationCache returns the adapter's lazily-initialized incremental
// canonical-encode cache. Safe for concurrent use.
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
		HomeDir:      home,
		DeviceID:     host,
		SecretsStore: ss,
	}
	a.upsertDesktopSession = a.upsertClaudeDesktopSession
	return a
}

func (a *Adapter) Name() string    { return "claude-code" }
func (a *Adapter) Version() string { return Version }

// Discover reports claude-code installed when <HomeDir>/.claude exists.
func (a *Adapter) Discover() (adapter.Discovery, error) {
	d := a.surfaceDiscovery()
	if d.Installed {
		// Session transcripts live in ~/.claude/projects/<encoded-cwd>/
		// <uuid>.jsonl — nested, so watch recursively to capture them.
		projects := filepath.Join(a.HomeDir, ".claude", "projects")
		if fi, err := os.Stat(projects); err == nil && fi.IsDir() {
			d.RecursiveRoots = append(d.RecursiveRoots, projects)
		}
		// Personal skills live in ~/.claude/skills/<name>/SKILL.md — also
		// nested, so the flat root watch never saw agent-native skill
		// creation (skills were otherwise silently import-blind).
		skills := filepath.Join(a.HomeDir, ".claude", "skills")
		if fi, err := os.Stat(skills); err == nil && fi.IsDir() {
			d.RecursiveRoots = append(d.RecursiveRoots, skills)
		}
		// User-scope MCP servers live in ~/.claude.json (the `claude mcp
		// add -s user` target) — a single file at the HOME root, outside
		// every directory root (agent-native MCP additions
		// were invisible to sync, and ~/.claude/.mcp.json — the old
		// global tool destination — is a file Claude Code never reads).
		userCfg := a.userConfigPath()
		if fi, err := os.Stat(userCfg); err == nil && !fi.IsDir() {
			d.WatchFiles = append(d.WatchFiles, userCfg)
		}
		// Claude Code Desktop embeds the same Claude Code engine and consumes
		// the shared ~/.claude state above. Its app-owned catalog must be watched:
		// Desktop titles can arrive after the final transcript append. Catalog
		// events resolve back to the matching CLI transcript and never become
		// artifacts; Aplexica writes only its deterministic materialized records.
		desktopCatalogPresent := false
		for _, catalog := range a.desktopSessionCatalogRoots() {
			if fi, err := os.Stat(catalog); err == nil && fi.IsDir() {
				d.MetadataRoots = append(d.MetadataRoots, catalog)
				desktopCatalogPresent = true
			}
		}
		if desktopCatalogPresent {
			d.Detail += "; Claude Code Desktop surface present"
		}
	}
	return d, nil
}

// userConfigPath is ~/.claude.json — Claude Code's main user config, which
// holds the user-scope mcpServers map among much other state.
func (a *Adapter) userConfigPath() string {
	return filepath.Join(a.HomeDir, ".claude.json")
}

// inferScope returns ScopeGlobal when the path is under <HomeDir>/.claude/
// (or IS ~/.claude.json, the user-scope config at the HOME root), otherwise
// ScopeProject. ScopeNamespace is reserved for V2 team features.
func (a *Adapter) inferScope(absPath string) acf.Scope {
	if a.HomeDir == "" {
		return acf.ScopeProject
	}
	if absPath == a.userConfigPath() {
		return acf.ScopeGlobal
	}
	globalRoot := filepath.Join(a.HomeDir, ".claude") + string(filepath.Separator)
	if strings.HasPrefix(absPath, globalRoot) {
		return acf.ScopeGlobal
	}
	return acf.ScopeProject
}

// opaqueParams bundles the per-adapter values used by all memory/skill/
// conversation Import calls. Tool uses its own logic because of secrets
// extraction.
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
