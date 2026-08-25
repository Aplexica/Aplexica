//go:build tray

package main

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"fyne.io/systray"

	"github.com/aplexica/aplexica/internal/daemon"
	"github.com/aplexica/aplexica/internal/i18n"
	"github.com/aplexica/aplexica/internal/pausestate"
	"github.com/aplexica/aplexica/internal/safepath"
)

// conflictSlotCount is the number of pre-allocated per-conflict submenu
// items the tray reserves at OnReady time. systray's API doesn't
// support removing menu items at runtime; the established pattern is
// pre-allocate + Show/Hide. 16 visible slots + 1 overflow slot is
// comfortable for realistic conflict counts (typical: 0–3); the
// overflow item points users at `aplexica conflicts list` when more
// than 16 artifacts diverge concurrently (rare).
const conflictSlotCount = 16

// pendingProjectSlotCount mirrors conflictSlotCount for the
// "Pending projects (N) →" submenu (v0.58.0; BRD-02 §4.13). 16 is
// comfortable for realistic counts (typical: 0–5 when a user has
// just restored a bundle on a fresh device); overflow points at
// `aplexica pending list` for the full table.
const pendingProjectSlotCount = 16

// adapterSlotCount is the pre-allocated slot count for the
// "Adapters →" and "⚠ Adapter errors →" submenus (v0.64.0;
// ADR-0159 Cand B + D surfaces). V1 has 5 adapters (claude-code,
// codex, hermes, openclaw, kilo); 16 leaves comfortable headroom
// for the V2 adapter expansion (aider, cursor, cline, copilot,
// continue, goose) without churning the slot allocation.
const adapterSlotCount = 16

// tray owns the systray UI state machine. A single instance lives in
// main. All systray.Set*/AddMenuItem calls flow through methods on tray
// under tray.mu — the upstream lib documents that mutations aren't safe
// to call from arbitrary goroutines on every backend.
type tray struct {
	aplexicaPath string

	mu         sync.Mutex
	cur        TrayState
	lastSnap   StatusSnapshot
	lastActive time.Time
	watchedDir string // remembered for resume-after-pause

	miStatus           *systray.MenuItem
	miConflicts        *systray.MenuItem
	miConflictKids     []*systray.MenuItem // conflictSlotCount visible slots, pre-allocated under miConflicts
	miConflictMore     *systray.MenuItem   // overflow slot — only shown when conflicts > conflictSlotCount
	miPendingProjects  *systray.MenuItem   // v0.58.0: parent of the Pending-projects submenu (BRD-02 §4.13)
	miPendingKids      []*systray.MenuItem // pendingProjectSlotCount visible slots
	miPendingMore      *systray.MenuItem   // overflow — points at `aplexica pending list`
	miAdapters         *systray.MenuItem   // v0.64.0: parent of the Adapters submenu (ADR-0159 Cand B)
	miAdapterKids      []*systray.MenuItem // adapterSlotCount visible slots, pre-allocated
	miAdapterErrors    *systray.MenuItem   // v0.64.0: parent of the Adapter errors submenu (ADR-0159 Cand D)
	miAdapterErrorKids []*systray.MenuItem // adapterSlotCount visible slots, pre-allocated
	miOpenWeb          *systray.MenuItem   // v0.107.0: "Open Aplexica" — launches the local web UI
	miOpenDir          *systray.MenuItem
	miPauseResume      *systray.MenuItem
	miDaemonControl    *systray.MenuItem
	miPauseFor         *systray.MenuItem // v0.49.0: parent of the timed-pause submenu
	miPauseFor1h       *systray.MenuItem // v0.49.0: pause for 1 hour (auto-resume after)
	miPauseFor4h       *systray.MenuItem // v0.49.0: pause for 4 hours (auto-resume after)
	miPauseForRestart  *systray.MenuItem // v0.49.0: pause until next manual resume (same as Pause sync toggle)
	miPauseForCustom   *systray.MenuItem // v0.49.0: pause for a user-entered duration
	miOpenLogs         *systray.MenuItem
	miOpenStatusReport *systray.MenuItem // v0.48.0: BRD-03 §4.9.3 — opens `aplexica status` in a terminal
	miOpenConfig       *systray.MenuItem // v0.48.0: BRD-03 §4.9.3 — opens ~/.aplexica/config.toml in editor
	miAbout            *systray.MenuItem
	miQuit             *systray.MenuItem
}

