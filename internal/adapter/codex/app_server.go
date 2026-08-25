package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	codexAppServerRegistrationTimeout = 5 * time.Second
	codexExecutableBits               = os.FileMode(0o111)
	codexWindowsBinScanMaxEntries     = 256
	codexWindowsPackageFamily         = "OpenAI.Codex_2p2nqsd0c76g0"
)

// bestEffortRegisterAppThread asks Codex to load the rollout it just received.
// thread/resume is Codex's supported registration path: Codex remains the sole
// owner of its private indexes/databases, while the deterministic thread ID
// makes repeated registrations idempotent. CLI app-server is consulted only
// for named fork branches, where the branch label would otherwise be absent
// from `codex resume`; ordinary main-branch backfills retain the lower-cost
// Desktop-only behavior.
func (a *Adapter) bestEffortRegisterAppThread(threadID, cwd, title string, includeCLI bool) {
	if threadID == "" || a.HomeDir == "" || a.findAppServerExecutables == nil || a.registerAppServerThread == nil {
		return
	}

	codexHome := filepath.Join(a.HomeDir, ".codex")
	ctx, cancel := context.WithTimeout(context.Background(), codexAppServerRegistrationTimeout)
	defer cancel()
	candidates := a.findAppServerExecutables(a.HomeDir)
	if includeCLI {
		candidates = append(candidates, filterUsableCodexExecutables(a.codexCLIExecutableCandidates())...)
	}
	for _, executable := range candidates {
		if ctx.Err() != nil {
			return
		}
		err := a.registerAppServerThread(ctx, executable, codexHome, threadID, cwd, title)
		if err == nil {
			return
		}
	}
}

// codexAppServerExecutables returns native-app bundled app-server hosts in
// preference order. Deliberately exclude PATH and ~/.local/bin: a CLI-only
// installation supports the app-server subcommand but has no Desktop inventory
// to refresh, so launching it would add latency while assuming a second surface
// exists. Any stale app candidate is filtered before registration.
func codexAppServerExecutables(homeDir string) []string {
	var candidates []string
	if runtime.GOOS == "darwin" {
		appRoots := []string{filepath.Join(homeDir, "Applications")}
		if codexActualUserHome(homeDir) {
			appRoots = append([]string{filepath.Join(string(filepath.Separator), "Applications")}, appRoots...)
		}
		for _, appRoot := range appRoots {
			candidates = append(candidates,
				filepath.Join(appRoot, "Codex.app", "Contents", "Resources", "codex"),
				filepath.Join(appRoot, "ChatGPT.app", "Contents", "Resources", "codex"),
			)
		}
	}
	if runtime.GOOS == "windows" && homeDir != "" {
		candidates = append(candidates, codexWindowsAppServerCandidates(codexWindowsLocalAppData(homeDir))...)
	}
	return filterUsableCodexExecutables(candidates)
}

// codexWindowsAppServerCandidates covers the writable runtime cache used by
// the Store/MSIX desktop app. Current builds may place codex.exe directly in
// bin or in one version/hash child. The package-protected WindowsApps copy is
// deliberately not executed: access and dependency resolution vary by host,
// while these per-user copies are the app's own app-server launch targets.
func codexWindowsAppServerCandidates(localAppData string) []string {
	if localAppData == "" {
		return nil
	}
	binRoots := []string{
		filepath.Join(localAppData, "OpenAI", "Codex", "bin"),
		filepath.Join(localAppData, "Packages", codexWindowsPackageFamily, "LocalCache", "Local", "OpenAI", "Codex", "bin"),
	}
	var candidates []string
	for _, root := range binRoots {
		candidates = append(candidates, filepath.Join(root, "codex.exe"))
		f, err := os.Open(root)
		if err != nil {
			continue
		}
		entries, readErr := f.ReadDir(codexWindowsBinScanMaxEntries)
		_ = f.Close()
		if readErr != nil && readErr != io.EOF {
			continue
		}
		for _, entry := range entries {
			if entry.Type()&os.ModeSymlink != 0 || !entry.IsDir() {
				continue
			}
			candidates = append(candidates, filepath.Join(root, entry.Name(), "codex.exe"))
		}
	}
	return filterUsableCodexExecutables(candidates)
}

func codexWindowsLocalAppData(homeDir string) string {
	local := filepath.Join(homeDir, "AppData", "Local")
	if codexActualUserHome(homeDir) {
		if env := os.Getenv("LOCALAPPDATA"); filepath.IsAbs(env) {
			local = env
		}
	}
	return local
}

func codexWindowsRoamingAppData(homeDir string) string {
	roaming := filepath.Join(homeDir, "AppData", "Roaming")
	if codexActualUserHome(homeDir) {
		if env := os.Getenv("APPDATA"); filepath.IsAbs(env) {
			roaming = env
		}
	}
	return roaming
}

