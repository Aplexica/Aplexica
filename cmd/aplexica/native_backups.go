package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aplexica/aplexica/internal/adapter"
	"github.com/aplexica/aplexica/internal/adapter/codex"
	"github.com/aplexica/aplexica/internal/atomicfile"
	"github.com/aplexica/aplexica/internal/nativebackup"
	"github.com/aplexica/aplexica/internal/sync"
)

// nativeBackupsRoot is where native snapshots live:
// ~/.aplexica/backups. Both the first-run pre-sync snapshot and the
// reversible pre-restore snapshots are siblings under this directory so
// List enumerates them together.
func nativeBackupsRoot() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".aplexica", "backups")
}

// initialDoneMarker is the sentinel file whose presence means the
// first-run native snapshot already ran (so subsequent daemon starts
// skip it). It lives inside the backups root rather than the state dir
// so removing the backups directory wholesale resets the first-run
// behavior alongside the snapshots it gates.
func initialDoneMarker(backupsRoot string) string {
	return filepath.Join(backupsRoot, ".initial-done")
}

// agentRootsFromDiscoveries flattens the daemon's per-adapter discovery
// results into the nativebackup.AgentRoots the snapshotter consumes.
// Only installed agents with at least one native root are included;
// agents that aren't installed (or expose no roots) contribute nothing.
// Output is sorted by agent name for deterministic snapshots/logs.
func agentRootsFromDiscoveries(discoveries map[string]adapter.Discovery) []nativebackup.AgentRoots {
	out := make([]nativebackup.AgentRoots, 0, len(discoveries))
	for name, d := range discoveries {
		if !d.Installed {
			continue
		}
		roots := compactBackupRoots(append(append([]string{}, d.GlobalRoots...), d.RecursiveRoots...))
		if len(roots) == 0 {
			continue
		}
		out = append(out, nativebackup.AgentRoots{
			Name:         name,
			Roots:        roots,
			ExcludePaths: nativeBackupExcludePaths(name, roots),
			RedactFiles:  nativeBackupRedactFiles(name, roots),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// nativeBackupRedactFiles identifies mixed user-configuration and credential
// files that must remain restorable without copying their machine secrets.
// Unlike ExcludePaths, these files are copied through a typed, fail-closed
// transformer; the native source is never modified.
func nativeBackupRedactFiles(agentName string, roots []string) []nativebackup.FileRedaction {
	if agentName != "openclaw" {
		return nil
	}
	var out []nativebackup.FileRedaction
	for _, root := range roots {
		if filepath.Base(root) != ".openclaw" {
			continue
		}
		out = append(out, nativebackup.FileRedaction{
			Path: filepath.Join(root, "openclaw.json"),
			Kind: nativebackup.FileRedactionOpenClawConfig,
		})
	}
	return out
}

var codexMachineStateRelativePaths = []string{
	"auth.json",
	".tmp",
	"cache",
	"packages",
	"plugins/cache",
	"plugins/.plugin-appserver",
	"computer-use/Codex Computer Use.app",
	"logs_2.sqlite",
	"logs_2.sqlite-wal",
	"logs_2.sqlite-shm",
	"sqlite/logs_2.sqlite",
	"sqlite/logs_2.sqlite-wal",
	"sqlite/logs_2.sqlite-shm",
	"models_cache.json",
	"shell_snapshots",
	"process_manager",
}

const openClawCodexHomePrefixComponents = 4

func addCodexMachineStateExclusions(add func(string, ...string), root string) {
	add(root, codexMachineStateRelativePaths...)
}

func codexMachineStateRelativePathExcluded(relative string) bool {
	clean := filepath.Clean(relative)
	if clean == "." || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return false
	}
	for _, candidate := range codexMachineStateRelativePaths {
		candidate = filepath.FromSlash(candidate)
		if clean == candidate || strings.HasPrefix(clean, candidate+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// nativeBackupDynamicTargetExcluded recognizes machine-local state in bounded
// agent layouts whose instance ID is part of the path. It operates only on a
// NativeTarget already resolved beneath an authenticated source root, and uses
// exact path components so an agent ID containing similar text cannot widen
// the exclusion into user sessions or workspaces.
func nativeBackupDynamicTargetExcluded(target nativebackup.NativeTarget) bool {
	if target.Agent != "openclaw" {
		return false
	}
	clean := filepath.Clean(target.RelativePath)
	if clean == "." || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return false
	}
	components := strings.Split(clean, string(filepath.Separator))
	if len(components) <= openClawCodexHomePrefixComponents || components[0] != "agents" || components[1] == "" ||
		components[2] != "agent" || components[3] != "codex-home" {
		return false
	}
	return codexMachineStateRelativePathExcluded(filepath.Join(components[openClawCodexHomePrefixComponents:]...))
}

// openClawEmbeddedCodexHomes returns the installer-managed Codex homes nested
// under OpenClaw agents. The conventional main home is always covered so an
// absent adapter can still sanitize older authenticated history. Additional
// agent IDs are admitted only through real directory components beneath the
// real agents directory; symlinks and non-directories are never traversed.
func openClawEmbeddedCodexHomes(root string) []string {
	mainHome := filepath.Join(root, "agents", "main", "agent", "codex-home")
	homes := []string{mainHome}
	seen := map[string]struct{}{filepath.Clean(mainHome): {}}
	agentsRoot := filepath.Join(root, "agents")
	if !nativeBackupRealDirectory(agentsRoot) {
		return homes
	}
	entries, err := os.ReadDir(agentsRoot)
	if err != nil {
		return homes
	}
	for _, entry := range entries {
		agentRoot := filepath.Join(agentsRoot, entry.Name())
		embeddedAgentRoot := filepath.Join(agentRoot, "agent")
		codexHome := filepath.Join(embeddedAgentRoot, "codex-home")
		if !nativeBackupRealDirectory(agentRoot) ||
			!nativeBackupRealDirectory(embeddedAgentRoot) ||
			!nativeBackupRealDirectory(codexHome) {
			continue
		}
		clean := filepath.Clean(codexHome)
		if _, duplicate := seen[clean]; duplicate {
			continue
		}
		seen[clean] = struct{}{}
		homes = append(homes, clean)
	}
	sort.Strings(homes)
	return homes
}

func nativeBackupRealDirectory(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.IsDir() && info.Mode()&os.ModeSymlink == 0
}

// nativeBackupExcludePaths separates user-authored agent state from
// rebuildable application state that happens to live under the same broad
// discovery root. Native restore is overlay-only, so omitting these paths
// leaves the destination machine's current runtime/cache/credentials untouched
// while conversations, memories, skills, settings, and local work remain
// restorable.
func nativeBackupExcludePaths(agentName string, roots []string) []string {
	seen := map[string]struct{}{}
	var out []string
	add := func(root string, relative ...string) {
		for _, rel := range relative {
			path := filepath.Clean(filepath.Join(root, filepath.FromSlash(rel)))
			if _, duplicate := seen[path]; duplicate {
				continue
			}
			seen[path] = struct{}{}
			out = append(out, path)
		}
	}

	for _, root := range roots {
		switch agentName {
		case "codex":
			if filepath.Base(root) != ".codex" {
				continue
			}
			addCodexMachineStateExclusions(add, root)
		case "claude-code":
			if filepath.Base(root) != ".claude" {
				continue
			}
			add(root,
				".credentials.json",
				"cache",
				"plugins/cache",
				"plugins/marketplaces",
				"plugins/plugin-catalog-cache.json",
				"security/agent-sdk-venv",
				"security/log.txt",
				"security/log.txt.1",
				"daemon/control.key",
				"daemon.log",
				"daemon.lock",
				"daemon.status.json",
				"daemon-auth-cooldown",
				"daemon-auth-status.json",
				"mcp-needs-auth-cache.json",
				"telemetry",
				"shell-snapshots",
				"session-env",
				"ide",
			)
		case "hermes":
			if filepath.Base(root) != ".hermes" {
				continue
			}
			// Keep Hermes' user-authored state (state.db*, response_store.db*,
			// kanban.db, sessions, cron, memories, skills, config, and working
			// source). These exact paths are machine credentials, redundant native
			// backups, downloaded runtimes, caches, logs, or installer/build output.
			// The .git exclusion is intentionally scoped to Hermes' installer-
			// managed checkout: unpublished Git history elsewhere is user data.
			add(root,
				"auth.json",
				".env",
				"state-snapshots",
				"backups",
				"node",
				"bin",
				"logs",
				"cache",
				"bootstrap-cache",
				"hermes-setup",
				"webui",
				"desktop",
				"webui.pid",
				"webui.ctl.env",
				"webui.log",
				"processes.json",
				"gateway_state.json",
				"gateway.pid",
				"gateway.lock",
				"provider_models_cache.json",
				"ollama_cloud_models_cache.json",
				"models_dev_cache.json",
				"models.json",
				"hermes-agent/.git",
				"hermes-agent/apps/desktop/release",
				"hermes-agent/apps/desktop/dist",
				"hermes-agent/apps/desktop/build",
			)
		case "kilo":
			// Kilo can expose config, legacy-home, and data roots. The same
			// machine-local names are safe to omit wherever they occur; generic
			// dependency-directory exclusions are enforced by nativebackup.
			add(root, "auth.json", "log", "telemetry-id")
		case "openclaw":
			if filepath.Base(root) != ".openclaw" {
				continue
			}
			// OpenClaw mixes user state and machine secrets/runtime beneath one
			// root. Preserve workspace, memory, conversations, skills, and source,
			// but never duplicate pairing identities, gateway tokens, downloaded
			// npm runtimes, or its embedded Codex backend caches.
			add(root,
				"npm",
				"tmp",
				"locks",
				"logs",
				"service-env",
				"update-check.json",
				"identity",
				"devices",
				"openclaw.json.bak",
				"openclaw.json.last-good",
			)
			for _, codexHome := range openClawEmbeddedCodexHomes(root) {
				addCodexMachineStateExclusions(add, codexHome)
			}
		}
	}
	sort.Strings(out)
	return out
}

func codexInternalSessionPaths(root string) []string {
	sessionsRoot := filepath.Join(root, "sessions")
	var out []string
	_ = filepath.WalkDir(sessionsRoot, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			if entry != nil && entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || filepath.Ext(path) != ".jsonl" ||
			!strings.HasPrefix(filepath.Base(path), "rollout-") {
			return nil
		}
		internal, classifyErr := codex.SessionIsInternal(path)
		if classifyErr == nil && internal {
			out = append(out, path)
		}
		return nil
	})
	sort.Strings(out)
	return out
}

// withNativeBackupContentExclusions resolves exclusions that require reading
// native file content. Keep this work at snapshot/restore/sanitizer boundaries
// rather than in discovery: web status polls agentRoots frequently and must not
// rescan every Codex rollout each time.
//
// Codex stores spawned worker/reviewer rollouts beside user-owned sessions.
// They are explicitly marked as subagents in session_meta, are not independent
// conversations, and are already excluded from canonical import. Excluding
// those exact files prevents every backup from multiplying the parent task's
// full context. Classification is fail-open: unreadable or malformed files are
// preserved as user data.
func withNativeBackupContentExclusions(agents []nativebackup.AgentRoots) []nativebackup.AgentRoots {
	out := make([]nativebackup.AgentRoots, len(agents))
	for i, agent := range agents {
		policy := agent
		policy.Roots = append([]string(nil), agent.Roots...)
		policy.ExcludePaths = append([]string(nil), agent.ExcludePaths...)
		policy.RedactFiles = append([]nativebackup.FileRedaction(nil), agent.RedactFiles...)
		if agent.Name == "codex" {
			seen := make(map[string]struct{}, len(policy.ExcludePaths))
			for _, path := range policy.ExcludePaths {
				seen[filepath.Clean(path)] = struct{}{}
			}
			for _, root := range policy.Roots {
				if filepath.Base(root) != ".codex" {
					continue
				}
				for _, path := range codexInternalSessionPaths(root) {
					path = filepath.Clean(path)
					if _, duplicate := seen[path]; duplicate {
						continue
					}
					seen[path] = struct{}{}
					policy.ExcludePaths = append(policy.ExcludePaths, path)
				}
			}
			sort.Strings(policy.ExcludePaths)
		}
		out[i] = policy
	}
	return out
}

// agentRootsFromAdapters refreshes discovery at the point a safety operation
// starts. Runtime-discoverable adapters may have been installed (or may have
// created their shared storage) after daemon startup, so the startup snapshot
// map is not authoritative for backup/restore safety throughout the process.
func agentRootsFromAdapters(adapters []adapter.Adapter) []nativebackup.AgentRoots {
	discoveries := make(map[string]adapter.Discovery, len(adapters))
	for _, ad := range adapters {
		discovery, err := ad.Discover()
		if err != nil {
			continue
		}
		discoveries[ad.Name()] = discovery
	}
	return agentRootsFromDiscoveries(discoveries)
}

// applyRuntimeBackupBlocks merges the sparse result of a runtime safety pass.
// EnsureStartupSafety reports failures observed by that invocation; absence is
// not a global verdict about every other adapter. Record every new failure, but
// clear only the adapter whose activation triggered and completed this pass.
func applyRuntimeBackupBlocks(blocker *syncd.AdapterBlocker, activated string, blocks map[string]string) {
	for name, reason := range blocks {
		blocker.Set(name, reason)
	}
	if activated == "" {
		return
	}
	if _, failed := blocks[activated]; !failed {
		blocker.Clear(activated)
	}
}

const nativeSafetyVerificationPendingReason = "native safety backup verification pending"

type nativeSafetyProgress func(agent, reason string)

// startNativeStartupSafety keeps the multi-gigabyte payload proof off the
// listener/watcher startup path without weakening the gate it protects. Every
// discovered adapter is blocked synchronously before the goroutine starts and
// is cleared only after its existing full-hash verification (or a replacement
// snapshot/explicit override) has completed and any changed safety record has
// been checkpointed. A panic or early return therefore leaves the adapter
// fail-closed at the pending reason.
func startNativeStartupSafety(m *nativeBackupManager, lg nativeBackupsLogger, agents []nativebackup.AgentRoots, blocker *syncd.AdapterBlocker) <-chan struct{} {
	done := make(chan struct{})
	exactAgents := append([]nativebackup.AgentRoots{}, agents...)
	for _, ag := range exactAgents {
		blocker.Set(ag.Name, nativeSafetyVerificationPendingReason)
	}
	go func() {
		defer close(done)
		blocked := m.ensureStartupSafetyForAgents(lg, exactAgents, func(agent, reason string) {
			if reason != "" {
				blocker.Set(agent, reason)
				return
			}
			blocker.Clear(agent)
		})
		if lg != nil {
			lg.Info("native startup safety verification complete", "agents", len(exactAgents), "blocked", len(blocked))
		}
	}()
	return done
}

func nativeSafetyRootSignatures(agents []nativebackup.AgentRoots) map[string]string {
	out := make(map[string]string, len(agents))
	for _, ag := range agents {
		out[ag.Name] = agentRootSignature(ag)
	}
	return out
}

// startupSafetyCoversDiscovery reports whether the runtime activation is the
// exact topology already gated by the startup pass. The activation hook may
// then rely on that live blocker instead of immediately hashing every safety
// snapshot a second time. A newly installed agent or changed root topology is
// not covered and still takes the synchronous fail-closed runtime path.
func startupSafetyCoversDiscovery(startupSignatures map[string]string, name string, discovery adapter.Discovery) bool {
	agents := agentRootsFromDiscoveries(map[string]adapter.Discovery{name: discovery})
	if len(agents) != 1 {
		return false
	}
	want, ok := startupSignatures[name]
	return ok && want == agentRootSignature(agents[0])
}

// Reuse the daemon's active-session cadence for the secondary safety gate. A
// dedicated sub-second literal here would be another hidden runtime tuning
// knob; this check has the same responsiveness requirement as live session
// catch-up and is not performance-sensitive once the block clears.
const nativeStartupSafetyPollInterval = daemonCodexSessionScanInterval

// waitForNativeStartupSafety gates native sync loops that bypass the
// orchestrator (currently hermeswatch). They must not touch an agent while its
// startup proof is pending or failed. Polling after the initial pass lets a
// user-created replacement backup or explicit override unblock the loop without
// requiring a daemon restart.
func waitForNativeStartupSafety(ctx context.Context, startupDone <-chan struct{}, blocker *syncd.AdapterBlocker, agent string) bool {
	select {
	case <-ctx.Done():
		return false
	case <-startupDone:
	}
	if _, blocked := blocker.Blocked(agent); !blocked {
		return true
	}
	ticker := time.NewTicker(nativeStartupSafetyPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
			if _, blocked := blocker.Blocked(agent); !blocked {
				return true
			}
		}
	}
}

func compactBackupRoots(roots []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(roots))
	for _, r := range roots {
		if r == "" {
			continue
		}
		clean := filepath.Clean(r)
		if _, ok := seen[clean]; ok {
			continue
		}
		seen[clean] = struct{}{}
		out = append(out, clean)
	}
	sort.Slice(out, func(i, j int) bool {
		if len(out[i]) == len(out[j]) {
			return out[i] < out[j]
		}
		return len(out[i]) < len(out[j])
	})
	kept := out[:0]
	for _, r := range out {
		nested := false
		for _, existing := range kept {
			prefix := strings.TrimRight(existing, string(filepath.Separator)) + string(filepath.Separator)
			if r == existing || strings.HasPrefix(r, prefix) {
				nested = true
				break
			}
		}
		if !nested {
			kept = append(kept, r)
		}
	}
	sort.Strings(kept)
	return kept
}

// nativeBackupsLogger is the minimal logging surface the first-run
// snapshot trigger needs. The daemon's *daemon.Logger satisfies it.
type nativeBackupsLogger interface {
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}

// runFirstRunNativeBackup takes the one-time pre-Aplexica safety
// snapshot if it hasn't run before. It is a no-op (logged at INFO) when
// the .initial-done marker is present or when no agent exposes a native
// root. On success it writes the marker so subsequent starts skip the
// snapshot. Errors are logged, not returned — a failed safety snapshot
// must never block the daemon from starting (and fan-out is impossible
// pre-config anyway, per Part 1's safe-by-default rules).
//
// Safe to run in a goroutine: it touches only the backups directory.
func runFirstRunNativeBackup(lg nativeBackupsLogger, backupsRoot string, agents []nativebackup.AgentRoots) {
	marker := initialDoneMarker(backupsRoot)
	if _, err := os.Stat(marker); err == nil {
		lg.Info("first-run native backup skipped", "reason", "already done", "marker", marker)
		return
	}
	if len(agents) == 0 {
		lg.Info("first-run native backup skipped", "reason", "no installed agents with native roots")
		// Still write the marker so we don't re-scan on every start. A
		// later install of an agent is covered by the agent's own
		// import path, not this one-time pre-sync snapshot.
		if err := writeInitialDoneMarker(marker); err != nil {
			lg.Warn("first-run native backup: write marker failed", "err", err)
		}
		return
	}

	// Colon-free timestamp (matches the pre-restore dir layout + the
	// restore-native --from help example, and is a legal dir name on
	// Windows where RFC3339's colons are not).
	dest := filepath.Join(backupsRoot, nativebackup.SnapshotPrefix+time.Now().UTC().Format("2006-01-02T15-04-05Z"))
	lg.Info("first-run native backup starting",
		"dest", dest, "agents", len(agents))
	man, err := nativebackup.SnapshotAuthenticated(withNativeBackupContentExclusions(agents), dest)
	if err != nil {
		// A snapshot is not restorable until manifest.json is committed. Never
		// retain the partial tree: repeated startup retries otherwise accumulate
		// another full native-state copy each time.
		_ = os.RemoveAll(dest)
		lg.Error("first-run native backup failed; daemon continues", "err", err, "dest", dest)
		return
	}
	var totalBytes int64
	var fileCount int
	for _, ag := range man.Agents {
		for _, fe := range ag.Roots {
			totalBytes += fe.Bytes
			fileCount++
		}
	}
	lg.Info("first-run native backup complete",
		"dest", dest, "files", fileCount, "bytes", totalBytes)
	if err := writeInitialDoneMarker(marker); err != nil {
		lg.Warn("first-run native backup: write marker failed (will re-run next start)", "err", err)
	}
}

type nativeBackupSafetyState struct {
	Agents map[string]nativeBackupSafetyRecord `json:"agents"`
}

type nativeBackupSafetyRecord struct {
	RootSignature string    `json:"rootSignature,omitempty"`
	BackupID      string    `json:"backupId,omitempty"`
	LastBackupAt  time.Time `json:"lastBackupAt,omitempty"`
	LastError     string    `json:"lastError,omitempty"`
	LastFailureAt time.Time `json:"lastFailureAt,omitempty"`
	Override      bool      `json:"override,omitempty"`
	OverrideAt    time.Time `json:"overrideAt,omitempty"`
}

type nativeBackupManager struct {
	backupsRoot                 string
	agentRoots                  func() []nativebackup.AgentRoots
	mu                          sync.Mutex
	opMu                        sync.Mutex
	cloudMu                     sync.Mutex
	restoreCoordinator          nativebackup.NativeRestoreCoordinator
	snapshotSafety              func([]nativebackup.AgentRoots, string) (nativebackup.Manifest, error)
	removeCloudStaging          func([]string) (int, error)
	recoverSanitizeTransactions func(context.Context, string, string, bool) (nativebackup.SanitizeRecoveryResult, error)
	sanitizeSnapshot            func(context.Context, string, nativebackup.SanitizeOptions) (nativebackup.SanitizeResult, error)
	removeIncompletePreSync     func(string) error
}

func newNativeBackupManager(backupsRoot string, agentRoots func() []nativebackup.AgentRoots) *nativeBackupManager {
	return &nativeBackupManager{
		backupsRoot:                 backupsRoot,
		agentRoots:                  agentRoots,
		snapshotSafety:              nativebackup.SnapshotAuthenticated,
		removeCloudStaging:          removeCloudStaging,
		recoverSanitizeTransactions: nativebackup.RecoverSanitizeTransactionsContext,
		sanitizeSnapshot:            nativebackup.SanitizeSnapshotContext,
	}
}

func (m *nativeBackupManager) recoverSanitize(ctx context.Context, cleanup bool) (nativebackup.SanitizeRecoveryResult, error) {
	recoverTransactions := m.recoverSanitizeTransactions
	if recoverTransactions == nil {
		recoverTransactions = nativebackup.RecoverSanitizeTransactionsContext
	}
	return recoverTransactions(ctx, m.backupsRoot, m.manifestKeyPath(), cleanup)
}

func (m *nativeBackupManager) Restore(ctx context.Context, dir, agent string) (nativebackup.RestoreResult, error) {
	if m == nil || m.agentRoots == nil {
		return nativebackup.RestoreResult{}, fmt.Errorf("native backup manager unavailable")
	}
	m.opMu.Lock()
	defer m.opMu.Unlock()
	coordinator := m.restoreCoordinator
	if coordinator == nil {
		coordinator = nativebackup.LocalRestoreCoordinator{LockPath: filepath.Join(filepath.Dir(m.backupsRoot), "state", "native-restore.lock")}
	}
	currentRoots := withNativeBackupContentExclusions(m.agentRoots())
	return nativebackup.RestoreWithOptions(ctx, dir, nativebackup.NativeRestoreOptions{
		Agent: agent, CurrentAgentRoots: currentRoots, ManifestKeyPath: m.manifestKeyPath(),
		Coordinator: coordinator, ExcludeTarget: nativeBackupDynamicTargetExcluded,
	})
}

func (m *nativeBackupManager) manifestKeyPath() string {
	return filepath.Join(filepath.Dir(m.backupsRoot), "keys", "native-manifest-hmac-v2")
}

func (m *nativeBackupManager) Create(kind string, agents []string) (nativebackup.BackupInfo, error) {
	return m.CreateContext(context.Background(), kind, agents)
}

func (m *nativeBackupManager) CreateContext(ctx context.Context, kind string, agents []string) (nativebackup.BackupInfo, error) {
	m.opMu.Lock()
	defer m.opMu.Unlock()

	m.mu.Lock()
	selected, err := selectAgentRoots(m.agentRoots(), agents)
	if err != nil {
		m.mu.Unlock()
		return nativebackup.BackupInfo{}, err
	}
	prefix, err := backupPrefixForKind(kind)
	if err != nil {
		m.mu.Unlock()
		return nativebackup.BackupInfo{}, err
	}
	id, dest, err := m.allocateBackupDestination(prefix, selected, time.Now().UTC())
	if err != nil {
		m.mu.Unlock()
		return nativebackup.BackupInfo{}, err
	}
	m.mu.Unlock()
	selected = withNativeBackupContentExclusions(selected)

	man, err := nativebackup.SnapshotContextAuthenticated(ctx, selected, dest)
	if err != nil {
		_ = os.RemoveAll(dest)
		return nativebackup.BackupInfo{}, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if kind == "manual" || kind == "scheduled" || kind == "pre-sync" {
		_ = m.recordSuccessfulSafetyBackup(selected, id, man.CreatedAt)
	}
	if kind == "" || kind == "manual" || kind == "scheduled" {
		retention, err := loadNativeBackupRetention(m.retentionPath())
		if err != nil {
			return nativebackup.BackupInfo{}, err
		}
		if err := m.pruneRetainedHistoryLocked(retention); err != nil {
			return nativebackup.BackupInfo{}, err
		}
	}
	infos, err := nativebackup.List(m.backupsRoot)
	if err != nil {
		return nativebackup.BackupInfo{}, err
	}
	for _, info := range infos {
		if info.ID == id {
			return info, nil
		}
	}
	return nativebackup.BackupInfo{
		ID:         id,
		Path:       dest,
		Kind:       kind,
		CreatedAt:  man.CreatedAt,
		Agents:     agentNames(selected),
		Location:   "local",
		TotalBytes: manifestTotalBytes(man),
		FileCount:  manifestFileCount(man),
	}, nil
}

func (m *nativeBackupManager) CreateCloudStaging(kind string, agents []string) (nativebackup.BackupInfo, error) {
	return m.CreateCloudStagingContext(context.Background(), kind, agents)
}

func (m *nativeBackupManager) CreateCloudStagingContext(ctx context.Context, kind string, agents []string) (nativebackup.BackupInfo, error) {
	m.opMu.Lock()
	defer m.opMu.Unlock()

	m.mu.Lock()
	selected, err := selectAgentRoots(m.agentRoots(), agents)
	if err != nil {
		m.mu.Unlock()
		return nativebackup.BackupInfo{}, err
	}
	prefix, err := backupPrefixForKind(kind)
	if err != nil {
		m.mu.Unlock()
		return nativebackup.BackupInfo{}, err
	}
	// Bound any orphaned staging dirs left by an interrupted prior run before
	// adding another, so .cloud-staging can never grow without bound even if a
	// run dies before its own cleanup. Fail closed when cleanup cannot finish:
	// allocating another multi-gigabyte raw tree would turn a transient sharing
	// violation into unbounded growth. Safe here — opMu is held, so no other
	// staging operation races this sweep.
	if _, err := m.sweepCloudStagingLocked(cloudStagingRetain); err != nil {
		m.mu.Unlock()
		return nativebackup.BackupInfo{}, fmt.Errorf("cloud staging cleanup must complete before allocation: %w", err)
	}
	id, dest, err := allocateBackupDestinationIn(m.cloudStagingRoot(), prefix, selected, time.Now().UTC())
	if err != nil {
		m.mu.Unlock()
		return nativebackup.BackupInfo{}, err
	}
	m.mu.Unlock()
	selected = withNativeBackupContentExclusions(selected)

	man, err := nativebackup.SnapshotContextAuthenticated(ctx, selected, dest)
	if err != nil {
		_ = os.RemoveAll(dest)
		return nativebackup.BackupInfo{}, err
	}
	return nativebackup.BackupInfo{
		ID:         id,
		Path:       dest,
		Kind:       kind,
		CreatedAt:  man.CreatedAt,
		Agents:     agentNames(selected),
		TotalBytes: manifestTotalBytes(man),
		FileCount:  manifestFileCount(man),
		Location:   "cloud",
		Encrypted:  true,
	}, nil
}

func (m *nativeBackupManager) Delete(id string) (nativebackup.BackupInfo, error) {
	if _, ok := nativebackup.SnapshotKindFromID(id); !ok {
		return nativebackup.BackupInfo{}, fmt.Errorf("invalid backup id %q", id)
	}
	m.opMu.Lock()
	defer m.opMu.Unlock()

	m.mu.Lock()
	dir, err := resolveBackupDir(m.backupsRoot, id)
	if err != nil {
		m.mu.Unlock()
		return nativebackup.BackupInfo{}, err
	}
	info, err := m.infoForBackupLocked(id)
	if err != nil {
		m.mu.Unlock()
		return nativebackup.BackupInfo{}, err
	}
	m.mu.Unlock()

	if err := os.RemoveAll(dir); err != nil {
		return nativebackup.BackupInfo{}, fmt.Errorf("delete backup %q: %w", id, err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.clearSafetyBackupReferenceLocked(id); err != nil {
		return nativebackup.BackupInfo{}, err
	}
	return info, nil
}

func (m *nativeBackupManager) EnsureStartupSafety(lg nativeBackupsLogger) map[string]string {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	return m.ensureStartupSafetyLocked(lg, nil, nil)
}

func (m *nativeBackupManager) ensureStartupSafetyForAgents(lg nativeBackupsLogger, agents []nativebackup.AgentRoots, progress nativeSafetyProgress) map[string]string {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	return m.ensureStartupSafetyLocked(lg, agents, progress)
}

// ensureStartupSafetyLocked performs the authoritative safety proof. Caller
// holds opMu. A nil agents slice preserves the dynamic EnsureStartupSafety
// behavior; a non-nil slice pins startup verification to the exact adapters
// that were synchronously gated before its background goroutine was launched.
func (m *nativeBackupManager) ensureStartupSafetyLocked(lg nativeBackupsLogger, agents []nativebackup.AgentRoots, progress nativeSafetyProgress) map[string]string {
	// A process death between the sanitizer's two directory renames can leave a
	// referenced BackupID temporarily hidden under its rollback transaction.
	// Recover only the visibility-critical renames here before safety validation;
	// full hashing and potentially multi-gigabyte cleanup stay asynchronous.
	if recovery, err := m.recoverSanitize(context.Background(), false); err != nil {
		if lg != nil {
			lg.Warn("native backup sanitizer: fast recovery incomplete", "err", err)
		}
	} else if recovery.Recovered > 0 && lg != nil {
		lg.Info("native backup sanitizer: recovered interrupted swaps", "recovered", recovery.Recovered)
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	state, err := loadNativeBackupSafetyState(m.safetyPath())
	if err != nil && lg != nil {
		lg.Warn("native backup safety state unreadable; rebuilding best effort", "err", err)
	}
	if state.Agents == nil {
		state.Agents = map[string]nativeBackupSafetyRecord{}
	}
	// This pass already runs off the listener startup path. Reclaim crash-left,
	// unreferenced pre-sync allocations before considering another full copy so
	// repeated process deaths cannot add one native tree on every restart. The
	// post-start sweep repeats this idempotently for cleanup failures and other
	// history policy, but it is deliberately no longer the first reclamation
	// point for manifestless safety snapshots.
	_ = m.pruneIncompleteSnapshotsLocked(state, lg)
	if agents == nil {
		agents = m.agentRoots()
	}
	blocked := map[string]string{}
	if len(agents) == 0 {
		return blocked
	}
	authenticatedCandidates, legacy := m.preSyncSafetyCandidatesLocked()
	checkpoint := func(agentIndex int) bool {
		if err := writeNativeBackupSafetyState(m.safetyPath(), state); err != nil {
			reason := fmt.Sprintf("persist native backup safety state: %v", err)
			// Do not inspect or snapshot another agent after a checkpoint failure.
			// None of the remaining agents has completed this startup safety pass,
			// so fail closed for all of them as well as the current agent.
			for _, pending := range agents[agentIndex:] {
				blocked[pending.Name] = reason
				if progress != nil {
					progress(pending.Name, reason)
				}
			}
			if lg != nil {
				lg.Error("native backup safety state checkpoint failed; blocking remaining agents", "agent", agents[agentIndex].Name, "err", err)
			}
			return false
		}
		return true
	}
	snapshotSafety := m.snapshotSafety
	if snapshotSafety == nil {
		snapshotSafety = nativebackup.SnapshotAuthenticated
	}
	for agentIndex, ag := range agents {
		recordChanged := false
		sig := agentRootSignature(ag)
		rec := state.Agents[ag.Name]
		if rec.RootSignature != "" && rec.RootSignature != sig {
			// A backup/override for an older root topology cannot protect newly
			// discovered roots. Clear it before either adopting an exact-root
			// orphan or taking a replacement; otherwise a failed replacement
			// could accidentally carry the stale BackupID into the new record.
			rec = nativeBackupSafetyRecord{}
			state.Agents[ag.Name] = rec
			recordChanged = true
		}
		if rec.RootSignature == sig && rec.BackupID != "" {
			if err := m.validateSafetyBackupLocked(rec.BackupID, ag.Name); err == nil {
				if progress != nil {
					progress(ag.Name, "")
				}
				continue
			} else if lg != nil {
				lg.Warn("native safety backup missing or unreadable; taking a replacement", "agent", ag.Name, "backup", rec.BackupID, "err", err)
			}
			rec.BackupID = ""
			rec.LastError = "referenced safety backup is missing or unreadable"
			state.Agents[ag.Name] = rec
			recordChanged = true
			if info, ok := m.newestValidAuthenticatedPreSyncLocked(ag, authenticatedCandidates); ok {
				rec.RootSignature = sig
				rec.BackupID = info.ID
				rec.LastBackupAt = info.CreatedAt
				rec.LastError = ""
				rec.LastFailureAt = time.Time{}
				rec.Override = false
				rec.OverrideAt = time.Time{}
				state.Agents[ag.Name] = rec
				if !checkpoint(agentIndex) {
					return blocked
				}
				if lg != nil {
					lg.Info("native safety backup replacement recovered", "agent", ag.Name, "backup", info.ID)
				}
				if progress != nil {
					progress(ag.Name, "")
				}
				continue
			}
		}
		if rec.RootSignature == sig && rec.Override {
			if lg != nil {
				lg.Warn("native backup safety override active; agent allowed without backup", "agent", ag.Name)
			}
			if recordChanged {
				state.Agents[ag.Name] = rec
				if !checkpoint(agentIndex) {
					return blocked
				}
			}
			if progress != nil {
				progress(ag.Name, "")
			}
			continue
		}
		if rec.BackupID == "" {
			if info, ok := m.newestValidAuthenticatedPreSyncLocked(ag, authenticatedCandidates); ok {
				rec.RootSignature = sig
				rec.BackupID = info.ID
				rec.LastBackupAt = info.CreatedAt
				rec.LastError = ""
				rec.LastFailureAt = time.Time{}
				rec.Override = false
				rec.OverrideAt = time.Time{}
				state.Agents[ag.Name] = rec
				if !checkpoint(agentIndex) {
					return blocked
				}
				if progress != nil {
					progress(ag.Name, "")
				}
				continue
			}
			if info, ok := legacy[ag.Name]; ok && rec.RootSignature == "" {
				legacyManifest, legacyErr := nativebackup.ReadSnapshotManifestContext(context.Background(), info.Path)
				if legacyErr == nil && manifestMatchesAgentRoots(legacyManifest, ag) && m.validateSafetyBackupLocked(info.ID, ag.Name) == nil {
					rec.RootSignature = sig
					rec.BackupID = info.ID
					rec.LastBackupAt = info.CreatedAt
					rec.LastError = ""
					rec.Override = false
					state.Agents[ag.Name] = rec
					if !checkpoint(agentIndex) {
						return blocked
					}
					if progress != nil {
						progress(ag.Name, "")
					}
					continue
				}
			}
		}
		// A stale state record may have protected a manifestless tree from the
		// initial sweep until validateSafetyBackupLocked proved the reference
		// unusable and cleared it above. Re-run the idempotent cleanup at the exact
		// allocation boundary; it also removes a partial left by an earlier agent
		// in this same pass before another full native tree is started.
		id := nativebackup.SnapshotPrefix + safeBackupIDSegment(ag.Name) + "-" + time.Now().UTC().Format("2006-01-02T15-04-05Z")
		dest := filepath.Join(m.backupsRoot, id)
		var man nativebackup.Manifest
		cleanupErr := m.pruneIncompleteSnapshotsLocked(state, lg)
		var snapErr error
		if cleanupErr != nil {
			snapErr = fmt.Errorf("reclaim incomplete pre-sync snapshots before allocation: %w", cleanupErr)
		} else {
			if lg != nil {
				lg.Info("native safety backup starting", "agent", ag.Name, "dest", dest)
			}
			selected := withNativeBackupContentExclusions([]nativebackup.AgentRoots{ag})
			man, snapErr = snapshotSafety(selected, dest)
		}
		if snapErr != nil {
			// A failed copy without a manifest is neither verifiable nor restorable;
			// the next allocation boundary or post-start sweep reclaims it. A failed
			// pre-allocation cleanup reaches this same branch without starting a copy.
			rec.RootSignature = sig
			rec.BackupID = ""
			rec.LastBackupAt = time.Time{}
			rec.LastError = snapErr.Error()
			rec.LastFailureAt = time.Now().UTC()
			state.Agents[ag.Name] = rec
			blocked[ag.Name] = snapErr.Error()
			if !checkpoint(agentIndex) {
				return blocked
			}
			if lg != nil {
				lg.Error("native safety backup failed; blocking agent until backup or override", "agent", ag.Name, "err", snapErr)
			}
			if progress != nil {
				progress(ag.Name, snapErr.Error())
			}
			continue
		}
		rec.RootSignature = sig
		rec.BackupID = id
		rec.LastBackupAt = man.CreatedAt
		rec.LastError = ""
		rec.LastFailureAt = time.Time{}
		rec.Override = false
		rec.OverrideAt = time.Time{}
		state.Agents[ag.Name] = rec
		if !checkpoint(agentIndex) {
			return blocked
		}
		if lg != nil {
			lg.Info("native safety backup complete", "agent", ag.Name, "dest", dest)
		}
		if progress != nil {
			progress(ag.Name, "")
		}
	}
	return blocked
}

// SweepNativeBackupHistory reclaims interrupted and redundant pre-sync
// snapshots after startup. EnsureStartupSafety runs first and durably records
// the current protected snapshots; bulk deletion stays off the daemon's
// listener/watch startup path.
func (m *nativeBackupManager) SweepNativeBackupHistory(lg nativeBackupsLogger) {
	m.opMu.Lock()
	defer m.opMu.Unlock()

	recovery, recoveryErr := m.recoverSanitize(context.Background(), true)
	sanitizeSafe := recoveryErr == nil && recovery.Pending == 0
	if recoveryErr != nil {
		if lg != nil {
			lg.Warn("native backup sanitizer: transaction recovery incomplete", "err", recoveryErr,
				"recovered", recovery.Recovered, "finalized", recovery.Finalized, "pending", recovery.Pending)
		}
	} else if lg != nil {
		if recovery.Pending > 0 {
			lg.Warn("native backup sanitizer: transaction recovery remains pending; new rebuilds deferred",
				"recovered", recovery.Recovered, "finalized", recovery.Finalized, "pending", recovery.Pending)
		} else if recovery.Recovered > 0 || recovery.Finalized > 0 {
			lg.Info("native backup sanitizer: transaction recovery complete",
				"recovered", recovery.Recovered, "finalized", recovery.Finalized)
		}
	}

	m.mu.Lock()
	state, err := loadNativeBackupSafetyState(m.safetyPath())
	if err != nil {
		if lg != nil {
			lg.Warn("native backup cleanup: safety state unreadable; history pruning skipped", "err", err)
		}
	} else {
		_ = m.pruneIncompleteSnapshotsLocked(state, lg)
		m.pruneUnreferencedPreSyncLocked(state, lg)
	}
	m.mu.Unlock()
	m.prunePreRestoreHistoryLocked(lg)

	if sanitizeSafe {
		m.sanitizeNativeBackupHistoryLocked(lg)
	}
}

// prunePreRestoreHistoryLocked reclaims interrupted restore allocations and
// bounds completed undo snapshots. SweepNativeBackupHistory already holds
// opMu; the restore lease additionally serializes this startup maintenance with
// a direct `restore-native` process that does not share the manager mutex.
func (m *nativeBackupManager) prunePreRestoreHistoryLocked(lg nativeBackupsLogger) {
	coordinator := m.restoreCoordinator
	if coordinator == nil {
		coordinator = nativebackup.LocalRestoreCoordinator{
			LockPath: filepath.Join(filepath.Dir(m.backupsRoot), "state", "native-restore.lock"),
		}
	}
	lease, err := coordinator.AcquireRestoreLease(context.Background(), nil)
	if err != nil {
		if lg != nil {
			lg.Warn("native backup cleanup: pre-restore lease unavailable", "err", err)
		}
		return
	}
	removed, pruneErr := nativebackup.PrunePreRestoreHistory(
		context.Background(), m.backupsRoot, nativebackup.MaxPreRestoreSnapshots, "",
	)
	closeErr := lease.Close()
	if pruneErr != nil || closeErr != nil {
		if lg != nil {
			lg.Warn("native backup cleanup: pre-restore pruning incomplete",
				"removed", removed, "err", errors.Join(pruneErr, closeErr))
		}
		return
	}
	if removed > 0 && lg != nil {
		lg.Info("native backup cleanup: bounded pre-restore history", "removed", removed,
			"retained", nativebackup.MaxPreRestoreSnapshots)
	}
}

// sanitizeNativeBackupHistoryLocked removes newly excluded cache/runtime/
// credential entries from existing authenticated local snapshots. Caller holds
// opMu, so Create/Delete/Restore and retention cannot race the journaled swap.
func (m *nativeBackupManager) sanitizeNativeBackupHistoryLocked(lg nativeBackupsLogger) {
	infos, err := nativebackup.List(m.backupsRoot)
	if err != nil {
		if lg != nil {
			lg.Warn("native backup sanitizer: list failed", "err", err)
		}
		return
	}
	policies := withNativeBackupContentExclusions(m.agentRoots())
	var sanitized, unchanged, legacy, failed, removedFiles, redactedFiles int
	var removedBytes int64
	sanitizeSnapshot := m.sanitizeSnapshot
	if sanitizeSnapshot == nil {
		sanitizeSnapshot = nativebackup.SanitizeSnapshotContext
	}
	for _, info := range infos {
		result, err := sanitizeSnapshot(context.Background(), info.Path, nativebackup.SanitizeOptions{
			CurrentAgentRoots:      policies,
			ManifestKeyPath:        m.manifestKeyPath(),
			ExcludeTarget:          nativeBackupDynamicTargetExcluded,
			KnownAgentExcludePaths: nativeBackupExcludePaths,
			KnownAgentRedactions:   nativeBackupRedactFiles,
		})
		if err != nil {
			failed++
			if lg != nil {
				lg.Warn("native backup sanitizer: snapshot left unchanged", "backup", info.ID, "err", err)
			}
			continue
		}
		switch result.Status {
		case nativebackup.SanitizeComplete:
			sanitized++
			removedFiles += result.RemovedFiles
			removedBytes += result.RemovedBytes
			redactedFiles += result.RedactedFiles
		case nativebackup.SanitizeLegacySkipped:
			legacy++
		case nativebackup.SanitizeUnchanged:
			unchanged++
		}
	}
	if lg != nil && (sanitized > 0 || legacy > 0 || failed > 0) {
		lg.Info("native backup sanitizer: maintenance complete",
			"sanitized", sanitized,
			"unchanged", unchanged,
			"legacySkipped", legacy,
			"failed", failed,
			"removedFiles", removedFiles,
			"removedBytes", removedBytes,
			"redactedFiles", redactedFiles,
		)
	}
}

// pruneIncompleteSnapshotsLocked removes only unreferenced, interrupted
// pre-sync snapshots. It runs both before startup-safety allocation and during
// the later history sweep; other snapshot kinds are handled by their own
// bounded policies, and a safety-state reference must be validated/replaced
// before its old tree can be reclaimed. Caller holds m.mu and opMu.
func (m *nativeBackupManager) pruneIncompleteSnapshotsLocked(state nativeBackupSafetyState, lg nativeBackupsLogger) error {
	keep := map[string]struct{}{}
	for _, rec := range state.Agents {
		if rec.BackupID != "" {
			keep[rec.BackupID] = struct{}{}
		}
	}
	entries, err := os.ReadDir(m.backupsRoot)
	if err != nil {
		if !os.IsNotExist(err) && lg != nil {
			lg.Warn("native backup cleanup: enumerate failed", "err", err)
		}
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	removed := 0
	var cleanupErrs []error
	removeIncomplete := m.removeIncompletePreSync
	if removeIncomplete == nil {
		removeIncomplete = os.RemoveAll
	}
	for _, de := range entries {
		if !de.IsDir() {
			continue
		}
		kind, ok := nativebackup.SnapshotKindFromID(de.Name())
		if !ok || kind != "pre-sync" {
			continue
		}
		if _, referenced := keep[de.Name()]; referenced {
			continue
		}
		path := filepath.Join(m.backupsRoot, de.Name())
		// Strict bounded/no-follow inspection is mandatory here: a crash or local
		// tampering can leave manifest.json as a FIFO, link, oversized file, or
		// malformed JSON. Cleanup must never block startup safety while deciding
		// whether a tree is complete.
		if _, err := nativebackup.ReadSnapshotManifestContext(context.Background(), path); err == nil {
			continue
		}
		if err := removeIncomplete(path); err != nil {
			wrapped := fmt.Errorf("remove incomplete pre-sync snapshot %s: %w", de.Name(), err)
			cleanupErrs = append(cleanupErrs, wrapped)
			if lg != nil {
				lg.Warn("native backup cleanup: partial snapshot removal failed", "backup", de.Name(), "err", wrapped)
			}
			continue
		}
		if _, err := os.Lstat(path); err == nil || !os.IsNotExist(err) {
			if err == nil {
				err = fmt.Errorf("path still exists")
			}
			wrapped := fmt.Errorf("verify incomplete pre-sync snapshot removal %s: %w", de.Name(), err)
			cleanupErrs = append(cleanupErrs, wrapped)
			if lg != nil {
				lg.Warn("native backup cleanup: partial snapshot removal could not be verified", "backup", de.Name(), "err", wrapped)
			}
			continue
		}
		removed++
	}
	if removed > 0 && lg != nil {
		lg.Info("native backup cleanup: removed incomplete snapshots", "removed", removed)
	}
	return errors.Join(cleanupErrs...)
}

func (m *nativeBackupManager) validateSafetyBackupLocked(id, agent string) error {
	if filepath.Base(id) != id {
		return fmt.Errorf("invalid backup id %q", id)
	}
	kind, ok := nativebackup.SnapshotKindFromID(id)
	if !ok || (kind != "pre-sync" && kind != "manual" && kind != "scheduled") {
		return fmt.Errorf("backup %q is not eligible for native safety", id)
	}
	backupDir := filepath.Join(m.backupsRoot, id)
	// Authenticate and structurally validate v2 metadata before inspecting the
	// requested agent. A corrupt safety-state reference to another agent must
	// not force startup to hash that unrelated snapshot. The full verifier below
	// independently re-reads/authenticates the manifest before and after hashing,
	// so this cheap classification does not weaken the recovery trust decision.
	man, authErr := nativebackup.AuthenticateSnapshotManifestContext(context.Background(), backupDir, m.manifestKeyPath())
	if authErr == nil {
		if !manifestContainsAgent(man, agent) {
			return fmt.Errorf("backup %q does not contain agent %q", id, agent)
		}
		verified, err := nativebackup.VerifyAuthenticatedSnapshotContext(context.Background(), backupDir, m.manifestKeyPath())
		if err != nil {
			return err
		}
		man = verified
	} else {
		// Legacy schema-0 safety references predate manifest authentication. Keep
		// their existing compatibility path, but never fall back to it for a
		// malformed or tampered v2 manifest.
		legacy, err := nativebackup.ReadSnapshotManifestContext(context.Background(), backupDir)
		if err != nil {
			return authErr
		}
		if legacy.Auth != (nativebackup.ManifestAuth{}) {
			return fmt.Errorf("backup %q contains authentication metadata but failed authentication", id)
		}
		switch legacy.SchemaVersion {
		case 2:
			return authErr
		case 0:
			// Pre-v2 safety snapshots were unsigned. Preserve compatibility while
			// validating every listed digest and the complete inventory below; the
			// bounded cleanup policy keeps
			// both the oldest baseline and newest fallback, so this legacy acceptance
			// cannot authorize deletion of the only recovery point.
			if err := nativebackup.VerifySnapshotFilesContext(context.Background(), backupDir, legacy); err != nil {
				return err
			}
			man = legacy
		default:
			return fmt.Errorf("backup %q uses unsupported manifest schema %d", id, legacy.SchemaVersion)
		}
	}
	found := false
	for _, entry := range man.Agents {
		if entry.Name != agent {
			continue
		}
		found = true
		for _, file := range entry.Roots {
			rel := filepath.Clean(filepath.FromSlash(file.Path))
			if rel == "." || rel == ".." || filepath.IsAbs(rel) || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return fmt.Errorf("backup %q contains unsafe path %q", id, file.Path)
			}
			info, err := os.Lstat(filepath.Join(backupDir, rel))
			if err != nil {
				return fmt.Errorf("backup %q file %q unavailable: %w", id, file.Path, err)
			}
			if !info.Mode().IsRegular() || info.Size() != file.Bytes {
				return fmt.Errorf("backup %q file %q does not match its manifest", id, file.Path)
			}
		}
	}
	if !found {
		return fmt.Errorf("backup %q does not contain agent %q", id, agent)
	}
	return nil
}

// pruneUnreferencedPreSyncLocked keeps the durable state references plus each
// agent's oldest pre-Aplexica rollback baseline and newest fallback. Only
// redundant intermediate pre-sync copies are removed. This bounds history
// without violating the first-run rollback contract or deleting all recovery
// points for an agent that is temporarily absent. Caller holds m.mu and opMu.
func (m *nativeBackupManager) pruneUnreferencedPreSyncLocked(state nativeBackupSafetyState, lg nativeBackupsLogger) {
	keep := map[string]struct{}{}
	for _, rec := range state.Agents {
		if rec.BackupID != "" {
			keep[rec.BackupID] = struct{}{}
		}
	}
	infos, err := nativebackup.List(m.backupsRoot)
	if err != nil {
		if lg != nil {
			lg.Warn("native backup cleanup: list safety snapshots failed", "err", err)
		}
		return
	}
	newestSeen := map[string]struct{}{}
	for _, info := range infos {
		if info.Kind != "pre-sync" || len(info.Agents) == 0 {
			continue
		}
		for _, agent := range info.Agents {
			if _, ok := newestSeen[agent]; ok {
				continue
			}
			newestSeen[agent] = struct{}{}
			keep[info.ID] = struct{}{}
		}
	}
	oldestSeen := map[string]struct{}{}
	for i := len(infos) - 1; i >= 0; i-- {
		info := infos[i]
		if info.Kind != "pre-sync" || len(info.Agents) == 0 {
			continue
		}
		for _, agent := range info.Agents {
			if _, ok := oldestSeen[agent]; ok {
				continue
			}
			oldestSeen[agent] = struct{}{}
			keep[info.ID] = struct{}{}
		}
	}
	removed := 0
	for _, info := range infos {
		if info.Kind != "pre-sync" {
			continue
		}
		if _, ok := keep[info.ID]; ok {
			continue
		}
		if err := os.RemoveAll(info.Path); err != nil {
			if lg != nil {
				lg.Warn("native backup cleanup: superseded safety snapshot removal failed", "backup", info.ID, "err", err)
			}
			continue
		}
		removed++
	}
	if removed > 0 && lg != nil {
		lg.Info("native backup cleanup: removed superseded safety snapshots", "removed", removed)
	}
}

func (m *nativeBackupManager) Override(agent string) (nativebackup.SafetyStatus, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	ag, ok := findAgentRoots(m.agentRoots(), agent)
	if !ok {
		return nativebackup.SafetyStatus{}, fmt.Errorf("agent %q has no backup roots", agent)
	}
	state, err := loadNativeBackupSafetyState(m.safetyPath())
	if err != nil {
		return nativebackup.SafetyStatus{}, err
	}
	if state.Agents == nil {
		state.Agents = map[string]nativeBackupSafetyRecord{}
	}
	rec := state.Agents[agent]
	rec.RootSignature = agentRootSignature(ag)
	rec.Override = true
	rec.OverrideAt = time.Now().UTC()
	state.Agents[agent] = rec
	if err := writeNativeBackupSafetyState(m.safetyPath(), state); err != nil {
		return nativebackup.SafetyStatus{}, err
	}
	return safetyStatusFromRecord(ag, rec, false), nil
}

func (m *nativeBackupManager) Status(blocked map[string]string) (nativebackup.Status, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	state, err := loadNativeBackupSafetyState(m.safetyPath())
	if err != nil {
		return nativebackup.Status{}, err
	}
	out := nativebackup.Status{Safety: []nativebackup.SafetyStatus{}}
	for _, ag := range m.agentRoots() {
		rec := state.Agents[ag.Name]
		_, isBlocked := blocked[ag.Name]
		out.Safety = append(out.Safety, safetyStatusFromRecord(ag, rec, isBlocked))
	}
	sort.Slice(out.Safety, func(i, j int) bool { return out.Safety[i].Agent < out.Safety[j].Agent })
	sched, err := loadNativeBackupSchedule(m.schedulePath())
	if err != nil {
		return nativebackup.Status{}, err
	}
	out.Schedule = sched
	retention, err := loadNativeBackupRetention(m.retentionPath())
	if err != nil {
		return nativebackup.Status{}, err
	}
	out.Retention = normalizeNativeBackupRetention(retention, m.agentRoots())
	return out, nil
}

func (m *nativeBackupManager) LoadSchedule() (nativebackup.ScheduleConfig, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return loadNativeBackupSchedule(m.schedulePath())
}

func (m *nativeBackupManager) SaveSchedule(cfg nativebackup.ScheduleConfig) (nativebackup.ScheduleConfig, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	existing, err := loadNativeBackupSchedule(m.schedulePath())
	if err != nil {
		return nativebackup.ScheduleConfig{}, err
	}
	cfg.LastRunAt = existing.LastRunAt
	if cfg.IntervalMinutes <= 0 {
		cfg.IntervalMinutes = 1440
	}
	if cfg.Destination == "" {
		cfg.Destination = "local"
	}
	sort.Strings(cfg.Agents)
	now := time.Now().UTC()
	if cfg.Enabled {
		cfg.NextRunAt = now.Add(time.Duration(cfg.IntervalMinutes) * time.Minute)
	} else {
		cfg.NextRunAt = time.Time{}
	}
	if err := writeNativeBackupSchedule(m.schedulePath(), cfg); err != nil {
		return nativebackup.ScheduleConfig{}, err
	}
	return cfg, nil
}

func (m *nativeBackupManager) LoadRetention() (nativebackup.RetentionConfig, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cfg, err := loadNativeBackupRetention(m.retentionPath())
	if err != nil {
		return nativebackup.RetentionConfig{}, err
	}
	return normalizeNativeBackupRetention(cfg, m.agentRoots()), nil
}

func (m *nativeBackupManager) SaveRetention(cfg nativebackup.RetentionConfig) (nativebackup.RetentionConfig, error) {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	m.mu.Lock()
	defer m.mu.Unlock()
	cfg = normalizeNativeBackupRetention(cfg, m.agentRoots())
	if err := writeNativeBackupRetention(m.retentionPath(), cfg); err != nil {
		return nativebackup.RetentionConfig{}, err
	}
	if err := m.pruneRetainedHistoryLocked(cfg); err != nil {
		return nativebackup.RetentionConfig{}, err
	}
	return cfg, nil
}

func (m *nativeBackupManager) RunScheduledIfDue(lg nativeBackupsLogger, creator func(kind string, agents []string, destination string) (nativebackup.BackupInfo, error)) {
	cfg, err := m.LoadSchedule()
	if err != nil {
		if lg != nil {
			lg.Warn("native backup schedule load failed", "err", err)
		}
		return
	}
	if !cfg.Enabled || cfg.IntervalMinutes <= 0 {
		return
	}
	now := time.Now().UTC()
	if cfg.NextRunAt.IsZero() {
		cfg.NextRunAt = now.Add(time.Duration(cfg.IntervalMinutes) * time.Minute)
		_ = writeNativeBackupSchedule(m.schedulePath(), cfg)
		return
	}
	if now.Before(cfg.NextRunAt) {
		return
	}
	destination := cfg.Destination
	if destination == "" {
		destination = "local"
	}
	create := creator
	if create == nil {
		create = func(kind string, agents []string, destination string) (nativebackup.BackupInfo, error) {
			if destination != "" && destination != "local" {
				return nativebackup.BackupInfo{}, fmt.Errorf("scheduled %s backup requires the local web/cloud accessor", destination)
			}
			return m.Create(kind, agents)
		}
	}
	if _, err := create("scheduled", cfg.Agents, destination); err != nil {
		if lg != nil {
			if errors.Is(err, errCloudPluginNotPaired) {
				// Expected steady state on an installed-but-unpaired device:
				// the pairing gate skipped the run before any staging I/O.
				// Not a failure — log quietly; the short retry below picks
				// the schedule back up promptly once the device is paired.
				lg.Info("scheduled cloud backup skipped: cloud plugin is not paired")
			} else {
				lg.Warn("scheduled native backup failed", "err", err)
			}
		}
		if errors.Is(err, errCloudBackupTooLarge) {
			// The current cloud transport uses one presigned PutObject and cannot
			// accept an object over 5 GiB. Retrying the same full snapshot every
			// five minutes creates a permanent CPU/disk storm and cannot succeed.
			// Keep the configured cadence so the job can recover naturally after
			// the selected roots shrink or multipart upload is introduced.
			cfg.NextRunAt = now.Add(time.Duration(cfg.IntervalMinutes) * time.Minute)
		} else {
			cfg.NextRunAt = now.Add(5 * time.Minute)
		}
	} else {
		cfg.LastRunAt = now
		cfg.NextRunAt = now.Add(time.Duration(cfg.IntervalMinutes) * time.Minute)
	}
	_ = writeNativeBackupSchedule(m.schedulePath(), cfg)
}

func (m *nativeBackupManager) safetyPath() string {
	return filepath.Join(m.backupsRoot, ".safety.json")
}

func (m *nativeBackupManager) schedulePath() string {
	return filepath.Join(m.backupsRoot, ".schedule.json")
}

func (m *nativeBackupManager) retentionPath() string {
	return filepath.Join(m.backupsRoot, ".retention.json")
}

func (m *nativeBackupManager) cloudKeyPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		// Callers will fail closed while opening this deliberately invalid
		// absolute path; never fall back to the legacy backup-adjacent key.
		return filepath.Join(string(filepath.Separator), ".aplexica-home-unavailable", "native-cloud-keyring-v2.cbor")
	}
	return filepath.Join(home, ".aplexica", "keys", "native-cloud-keyring-v2.cbor")
}

func (m *nativeBackupManager) migrateLegacyCloudKey() error {
	legacy := filepath.Join(m.backupsRoot, ".cloud", "backup.key")
	return nativebackup.MigrateLegacyCloudBackupKey(legacy, m.cloudKeyPath())
}

func (m *nativeBackupManager) cloudStagingRoot() string {
	return filepath.Join(m.backupsRoot, ".cloud-staging")
}

// cloudStagingRetain is zero because every object under .cloud-staging is a
// disposable working copy. Retaining even one interrupted snapshot can keep
// several gigabytes indefinitely; logs and job status carry the useful
// post-mortem information instead.
const cloudStagingRetain = 0

// SweepCloudStaging reclaims orphaned cloud-staging directories and transient
// archive files. It is called
// once at daemon startup (in a goroutine — see cmd_daemon) so installs that
// accumulated staged snapshots from an interrupted or pre-fix run (the
// .cloud-staging disk leak) release that disk on upgrade.
//
// The removal set is snapshotted under m.mu/opMu, then the (potentially
// many-GB, I/O-bound) deletion runs without either lock. cloudMu is the
// complete cloud-job lifetime lease, so no production cloud backup can
// allocate a staging path after enumeration. Releasing opMu before deletion is
// essential: startup safety and local backups must not wait while a pre-fix
// 100+ GB leak is reclaimed. Best-effort: failures are logged, never fatal.
func (m *nativeBackupManager) SweepCloudStaging(lg nativeBackupsLogger) {
	// cloudMu spans the entire stage -> archive -> upload lifetime in
	// createCloudBackup. Without it, a startup sweep or second scheduled run
	// could delete a staging tree after CreateCloudStagingContext released opMu
	// but while encryption/upload was still reading it.
	m.cloudMu.Lock()
	defer m.cloudMu.Unlock()
	m.opMu.Lock()
	m.mu.Lock()
	victims, err := m.cloudStagingVictimsLocked(cloudStagingRetain)
	m.mu.Unlock()
	m.opMu.Unlock()
	if err != nil {
		if lg != nil {
			lg.Warn("cloud-staging sweep: enumerate failed", "err", err)
		}
		return
	}
	if len(victims) == 0 {
		return
	}
	if lg != nil {
		lg.Info("cloud-staging sweep: reclaiming orphaned staged backups",
			"count", len(victims), "keep", cloudStagingRetain)
	}
	remove := m.removeCloudStaging
	if remove == nil {
		remove = removeCloudStaging
	}
	removed, rmErr := remove(victims)
	if lg != nil {
		if rmErr != nil {
			lg.Warn("cloud-staging sweep: some removals failed", "err", rmErr, "removed", removed)
		} else {
			lg.Info("cloud-staging sweep: reclaimed orphaned staged backups", "removed", removed)
		}
	}
}

// sweepCloudStagingLocked removes all but the newest `keep` snapshot-ID
// directories plus every orphan archive/metadata temp file under
// .cloud-staging. Callers must hold m.mu and m.opMu so no in-flight staging op
// races the removal. Used at the pre-allocation point in
// CreateCloudStagingContext, where the victim set is normally tiny; the startup
// path uses SweepCloudStaging so the delete happens off m.mu.
func (m *nativeBackupManager) sweepCloudStagingLocked(keep int) (int, error) {
	victims, err := m.cloudStagingVictimsLocked(keep)
	if err != nil {
		return 0, err
	}
	remove := m.removeCloudStaging
	if remove == nil {
		remove = removeCloudStaging
	}
	return remove(victims)
}

// cloudStagingVictimsLocked returns the absolute paths of staged snapshot
// directories to remove — all but the newest `keep`, newest determined by
// mod-time — plus every encrypted *.apxbk and *.metadata.json transient file.
// Those files are created beside the staged directory and normally removed by
// defer; after a crash no defer runs, so ignoring them caused a second
// unbounded staging leak. Enumeration only — does no deletion. Callers must
// hold m.mu and m.opMu.
func (m *nativeBackupManager) cloudStagingVictimsLocked(keep int) ([]string, error) {
	if keep < 0 {
		keep = 0
	}
	root := m.cloudStagingRoot()
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("sweep cloud staging: read %s: %w", root, err)
	}

	type stagedDir struct {
		name    string
		modTime time.Time
	}
	var staged []stagedDir
	var transient []string
	for _, de := range entries {
		if !de.IsDir() {
			if isCloudStagingTransient(de.Name()) {
				transient = append(transient, filepath.Join(root, de.Name()))
			}
			continue
		}
		if _, ok := nativebackup.SnapshotKindFromID(de.Name()); !ok {
			continue
		}
		var modTime time.Time
		if fi, statErr := de.Info(); statErr == nil {
			modTime = fi.ModTime()
		}
		staged = append(staged, stagedDir{name: de.Name(), modTime: modTime})
	}

	// Newest first so the most recent `keep` survive.
	sort.Slice(staged, func(i, j int) bool {
		if !staged[i].modTime.Equal(staged[j].modTime) {
			return staged[i].modTime.After(staged[j].modTime)
		}
		return staged[i].name > staged[j].name
	})

	start := keep
	if start > len(staged) {
		start = len(staged)
	}
	victims := make([]string, 0, len(transient)+len(staged)-start)
	victims = append(victims, transient...)
	for _, d := range staged[start:] {
		victims = append(victims, filepath.Join(root, d.name))
	}
	sort.Strings(victims)
	return victims, nil
}

func isCloudStagingTransient(name string) bool {
	return strings.HasSuffix(name, ".apxbk") ||
		strings.HasSuffix(name, ".metadata.json") ||
		strings.HasPrefix(name, ".aplexica-cloud-archive-")
}

// removeCloudStaging deletes the given staging paths and returns the count
// removed plus the first error. Slow (I/O over potentially many GB) — call
// WITHOUT m.mu held so reclaiming disk never blocks unrelated manager reads.
// The startup caller holds cloudMu (but deliberately not opMu) to exclude
// production staging allocation and upload. os.RemoveAll is idempotent, so a
// path already removed by a concurrent sweep is a harmless no-op.
func removeCloudStaging(victims []string) (int, error) {
	removed := 0
	var firstErr error
	for _, path := range victims {
		if err := os.RemoveAll(path); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("sweep cloud staging: remove %s: %w", filepath.Base(path), err)
			}
			continue
		}
		removed++
	}
	return removed, firstErr
}

func (m *nativeBackupManager) cloudDownloadsRoot() string {
	return filepath.Join(m.backupsRoot, ".cloud-downloads")
}

func (m *nativeBackupManager) recordSuccessfulSafetyBackup(agents []nativebackup.AgentRoots, backupID string, at time.Time) error {
	state, err := loadNativeBackupSafetyState(m.safetyPath())
	if err != nil {
		return err
	}
	if state.Agents == nil {
		state.Agents = map[string]nativeBackupSafetyRecord{}
	}
	for _, ag := range agents {
		state.Agents[ag.Name] = nativeBackupSafetyRecord{
			RootSignature: agentRootSignature(ag),
			BackupID:      backupID,
			LastBackupAt:  at,
		}
	}
	return writeNativeBackupSafetyState(m.safetyPath(), state)
}

func (m *nativeBackupManager) infoForBackupLocked(id string) (nativebackup.BackupInfo, error) {
	infos, err := nativebackup.List(m.backupsRoot)
	if err != nil {
		return nativebackup.BackupInfo{}, err
	}
	for _, info := range infos {
		if info.ID == id {
			return info, nil
		}
	}
	return nativebackup.BackupInfo{}, fmt.Errorf("no such backup %q under %s", id, m.backupsRoot)
}

func (m *nativeBackupManager) clearSafetyBackupReferenceLocked(backupID string) error {
	state, err := loadNativeBackupSafetyState(m.safetyPath())
	if err != nil {
		return err
	}
	changed := false
	for agent, rec := range state.Agents {
		if rec.BackupID != backupID {
			continue
		}
		rec.BackupID = ""
		rec.LastBackupAt = time.Time{}
		state.Agents[agent] = rec
		changed = true
	}
	if !changed {
		return nil
	}
	return writeNativeBackupSafetyState(m.safetyPath(), state)
}

func (m *nativeBackupManager) pruneRetainedHistoryLocked(cfg nativebackup.RetentionConfig) error {
	cfg = normalizeNativeBackupRetention(cfg, m.agentRoots())
	infos, err := nativebackup.List(m.backupsRoot)
	if err != nil {
		return err
	}
	seenByAgent := map[string]int{}
	for _, info := range infos {
		if info.Kind != "manual" && info.Kind != "scheduled" {
			continue
		}
		if len(info.Agents) == 0 {
			continue
		}
		keep := false
		for _, agent := range info.Agents {
			if seenByAgent[agent] < retentionLimitForAgent(cfg, agent) {
				keep = true
				break
			}
		}
		if keep {
			for _, agent := range info.Agents {
				seenByAgent[agent]++
			}
			continue
		}
		if err := os.RemoveAll(info.Path); err != nil {
			return fmt.Errorf("prune native backup %s: %w", info.ID, err)
		}
	}
	return nil
}

type authenticatedPreSyncCandidate struct {
	info     nativebackup.BackupInfo
	manifest nativebackup.Manifest
}

// preSyncSafetyCandidatesLocked enumerates candidate directories without the
// user-facing List helper. List intentionally accepts old/unreadable manifests
// and uses os.ReadFile so it can display them; startup is a trust boundary and
// must instead perform a bounded, no-follow manifest read before using any
// metadata. Authenticated candidates are ordered by signed CreatedAt. Genuine
// schema-0 candidates remain available through the compatibility map, but any
// authentication marker on an auth failure is an obvious downgrade and is not
// treated as legacy.
func (m *nativeBackupManager) preSyncSafetyCandidatesLocked() ([]authenticatedPreSyncCandidate, map[string]nativebackup.BackupInfo) {
	legacy := map[string]nativebackup.BackupInfo{}
	entries, err := os.ReadDir(m.backupsRoot)
	if err != nil {
		return nil, legacy
	}
	var authenticated []authenticatedPreSyncCandidate
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		id := entry.Name()
		kind, ok := nativebackup.SnapshotKindFromID(id)
		if !ok || kind != "pre-sync" {
			continue
		}
		path := filepath.Join(m.backupsRoot, id)
		man, authErr := nativebackup.AuthenticateSnapshotManifestContext(context.Background(), path, m.manifestKeyPath())
		if authErr == nil {
			authenticated = append(authenticated, authenticatedPreSyncCandidate{
				info: nativebackup.BackupInfo{
					ID: id, Path: path, Kind: kind, CreatedAt: man.CreatedAt,
					Agents: manifestAgentNames(man), TotalBytes: manifestTotalBytes(man),
					FileCount: manifestFileCount(man), Location: "local",
				},
				manifest: man,
			})
			continue
		}

		man, legacyErr := nativebackup.ReadSnapshotManifestContext(context.Background(), path)
		if legacyErr != nil || man.SchemaVersion != 0 || man.Auth != (nativebackup.ManifestAuth{}) {
			continue
		}
		info := nativebackup.BackupInfo{
			ID: id, Path: path, Kind: kind, CreatedAt: man.CreatedAt,
			Agents: manifestAgentNames(man), TotalBytes: manifestTotalBytes(man),
			FileCount: manifestFileCount(man), Location: "local",
		}
		for _, name := range info.Agents {
			current, exists := legacy[name]
			if !exists || info.CreatedAt.After(current.CreatedAt) || (info.CreatedAt.Equal(current.CreatedAt) && info.ID > current.ID) {
				legacy[name] = info
			}
		}
	}
	sort.Slice(authenticated, func(i, j int) bool {
		if !authenticated[i].info.CreatedAt.Equal(authenticated[j].info.CreatedAt) {
			return authenticated[i].info.CreatedAt.After(authenticated[j].info.CreatedAt)
		}
		return authenticated[i].info.ID > authenticated[j].info.ID
	})
	return authenticated, legacy
}