func newTray(aplexicaPath string) *tray {
	return &tray{aplexicaPath: aplexicaPath, cur: StateError}
}

// conflictSlotLabel produces the human-readable submenu label for one
// conflict: truncated artifact ID + " (" + kind + ")". Kept short so
// it fits in the platform-native menu width across systems.
func conflictSlotLabel(c Conflict) string {
	id := c.ArtifactID
	const max = 40
	if len(id) > max {
		id = id[:max] + "…"
	}
	if c.Kind != "" {
		return id + "  (" + c.Kind + ")"
	}
	return id
}

// onReady builds the menu and starts the click-dispatch goroutine.
// systray invokes the OnReady callback once after the platform-native
// event loop is running.
func (t *tray) onReady(logDir string) {
	systray.SetIcon(regularIconFor(StateError))
	systray.SetTitle("")
	systray.SetTooltip(i18n.T("tray_tooltip_starting"))

	t.miOpenWeb = systray.AddMenuItem(
		i18n.T("tray_menu_open_web"),
		i18n.T("tray_tooltip_open_web"))
	systray.AddSeparator()

	t.miStatus = systray.AddMenuItem(i18n.T("tray_menu_status_starting"), "")
	t.miStatus.Disable()
	systray.AddSeparator()

	t.miConflicts = systray.AddMenuItem(
		i18n.Tf("tray_menu_conflicts_count", 0),
		i18n.T("tray_tooltip_conflicts"))
	t.miConflicts.Disable()
	t.miConflictKids = make([]*systray.MenuItem, conflictSlotCount)
	for i := 0; i < conflictSlotCount; i++ {
		child := t.miConflicts.AddSubMenuItem("", i18n.T("tray_tooltip_conflict_slot"))
		child.Hide()
		t.miConflictKids[i] = child
	}
	t.miConflictMore = t.miConflicts.AddSubMenuItem("", i18n.T("tray_tooltip_conflicts_more"))
	t.miConflictMore.Hide()

	// v0.58.0: BRD-02 §4.13 "Pending projects (N) →" submenu.
	t.miPendingProjects = systray.AddMenuItem(
		i18n.Tf("tray_menu_pending_projects_count", 0),
		i18n.T("tray_tooltip_pending_projects"))
	t.miPendingProjects.Disable()
	t.miPendingKids = make([]*systray.MenuItem, pendingProjectSlotCount)
	for i := 0; i < pendingProjectSlotCount; i++ {
		child := t.miPendingProjects.AddSubMenuItem("",
			i18n.T("tray_tooltip_pending_project_slot"))
		child.Hide()
		t.miPendingKids[i] = child
	}
	t.miPendingMore = t.miPendingProjects.AddSubMenuItem("",
		i18n.T("tray_tooltip_pending_more"))
	t.miPendingMore.Hide()

	t.miOpenDir = systray.AddMenuItem(i18n.T("tray_menu_open_watched_dir"), i18n.T("tray_tooltip_open_watched"))
	t.miPauseResume = systray.AddMenuItem(i18n.T("tray_menu_pause"), i18n.T("tray_tooltip_pause"))
	t.miDaemonControl = systray.AddMenuItem(i18n.T("tray_menu_daemon_stop"), i18n.T("tray_tooltip_daemon_control"))
	// v0.49.0: "Pause sync for…" submenu (BRD-03 §4.9.3).
	t.miPauseFor = systray.AddMenuItem(
		i18n.T("tray_menu_pause_for"), i18n.T("tray_tooltip_pause_for"))
	t.miPauseFor1h = t.miPauseFor.AddSubMenuItem(
		i18n.T("tray_menu_pause_for_1h"), "")
	t.miPauseFor4h = t.miPauseFor.AddSubMenuItem(
		i18n.T("tray_menu_pause_for_4h"), "")
	t.miPauseForRestart = t.miPauseFor.AddSubMenuItem(
		i18n.T("tray_menu_pause_for_restart"), "")
	t.miPauseForCustom = t.miPauseFor.AddSubMenuItem(
		i18n.T("tray_menu_pause_for_custom"), "")
	t.miOpenLogs = systray.AddMenuItem(i18n.T("tray_menu_open_logs"), i18n.T("tray_tooltip_open_logs"))
	t.miOpenStatusReport = systray.AddMenuItem(
		i18n.T("tray_menu_open_status_report"), i18n.T("tray_tooltip_open_status_report"))
	t.miOpenConfig = systray.AddMenuItem(
		i18n.T("tray_menu_open_config"), i18n.T("tray_tooltip_open_config"))
	systray.AddSeparator()
	t.miAbout = systray.AddMenuItem(i18n.T("tray_menu_about"), "")
	t.miQuit = systray.AddMenuItem(i18n.T("tray_menu_quit"), i18n.T("tray_tooltip_quit"))

	go t.clickLoop(logDir)
}