func codexWindowsDesktopInstallPresent(localAppData string) bool {
	for _, root := range []string{
		filepath.Join(localAppData, "Packages", codexWindowsPackageFamily),
		filepath.Join(localAppData, "OpenAI", "Codex", "bin"),
	} {
		if codexDirectoryPresent(root) {
			return true
		}
	}
	return false
}

func codexExecutableName() string {
	if runtime.GOOS == "windows" {
		return "codex.exe"
	}
	return "codex"
}

func isUsableCodexExecutable(path string) bool {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	return runtime.GOOS == "windows" || info.Mode().Perm()&codexExecutableBits != 0
}

// registerCodexAppServerThread runs a short-lived stdio app-server connection.
// CODEX_HOME points it at the rollout root selected by this Adapter. The
// exchange sends only protocol metadata and the opaque thread ID; conversation
// bodies stay in the already-written rollout file.
func registerCodexAppServerThread(ctx context.Context, executable, codexHome, threadID, cwd, title string) error {
	// stdio is app-server's documented default transport. Omitting --listen
	// also keeps registration compatible with older builds that expose the
	// stable app-server command but predate the explicit listener flag.
	cmd := exec.CommandContext(ctx, executable, "app-server")
	cmd.Env = envWithValue(os.Environ(), "CODEX_HOME", codexHome)
	cmd.Stderr = io.Discard

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("codex app-server stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("codex app-server stdout: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start codex app-server: %w", err)
	}

	protocolErr := resumeCodexAppServerThread(stdout, stdin, threadID, cwd, title)
	_ = stdin.Close()
	waitErr := cmd.Wait()
	if protocolErr != nil {
		return protocolErr
	}
	if waitErr != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("codex app-server exit: %w", waitErr)
	}
	return nil
}

type codexAppServerRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type codexAppServerResponse struct {
	ID     json.RawMessage         `json:"id"`
	Method string                  `json:"method"`
	Error  *codexAppServerRPCError `json:"error"`
}

// resumeCodexAppServerThread performs the stable JSONL/JSON-RPC handshake used
// by `codex app-server`: initialize, initialized, then thread/resume. The
// jsonrpc header is intentionally absent because app-server omits it on wire.
func resumeCodexAppServerThread(stdout io.Reader, stdin io.Writer, threadID, cwd, title string) error {
	encoder := json.NewEncoder(stdin)
	decoder := json.NewDecoder(stdout)

	if err := encoder.Encode(map[string]any{
		"method": "initialize",
		"id":     0,
		"params": map[string]any{
			"clientInfo": map[string]string{
				"name":    "aplexica",
				"title":   "Aplexica",
				"version": Version,
			},
		},
	}); err != nil {
		return fmt.Errorf("codex app-server initialize request: %w", err)
	}
	if err := awaitCodexAppServerResponse(decoder, 0, "initialize"); err != nil {
		return err
	}

	if err := encoder.Encode(map[string]any{"method": "initialized", "params": map[string]any{}}); err != nil {
		return fmt.Errorf("codex app-server initialized notification: %w", err)
	}
	resumeParams := map[string]string{"threadId": threadID}
	if cwd != "" {
		resumeParams["cwd"] = cwd
	}
	if err := encoder.Encode(map[string]any{
		"method": "thread/resume",
		"id":     1,
		"params": resumeParams,
	}); err != nil {
		return fmt.Errorf("codex app-server thread/resume request: %w", err)
	}
	if err := awaitCodexAppServerResponse(decoder, 1, "thread/resume"); err != nil {
		return err
	}
	if title == "" {
		return nil
	}
	if err := encoder.Encode(map[string]any{
		"method": "thread/name/set",
		"id":     2,
		"params": map[string]string{"threadId": threadID, "name": title},
	}); err != nil {
		return fmt.Errorf("codex app-server thread/name/set request: %w", err)
	}
	return awaitCodexAppServerResponse(decoder, 2, "thread/name/set")
}

func awaitCodexAppServerResponse(decoder *json.Decoder, id int, method string) error {
	for {
		var response codexAppServerResponse
		if err := decoder.Decode(&response); err != nil {
			return fmt.Errorf("codex app-server %s response: %w", method, err)
		}
		// Notifications and server-initiated requests can be interleaved with
		// responses. Only a method-less response bearing our exact id settles it.
		if response.Method != "" || string(response.ID) != fmt.Sprint(id) {
			continue
		}
		if response.Error != nil {
			return fmt.Errorf("codex app-server %s failed (%d): %s", method, response.Error.Code, response.Error.Message)
		}
		return nil
	}
}

func envWithValue(env []string, key, value string) []string {
	result := make([]string, 0, len(env)+1)
	for _, entry := range env {
		name, _, ok := strings.Cut(entry, "=")
		if ok && strings.EqualFold(name, key) {
			continue
		}
		result = append(result, entry)
	}
	return append(result, key+"="+value)
}