// newestValidAuthenticatedPreSyncLocked finds a completed pre-sync snapshot
// that can safely replace a missing/corrupt state reference. This is the
// recovery path for a daemon interruption after manifest commit but before an
// older all-agents state write: adopt the already-copied bytes instead of
// creating another full snapshot on the next start.
//
// Authentication, file presence/size, and safe relative paths are checked by
// the same full validator used for an existing safety reference. Source roots
// must also exactly match the agent's current root set; a valid snapshot of an
// older topology cannot satisfy today's safety gate.
func (m *nativeBackupManager) newestValidAuthenticatedPreSyncLocked(ag nativebackup.AgentRoots, candidates []authenticatedPreSyncCandidate) (nativebackup.BackupInfo, bool) {
	for _, candidate := range candidates {
		// Candidate enumeration already authenticated the bounded manifest; never
		// open multi-gigabyte payloads until signed agent and source-root metadata
		// are an exact match.
		if !manifestMatchesAgentRoots(candidate.manifest, ag) {
			continue
		}
		verified, err := nativebackup.VerifyAuthenticatedSnapshotContext(context.Background(), candidate.info.Path, m.manifestKeyPath())
		if err != nil || !manifestMatchesAgentRoots(verified, ag) {
			continue
		}
		info := candidate.info
		// The full verifier deliberately re-reads the manifest. Use its timestamp
		// rather than carrying metadata from a preflight object that may have been
		// replaced between classification and verification.
		info.CreatedAt = verified.CreatedAt
		return info, true
	}
	return nativebackup.BackupInfo{}, false
}

