package codex

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/aplexica/aplexica/internal/adapter"
)

var codexInstallMarkers = []string{
	"auth.json", "config.toml", "config.json", "version.json", "installation_id",
}

// CandidateDiscovery implements adapter.RuntimeDiscoverable. The daemon polls
// these conventional roots even after a negative startup probe, so either the
// CLI or Desktop app can be installed later and begin participating without a
// restart. Missing paths are never created by this method.
func (a *Adapter) CandidateDiscovery() adapter.Discovery {
	if a.HomeDir == "" {
		return adapter.Discovery{}
	}
	root := filepath.Join(a.HomeDir, ".codex")
	global := []string{root, filepath.Join(root, "memories")}
	recursive := []string{filepath.Join(root, "sessions"), a.userSkillsDir()}
	legacySkills := filepath.Join(root, "skills")
	if codexDirectoryPresent(legacySkills) {
		recursive = append(recursive, legacySkills)
	}
	return adapter.Discovery{GlobalRoots: global, RecursiveRoots: recursive}
}

func (a *Adapter) surfaceDiscovery() adapter.Discovery {
	if a.HomeDir == "" {
		return adapter.Discovery{Detail: "no home directory"}
	}
	root := filepath.Join(a.HomeDir, ".codex")
	rootPresent := codexDirectoryPresent(root)
	cliPresent := a.codexCLISurfaceInstalled()
	desktopPresent := a.codexDesktopSurfaceInstalled()
	markerPresent := false
	if rootPresent {
		for _, marker := range codexInstallMarkers {
			if _, err := os.Stat(filepath.Join(root, marker)); err == nil {
				markerPresent = true
				break
			}
		}
	}
	runtimeToken := a.codexDesktopRuntimeToken()
	if !cliPresent && !desktopPresent && !markerPresent {
		return adapter.Discovery{RuntimeToken: runtimeToken, Detail: root + " present without a Codex install signal"}
	}
	var roots []string
	if rootPresent {
		roots = []string{root}
	}
	var active []adapter.Surface
	if cliPresent {
		active = append(active, adapter.SurfaceCLI)
	}
	if desktopPresent {
		active = append(active, adapter.SurfaceDesktop)
	}
	detail := fmt.Sprintf("Codex surfaces: cli=%t desktop=%t shared-storage=%t", cliPresent, desktopPresent, rootPresent)
	return adapter.Discovery{Installed: true, ActiveSurfaces: active, GlobalRoots: roots, RuntimeToken: runtimeToken, Detail: detail}
}

// codexDesktopRuntimeToken fingerprints the currently usable app-server
// helpers. On Windows the Store/MSIX package can appear before its per-user
// helper cache, so Desktop installed-ness alone is not enough to know that
// native session registration can run. A stable, sorted token makes helper
// arrival observable to the daemon without coupling it to CLI availability.
func (a *Adapter) codexDesktopRuntimeToken() string {
	candidates := a.codexDesktopExecutableCandidates()
	sort.Strings(candidates)
	return strings.Join(candidates, "\x00")
}

func (a *Adapter) codexCLISurfaceInstalled() bool {
	for _, candidate := range a.codexCLIExecutableCandidates() {
		if isUsableCodexExecutable(candidate) {
			return true
		}
	}
	return false
}

func (a *Adapter) codexCLIExecutableCandidates() []string {
	if a.CLIExecutablePaths != nil {
		return append([]string(nil), a.CLIExecutablePaths...)
	}
	var candidates []string
	if codexActualUserHome(a.HomeDir) {
		if path, lookErr := exec.LookPath("codex"); lookErr == nil {
			candidates = append(candidates, path)
		}
		candidates = append(candidates, codexStableCLIExecutableCandidates(runtime.GOOS, codexWindowsRoamingAppData(a.HomeDir))...)
	}
	if a.HomeDir != "" {
		candidates = append(candidates, filepath.Join(a.HomeDir, ".local", "bin", codexExecutableName()))
		if runtime.GOOS == "windows" {
			// The standalone Windows installer is a CLI surface. Keep it
			// separate from Store/MSIX Desktop detection so either surface can
			// exist independently.
			candidates = append(candidates, filepath.Join(codexWindowsLocalAppData(a.HomeDir), "Programs", "OpenAI", "Codex", "bin", "codex.exe"))
		}
	}
	return candidates
}

// codexStableCLIExecutableCandidates complements PATH with standard locations
// that a service manager commonly omits. Re-probing their absolute paths lets
// a later Homebrew/npm install activate in the already-running daemon.
func codexStableCLIExecutableCandidates(goos, roamingAppData string) []string {
	switch goos {
	case "darwin":
		return []string{
			filepath.Join(string(filepath.Separator), "opt", "homebrew", "bin", "codex"),
			filepath.Join(string(filepath.Separator), "usr", "local", "bin", "codex"),
		}
	case "linux":
		return []string{
			filepath.Join(string(filepath.Separator), "usr", "local", "bin", "codex"),
			filepath.Join(string(filepath.Separator), "home", "linuxbrew", ".linuxbrew", "bin", "codex"),
		}
	case "windows":
		return []string{
			filepath.Join(roamingAppData, "npm", "codex.cmd"),
			filepath.Join(roamingAppData, "npm", "codex.exe"),
		}
	default:
		return nil
	}
}

func (a *Adapter) codexDesktopSurfaceInstalled() bool {
	if len(a.codexDesktopExecutableCandidates()) > 0 {
		return true
	}
	return a.DesktopExecutablePaths == nil && runtime.GOOS == "windows" &&
		codexWindowsDesktopInstallPresent(codexWindowsLocalAppData(a.HomeDir))
}

func (a *Adapter) codexDesktopExecutableCandidates() []string {
	if a.DesktopExecutablePaths != nil {
		return filterUsableCodexExecutables(a.DesktopExecutablePaths)
	}
	return codexAppServerExecutables(a.HomeDir)
}

func filterUsableCodexExecutables(candidates []string) []string {
	seen := make(map[string]struct{}, len(candidates))
	result := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		if !isUsableCodexExecutable(candidate) {
			continue
		}
		clean := filepath.Clean(candidate)
		if _, duplicate := seen[clean]; duplicate {
			continue
		}
		seen[clean] = struct{}{}
		result = append(result, clean)
	}
	return result
}

func codexDirectoryPresent(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func codexActualUserHome(home string) bool {
	current, err := user.Current()
	return err == nil && sameCodexPath(home, current.HomeDir)
}
