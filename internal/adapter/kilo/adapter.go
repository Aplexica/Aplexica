// Package kilo implements the Aplexica adapter for the Kilo agent.
// V0.4.0 covers memory (AGENTS.md) and skill (SKILL.md) — the two
// portable open formats Kilo shares with the broader agent ecosystem.
// Conversation import is read-only from Kilo's SQLite session DB; export is
// deferred until Kilo's native write-back semantics are stable.
package kilo

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

const Version = "0.1.1"

// Adapter is the Kilo adapter. HomeDir defaults to the user's home directory;
// tests may override it. SecretsStore is wired for consistency with the other
// adapters but is unused in v0.4.0 (no tool-kind operations yet).
type Adapter struct {
	HomeDir  string
	DeviceID string
	// deviceIDMu guards DeviceID after construction: pairing rotates the
	// cloud device id at runtime (SetDeviceID) while imports read it.
	deviceIDMu   sync.RWMutex
	SecretsStore *secrets.Store

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
		_ = ss.Init() // best-effort; CLI callers explicitly re-init with their override
	}
	return &Adapter{HomeDir: home, DeviceID: host, SecretsStore: ss}
}

func (a *Adapter) Name() string    { return "kilo" }
func (a *Adapter) Version() string { return Version }

// Discover reports Kilo installed when a Kilo executable exists and any known
// global Kilo location exists: ~/.config/kilo (config, AGENTS.md, kilo.jsonc),
// ~/.kilo (skills/rules), or the current CLI session DB under the XDG data
// directory. Aplexica can write .kilo/.config files during fan-out, so files
// alone are not an install signal.
func (a *Adapter) Discover() (adapter.Discovery, error) {
	if a.HomeDir == "" {
		return adapter.Discovery{Installed: false, Detail: "no home directory"}, nil
	}
	bin := kiloBinary(a.HomeDir)
	var globalRoots []string
	var recursiveRoots []string
	var details []string
	addDir := func(path string, recursive bool) {
		fi, err := os.Stat(path)
		if err != nil || !fi.IsDir() {
			return
		}
		if recursive {
			recursiveRoots = append(recursiveRoots, path)
		} else {
			globalRoots = append(globalRoots, path)
		}
		details = append(details, path+" present")
	}
	addDir(a.configRoot(), false)
	addDir(a.legacyHomeRoot(), true)
	for _, dbPath := range a.kiloDBCandidates() {
		if _, err := os.Stat(dbPath); err == nil {
			addDir(filepath.Dir(dbPath), false)
			details = append(details, dbPath+" present")
			break
		}
	}
	if len(globalRoots) == 0 && len(recursiveRoots) == 0 && len(details) == 0 {
		return adapter.Discovery{Installed: false, Detail: "no Kilo global config, skills, or session DB found"}, nil
	}
	if bin == "" {
		return adapter.Discovery{Installed: false, Detail: strings.Join(details, "; ") + "; executable not found: kilo"}, nil
	}
	return adapter.Discovery{
		Installed:      true,
		GlobalRoots:    globalRoots,
		RecursiveRoots: recursiveRoots,
		Detail:         strings.Join(details, "; "),
	}, nil
}

// inferScope returns ScopeGlobal for files under Kilo's user-level config,
// skills, or data roots; project files remain ScopeProject.
func (a *Adapter) inferScope(absPath string) acf.Scope {
	for _, root := range a.globalRootsForScope() {
		if pathInRoot(absPath, root) {
			return acf.ScopeGlobal
		}
	}
	return acf.ScopeProject
}

// opaqueParams bundles the per-adapter values used by ImportOpaque calls.
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

func (a *Adapter) configRoot() string {
	if a.HomeDir == "" {
		return ""
	}
	return filepath.Join(a.HomeDir, ".config", "kilo")
}

func (a *Adapter) legacyHomeRoot() string {
	if a.HomeDir == "" {
		return ""
	}
	return filepath.Join(a.HomeDir, ".kilo")
}

func (a *Adapter) globalRootsForScope() []string {
	if a.HomeDir == "" {
		return nil
	}
	roots := []string{a.configRoot(), a.legacyHomeRoot()}
	for _, dbPath := range a.kiloDBCandidates() {
		roots = append(roots, filepath.Dir(dbPath))
	}
	return roots
}

func pathInRoot(absPath, root string) bool {
	if root == "" {
		return false
	}
	clean := filepath.Clean(absPath)
	root = filepath.Clean(root)
	return clean == root || strings.HasPrefix(clean, root+string(filepath.Separator))
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