func manifestAgentNames(man nativebackup.Manifest) []string {
	out := make([]string, 0, len(man.Agents))
	for _, agent := range man.Agents {
		out = append(out, agent.Name)
	}
	return out
}

func manifestContainsAgent(man nativebackup.Manifest, agent string) bool {
	for _, entry := range man.Agents {
		if entry.Name == agent {
			return true
		}
	}
	return false
}

func manifestMatchesAgentRoots(man nativebackup.Manifest, ag nativebackup.AgentRoots) bool {
	want := make([]string, 0, len(ag.Roots))
	for _, root := range ag.Roots {
		absolute, err := filepath.Abs(root)
		if err != nil {
			return false
		}
		want = append(want, filepath.Clean(absolute))
	}
	sort.Strings(want)
	for _, entry := range man.Agents {
		if entry.Name != ag.Name {
			continue
		}
		got := append([]string{}, entry.SourceRoots...)
		for i := range got {
			got[i] = filepath.Clean(got[i])
		}
		sort.Strings(got)
		if len(got) != len(want) {
			continue
		}
		matches := true
		for i := range want {
			if got[i] != want[i] {
				matches = false
				break
			}
		}
		if matches {
			return true
		}
	}
	return false
}

func (m *nativeBackupManager) allocateBackupDestination(prefix string, selected []nativebackup.AgentRoots, at time.Time) (string, string, error) {
	return allocateBackupDestinationIn(m.backupsRoot, prefix, selected, at)
}