// clickLoop dispatches menu-item click events. Each MenuItem.ClickedCh
// is its own channel — we fan-in via select. Exits when the user picks
// Quit (which closes the systray event loop).
//
// Per-conflict child slots get their own goroutine each (the static
// select size limit in Go would otherwise cap us; spawning N small
// goroutines is the idiomatic systray pattern). Each handler reads
// the CURRENT conflict at its index from t.lastSnap.Conflicts under
// t.mu — when the index is out of range the slot is hidden and the
// handler never fires.
func (t *tray) clickLoop(logDir string) {
	for i := 0; i < conflictSlotCount; i++ {
		idx := i
		go func() {
			for range t.miConflictKids[idx].ClickedCh {
				t.openConflictAt(idx)
			}
		}()
	}
	go func() {
		for range t.miConflictMore.ClickedCh {
			t.openConflictsList()
		}
	}()
	// v0.58.0: per-pending-project click handlers (same pre-
	// allocated-slot pattern as the conflicts submenu).
	for i := 0; i < pendingProjectSlotCount; i++ {
		idx := i
		go func() {
			for range t.miPendingKids[idx].ClickedCh {
				t.openPendingProjectAt(idx)
			}
		}()
	}
	go func() {
		for range t.miPendingMore.ClickedCh {
			t.openPendingList()
		}
	}()

	for {
		select {
		case <-t.miConflicts.ClickedCh:
			// The parent label itself was clicked (vs. a child). With
			// child slots populated, this is rarely how users navigate
			// — but if they do, fall back to opening `aplexica
			// conflicts list` for the full set.
			t.openConflictsList()
		case <-t.miOpenWeb.ClickedCh:
			t.openWebUI()
		case <-t.miOpenDir.ClickedCh:
			t.mu.Lock()
			dir := t.watchedDir
			t.mu.Unlock()
			if dir == "" {
				log.Printf("open dir: no watched dir known yet")
				continue
			}
			if err := openPath(dir); err != nil {
				log.Printf("open dir: %v", err)
			}
		case <-t.miPauseResume.ClickedCh:
			t.togglePauseResume()
		case <-t.miDaemonControl.ClickedCh:
			t.toggleDaemonControl()
		case <-t.miPauseFor1h.ClickedCh:
			t.pauseForDuration(1 * time.Hour)
		case <-t.miPauseFor4h.ClickedCh:
			t.pauseForDuration(4 * time.Hour)
		case <-t.miPauseForRestart.ClickedCh:
			t.doSyncPause(0)
		case <-t.miPauseForCustom.ClickedCh:
			d, err := askDuration(i18n.T("tray_prompt_custom_pause"), "30m")
			if err != nil {
				if err != errPromptCancelled {
					log.Printf("custom pause: %v", err)
				}
				continue
			}
			t.pauseForDuration(d)
		case <-t.miOpenLogs.ClickedCh:
			if err := openPath(logDir); err != nil {
				log.Printf("open logs: %v", err)
			}
		case <-t.miOpenStatusReport.ClickedCh:
			t.openStatusReport()
		case <-t.miOpenConfig.ClickedCh:
			t.openConfig()
		case <-t.miAbout.ClickedCh:
			t.showAbout()
		case <-t.miQuit.ClickedCh:
			// Record as well as log: systray.Quit() drives main's onExit,
			// which cancels the context, so the run goroutine's ctx.Done()
			// branch fires right behind us and must name the same cause
			// rather than reporting an unattributed cancel.
			shutdownSource.record(reasonUserQuit)
			logShutdown(reasonUserQuit)
			systray.Quit()
			return
		}
	}
}

