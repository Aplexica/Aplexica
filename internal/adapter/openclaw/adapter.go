// Package openclaw implements the Aplexica adapter for the OpenClaw agent
// (https://github.com/openclaw/openclaw). V0.24.0 ships full 4/4 coverage:
//
//   - Memory   — MEMORY.md, AGENTS.md, CLAUDE.md, DREAMS.md, and
//     memory/YYYY-MM-DD[-slug].md daily notes.
//   - Skill    — SKILL.md.
//   - Tool     — openclaw.json's mcp.servers section (mapped to/from the
//     canonical mcp schema from v0.3.0; per-server shape is identical to
//     claudecode's .mcp.json entries).
//   - Conversation — opaque JSONL transcripts (typically under
//     ~/.openclaw/agents/<id>/sessions/). Format string
//     "openclaw.session.jsonl". A canonical translator (acf.conversation.v1)
//     for OpenClaw transcripts is a follow-up — pi-coding-agent transcripts
//     are Claude Code-derived so claudecode.EncodeCanonical may be largely
//     reusable after a shared-package refactor. Inbound, foreign
//     conversations materialize into OpenClaw's own session store (index +
//     v3 transcript) via MaterializeConversationSession — see
//     session_transcode.go for why that store (and not the configurable
//     LLM backend's) is the sync surface.
package openclaw

import (
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter"
	"github.com/aplexica/aplexica/internal/project"
	"github.com/aplexica/aplexica/internal/secrets"
)

const Version = "0.3.1"

// Adapter is the OpenClaw adapter. SecretsStore mirrors the other adapters
// for structural symmetry even though V1 has no tool importer that uses it.
type Adapter struct {
	HomeDir  string
	DeviceID string
	// deviceIDMu guards DeviceID after construction: pairing rotates the
	// cloud device id at runtime (SetDeviceID) while imports read it.
	deviceIDMu   sync.RWMutex
	SecretsStore *secrets.Store

	// CanonicalConversations, when true, makes ImportConversation produce
	// acf.conversation.v1 payloads (structured event list) instead of the
	// legacy opaque "openclaw.session.jsonl" format. Default false for
	// backward compatibility with v0.24.0 conversation artifacts. Added in
	// v0.24.1; the CLI exposes it via `aplexica import --canonical`. Mirrors
	// claudecode/codex/hermes (v0.15.0/v0.16.0/v0.17.0).
	CanonicalConversations bool

	// Registry, when set, lets the shared import pipeline keep files under a
	// registered local project at project scope (skipping the ad-hoc→global
	// downgrade). nil = today's behavior. Wired by the daemon.
	Registry *project.Registry
}

func New() *Adapter {
	home, _ := os.UserHomeDir()
	host, _ := os.Hostname()
	var ss *secrets.Store
	if home != "" {
		ss = &secrets.Store{Root: filepath.Join(home, ".aplexica", "secrets")}
		_ = ss.Init()
	}
	return &Adapter{HomeDir: home, DeviceID: host, SecretsStore: ss}
}

func (a *Adapter) Name() string    { return "openclaw" }
func (a *Adapter) Version() string { return Version }

// Discover reports openclaw installed when <HomeDir>/.openclaw exists and
// shows an install signal: openclaw's own config file (Aplexica's
// materializers never write it) or the openclaw executable — the latter is
// often installed into an app-managed node prefix the daemon's minimal PATH
// can't see, so the marker matters.
// The workspace/ subdir — where the memory and config artifacts actually
// live (see NativePath) — is advertised as its own root: the daemon's
// native-root watcher is FLAT, so without this an edit made by openclaw
// inside workspace/ never imports (memory sync was silently export-only;
// found in E2E F2). Mirrors codex's memories-subdir pattern.
func (a *Adapter) Discover() (adapter.Discovery, error) {
	d := adapter.ProbeGlobalRootWithInstallSignals(a.HomeDir, ".openclaw",
		[]string{"openclaw.json"},
		"openclaw")
	if d.Installed {
		ws := filepath.Join(a.HomeDir, ".openclaw", "workspace")
		if fi, err := os.Stat(ws); err == nil && fi.IsDir() {
			d.GlobalRoots = append(d.GlobalRoots, ws)
		}
		// Native session transcripts live at agents/<id>/sessions/<uuid>.jsonl
		// — without watching them, conversations born in OpenClaw's TUI never
		// entered the sync mesh (outbound conversation sync regression).
		// FLAT roots on purpose: the transcripts are direct children, and a
		// recursive watch of agents/ would reach the backend-internal
		// agent/codex-home tree (whose rollouts must NOT import as openclaw
		// conversations — the backend is swappable).
		sessions, _ := filepath.Glob(filepath.Join(a.HomeDir, ".openclaw", "agents", "*", "sessions"))
		for _, s := range sessions {
			if fi, err := os.Stat(s); err == nil && fi.IsDir() {
				d.GlobalRoots = append(d.GlobalRoots, s)
			}
		}
	}
	return d, nil
}

// inferScope returns ScopeGlobal when the path is under <HomeDir>/.openclaw/,
// otherwise ScopeProject.
func (a *Adapter) inferScope(absPath string) acf.Scope {
	if a.HomeDir == "" {
		return acf.ScopeProject
	}
	globalRoot := filepath.Join(a.HomeDir, ".openclaw") + string(filepath.Separator)
	if strings.HasPrefix(absPath, globalRoot) {
		return acf.ScopeGlobal
	}
	return acf.ScopeProject
}

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