func allocateBackupDestinationIn(root, prefix string, selected []nativebackup.AgentRoots, at time.Time) (string, string, error) {
	base := prefix + at.Format("2006-01-02T15-04-05Z")
	if len(selected) == 1 {
		base = prefix + safeBackupIDSegment(selected[0].Name) + "-" + at.Format("2006-01-02T15-04-05Z")
	}
	for i := 0; i < 100; i++ {
		id := base
		if i > 0 {
			id = fmt.Sprintf("%s-%02d", base, i+1)
		}
		dest := filepath.Join(root, id)
		if _, err := os.Stat(dest); os.IsNotExist(err) {
			return id, dest, nil
		} else if err != nil {
			return "", "", err
		}
	}
	return "", "", fmt.Errorf("could not allocate unique native backup id under %s", root)
}

func manifestTotalBytes(man nativebackup.Manifest) int64 {
	var total int64
	for _, ag := range man.Agents {
		for _, fe := range ag.Roots {
			total += fe.Bytes
		}
	}
	return total
}

func manifestFileCount(man nativebackup.Manifest) int {
	var total int
	for _, ag := range man.Agents {
		total += len(ag.Roots)
	}
	return total
}

func selectAgentRoots(all []nativebackup.AgentRoots, names []string) ([]nativebackup.AgentRoots, error) {
	if len(names) == 0 {
		if len(all) == 0 {
			return nil, fmt.Errorf("no agents with backup roots")
		}
		return all, nil
	}
	want := map[string]struct{}{}
	for _, name := range names {
		if name != "" {
			want[name] = struct{}{}
		}
	}
	var out []nativebackup.AgentRoots
	for _, ag := range all {
		if _, ok := want[ag.Name]; ok {
			out = append(out, ag)
			delete(want, ag.Name)
		}
	}
	if len(want) > 0 {
		missing := make([]string, 0, len(want))
		for name := range want {
			missing = append(missing, name)
		}
		sort.Strings(missing)
		return nil, fmt.Errorf("unknown or rootless agents: %s", strings.Join(missing, ", "))
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no agents selected")
	}
	return out, nil
}