// openConflictAt opens `aplexica conflicts show <id>` in a new terminal
// for the conflict at index idx in the most-recent snapshot. Bounds-
// safe: silently no-ops when the index is out of range (the slot
// should have been hidden in that case).
func (t *tray) openConflictAt(idx int) {
	t.mu.Lock()
	conflicts := t.lastSnap.Conflicts
	t.mu.Unlock()
	if idx < 0 || idx >= len(conflicts) {
		return
	}
	id := conflicts[idx].ArtifactID
	if err := safepath.ValidateStoreComponent(id); err != nil {
		log.Printf("refusing terminal open for unsafe conflict identifier")
		return
	}
	if err := openTerminalRun(t.aplexicaPath, "conflicts", "show", id); err != nil {
		log.Printf("open terminal for conflict %s: %v", id, err)
	}
}

// openConflictsList opens `aplexica conflicts list` in a new terminal —
// used by the overflow slot when more than conflictSlotCount conflicts
// exist, and as a fallback when the user clicks the parent header.
func (t *tray) openConflictsList() {
	if err := openTerminalRun(t.aplexicaPath, "conflicts", "list"); err != nil {
		log.Printf("open terminal for conflicts list: %v", err)
	}
}

// openPendingProjectAt opens a new terminal pre-typed with the
// `aplexica project link <id> <path>` command for the pending project
// at index idx in the most-recent snapshot. v0.58.0; BRD-02 §4.13.
// Bounds-safe: silently no-ops when idx is out of range (the slot
// should have been hidden in that case).
func (t *tray) openPendingProjectAt(idx int) {
	t.mu.Lock()
	projects := t.lastSnap.DaemonInfo
	t.mu.Unlock()
	if projects == nil || idx < 0 || idx >= len(projects.PendingProjects) {
		return
	}
	p := projects.PendingProjects[idx]
	id, _ := p["id"].(string)
	if id == "" {
		return
	}
	// A pending-project ID can originate from a bundle authored on another
	// device or a cloned repo's remote URL, so it is attacker-influenceable.
	// Refuse to interpolate one carrying shell metacharacters into a
	// terminal command (command-injection defense).
	if !safeProjectID(id) {
		log.Printf("refusing terminal open for unsafe pending-project id %q", id)
		return
	}
	// We can't auto-resolve the target path — the user has to point us at
	// where the project is cloned. Open a terminal with the command
	// pre-typed so they only have to fill in the path.
	if err := openTerminalRun(t.aplexicaPath, "project", "link", id, "<local-path>"); err != nil {
		log.Printf("open terminal for pending project %s: %v", id, err)
	}
}

// openPendingList opens `aplexica pending list` in a new terminal —
// used by the overflow slot when more than pendingProjectSlotCount
// projects exist, and as a fallback when the user clicks the parent
// header. v0.58.0.
func (t *tray) openPendingList() {
	if err := openTerminalRun(t.aplexicaPath, "pending", "list"); err != nil {
		log.Printf("open terminal for pending list: %v", err)
	}
}

// openStatusReport opens `aplexica status` (one-shot, human-readable)
// in a new terminal. v0.48.0; BRD-03 §4.9.3 "Open status report" menu
// item. Includes the state-dir + conflicts-root forwarding so the
// status report matches what the tray itself is watching.
func (t *tray) openStatusReport() {
	args := []string{t.aplexicaPath, "status"}
	if *flagStateDir != "" {
		args = append(args, "--state-dir", *flagStateDir)
	}
	if *flagConflictsRoot != "" {
		args = append(args, "--conflicts-root", *flagConflictsRoot)
	}
	if err := openTerminalRun(args...); err != nil {
		log.Printf("open status report: %v", err)
	}
}

// openConfig opens ~/.aplexica/config.toml in the user's default
// editor. v0.48.0; BRD-03 §4.9.3 "Open config" menu item.
//
// macOS / Linux : openPath shells to `open` / `xdg-open`, which routes
//
//	to the user's TOML-associated editor (TextEdit /
//	GNOME Text Editor / KDE Kate / etc.).
//
// Windows       : openPath uses `explorer`, which opens the file with
//
//	its default associated application — typically
//	Notepad for .toml.
//
// Missing config file is OK: the daemon writes it lazily, and even an
// empty file path is valid for openPath (the editor will create the
// file on save). Best-effort throughout.
func (t *tray) openConfig() {
	home, err := os.UserHomeDir()
	if err != nil {
		log.Printf("open config: cannot resolve home dir: %v", err)
		return
	}
	p := filepath.Join(home, ".aplexica", "config.toml")
	if err := openPath(p); err != nil {
		log.Printf("open config: %v", err)
	}
}

