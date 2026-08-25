// Package hermes implements the Aplexica adapter for the Hermes agent
// (Nous Research). V0.11.0 covers all four ACF kinds: memory
// (MEMORY.md + USER.md), skill (SKILL.md), tool (config.yaml mcp_servers
// section), and conversation (sessions read from / written to ~/.hermes/state.db
// via the internal/hermesdb wrapper).
package hermes

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

const Version = "0.2.1"

// Adapter is the Hermes adapter.
type Adapter struct {
	HomeDir  string
	DeviceID string
	// deviceIDMu guards DeviceID after construction: pairing rotates the
	// cloud device id at runtime (SetDeviceID) while imports read it.
	deviceIDMu   sync.RWMutex
	SecretsStore *secrets.Store

	// CanonicalConversations: when true, ImportConversationsFromDB encodes
	// each session as acf.conversation.v1 (canonical events) instead of the
	// legacy acf.hermes.session.v1 SessionBundle. ExportConversationsToDB
	// transparently consumes BOTH formats regardless of this flag.
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

func (a *Adapter) Name() string    { return "hermes" }
func (a *Adapter) Version() string { return Version }

// Discover reports hermes installed when <HomeDir>/.hermes exists and shows an
// install signal: a Hermes-authored file (auth/install metadata — Aplexica may
// create .hermes/memories while materializing synced memory, so the directory
// alone is not an install signal) or the hermes executable. Hermes is a GUI
// app whose CLI shim lives in an app-managed node prefix, invisible to the
// daemon's minimal launchd PATH — the marker files are the reliable signal.
func (a *Adapter) Discover() (adapter.Discovery, error) {
	return adapter.ProbeGlobalRootWithInstallSignals(a.HomeDir, ".hermes",
		[]string{"auth.json", ".install_method", "SOUL.md"},
		"hermes"), nil
}

// inferScope returns ScopeGlobal when the path is under <HomeDir>/.hermes/,
// otherwise ScopeProject.
func (a *Adapter) inferScope(absPath string) acf.Scope {
	if a.HomeDir == "" {
		return acf.ScopeProject
	}
	globalRoot := filepath.Join(a.HomeDir, ".hermes") + string(filepath.Separator)
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