func findAgentRoots(all []nativebackup.AgentRoots, name string) (nativebackup.AgentRoots, bool) {
	for _, ag := range all {
		if ag.Name == name {
			return ag, true
		}
	}
	return nativebackup.AgentRoots{}, false
}

func backupPrefixForKind(kind string) (string, error) {
	switch kind {
	case "", "manual":
		return nativebackup.ManualPrefix, nil
	case "scheduled":
		return nativebackup.ScheduledPrefix, nil
	case "pre-sync":
		return nativebackup.SnapshotPrefix, nil
	default:
		return "", fmt.Errorf("unsupported backup kind %q", kind)
	}
}

func agentNames(agents []nativebackup.AgentRoots) []string {
	out := make([]string, 0, len(agents))
	for _, ag := range agents {
		out = append(out, ag.Name)
	}
	sort.Strings(out)
	return out
}

func safeBackupIDSegment(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-_")
	if out == "" {
		return "agent"
	}
	return out
}

func agentRootSignature(ag nativebackup.AgentRoots) string {
	roots := append([]string{}, ag.Roots...)
	sort.Strings(roots)
	h := sha256.New()
	_, _ = h.Write([]byte(ag.Name))
	for _, root := range roots {
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(root))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func loadNativeBackupSafetyState(path string) (nativeBackupSafetyState, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nativeBackupSafetyState{Agents: map[string]nativeBackupSafetyRecord{}}, nil
		}
		return nativeBackupSafetyState{}, err
	}
	var out nativeBackupSafetyState
	if err := json.Unmarshal(data, &out); err != nil {
		return nativeBackupSafetyState{}, err
	}
	if out.Agents == nil {
		out.Agents = map[string]nativeBackupSafetyRecord{}
	}
	return out, nil
}