func (t *tray) togglePauseResume() {
	if t.syncPaused() {
		t.doSyncResume()
		return
	}
	t.doSyncPause(0)
}

func (t *tray) toggleDaemonControl() {
	t.mu.Lock()
	daemonUp := t.lastSnap.DaemonAvailable
	t.mu.Unlock()
	if daemonUp {
		if t.doDaemonStop() {
			t.apply(StatusSnapshot{Timestamp: time.Now()}, time.Now(), *flagActiveWindow, *flagPausedThreshold)
		}
		return
	}
	t.doDaemonStart()
}

func daemonControlArgs(command string, watchedDir string) []string {
	args := []string{"daemon"}
	if *flagStateDir != "" {
		args = append(args, "--state-dir", *flagStateDir)
	}
	if command == "start" && *flagLogDir != "" {
		args = append(args, "--log-dir", *flagLogDir)
	}
	args = append(args, command)
	if command == "start" && watchedDir != "" {
		args = append(args, "--dir", watchedDir)
	}
	return args
}

// doDaemonStop runs `aplexica daemon stop` synchronously, logging the outcome.
func (t *tray) doDaemonStop() bool {
	out, err := runAplexica(t.aplexicaPath, daemonControlArgs("stop", "")...)
	if err != nil {
		log.Printf("daemon stop failed: %v: %s", err, out)
		return false
	}
	log.Printf("daemon stop ok: %s", out)
	return true
}

// daemonStartDir resolves the directory passed to `aplexica daemon start`.
// Prefer the last live daemon snapshot, then fall back to config.json for
// installs that persist the watched dir there. Older installs did not persist
// it, so finally use the same home-directory default as `daemon start`.
func (t *tray) daemonStartDir() string {
	t.mu.Lock()
	dir := t.watchedDir
	t.mu.Unlock()
	if dir != "" {
		return dir
	}
	stateDir, ok := effectiveStateDir()
	if ok {
		cfg, err := daemon.LoadConfig(filepath.Join(stateDir, "config.json"))
		if err != nil {
			return ""
		}
		if cfg != nil && cfg.Dir != "" {
			return cfg.Dir
		}
	}
	home, err := os.UserHomeDir()
	if err == nil {
		return home
	}
	return ""
}

// doDaemonStart runs `aplexica daemon start --dir <last-known>`.
func (t *tray) doDaemonStart() bool {
	dir := t.daemonStartDir()
	if dir == "" {
		log.Printf("daemon start: no previously-watched dir known; cannot daemon start")
		_ = showInfoDialog(i18n.T("tray_menu_daemon_start"), i18n.T("tray_daemon_start_missing_dir"))
		return false
	}
	out, err := runAplexica(t.aplexicaPath, daemonControlArgs("start", dir)...)
	if err != nil {
		log.Printf("daemon start failed: %v: %s", err, out)
		return false
	}
	log.Printf("daemon start ok: %s", out)
	return true
}

func (t *tray) pauseStore() (*pausestate.Store, bool) {
	stateDir, ok := effectiveStateDir()
	if !ok {
		log.Printf("sync pause: cannot resolve state dir")
		return nil, false
	}
	return &pausestate.Store{Path: pausestate.DefaultPath(stateDir)}, true
}

func (t *tray) syncPaused() bool {
	store, ok := t.pauseStore()
	if !ok {
		return false
	}
	paused, _ := store.IsPaused("", time.Now().UTC())
	return paused
}

func (t *tray) doSyncPause(d time.Duration) {
	store, ok := t.pauseStore()
	if !ok {
		return
	}
	if err := store.PauseGlobal(d); err != nil {
		log.Printf("sync pause failed: %v", err)
		return
	}
	if d > 0 {
		log.Printf("sync pause ok: auto-resume after %v", d)
	} else {
		log.Printf("sync pause ok")
	}
}

func (t *tray) doSyncResume() {
	store, ok := t.pauseStore()
	if !ok {
		return
	}
	if err := store.ResumeGlobal(); err != nil {
		log.Printf("sync resume failed: %v", err)
		return
	}
	log.Printf("sync resume ok")
}

