package claudecode

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/aplexica/aplexica/internal/adapter"
)

const claudeExecutableBits = os.FileMode(0o111)

const claudeWindowsPackageFamily = "Claude_pzs8sxrjxfjjc"

// Bumping this token makes the daemon run its bounded conversation catch-up
// when non-activating Desktop catalog support first becomes available,
// including after a Desktop installation while the daemon is already running.
const claudeDesktopRegistrationRuntimeToken = "catalog-upsert-v5"

// CandidateDiscovery implements adapter.RuntimeDiscoverable. These paths are
// polled even when neither Claude surface exists at daemon startup, allowing a
// later CLI or Desktop installation to become active without pre-creating the
// agent's directories.
func (a *Adapter) CandidateDiscovery() adapter.Discovery {
	if a.HomeDir == "" {
		return adapter.Discovery{}
	}
	root := filepath.Join(a.HomeDir, ".claude")
	return adapter.Discovery{
		GlobalRoots:    []string{root},
		RecursiveRoots: []string{filepath.Join(root, "projects"), filepath.Join(root, "skills")},
		MetadataRoots:  a.desktopSessionCatalogRoots(),
		WatchFiles:     []string{filepath.Join(a.HomeDir, ".claude.json")},
	}
}

func (a *Adapter) surfaceDiscovery() adapter.Discovery {
	if a.HomeDir == "" {
		return adapter.Discovery{Detail: "no home directory"}
	}
	root := filepath.Join(a.HomeDir, ".claude")
	rootPresent := directoryPresent(root)
	cliPresent := a.claudeCLISurfaceInstalled()
	desktopPresent := a.claudeDesktopSurfaceInstalled()
	runtimeToken := ""
	if desktopPresent {
		runtimeToken = claudeDesktopRegistrationRuntimeToken
	}
	if !rootPresent && !cliPresent && !desktopPresent {
		return adapter.Discovery{RuntimeToken: runtimeToken, Detail: root + " and Claude surfaces not found"}
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
	detail := fmt.Sprintf("Claude surfaces: cli=%t desktop=%t shared-storage=%t", cliPresent, desktopPresent, rootPresent)
	return adapter.Discovery{Installed: true, ActiveSurfaces: active, GlobalRoots: roots, RuntimeToken: runtimeToken, Detail: detail}
}

func (a *Adapter) claudeCLISurfaceInstalled() bool {
	for _, candidate := range a.claudeCLIExecutableCandidates() {
		if usableClaudeExecutable(candidate) {
			return true
		}
	}
	return false
}

func (a *Adapter) claudeCLIExecutableCandidates() []string {
	if a.CLIExecutablePaths != nil {
		return append([]string(nil), a.CLIExecutablePaths...)
	}
	var candidates []string
	if claudeActualUserHome(a.HomeDir) {
		if path, lookErr := exec.LookPath("claude"); lookErr == nil {
			candidates = append(candidates, path)
		}
		candidates = append(candidates, claudeStableCLIExecutableCandidates(runtime.GOOS, claudeWindowsRoamingAppData(a.HomeDir))...)
	}
	if a.HomeDir != "" {
		name := "claude"
		if runtime.GOOS == "windows" {
			name = "claude.exe"
		}
		candidates = append(candidates, filepath.Join(a.HomeDir, ".local", "bin", name))
	}
	return candidates
}

// claudeStableCLIExecutableCandidates covers standard install locations that
// may be absent from a long-running daemon's fixed/minimal PATH. The poller
// stats these paths on every discovery pass, so a later Homebrew/npm install is
// visible without restarting launchd, systemd, or the Windows scheduled task.
func claudeStableCLIExecutableCandidates(goos, roamingAppData string) []string {
	switch goos {
	case "darwin":
		return []string{
			filepath.Join(string(filepath.Separator), "opt", "homebrew", "bin", "claude"),
			filepath.Join(string(filepath.Separator), "usr", "local", "bin", "claude"),
		}
	case "linux":
		return []string{
			filepath.Join(string(filepath.Separator), "usr", "local", "bin", "claude"),
			filepath.Join(string(filepath.Separator), "home", "linuxbrew", ".linuxbrew", "bin", "claude"),
		}
	case "windows":
		return []string{
			filepath.Join(roamingAppData, "npm", "claude.cmd"),
			filepath.Join(roamingAppData, "npm", "claude.exe"),
		}
	default:
		return nil
	}
}

func (a *Adapter) claudeDesktopSurfaceInstalled() bool {
	if a.hasDesktopSessionCatalog() {
		return true
	}
	for _, candidate := range a.claudeDesktopAppCandidates() {
		if info, err := os.Stat(candidate); err == nil && (info.IsDir() || info.Mode().IsRegular()) {
			return true
		}
	}
	return false
}

func (a *Adapter) claudeDesktopAppCandidates() []string {
	if a.DesktopAppPaths != nil {
		return append([]string(nil), a.DesktopAppPaths...)
	}
	if a.HomeDir == "" {
		return nil
	}
	switch runtime.GOOS {
	case "darwin":
		candidates := []string{filepath.Join(a.HomeDir, "Applications", "Claude.app")}
		if claudeActualUserHome(a.HomeDir) {
			candidates = append([]string{filepath.Join(string(filepath.Separator), "Applications", "Claude.app")}, candidates...)
		}
		return candidates
	case "windows":
		return claudeWindowsDesktopAppCandidates(claudeWindowsLocalAppData(a.HomeDir))
	case "linux":
		return []string{
			filepath.Join(string(filepath.Separator), "opt", "Claude", "claude"),
			filepath.Join(string(filepath.Separator), "usr", "bin", "claude-desktop"),
		}
	default:
		return nil
	}
}

func claudeWindowsLocalAppData(homeDir string) string {
	local := filepath.Join(homeDir, "AppData", "Local")
	if currentHome, err := os.UserHomeDir(); err == nil && sameClaudePath(homeDir, currentHome) {
		if env := os.Getenv("LOCALAPPDATA"); filepath.IsAbs(env) {
			local = env
		}
	}
	return local
}

func claudeWindowsRoamingAppData(homeDir string) string {
	roaming := filepath.Join(homeDir, "AppData", "Roaming")
	if currentHome, err := os.UserHomeDir(); err == nil && sameClaudePath(homeDir, currentHome) {
		if env := os.Getenv("APPDATA"); filepath.IsAbs(env) {
			roaming = env
		}
	}
	return roaming
}

func claudeWindowsDesktopAppCandidates(localAppData string) []string {
	return []string{
		// Current Claude Desktop is an MSIX package. Package presence is the
		// install signal; the protected executable itself is never launched.
		filepath.Join(localAppData, "Packages", claudeWindowsPackageFamily),
		// Legacy non-MSIX layouts remain read-only install signals.
		filepath.Join(localAppData, "AnthropicClaude", "Claude.exe"),
		filepath.Join(localAppData, "Programs", "Claude", "Claude.exe"),
	}
}

func usableClaudeExecutable(path string) bool {
	if strings.TrimSpace(path) == "" {
		return false
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	return runtime.GOOS == "windows" || info.Mode().Perm()&claudeExecutableBits != 0
}

func directoryPresent(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func claudeActualUserHome(home string) bool {
	current, err := user.Current()
	return err == nil && sameClaudePath(home, current.HomeDir)
}