func writeNativeBackupSafetyState(path string, state nativeBackupSafetyState) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return atomicfile.WriteFile(path, data, 0o600)
}

func loadNativeBackupSchedule(path string) (nativebackup.ScheduleConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nativebackup.ScheduleConfig{IntervalMinutes: 1440}, nil
		}
		return nativebackup.ScheduleConfig{}, err
	}
	var out nativebackup.ScheduleConfig
	if err := json.Unmarshal(data, &out); err != nil {
		return nativebackup.ScheduleConfig{}, err
	}
	if out.IntervalMinutes <= 0 {
		out.IntervalMinutes = 1440
	}
	if out.Destination == "" {
		out.Destination = "local"
	}
	sort.Strings(out.Agents)
	return out, nil
}

func writeNativeBackupSchedule(path string, cfg nativebackup.ScheduleConfig) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o600)
}

func loadNativeBackupRetention(path string) (nativebackup.RetentionConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nativebackup.RetentionConfig{PerAgent: map[string]int{}}, nil
		}
		return nativebackup.RetentionConfig{}, err
	}
	var out nativebackup.RetentionConfig
	if err := json.Unmarshal(data, &out); err != nil {
		return nativebackup.RetentionConfig{}, err
	}
	if out.PerAgent == nil {
		out.PerAgent = map[string]int{}
	}
	return out, nil
}