// pauseForDuration persists a global sync pause that auto-expires after
// d elapses. The daemon and tray both consult the same pause-state file,
// so no tray-owned resume goroutine is needed.
func (t *tray) pauseForDuration(d time.Duration) {
	t.doSyncPause(d)
}

func (t *tray) showAbout() {
	t.mu.Lock()
	v := ""
	if t.lastSnap.DaemonInfo != nil {
		v = t.lastSnap.DaemonInfo.Version
	}
	t.mu.Unlock()
	body := i18n.Tf("tray_about_body", v)
	if err := showInfoDialog(i18n.T("tray_about_title"), body); err != nil {
		log.Printf("about dialog: %v — falling back to stderr", err)
		log.Printf("%s", body)
	}
}

// apply is called from the run goroutine on every new snapshot OR on
// the watchdog tick. Holds t.mu for the entire repaint so systray
// callbacks stay serialized.
func (t *tray) apply(s StatusSnapshot, now time.Time, activeWindow, pausedThreshold time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()

	// Activity heuristic: snapshots arriving while the daemon stays up
	// counts as "the daemon is alive and doing its job." Conservative
	// enough that long-quiet periods correctly fall through to Paused.
	if s.DaemonAvailable && t.lastSnap.DaemonAvailable {
		t.lastActive = now
	}
	t.lastSnap = s
	if s.DaemonInfo != nil && s.DaemonInfo.WatchedDir != "" {
		t.watchedDir = s.DaemonInfo.WatchedDir
	}
	syncPaused := t.syncPaused()
	if s.DaemonInfo != nil {
		s.DaemonInfo.Paused = syncPaused
	}

	state := deriveState(s, t.lastActive, now, activeWindow, pausedThreshold)
	if state != t.cur {
		systray.SetIcon(regularIconFor(state))
		t.cur = state
	}

	var header string
	if s.DaemonInfo != nil && s.DaemonInfo.WatchedDir != "" {
		header = i18n.Tf("tray_menu_status_with_dir", state.String(), s.DaemonInfo.WatchedDir)
	} else {
		header = i18n.Tf("tray_menu_status_simple", state.String())
	}
	// v0.44.0: surface PendingImports in the header when > 0. Renders
	// as e.g. "Status: active  (/proj) — 3 pending". The append happens
	// after the watched-dir splice so a single-glance read still shows
	// the state name first.
	if s.DaemonInfo != nil && s.DaemonInfo.PendingImports > 0 {
		header += i18n.Tf("tray_menu_status_pending", s.DaemonInfo.PendingImports)
	}
	t.miStatus.SetTitle(header)

	tip := header
	if s.ConflictCount > 0 {
		tip = i18n.Tf("tray_tooltip_with_conflicts", header, s.ConflictCount)
	}
	systray.SetTooltip(tip)

	systray.SetTitle("")

	t.miConflicts.SetTitle(i18n.Tf("tray_menu_conflicts_count", s.ConflictCount))
	if s.ConflictCount == 0 {
		t.miConflicts.Disable()
	} else {
		t.miConflicts.Enable()
	}

	// Per-conflict submenu (v0.40.0). Repaint each tick:
	//   - first min(N, conflictSlotCount) slots get labeled + shown
	//   - remaining slots get hidden
	//   - overflow slot is shown when N > conflictSlotCount
	//
	// Bound the labeled slots by len(s.Conflicts), NOT s.ConflictCount:
	// the two are independent wire fields (snapshot.go) and a daemon
	// bug / truncated JSON line / display-truncated array can deliver
	// ConflictCount > len(Conflicts). Indexing by the count would then
	// panic (index-out-of-range) on the tray's only run goroutine and
	// drop the icon. Mirrors the pending-projects pvisible := len(pp)
	// bound below. The header/badge/overflow text keep using
	// s.ConflictCount so the user still sees the daemon's total.
	visible := len(s.Conflicts)
	if visible > conflictSlotCount {
		visible = conflictSlotCount
	}
	for i, child := range t.miConflictKids {
		if i < visible {
			c := s.Conflicts[i]
			child.SetTitle(conflictSlotLabel(c))
			child.Show()
		} else {
			child.Hide()
		}
	}
	if s.ConflictCount > conflictSlotCount {
		t.miConflictMore.SetTitle(i18n.Tf("tray_conflict_more_format",
			s.ConflictCount-conflictSlotCount))
		t.miConflictMore.Show()
	} else {
		t.miConflictMore.Hide()
	}

	if syncPaused {
		t.miPauseResume.SetTitle(i18n.T("tray_menu_resume"))
	} else {
		t.miPauseResume.SetTitle(i18n.T("tray_menu_pause"))
	}
	if s.DaemonAvailable {
		t.miDaemonControl.SetTitle(i18n.T("tray_menu_daemon_stop"))
	} else {
		t.miDaemonControl.SetTitle(i18n.T("tray_menu_daemon_start"))
	}

	// v0.107.0: refresh the "Open Aplexica" menu item's enabled
	// state based on whether the daemon is up. updateOpenWebState
	// expects t.mu held (we already hold it for the rest of apply).
	t.updateOpenWebState()

	// v0.58.0: BRD-02 §4.13 "Pending projects (N) →" submenu repaint.
	// Same pre-allocate + Show/Hide pattern as the conflicts submenu.
	var pp []map[string]any
	if s.DaemonInfo != nil {
		pp = s.DaemonInfo.PendingProjects
	}
	t.miPendingProjects.SetTitle(i18n.Tf("tray_menu_pending_projects_count", len(pp)))
	if len(pp) == 0 {
		t.miPendingProjects.Disable()
	} else {
		t.miPendingProjects.Enable()
	}
	pvisible := len(pp)
	if pvisible > pendingProjectSlotCount {
		pvisible = pendingProjectSlotCount
	}
	for i, child := range t.miPendingKids {
		if i < pvisible {
			child.SetTitle(pendingSlotLabel(pp[i]))
			child.Show()
		} else {
			child.Hide()
		}
	}
	if len(pp) > pendingProjectSlotCount {
		t.miPendingMore.SetTitle(
			i18n.Tf("tray_conflict_more_format", len(pp)-pendingProjectSlotCount))
		t.miPendingMore.Show()
	} else {
		t.miPendingMore.Hide()
	}
}

