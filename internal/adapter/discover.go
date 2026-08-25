package adapter

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// Discovery is the result of an adapter's Discover() probe. It answers the
// BRD-03 FR-03.3 question: "is this agent installed on this machine, and
// where is its native GLOBAL storage?" Project-scope storage is discovered
// dynamically by the daemon's project-detection scan and is NOT reported here.
type Discovery struct {
	// Installed is true when the agent's native global storage and install
	// signal were found.
	Installed bool `json:"installed"`
	// ActiveSurfaces reports which independently installable runtimes were
	// positively detected during this probe. Capabilities.Surfaces is the
	// supported set; this is the currently present subset. It may be empty for
	// legacy adapters that do not expose per-surface probes.
	ActiveSurfaces []Surface `json:"activeSurfaces,omitempty"`
	// GlobalRoots are absolute paths (directories) the daemon should watch
	// for this agent's global-scope artifacts. Empty when !Installed. Watched
	// NON-recursively (direct children only) unless the daemon runs in
	// recursive mode.
	GlobalRoots []string `json:"globalRoots,omitempty"`
	// RecursiveRoots are absolute directories the daemon should watch
	// RECURSIVELY regardless of the daemon's global recursive flag, because the
	// agent stores artifacts in nested subdirectories a flat watcher can't
	// reach — e.g. Codex session transcripts live in
	// ~/.codex/sessions/<YYYY>/<MM>/<DD>/rollout-*.jsonl. Kept separate from
	// GlobalRoots so the common flat-directory case stays cheap.
	RecursiveRoots []string `json:"recursiveRoots,omitempty"`
	// MetadataRoots are app-owned directories watched recursively only as
	// read-only triggers. Their files may resolve back to native artifacts, but
	// the metadata files themselves are never artifacts and these roots are
	// excluded from native backup and restore inventories.
	MetadataRoots []string `json:"metadataRoots,omitempty"`
	// WatchFiles are absolute paths of INDIVIDUAL FILES the daemon should
	// watch for this agent — config files that live outside any watchable
	// directory root. E.g. `claude mcp add -s user` writes the mcpServers
	// key into ~/.claude.json at the HOME root: watching all of $HOME would
	// be noisy and grant the adapter ownership of unrelated files, so the
	// daemon watches just this path (events for anything else in its parent
	// dir are filtered out).
	WatchFiles []string `json:"watchFiles,omitempty"`
	// RuntimeToken is opaque adapter-owned readiness state used only by the
	// running daemon's late-install poller. It lets an adapter report a change
	// that matters to materialization even when its public roots and active
	// surfaces are unchanged (for example, a Desktop app-server helper arriving
	// after the Windows app package). It is intentionally omitted from APIs.
	RuntimeToken string `json:"-"`
	// Detail is a short human-readable note (e.g. "~/.claude present" or
	// "project-scoped only"). Surfaced in status/UI diagnostics.
	Detail string `json:"detail,omitempty"`
}

// ProbeGlobalRoot is the shared Discover() implementation for adapters whose
// installed-ness is determined by the existence of a single global directory
// under the user's home (e.g. ~/.claude, ~/.codex). relRoot is the path
// relative to homeDir (e.g. ".claude"). Returns Installed=false when homeDir
// is empty, the path is missing, or the path is not a directory.
func ProbeGlobalRoot(homeDir, relRoot string) Discovery {
	if homeDir == "" {
		return Discovery{Installed: false, Detail: "no home directory"}
	}
	root := filepath.Join(homeDir, relRoot)
	fi, err := os.Stat(root)
	if err != nil || !fi.IsDir() {
		return Discovery{Installed: false, Detail: root + " not found"}
	}
	return Discovery{Installed: true, ActiveSurfaces: []Surface{SurfaceCLI}, GlobalRoots: []string{root}, Detail: root + " present"}
}

// ProbeGlobalRootWithExecutable is like ProbeGlobalRoot, but only reports the
// agent installed when one of executableNames is available on PATH. This keeps
// Aplexica-created native folders from becoming install signals on machines
// where the agent itself is not installed.
func ProbeGlobalRootWithExecutable(homeDir, relRoot string, executableNames ...string) Discovery {
	return ProbeGlobalRootWithInstallSignals(homeDir, relRoot, nil, executableNames...)
}

// ProbeGlobalRootWithInstallSignals is ProbeGlobalRoot plus an install-signal
// check: the root directory alone is not proof the agent is installed (Aplexica
// itself materializes files there), so the agent must ALSO show either an
// agent-authored marker file under the root or a resolvable executable.
//
// markerRelPaths are paths RELATIVE TO THE GLOBAL ROOT that only the agent's
// own install/runtime writes (auth/config/version files) — never Aplexica's
// materializers. They matter because the daemon runs under launchd or a
// Windows scheduled task with a minimal PATH: an install whose binary lives in
// ~/.local/bin or an app-managed prefix can be invisible to exec.LookPath.
func ProbeGlobalRootWithInstallSignals(homeDir, relRoot string, markerRelPaths []string, executableNames ...string) Discovery {
	d := ProbeGlobalRoot(homeDir, relRoot)
	if !d.Installed {
		return d
	}
	root := filepath.Join(homeDir, relRoot)
	for _, rel := range markerRelPaths {
		if rel == "" {
			continue
		}
		if _, err := os.Stat(filepath.Join(root, rel)); err == nil {
			return d
		}
	}
	if executableAvailableFrom(homeDir, executableNames...) {
		return d
	}
	d.Installed = false
	d.ActiveSurfaces = nil
	d.GlobalRoots = nil
	d.RecursiveRoots = nil
	d.WatchFiles = nil
	d.Detail = d.Detail + "; no install signal (markers: " + strings.Join(markerRelPaths, ", ") +
		"; executables: " + strings.Join(executableNames, ", ") + ")"
	return d
}

// ExecutableAvailable reports whether any named command can be resolved from
// PATH. exec.LookPath handles PATHEXT on Windows, so callers should pass the
// command name users type, not an OS-specific filename.
func ExecutableAvailable(names ...string) bool {
	return executableAvailableFrom("", names...)
}

// executableAvailableFrom resolves each name via PATH and, when homeDir is
// non-empty, via <homeDir>/.local/bin — the XDG-ish user bin where npm-style
// agent installers drop their shims and which daemon launch contexts
// (launchd, Windows scheduled tasks) do not have on PATH.
func executableAvailableFrom(homeDir string, names ...string) bool {
	for _, name := range names {
		if name == "" {
			continue
		}
		if _, err := exec.LookPath(name); err == nil {
			return true
		}
		if homeDir == "" {
			continue
		}
		candidate := filepath.Join(homeDir, ".local", "bin", name)
		if isExecutableFile(candidate) {
			return true
		}
		if runtime.GOOS == "windows" {
			for _, ext := range []string{".exe", ".cmd", ".bat"} {
				if isExecutableFile(candidate + ext) {
					return true
				}
			}
		}
	}
	return false
}

// anyExecBit masks the owner/group/other execute permission bits.
const anyExecBit = 0o111

func isExecutableFile(path string) bool {
	fi, err := os.Stat(path)
	if err != nil || fi.IsDir() {
		return false
	}
	if runtime.GOOS == "windows" {
		return true // existence is the signal; Windows has no exec bit
	}
	return fi.Mode()&anyExecBit != 0
}