func writeNativeBackupRetention(path string, cfg nativebackup.RetentionConfig) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o600)
}

func normalizeNativeBackupRetention(cfg nativebackup.RetentionConfig, agents []nativebackup.AgentRoots) nativebackup.RetentionConfig {
	out := nativebackup.RetentionConfig{PerAgent: map[string]int{}}
	for _, ag := range agents {
		limit := cfg.PerAgent[ag.Name]
		if limit <= 0 {
			limit = nativebackup.DefaultRetentionPerAgent
		}
		out.PerAgent[ag.Name] = limit
	}
	return out
}

func retentionLimitForAgent(cfg nativebackup.RetentionConfig, agent string) int {
	if cfg.PerAgent != nil {
		if limit := cfg.PerAgent[agent]; limit > 0 {
			return limit
		}
	}
	return nativebackup.DefaultRetentionPerAgent
}

func safetyStatusFromRecord(ag nativebackup.AgentRoots, rec nativeBackupSafetyRecord, blocked bool) nativebackup.SafetyStatus {
	sig := agentRootSignature(ag)
	state := "backup_required"
	if rec.RootSignature == sig && rec.BackupID != "" {
		state = "protected"
	}
	if rec.RootSignature == sig && rec.Override {
		state = "overridden"
	}
	if blocked {
		state = "blocked"
	}
	return nativebackup.SafetyStatus{
		Agent:         ag.Name,
		State:         state,
		Roots:         append([]string{}, ag.Roots...),
		RootSignature: sig,
		BackupID:      rec.BackupID,
		LastBackupAt:  rec.LastBackupAt,
		LastError:     rec.LastError,
		LastFailureAt: rec.LastFailureAt,
		Override:      rec.Override && rec.RootSignature == sig,
		OverrideAt:    rec.OverrideAt,
		Blocked:       blocked,
	}
}