// pendingSlotLabel formats a single pending-project entry for the
// submenu. v0.58.0; ID is the canonical identifier and the artifact
// count is shown alongside ("github.com/owner/repo  (3 artifacts)").
// Truncates the ID at 40 chars so the label fits in narrow menus.
func pendingSlotLabel(p map[string]any) string {
	id, _ := p["id"].(string)
	const max = 40
	if len(id) > max {
		id = id[:max] + "…"
	}
	count := 0
	if c, ok := p["artifactCount"].(float64); ok {
		count = int(c)
	}
	if count > 0 {
		return id + "  (" + strconv.Itoa(count) + " artifacts)"
	}
	return id
}

// run is the goroutine-callable driver. Selects on the snapshot channel
// and a watchdog ticker that drives Active→Idle decay when no new
// snapshots arrive. Exits on ctx cancel or snapshot channel close.
func (t *tray) run(ctx context.Context, snapshots <-chan StatusSnapshot,
	activeWindow, pausedThreshold time.Duration) {

	tick := time.NewTicker(activeWindow)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			// The cancel SOURCE is recorded by whoever cancelled (signal
			// watcher, traycontrol quit-for-update, Quit menu item) — see
			// shutdown.go. Without it this line could only say "cancelled".
			logShutdown(reasonContextCancelled + shutdownSource.cause())
			systray.Quit()
			return
		case s, ok := <-snapshots:
			if !ok {
				// Attribute, don't guess. superviseStatus loops
				// `for ctx.Err() == nil` and main closes this channel
				// only once it returns, so in production the feed
				// closes BECAUSE the context was cancelled — leaving
				// both select cases ready, which Go resolves at
				// random. Reporting the feed here would misname a
				// plain SIGTERM shutdown about half the time.
				// reasonFeedClosed stays for the genuine case: a feed
				// that ended while the context is still live.
				if ctx.Err() != nil {
					logShutdown(reasonContextCancelled + shutdownSource.cause())
				} else {
					logShutdown(reasonFeedClosed)
				}
				systray.Quit()
				return
			}
			t.apply(s, time.Now(), activeWindow, pausedThreshold)
		case <-tick.C:
			t.mu.Lock()
			last := t.lastSnap
			t.mu.Unlock()
			t.apply(last, time.Now(), activeWindow, pausedThreshold)
		}
	}
}