// writeInitialDoneMarker creates the backups dir if needed and writes
// the .initial-done sentinel with the current UTC timestamp as its
// (informational) contents.
func writeInitialDoneMarker(marker string) error {
	if err := os.MkdirAll(filepath.Dir(marker), 0o700); err != nil {
		return fmt.Errorf("create backups dir: %w", err)
	}
	stamp := time.Now().UTC().Format(time.RFC3339Nano) + "\n"
	return os.WriteFile(marker, []byte(stamp), 0o600)
}

// resolveBackupDir maps a backup ID (the snapshot directory base name,
// as returned by List) to its absolute path under backupsRoot, and
// verifies the directory exists. A non-existent ID is an error so the
// CLI/API surface a clear "no such backup" rather than a confusing
// downstream manifest-read failure.
func resolveBackupDir(backupsRoot, id string) (string, error) {
	if id == "" {
		return "", fmt.Errorf("backup id is required")
	}
	// Guard against path traversal: the ID must be a plain directory
	// name, not a path. filepath.Base collapses any separators.
	if filepath.Base(id) != id {
		return "", fmt.Errorf("invalid backup id %q", id)
	}
	dir := filepath.Join(backupsRoot, id)
	info, err := os.Stat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("no such backup %q under %s", id, backupsRoot)
		}
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("backup %q is not a directory", id)
	}
	return dir, nil
}

// latestPreSyncID returns the ID of the newest pre-sync-* snapshot, or
// an error if none exist. Used as the CLI's default --from.
func latestPreSyncID(backupsRoot string) (string, error) {
	infos, err := nativebackup.List(backupsRoot)
	if err != nil {
		return "", err
	}
	for _, bi := range infos { // List is newest-first
		if bi.Kind == "pre-sync" {
			return bi.ID, nil
		}
	}
	return "", fmt.Errorf("no pre-sync snapshots found under %s", backupsRoot)
}
