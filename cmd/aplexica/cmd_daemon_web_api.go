package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter"
	"github.com/aplexica/aplexica/internal/conflicts"
	"github.com/aplexica/aplexica/internal/daemon"
	"github.com/aplexica/aplexica/internal/keys"
	"github.com/aplexica/aplexica/internal/nativebackup"
	"github.com/aplexica/aplexica/internal/onboarding"
	"github.com/aplexica/aplexica/internal/pausestate"
	"github.com/aplexica/aplexica/internal/pending"
	"github.com/aplexica/aplexica/internal/plugin/proto"
	"github.com/aplexica/aplexica/internal/plugin/secureexec"
	"github.com/aplexica/aplexica/internal/plugin/truststate"
	"github.com/aplexica/aplexica/internal/project"
	"github.com/aplexica/aplexica/internal/rbac"
	"github.com/aplexica/aplexica/internal/secrets"
	syncd "github.com/aplexica/aplexica/internal/sync"
	"github.com/aplexica/aplexica/internal/syncgate"
	"github.com/aplexica/aplexica/internal/syncrules"
	"github.com/aplexica/aplexica/internal/transport"
	apiweb "github.com/aplexica/aplexica/internal/web/api"
)

const (
	conversationSearchDefaultLimit  = 25
	conversationSearchMaxLimit      = 100
	conversationSearchScanLimit     = 500
	conversationSummaryPreviewTurns = 6
)

// webAPIDeps bundles every accessor the W4 endpoints need so the
// daemon's startup path can construct them once and pass them to
// web.Server.UseProtected in one shot. Keeping the deps explicit (vs.
// a single mega-Backend) mirrors the per-group seam each handler
// declares — the daemon-side adapter layer stays small and obvious.
type webAPIDeps struct {
	store    *acf.Store
	adapters []adapter.Adapter
	// discoveries maps adapter name -> startup discovery result (FR-03.3).
	// The web accessors refresh entries on demand so late installs or newly
	// approved agent roots show up without requiring a daemon restart.
	discoveriesMu sync.RWMutex
	discoveries   map[string]adapter.Discovery
	conf          *conflicts.Store
	pauseStore    *pausestate.Store
	pendingFn     func() ([]pending.Project, error)
	projectReg    *project.Registry
	orch          *syncd.Orchestrator

	rulesPath  string
	configPath string
	stateDir   string
	startedAt  time.Time
	pid        int
	watchedDir string
	version    string

	// secretsRoot is the local secrets store root (~/.aplexica/secrets). The
	// remote accessor uses it to load this device's X25519 wrap keypair so it
	// can hand the public key to the plugin at pairing time.
	secretsRoot string

	// backupsRoot is the native-snapshot directory (~/.aplexica/backups)
	// surfaced via nativeBackupsWebAccessor for GET /api/native-backups
	// + POST /api/native-backups/restore.
	backupsRoot   string
	backupMgr     *nativeBackupManager
	backupBlocker *syncd.AdapterBlocker

	// Remote-plugin runner (nil for OSS-only daemons).
	// Surfaced by daemonWebAccessor's RemoteStatusAccessor methods so
	// the SPA's Dashboard can render Cloud Sync state.
	remoteRunner *daemon.RemoteRunner

	// ctl is the daemon's control server. The pair flow pushes a rotated
	// cloud device id into it (applyCloudDeviceIdentity) so the status
	// response CLI adapter construction reads never reports a retired
	// identity. nil in tests that don't exercise the status path.
	ctl *daemon.ControlServer

	// Client-side RBAC: resolves the caller's per-namespace role and
	// capabilities (over the remote plugin) for GET /api/rbac/namespace/{id}.
	// nil for OSS-only daemons / tests (the rbacWebAccessor then reports an
	// unpaired, no-access state).
	roleService *daemon.RoleService

	// cloudRules holds the latest cloud-pushed selective-sync ruleset
	// (remote.rules_update). The rules accessor merges these over the
	// user's rules.toml for both the live engine and the read-only
	// "Synced from cloud" rows in the local portal. nil for OSS-only
	// daemons / tests (treated as no cloud rules).
	cloudRules *cloudRuleStore

	// logger is the daemon's structured logger, used by accessors that run
	// async work (e.g. the backfill fan-out on a gate enable). May be nil
	// in tests.
	logger *daemon.RotatingLogger

	// daemonCtx returns the daemon's long-lived run context. Async web work
	// such as manual native backups must use this rather than r.Context(), so
	// page navigation or browser disconnects do not cancel daemon-owned work.
	daemonCtx func() context.Context

	// Cached config snapshot — read RemoteConfigured/RemoteEnabled
	// from this so the accessor doesn't have to reload on every
	// request. This is a startup snapshot taken when deps is built; it
	// is NOT refreshed on daemon reload (reloadDaemonConfigPackage only
	// re-applies the daemon.project_scan_* globals and never reconstructs
	// deps). Live conn-state is read separately from remoteRunner.
	remoteCfg                      daemon.RemoteConfig
	remotePluginVerifier           func(string) (proto.VerifiedRemotePlugin, error)
	remotePluginCheckpointVerifier func(string, proto.VerifiedRemotePlugin) error
	remotePluginCommandPreparer    remotePluginCommandPreparer
}

func (d *webAPIDeps) discoveryFor(ad adapter.Adapter) adapter.Discovery {
	if d == nil || ad == nil {
		return adapter.Discovery{}
	}
	name := ad.Name()
	fresh, err := ad.Discover()
	if err != nil {
		fresh = adapter.Discovery{Installed: false, Detail: err.Error()}
	}
	d.discoveriesMu.Lock()
	if d.discoveries == nil {
		d.discoveries = make(map[string]adapter.Discovery, 1)
	}
	d.discoveries[name] = fresh
	d.discoveriesMu.Unlock()
	return fresh
}

// daemonWebAccessor satisfies api.DaemonAccessor. The Pause/Resume
// helpers flip the global pause flag for one hour by default — the
// SPA exposes this as a binary toggle in V1 (per-adapter pauses are
// CLI-only). The status fields read live data from the orchestrator.
type daemonWebAccessor struct {
	deps *webAPIDeps
}

func (d *daemonWebAccessor) Version() string      { return d.deps.version }
func (d *daemonWebAccessor) PID() int             { return d.deps.pid }
func (d *daemonWebAccessor) WatchedDir() string   { return d.deps.watchedDir }
func (d *daemonWebAccessor) StartedAt() time.Time { return d.deps.startedAt }

func (d *daemonWebAccessor) Paused() bool {
	if d.deps.pauseStore == nil {
		return false
	}
	paused, _ := d.deps.pauseStore.IsPaused("", time.Now())
	return paused
}

func (d *daemonWebAccessor) State() string {
	if d.Paused() {
		return "paused"
	}
	if d.deps.orch == nil {
		return "idle"
	}
	if last := d.deps.orch.LastActivity(); !last.IsZero() && time.Since(last) < 60*time.Second {
		return "active"
	}
	return "idle"
}

func (d *daemonWebAccessor) PendingImports() int {
	if d.deps.orch == nil {
		return 0
	}
	return d.deps.orch.PendingImports()
}

// Pause toggles a 1-hour global pause via the pausestate.Store. One
// hour is the same default the tray's "Pause syncing" item uses.
func (d *daemonWebAccessor) Pause() error {
	if d.deps.pauseStore == nil {
		return errors.New("daemon: pause store not wired")
	}
	return d.deps.pauseStore.PauseGlobal(60 * time.Minute)
}

func (d *daemonWebAccessor) Resume() error {
	if d.deps.pauseStore == nil {
		return errors.New("daemon: pause store not wired")
	}
	return d.deps.pauseStore.ResumeGlobal()
}

// agentsWebAccessor satisfies api.AgentsAccessor. RecentEvents is
// always [] in V1; W5 will replace this with a real ring-buffer wired
// to the SSE event bus.
type agentsWebAccessor struct {
	deps *webAPIDeps
	// readEvents overrides the canonical-store event reader. nil in
	// production (falls back to deps.store.ReadEvents). Tests set it to a
	// counting wrapper so they can assert the agent-detail feed parses only
	// a BOUNDED number of event logs — the regression guard behind the
	// large-store "Agents screen took 17s" fix, where the feed used to read
	// the full event log of every attributed artifact on each page load.
	readEvents func(acf.Kind, string) ([]acf.Event, error)
}

// readEventsFn returns the event reader, honouring the test override.
// Callers must have already established deps.store != nil.
func (a *agentsWebAccessor) readEventsFn() func(acf.Kind, string) ([]acf.Event, error) {
	if a.readEvents != nil {
		return a.readEvents
	}
	return a.deps.store.ReadEvents
}

func (a *agentsWebAccessor) List() []apiweb.AgentSummary {
	states := map[string]string{}
	touched := map[string]time.Time{}
	if a.deps.orch != nil {
		states = a.deps.orch.AdapterStates()
		// LastActivity per-adapter isn't exposed; surface the
		// orchestrator's global LastActivity as a fallback so the SPA
		// has SOMETHING to show. Real per-adapter timestamps land in
		// W5 when the event ring buffer arrives.
		gActivity := a.deps.orch.LastActivity()
		for name := range states {
			touched[name] = gActivity
		}
	}
	out := make([]apiweb.AgentSummary, 0, len(a.deps.adapters))
	for _, ad := range a.deps.adapters {
		d := a.deps.discoveryFor(ad)
		caps := ad.Capabilities()
		out = append(out, apiweb.AgentSummary{
			Name:           ad.Name(),
			Version:        ad.Version(),
			Surfaces:       surfaceStrings(caps.Surfaces),
			ActiveSurfaces: surfaceStrings(d.ActiveSurfaces),
			SyncState:      fallbackState(states[ad.Name()]),
			LastActivity:   touched[ad.Name()],
			Installed:      d.Installed,
			GlobalRoots:    d.GlobalRoots,
			ArtifactCount:  a.artifactCount(ad.Name(), d.GlobalRoots),
			SyncEnabled:    a.deps.orch != nil && a.deps.orch.FanOutEnabled(ad.Name()),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// artifactCount returns the number of canonical-store artifacts attributed to
// agentName for the FR-01.28 per-agent count: an artifact counts when the
// agent received it via fan-out (SyncedAgents) OR it originated from the
// agent's native global storage (SourcePath under a discovered GlobalRoot).
func (a *agentsWebAccessor) artifactCount(agentName string, roots []string) int {
	if a.deps.store == nil {
		return 0
	}
	n := 0
	for _, kind := range []acf.Kind{acf.KindMemory, acf.KindSkill, acf.KindTool, acf.KindConversation} {
		arts, err := a.deps.store.ListArtifacts(kind)
		if err != nil {
			continue
		}
		for _, art := range arts {
			if agentAttributed(art, agentName, roots) {
				n++
			}
		}
	}
	return n
}

// agentAttributed reports whether artifact art should be counted for agentName.
func agentAttributed(art acf.Artifact, agentName string, roots []string) bool {
	sourceOwned, received := agentAttribution(art, agentName, roots)
	return sourceOwned || received
}

func agentAttribution(art acf.Artifact, agentName string, roots []string) (sourceOwned, received bool) {
	for _, sa := range art.SyncedAgents {
		if sa == agentName {
			received = true
			break
		}
	}
	if art.SourcePath != "" {
		// Normalize both sides to forward slashes: SourcePaths are recorded
		// with the OS separator, roots can arrive in either form, and a
		// `\`-built prefix never matches a `/`-recorded path on Windows.
		sp := filepath.ToSlash(art.SourcePath)
		for _, r := range roots {
			if r == "" {
				continue
			}
			prefix := strings.TrimRight(filepath.ToSlash(r), "/") + "/"
			if strings.HasPrefix(sp, prefix) {
				sourceOwned = true
				break
			}
		}
	}
	return sourceOwned, received
}

func (a *agentsWebAccessor) Get(name string) (apiweb.AgentDetail, bool) {
	for _, ad := range a.deps.adapters {
		if ad.Name() != name {
			continue
		}
		caps := ad.Capabilities()
		var states map[string]string
		var last time.Time
		if a.deps.orch != nil {
			states = a.deps.orch.AdapterStates()
			last = a.deps.orch.LastActivity()
		}
		// Namespaces in V1 mirror the artifact kinds the adapter
		// natively supports (memory/skill/tool/conversation). Real
		// per-namespace sync-mode tracking is a W5+ concern.
		nss := []string{}
		if caps.Artifacts.Memory {
			nss = append(nss, "memory")
		}
		if caps.Artifacts.Skill {
			nss = append(nss, "skill")
		}
		if caps.Artifacts.Tool {
			nss = append(nss, "tool")
		}
		if caps.Artifacts.Conversation {
			nss = append(nss, "conversation")
		}
		d := a.deps.discoveryFor(ad)
		watched := agentWatchedLocations(d, a.deps.projectReg, ad.Name())
		return apiweb.AgentDetail{
			AgentSummary: apiweb.AgentSummary{
				Name:           ad.Name(),
				Version:        ad.Version(),
				Surfaces:       surfaceStrings(caps.Surfaces),
				ActiveSurfaces: surfaceStrings(d.ActiveSurfaces),
				SyncState:      fallbackState(states[ad.Name()]),
				LastActivity:   last,
				Installed:      d.Installed,
				GlobalRoots:    watched,
				ArtifactCount:  a.artifactCount(ad.Name(), d.GlobalRoots),
			},
			Namespaces:   nss,
			RecentEvents: a.recentAgentEvents(ad.Name(), d.GlobalRoots, 25),
		}, true
	}
	return apiweb.AgentDetail{}, false
}

func surfaceStrings(surfaces []adapter.Surface) []string {
	if len(surfaces) == 0 {
		return nil
	}
	out := make([]string, len(surfaces))
	for i, surface := range surfaces {
		out[i] = string(surface)
	}
	return out
}

// agentWatchedLocations computes the "Watched Locations" list for the
// agent-detail portal page. It returns, in order and de-duplicated
// (first occurrence wins):
//  1. d.GlobalRoots — the adapter's global native roots (e.g. ~/.claude)
//  2. d.RecursiveRoots — roots watched recursively (e.g. ~/.claude/projects)
//  3. Every local-scope registered project the agent participates in.
//
// Project folders are sorted before appending for deterministic output.
// artifactCount and recentAgentEvents MUST keep receiving d.GlobalRoots
// (not the result of this function) because their path-attribution logic
// depends on the original global roots only.
func agentWatchedLocations(d adapter.Discovery, reg *project.Registry, agentName string) []string {
	seen := make(map[string]struct{})
	var out []string

	add := func(p string) {
		if p == "" {
			return
		}
		if _, dup := seen[p]; dup {
			return
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}

	for _, r := range d.GlobalRoots {
		add(r)
	}
	for _, r := range d.RecursiveRoots {
		add(r)
	}

	if reg != nil {
		entries := reg.List()
		// Sort project-folder group by path for deterministic output.
		sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
		for _, e := range entries {
			if e.EffectiveScope() != "local" {
				continue
			}
			// Empty Agents means "all installed agents".
			participates := len(e.Agents) == 0
			for _, a := range e.Agents {
				if a == agentName {
					participates = true
					break
				}
			}
			if participates {
				add(e.Path)
			}
		}
	}

	return out
}

// deriveEventType maps a canonical event to the EventRecord/AgentEvent Type
// vocabulary shared by the global /events feed and the agent-detail feed, so
// the two cannot drift:
//   - an internal retention snapshot is an "artifact.checkpoint";
//   - an event whose provenance device differs from this device (localDev,
//     when known) arrived over device-to-device sync — "artifact.synced";
//   - everything else is a local native import — "artifact.imported".
//
// localDev is this device's cloud identity (empty when unpaired); an empty
// localDev never claims a cross-device sync. recentAgentEvents layers an
// additional received-from-another-agent "artifact.synced" case on top of this
// for its own (non-snapshot) fan-out rows.
func deriveEventType(ev acf.Event, localDev string) string {
	switch {
	case ev.Type == acf.EventType(acf.EventTypeSnapshot):
		return "artifact.checkpoint"
	case localDev != "" && ev.Provenance.DeviceID != "" && ev.Provenance.DeviceID != localDev:
		return "artifact.synced"
	default:
		return "artifact.imported"
	}
}

func eventRecordFor(kind acf.Kind, art acf.Artifact, ev acf.Event, localDev string) apiweb.EventRecord {
	typ := deriveEventType(ev, localDev)
	targets := eventTargetAgents(art.SyncedAgents)
	rec := apiweb.EventRecord{
		Seq:        ev.Timestamp.UnixNano() / int64(time.Millisecond),
		Type:       typ,
		Timestamp:  ev.Timestamp,
		ArtifactID: art.ArtifactID,
		Kind:       string(kind),
		// The dashboard's "by agent" charts must contain adapter identities,
		// not internal producers such as retention, repair, conflict-resolution,
		// or CLI operation names. Keep the raw provenance in the canonical event;
		// expose Agent only when it is one of the registered V1 adapters. When it
		// is blank the portal can still attribute native files from SourcePath or
		// use TargetAgents, without inventing a non-agent chart series.
		Agent:        chartAgentName(ev.Provenance.SourceAgent),
		Name:         art.Name,
		Action:       eventAction(typ, targets),
		SourcePath:   redactedDisplayPath(art.SourcePath),
		TargetAgents: targets,
		Scope:        string(art.Scope),
		Origin:       eventOrigin(typ),
	}
	if art.Project != nil {
		rec.ProjectID = art.Project.ID
		rec.ProjectPath = redactedDisplayPath(art.Project.Path)
	}
	return rec
}

func chartAgentName(sourceAgent string) string {
	if _, ok := knownAgentNames[sourceAgent]; ok {
		return sourceAgent
	}
	return ""
}

func eventAction(typ string, targetAgents []string) string {
	switch typ {
	case "artifact.checkpoint":
		return "checkpointed"
	case "artifact.refused":
		return "refused"
	case "artifact.synced":
		return "synced"
	default:
		if len(targetAgents) > 0 {
			return "synced"
		}
		return "imported"
	}
}

func eventOrigin(typ string) string {
	switch typ {
	case "artifact.checkpoint":
		return "system"
	case "artifact.synced":
		return "remote"
	default:
		return "local"
	}
}

func eventTargetAgents(agents []string) []string {
	if len(agents) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(agents))
	out := make([]string, 0, len(agents))
	for _, a := range agents {
		if a == "" {
			continue
		}
		if _, ok := seen[a]; ok {
			continue
		}
		seen[a] = struct{}{}
		out = append(out, a)
	}
	sort.Strings(out)
	if len(out) == 0 {
		return nil
	}
	return out
}

func redactedDisplayPath(path string) string {
	if path == "" {
		return ""
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return path
	}
	path = strings.ReplaceAll(path, home+string(filepath.Separator), "~"+string(filepath.Separator))
	path = strings.ReplaceAll(path, home, "~")
	return path
}

// recentAgentEvents builds the agent-detail "Recent Events" list from the
// canonical store. Native files owned by the agent are shown as imports; fan-out
// received from other agents is shown only while the agent's sync gate is
// enabled, so a Sync off page doesn't imply active cross-agent writes.
// Most-recent first, capped at limit.
func (a *agentsWebAccessor) recentAgentEvents(name string, roots []string, limit int) []apiweb.AgentEvent {
	if a.deps.store == nil {
		return []apiweb.AgentEvent{}
	}
	receiveEnabled := a.deps.orch != nil && a.deps.orch.FanOutEnabled(name)
	// localDev lets us tell a cross-DEVICE sync (an event authored on another
	// paired device — even by the SAME agent) from a genuine local import.
	localDev := ""
	if a.deps.orch != nil {
		localDev = a.deps.orch.LocalDeviceID()
	}
	if limit <= 0 {
		return []apiweb.AgentEvent{}
	}

	// Phase 1 — gather the attributed artifacts from cheap metadata only (no
	// event-log reads). art.UpdatedAt is the timestamp of the artifact's
	// newest main-branch event (store.AppendEvent keeps them in lockstep), so
	// it is an upper bound on every event the artifact's log can contribute.
	type cand struct {
		kind        acf.Kind
		id          string
		artName     string
		updatedAt   time.Time
		sourceOwned bool
	}
	var cands []cand
	for _, kind := range []acf.Kind{acf.KindMemory, acf.KindSkill, acf.KindTool, acf.KindConversation} {
		arts, err := a.deps.store.ListArtifacts(kind)
		if err != nil {
			continue
		}
		for _, art := range arts {
			sourceOwned, received := agentAttribution(art, name, roots)
			if !sourceOwned && (!received || !receiveEnabled) {
				continue
			}
			cands = append(cands, cand{
				kind:        kind,
				id:          art.ArtifactID,
				artName:     art.Name,
				updatedAt:   art.UpdatedAt,
				sourceOwned: sourceOwned,
			})
		}
	}
	// Newest-updated first so we can stop reading once no remaining artifact
	// can beat the limit-th newest event already collected.
	sort.Slice(cands, func(i, j int) bool { return cands[i].updatedAt.After(cands[j].updatedAt) })

	type tev struct {
		typ    string
		ts     time.Time
		detail string
	}
	var evs []tev
	// Phase 2 — read event logs only for the most-recently-updated artifacts,
	// stopping as soon as the feed is full and the next artifact's newest
	// possible event is older than the oldest event we are already keeping.
	// On a large store this reads ~limit event logs instead of one per
	// attributed artifact (the source of the multi-second agent-detail load).
	for _, c := range cands {
		if len(evs) >= limit {
			sort.Slice(evs, func(i, j int) bool { return evs[i].ts.After(evs[j].ts) })
			evs = evs[:limit]
			if !c.updatedAt.After(evs[limit-1].ts) {
				break
			}
		}
		events, err := a.readEventsFn()(c.kind, c.id)
		if err != nil {
			continue
		}
		for _, ev := range events {
			// Type is derived by the shared helper so the agent-detail feed
			// and the global /events feed classify the same event identically.
			typ := deriveEventType(ev, localDev)
			detail := "imported " + string(c.kind)
			src := ev.Provenance.SourceAgent
			// An event whose provenance device differs from this device
			// arrived over device-to-device sync — label it "synced" even
			// when the same agent authored it on both ends (otherwise it is
			// indistinguishable from a local import and reads misleadingly).
			remoteDevice := localDev != "" && ev.Provenance.DeviceID != "" && ev.Provenance.DeviceID != localDev
			switch {
			case ev.Type == acf.EventType(acf.EventTypeSnapshot):
				detail = "internal checkpoint"
			case remoteDevice:
				detail = "synced " + string(c.kind) + " from another device"
			case !c.sourceOwned:
				// Received over cross-agent fan-out (not cross-device): the
				// helper classed this as a local import by device, so force
				// the user-facing "synced" label for a received artifact.
				typ = "artifact.synced"
				detail = "received " + string(c.kind)
				if src != "" && src != name {
					detail += " from " + src
				}
			}
			if c.artName != "" {
				detail += " · " + c.artName
			}
			evs = append(evs, tev{typ: typ, ts: ev.Timestamp, detail: detail})
		}
	}
	sort.Slice(evs, func(i, j int) bool { return evs[i].ts.After(evs[j].ts) })
	if len(evs) > limit {
		evs = evs[:limit]
	}
	out := make([]apiweb.AgentEvent, 0, len(evs))
	for _, e := range evs {
		out = append(out, apiweb.AgentEvent{Timestamp: e.ts, Type: e.typ, Detail: e.detail})
	}
	return out
}

func fallbackState(s string) string {
	if s == "" {
		return "idle"
	}
	return s
}

// eventsWebAccessor satisfies api.EventsAccessor. The feed is a global
// newest-first merge across artifact logs, but the dashboard usually needs only
// the first page. The accessor therefore lists cheap artifact metadata, sorts by
// a conservative "latest possible event" bound, and reads logs only until the
// requested page is complete. That keeps cold dashboard loads bounded by recent
// activity instead of the entire canonical history. The per-event Seq is the
// millisecond Unix timestamp, ties broken by a stable per-event EventID sort.
type eventsWebAccessor struct {
	deps *webAPIDeps
	// readEventHeaders overrides metadata-only canonical-store reads in tests so
	// performance regressions can assert the dashboard does not decode full
	// conversation payloads or scan the whole store.
	readEventHeaders func(acf.Kind, string, int64, int) ([]acf.Event, error)

	mu        sync.Mutex
	cacheKey  string
	cachePage apiweb.EventPage
}

type eventArtRef struct {
	kind     acf.Kind
	art      acf.Artifact
	upperSeq int64
}

// eventSeqRec pairs a wire record with its stable acf EventID so the sort order
// and the page boundary stay deterministic when several events share a
// millisecond (Seq is ms-truncated).
type eventSeqRec struct {
	rec apiweb.EventRecord
	id  string
}

func (e *eventsWebAccessor) Backfill(q apiweb.EventQuery) (apiweb.EventPage, error) {
	if e.deps.store == nil {
		return apiweb.EventPage{Events: []apiweb.EventRecord{}}, nil
	}
	if q.Limit <= 0 {
		return apiweb.EventPage{Events: []apiweb.EventRecord{}, NextBefore: q.Before}, nil
	}
	refs, sig, err := e.eventRefs()
	if err != nil {
		return apiweb.EventPage{}, err
	}

	key := fmt.Sprintf("%s:%d:%d", sig, q.Before, q.Limit)
	e.mu.Lock()
	if e.cacheKey == key {
		page := cloneEventPage(e.cachePage)
		e.mu.Unlock()
		return page, nil
	}
	e.mu.Unlock()

	page, err := e.materializePage(refs, q)
	if err != nil {
		return apiweb.EventPage{}, err
	}
	if page.Events == nil {
		page.Events = []apiweb.EventRecord{}
	}

	e.mu.Lock()
	e.cacheKey = key
	e.cachePage = cloneEventPage(page)
	e.mu.Unlock()
	return page, nil
}

func (e *eventsWebAccessor) readEventHeadersFn() func(acf.Kind, string, int64, int) ([]acf.Event, error) {
	if e.readEventHeaders != nil {
		return e.readEventHeaders
	}
	return e.deps.store.ReadRecentEventHeaders
}

func (e *eventsWebAccessor) eventRefs() ([]eventArtRef, string, error) {
	kinds := []acf.Kind{acf.KindMemory, acf.KindSkill, acf.KindTool, acf.KindConversation}
	var refs []eventArtRef
	sig := fnv.New64a()
	for _, kind := range kinds {
		arts, err := e.deps.store.ListArtifacts(kind)
		if err != nil {
			return nil, "", fmt.Errorf("events: list %s: %w", kind, err)
		}
		for _, a := range arts {
			upper := a.UpdatedAt
			mt, err := e.deps.store.EventLogModTime(kind, a.ArtifactID)
			if err != nil {
				return nil, "", fmt.Errorf("events: stat %s/%s: %w", kind, a.ArtifactID, err)
			}
			upperSeq := eventSeqMillis(upper)
			if upperSeq == 0 || hasNonMainBranch(a.BranchHeads) {
				if mtSeq := eventSeqMillis(mt); mtSeq > upperSeq {
					upperSeq = mtSeq
				}
			}
			refs = append(refs, eventArtRef{
				kind:     kind,
				art:      a,
				upperSeq: upperSeq,
			})
			// The signature must change on every event append so the cache never
			// goes stale: HeadEventHash bumps on a main-branch event, BranchHeads
			// on any branch event. The event-log mtime is included for external
			// repair. Normal artifacts prioritize by event time so importing old
			// history does not make every old log look recent; branchy artifacts
			// also use mtime as a safe bound because side-branch events do not move
			// Artifact.UpdatedAt.
			_, _ = fmt.Fprintf(sig, "%s\x00%s\x00%s\x00%d\x00",
				kind, a.ArtifactID, a.HeadEventHash, mt.UnixNano())
			if len(a.BranchHeads) > 0 {
				names := make([]string, 0, len(a.BranchHeads))
				for b := range a.BranchHeads {
					names = append(names, b)
				}
				sort.Strings(names)
				for _, b := range names {
					_, _ = sig.Write([]byte(b))
					_, _ = sig.Write([]byte(a.BranchHeads[b]))
				}
			}
		}
	}
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].upperSeq != refs[j].upperSeq {
			return refs[i].upperSeq > refs[j].upperSeq
		}
		if refs[i].kind != refs[j].kind {
			return refs[i].kind > refs[j].kind
		}
		return refs[i].art.ArtifactID > refs[j].art.ArtifactID
	})
	return refs, fmt.Sprintf("%d:%016x", len(refs), sig.Sum64()), nil
}

func hasNonMainBranch(heads map[string]string) bool {
	for branch := range heads {
		if branch != "" && branch != acf.MainBranch {
			return true
		}
	}
	return false
}

func (e *eventsWebAccessor) materializePage(refs []eventArtRef, q apiweb.EventQuery) (apiweb.EventPage, error) {
	// localDev lets deriveEventType tell a cross-DEVICE sync (an event authored
	// on another paired device) from a local native import; empty when unpaired
	// or in tests with no orchestrator, in which case nothing is claimed as a
	// cross-device sync.
	localDev := ""
	if e.deps.orch != nil {
		localDev = e.deps.orch.LocalDeviceID()
	}

	targetLimit := q.Limit
	var recs []eventSeqRec
	for _, r := range refs {
		if len(recs) >= targetLimit {
			sortEventSeqRecs(recs)
			boundary := recs[targetLimit-1].rec.Seq
			for targetLimit < len(recs) && recs[targetLimit].rec.Seq == boundary {
				targetLimit++
			}
			if r.upperSeq < boundary {
				break
			}
		}
		evs, err := e.readEventHeadersFn()(r.kind, r.art.ArtifactID, q.Before, targetLimit)
		if err != nil {
			continue
		}
		for _, ev := range evs {
			rec := eventRecordFor(r.kind, r.art, ev, localDev)
			if q.Before > 0 && rec.Seq >= q.Before {
				continue
			}
			recs = append(recs, eventSeqRec{rec: rec, id: ev.EventID})
		}
	}
	sortEventSeqRecs(recs)
	q.Limit = targetLimit
	return eventPageFromRecs(recs, q), nil
}

func eventSeqMillis(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano() / int64(time.Millisecond)
}

func sortEventSeqRecs(recs []eventSeqRec) {
	// Newest-first: the Event stream is a "recent activity" feed, so the first
	// page (Before <= 0) surfaces the MOST RECENT events and the Before cursor
	// pages BACKWARD. EventID is a deterministic tiebreak for same-ms events.
	sort.Slice(recs, func(i, j int) bool {
		if recs[i].rec.Seq != recs[j].rec.Seq {
			return recs[i].rec.Seq > recs[j].rec.Seq
		}
		return recs[i].id > recs[j].id
	})
}

func eventPageFromRecs(recs []eventSeqRec, q apiweb.EventQuery) apiweb.EventPage {
	to := q.Limit
	if to > len(recs) {
		to = len(recs)
	}
	// Do not split a same-millisecond group across the page boundary: NextBefore
	// is the boundary Seq and the next page excludes it (Seq < Before), so any
	// same-ms event left behind would be dropped. Extend the page to cover the
	// whole boundary group.
	if to > 0 {
		for to < len(recs) && recs[to].rec.Seq == recs[to-1].rec.Seq {
			to++
		}
	}
	events := make([]apiweb.EventRecord, to)
	for i := 0; i < to; i++ {
		events[i] = recs[i].rec
	}
	page := apiweb.EventPage{Events: events}
	if to > 0 {
		page.NextBefore = recs[to-1].rec.Seq
	} else {
		page.NextBefore = q.Before
	}
	return page
}

func cloneEventPage(page apiweb.EventPage) apiweb.EventPage {
	out := apiweb.EventPage{NextBefore: page.NextBefore}
	if page.Events == nil {
		out.Events = []apiweb.EventRecord{}
	} else {
		out.Events = append([]apiweb.EventRecord(nil), page.Events...)
	}
	return out
}

// rulesWebAccessor satisfies api.RulesAccessor. Read returns the USER
// rules only (safe-by-default: no shipped always-on defaults are
// merged in — they are offered as opt-in presets). Add/Update/Delete
// operate on the user file at ~/.aplexica/rules.toml. A preset IS just a
// user rule with a classic name, so preset names are addable; Add only
// rejects a name that already exists in the user file.
type rulesWebAccessor struct {
	deps *webAPIDeps
}

// List returns the user's rules.toml rules (Source="local") merged with
// the cloud-pushed ruleset (Source="cloud", read-only). Cloud rules win on
// a name conflict, mirroring the live engine merge — so the list reflects
// exactly what is in effect on this device.
func (r *rulesWebAccessor) List() ([]syncrules.Rule, error) {
	user, err := loadAllRules(r.deps.rulesPath)
	if err != nil {
		return nil, err
	}
	for i := range user {
		user[i].Source = "local"
	}
	cloud := r.deps.cloudRules.get()
	for i := range cloud {
		cloud[i].Source = "cloud"
	}
	return mergeRules(user, cloud), nil
}

func (r *rulesWebAccessor) Get(name string) (syncrules.Rule, bool, error) {
	all, err := r.List()
	if err != nil {
		return syncrules.Rule{}, false, err
	}
	for _, ru := range all {
		if ru.Name == name {
			return ru, true, nil
		}
	}
	return syncrules.Rule{}, false, nil
}

// rejectIfCloud returns an error when name belongs to a cloud-managed rule.
// Cloud rules are read-only on the device — they are edited in the cloud
// portal — so the local Add/Update/Delete paths refuse to touch them. The
// portal also renders them read-only; this is the server-side backstop.
func (r *rulesWebAccessor) rejectIfCloud(name string) error {
	for _, c := range r.deps.cloudRules.get() {
		if c.Name == name {
			return fmt.Errorf("rule %q is synced from the cloud and is read-only on this device; edit it in the cloud portal", name)
		}
	}
	return nil
}

func (r *rulesWebAccessor) Add(rule syncrules.Rule) error {
	if err := r.rejectIfCloud(rule.Name); err != nil {
		return err
	}
	rule.Source = "" // never persist a provenance tag to the user file
	if err := rejectReservedAssignTags([]syncrules.Rule{rule}); err != nil {
		return err
	}
	if err := syncrules.Validate([]syncrules.Rule{rule}); err != nil {
		return err
	}
	user, err := loadUserRules(r.deps.rulesPath)
	if err != nil {
		return err
	}
	for _, u := range user.Sync.Rules {
		if u.Name == rule.Name {
			return fmt.Errorf("rule %q already exists", rule.Name)
		}
	}
	user.Sync.Rules = append(user.Sync.Rules, rule)
	if err := writeUserRules(r.deps.rulesPath, user); err != nil {
		return err
	}
	// FR-05.13: journal the change for the audit trail (best-effort, as the CLI).
	_ = journalRuleChange(r.deps.rulesPath, "add", map[string]any{"name": rule.Name})
	return r.hotReload()
}

// hotReload rebuilds the orchestrator's selective-sync engine from the
// freshly written user rules file and swaps it in live so rule edits via
// the API take effect without a daemon restart. A nil orchestrator (e.g.
// in unit tests) or a build error is non-fatal to the mutation that
// already persisted: a parse error means the on-disk file is malformed,
// but buildRulesEngineFromPath returns an EMPTY engine in that case
// (safe-by-default — never silently re-enables fan-out), so the swap is
// still safe. The error is returned so the caller can surface it.
func (r *rulesWebAccessor) hotReload() error {
	if r.deps.orch == nil {
		return nil
	}
	// Rebuild from the user file MERGED with the current cloud ruleset so a
	// local rule edit no longer clobbers cloud rules in the live engine.
	// Safe-by-default: a malformed user file contributes no user rules
	// (cloud rules still apply) and the parse error is surfaced.
	user, uerr := loadUserRulesQuiet(r.deps.rulesPath)
	eng, nerr := syncrules.New(mergeRules(user.Sync.Rules, r.deps.cloudRules.get()))
	r.deps.orch.SetRulesEngine(eng)
	if uerr != nil {
		return uerr
	}
	return nerr
}

func (r *rulesWebAccessor) Update(name string, rule syncrules.Rule) error {
	if err := r.rejectIfCloud(name); err != nil {
		return err
	}
	rule.Source = "" // never persist a provenance tag to the user file
	if err := rejectReservedAssignTags([]syncrules.Rule{rule}); err != nil {
		return err
	}
	if err := syncrules.Validate([]syncrules.Rule{rule}); err != nil {
		return err
	}
	user, err := loadUserRules(r.deps.rulesPath)
	if err != nil {
		return err
	}
	found := false
	for i, u := range user.Sync.Rules {
		if u.Name == name {
			user.Sync.Rules[i] = rule
			found = true
			break
		}
	}
	if !found {
		// Maybe it's a shipped default — fail with a useful message.
		defaults, derr := syncrules.ParseDefault()
		if derr == nil {
			for _, d := range defaults.Sync.Rules {
				if d.Name == name {
					return fmt.Errorf("rule %q is a shipped default; override with a higher-precedence user rule instead", name)
				}
			}
		}
		return apiweb.ErrRuleNotFound
	}
	if err := writeUserRules(r.deps.rulesPath, user); err != nil {
		return err
	}
	// FR-05.13: journal the change for the audit trail (best-effort, as the CLI).
	_ = journalRuleChange(r.deps.rulesPath, "edit", map[string]any{"name": name})
	return r.hotReload()
}

func (r *rulesWebAccessor) Delete(name string) error {
	if err := r.rejectIfCloud(name); err != nil {
		return err
	}
	user, err := loadUserRules(r.deps.rulesPath)
	if err != nil {
		return err
	}
	filtered := user.Sync.Rules[:0]
	found := false
	for _, u := range user.Sync.Rules {
		if u.Name == name {
			found = true
			continue
		}
		filtered = append(filtered, u)
	}
	if !found {
		defaults, derr := syncrules.ParseDefault()
		if derr == nil {
			for _, d := range defaults.Sync.Rules {
				if d.Name == name {
					return fmt.Errorf("rule %q is a shipped default; cannot delete (override instead)", name)
				}
			}
		}
		return apiweb.ErrRuleNotFound
	}
	user.Sync.Rules = filtered
	if err := writeUserRules(r.deps.rulesPath, user); err != nil {
		return err
	}
	// FR-05.13: journal the change for the audit trail (best-effort, as the CLI).
	_ = journalRuleChange(r.deps.rulesPath, "remove", map[string]any{"name": name})
	return r.hotReload()
}

// syncWebAccessor satisfies api.SyncAccessor. It reads + mutates the
// FR-03.3 await-config fan-out gate (daemon config cfg.Sync) and applies
// changes LIVE by rebuilding the orchestrator's SyncGate — so a portal
// toggle takes effect without a daemon restart (unlike the CLI path,
// where the gate is only rebuilt on the next daemon start).
type syncWebAccessor struct {
	deps *webAPIDeps
}

func (s *syncWebAccessor) State() (bool, map[string]bool, error) {
	cfg, err := daemon.LoadConfig(s.deps.configPath)
	if err != nil {
		return false, nil, err
	}
	return cfg.Sync.All, cfg.Sync.Agents, nil
}

func (s *syncWebAccessor) SetAll(enabled bool) error {
	wasAll := false
	if cfg, err := daemon.LoadConfig(s.deps.configPath); err == nil {
		wasAll = cfg.Sync.All
	}
	if err := s.mutate(func(c *daemon.Config) { c.Sync.All = enabled }); err != nil {
		return err
	}
	// Enabling the global flag newly enables agents that had no override —
	// backfill so existing artifacts fan out, not just future events.
	if enabled && !wasAll {
		s.backfill("all")
	}
	return nil
}

func (s *syncWebAccessor) SetAgent(name string, enabled bool) error {
	wasEnabled := s.deps.orch != nil && s.deps.orch.FanOutEnabled(name)
	if err := s.mutate(func(c *daemon.Config) {
		if c.Sync.Agents == nil {
			c.Sync.Agents = map[string]bool{}
		}
		c.Sync.Agents[name] = enabled
	}); err != nil {
		return err
	}
	// On a disabled -> enabled transition, backfill existing artifacts to
	// the newly-enabled target (the gate alone only fans FUTURE events).
	if enabled && !wasEnabled {
		s.backfill(name)
	}
	return nil
}

// backfill kicks off a one-time RefanOutAll pass asynchronously so the
// toggle request returns immediately. fanOut honors the (just-updated)
// gate + rules, so only enabled targets receive. Idempotent for targets
// already in sync.
func (s *syncWebAccessor) backfill(target string) {
	if s.deps.orch == nil {
		return
	}
	go func() {
		n, err := s.deps.orch.RefanOutAll(context.Background())
		if s.deps.logger == nil {
			return
		}
		if err != nil {
			s.deps.logger.Warn("fan-out backfill failed", "target", target, "err", err)
			return
		}
		s.deps.logger.Info("fan-out backfill complete (gate enabled)", "target", target, "artifacts_refanned", n)
	}()
}

// mutate loads the daemon config, applies fn, persists it, and swaps the
// live gate on the orchestrator so fan-out enablement changes immediately.
func (s *syncWebAccessor) mutate(fn func(*daemon.Config)) error {
	cfg, err := daemon.LoadConfig(s.deps.configPath)
	if err != nil {
		return err
	}
	fn(cfg)
	if err := daemon.WriteConfig(s.deps.configPath, cfg); err != nil {
		return err
	}
	if s.deps.orch != nil {
		s.deps.orch.SetSyncGate(syncgate.New(daemon.SyncGateConfig(*cfg)))
	}
	return nil
}

// nativeBackupsWebAccessor satisfies api.NativeBackupsAccessor. It
// lists the native snapshots under ~/.aplexica/backups and restores one
// of them over the live native roots. Restore is REVERSIBLE — the
// underlying nativebackup.Restore always snapshots the CURRENT native
// state into a sibling pre-restore-* directory first (structural, not a
// handler responsibility).
type nativeBackupsWebAccessor struct {
	deps *webAPIDeps
	jobs *nativeBackupJobManager
}

func (n *nativeBackupsWebAccessor) List() ([]nativebackup.BackupInfo, error) {
	local, err := nativebackup.List(n.deps.backupsRoot)
	if err != nil {
		return nil, err
	}
	for i := range local {
		if local[i].Location == "" {
			local[i].Location = "local"
		}
	}
	cloud, err := n.listCloudBackups(context.Background())
	if err == nil {
		local = append(local, cloud...)
		sort.Slice(local, func(i, j int) bool {
			if !local[i].CreatedAt.Equal(local[j].CreatedAt) {
				return local[i].CreatedAt.After(local[j].CreatedAt)
			}
			return local[i].ID > local[j].ID
		})
	}
	return local, nil
}

func (n *nativeBackupsWebAccessor) StartCreate(agents []string, destination string) (nativebackup.BackupJob, error) {
	jobs := n.jobs
	if jobs == nil {
		jobs = newNativeBackupJobManager()
		n.jobs = jobs
	}
	parent := context.Background()
	if n.deps != nil && n.deps.daemonCtx != nil {
		if ctx := n.deps.daemonCtx(); ctx != nil {
			parent = ctx
		}
	}
	return jobs.Start(parent, agents, destination, func(ctx context.Context) (nativebackup.BackupInfo, error) {
		return n.CreateKind(ctx, "manual", agents, destination)
	})
}

func (n *nativeBackupsWebAccessor) CancelJob(jobID string) (nativebackup.BackupJob, error) {
	if n.jobs == nil {
		return nativebackup.BackupJob{}, fmt.Errorf("backup job %q not found", jobID)
	}
	return n.jobs.Cancel(jobID)
}

func (n *nativeBackupsWebAccessor) CreateKind(ctx context.Context, kind string, agents []string, destination string) (nativebackup.BackupInfo, error) {
	if n.deps.backupMgr == nil {
		return nativebackup.BackupInfo{}, errors.New("native backup manager not wired")
	}
	if destination == "" {
		destination = "local"
	}
	if kind == "scheduled" {
		ctx = nativebackup.WithScheduledBackgroundBudget(
			ctx,
			nativebackup.DefaultScheduledThroughputBytesPerSecond,
			nativebackup.DefaultScheduledFilesPerSecond,
		)
	}
	if destination == "cloud" {
		info, err := n.createCloudBackup(ctx, kind, agents)
		if err != nil {
			return nativebackup.BackupInfo{}, err
		}
		n.clearBackupBlocksAndRescan(info.Agents)
		return info, nil
	}
	if destination != "local" {
		return nativebackup.BackupInfo{}, fmt.Errorf("unsupported backup destination %q", destination)
	}
	info, err := n.deps.backupMgr.CreateContext(ctx, kind, agents)
	if err != nil {
		return nativebackup.BackupInfo{}, err
	}
	n.clearBackupBlocksAndRescan(info.Agents)
	return info, nil
}

func (n *nativeBackupsWebAccessor) Status() (nativebackup.Status, error) {
	if n.deps.backupMgr == nil {
		return nativebackup.Status{}, errors.New("native backup manager not wired")
	}
	var blocked map[string]string
	if n.deps.backupBlocker != nil {
		blocked = n.deps.backupBlocker.Snapshot()
	}
	status, err := n.deps.backupMgr.Status(blocked)
	if err != nil {
		return nativebackup.Status{}, err
	}
	status.Cloud = n.cloudBackupStatus(context.Background())
	if status.Schedule.Destination == "" {
		status.Schedule.Destination = "local"
	}
	if n.jobs != nil {
		status.Jobs = n.jobs.List()
	}
	return status, nil
}

func (n *nativeBackupsWebAccessor) Override(agent string) (nativebackup.SafetyStatus, error) {
	if n.deps.backupMgr == nil {
		return nativebackup.SafetyStatus{}, errors.New("native backup manager not wired")
	}
	status, err := n.deps.backupMgr.Override(agent)
	if err != nil {
		return nativebackup.SafetyStatus{}, err
	}
	n.clearBackupBlocksAndRescan([]string{agent})
	return status, nil
}

func (n *nativeBackupsWebAccessor) SetSchedule(cfg nativebackup.ScheduleConfig) (nativebackup.ScheduleConfig, error) {
	if n.deps.backupMgr == nil {
		return nativebackup.ScheduleConfig{}, errors.New("native backup manager not wired")
	}
	if cfg.Destination == "" {
		cfg.Destination = "local"
	}
	if cfg.Destination != "local" && cfg.Destination != "cloud" {
		return nativebackup.ScheduleConfig{}, fmt.Errorf("unsupported backup destination %q", cfg.Destination)
	}
	return n.deps.backupMgr.SaveSchedule(cfg)
}

func (n *nativeBackupsWebAccessor) SetRetention(cfg nativebackup.RetentionConfig) (nativebackup.RetentionConfig, error) {
	if n.deps.backupMgr == nil {
		return nativebackup.RetentionConfig{}, errors.New("native backup manager not wired")
	}
	return n.deps.backupMgr.SaveRetention(cfg)
}

func (n *nativeBackupsWebAccessor) Restore(ctx context.Context, backupID, agent, location string) (nativebackup.RestoreResult, error) {
	if location == "cloud" {
		return n.restoreCloudBackup(ctx, backupID, agent)
	}
	dir, err := resolveBackupDir(n.deps.backupsRoot, backupID)
	if err != nil {
		return nativebackup.RestoreResult{}, err
	}
	if n.deps.backupMgr == nil {
		return nativebackup.RestoreResult{}, errors.New("native backup manager not wired")
	}
	return n.deps.backupMgr.Restore(ctx, dir, agent)
}

func (n *nativeBackupsWebAccessor) Delete(ctx context.Context, backupID, location string) (nativebackup.BackupInfo, error) {
	if backupID == "" {
		return nativebackup.BackupInfo{}, errors.New("backupId is required")
	}
	if location == "cloud" {
		return n.deleteCloudBackup(ctx, backupID)
	}
	if location != "" && location != "local" {
		return nativebackup.BackupInfo{}, fmt.Errorf("unsupported backup location %q", location)
	}
	if n.deps.backupMgr == nil {
		return nativebackup.BackupInfo{}, errors.New("native backup manager not wired")
	}
	return n.deps.backupMgr.Delete(backupID)
}

func (n *nativeBackupsWebAccessor) clearBackupBlocksAndRescan(agents []string) {
	if len(agents) == 0 {
		return
	}
	rootsByAgent := map[string][]string{}
	if n.deps.backupMgr != nil {
		for _, ag := range n.deps.backupMgr.agentRoots() {
			rootsByAgent[ag.Name] = append([]string{}, ag.Roots...)
		}
	}
	for _, agent := range agents {
		if n.deps.backupBlocker != nil {
			n.deps.backupBlocker.Clear(agent)
		}
		if n.deps.orch != nil {
			n.deps.orch.ScanRoots(context.Background(), rootsByAgent[agent])
		}
	}
}

// conflictsWebAccessor satisfies api.ConflictsAccessor. Resolve writes the
// chosen payload as an EventTypeResolution event, then clears the conflict
// marker. This mirrors `aplexica conflicts resolve` so the web and CLI
// surfaces make the same durable change.
type conflictsWebAccessor struct {
	deps *webAPIDeps
}

func (c *conflictsWebAccessor) List() ([]conflicts.Conflict, error) {
	if c.deps.conf == nil {
		return []conflicts.Conflict{}, nil
	}
	return c.deps.conf.ListSummaries()
}

func (c *conflictsWebAccessor) ConflictListSummary(conflict conflicts.Conflict) (apiweb.ConflictListSummary, bool) {
	if conflict.Kind != acf.KindConversation || c.deps.store == nil {
		return apiweb.ConflictListSummary{}, false
	}
	art, err := c.deps.store.ReadArtifact(acf.KindConversation, conflict.ArtifactID)
	if err != nil {
		return apiweb.ConflictListSummary{}, false
	}
	summary, ok := (&conversationsWebAccessor{deps: c.deps}).conversationSummary(art)
	if !ok {
		return apiweb.ConflictListSummary{}, false
	}
	if strings.TrimSpace(summary.Title) == "" || summary.Title == conflict.ArtifactID {
		return apiweb.ConflictListSummary{}, false
	}
	return apiweb.ConflictListSummary{
		Title:       summary.Title,
		Description: summary.Description,
	}, true
}

func (c *conflictsWebAccessor) Get(id string) (conflicts.Conflict, bool, error) {
	if c.deps.conf == nil {
		return conflicts.Conflict{}, false, nil
	}
	got, err := c.deps.conf.Get(id)
	if err != nil {
		return conflicts.Conflict{}, false, nil
	}
	if c.clearIfAutoResolvable(got) {
		return got, true, nil
	}
	return got, true, nil
}

func (c *conflictsWebAccessor) clearIfAutoResolvable(conflict conflicts.Conflict) bool {
	if c.deps.conf == nil || c.deps.store == nil {
		return false
	}
	analysis, err := c.Analyze(conflict)
	if err != nil || analysis == nil || !analysis.AutoResolvable {
		return false
	}
	// Compare-and-delete: clear only if the on-disk conflict still matches the
	// one we analyzed. If the orchestrator recorded a NEW (non-equivalent)
	// conflict between Analyze and here, ClearIf is a no-op — closing the TOCTOU
	// where the unconditional Clear would have deleted a freshly-recorded
	// genuine conflict. Returns whether it actually auto-resolved.
	cleared, _ := c.deps.conf.ClearIf(conflict)
	return cleared
}

func (c *conflictsWebAccessor) Analyze(conflict conflicts.Conflict) (*apiweb.ConflictAnalysis, error) {
	if c.deps.store == nil {
		return nil, nil
	}
	return apiweb.AnalyzeConflict(conflict, func(ctx context.Context, kind acf.Kind, artifactID, eventID string) (acf.Event, bool, error) {
		if err := ctx.Err(); err != nil {
			return acf.Event{}, false, err
		}
		events, err := c.deps.store.ReadEvents(kind, artifactID)
		if err != nil {
			return acf.Event{}, false, err
		}
		for _, event := range events {
			if event.EventID == eventID {
				return event, true, nil
			}
		}
		return acf.Event{}, false, nil
	})
}

func (c *conflictsWebAccessor) Resolve(id, action, manualBody string) error {
	if c.deps.conf == nil {
		return errors.New("conflicts: store not wired")
	}
	if c.deps.store == nil {
		return errors.New("conflicts: canonical store not wired")
	}
	conflict, err := c.deps.conf.Get(id)
	if err != nil {
		return apiweb.ErrConflictNotFound
	}
	payload, err := c.resolutionPayload(conflict, action, manualBody)
	if err != nil {
		return err
	}
	if err := c.appendResolutionEvent(conflict.Kind, conflict.ArtifactID, payload, "aplexica:web-resolve"); err != nil {
		return err
	}
	return c.deps.conf.Clear(id)
}

func (c *conflictsWebAccessor) resolutionPayload(conflict conflicts.Conflict, action, manualBody string) (json.RawMessage, error) {
	switch action {
	case apiweb.ResolveAcceptA:
		return c.headPayload(conflict, 0)
	case apiweb.ResolveAcceptB:
		return c.headPayload(conflict, 1)
	case apiweb.ResolveManual:
		return c.manualPayload(conflict, manualBody)
	default:
		return nil, fmt.Errorf("conflicts: unsupported resolve action %q", action)
	}
}

func (c *conflictsWebAccessor) headPayload(conflict conflicts.Conflict, idx int) (json.RawMessage, error) {
	if idx < 0 || idx >= len(conflict.Heads) {
		return nil, fmt.Errorf("conflicts: head %d not present", idx)
	}
	head := conflict.Heads[idx]
	events, err := c.deps.store.ReadEvents(conflict.Kind, conflict.ArtifactID)
	if err != nil {
		return nil, fmt.Errorf("conflicts: read events: %w", err)
	}
	for _, event := range events {
		if event.EventID == head.EventID {
			return append(json.RawMessage(nil), event.Payload...), nil
		}
	}
	if len(head.FullPayload) > 0 {
		return append(json.RawMessage(nil), head.FullPayload...), nil
	}
	return nil, fmt.Errorf("conflicts: winner event %s not found in artifact log", head.EventID)
}

func (c *conflictsWebAccessor) manualPayload(conflict conflicts.Conflict, manualBody string) (json.RawMessage, error) {
	body := strings.TrimSpace(manualBody)
	if body == "" {
		return nil, errors.New("conflicts: manual body is required")
	}
	if strings.HasPrefix(body, "{") {
		var meta struct {
			Format string `json:"format"`
		}
		if err := json.Unmarshal([]byte(body), &meta); err == nil && meta.Format != "" {
			return append(json.RawMessage(nil), body...), nil
		}
	}
	format := c.manualPayloadFormat(conflict)
	switch conflict.Kind {
	case acf.KindMemory:
		if format == "" {
			format = "markdown"
		}
		return acf.EncodePayload(acf.MemoryPayload{Format: format, Content: manualBody})
	case acf.KindSkill:
		if format == "" {
			format = "skill.md"
		}
		return acf.EncodePayload(acf.SkillPayload{Format: format, Content: manualBody})
	case acf.KindTool:
		if format == "" {
			format = "manual"
		}
		return acf.EncodePayload(acf.ToolPayload{Format: format, Content: manualBody})
	case acf.KindConversation:
		return nil, errors.New("conflicts: manual conversation merge requires a full JSON conversation payload")
	default:
		return nil, fmt.Errorf("conflicts: manual merge is not supported for kind %s", conflict.Kind)
	}
}

func (c *conflictsWebAccessor) manualPayloadFormat(conflict conflicts.Conflict) string {
	for i := range conflict.Heads {
		payload, err := c.headPayload(conflict, i)
		if err != nil || len(payload) == 0 {
			continue
		}
		var meta struct {
			Format string `json:"format"`
		}
		if json.Unmarshal(payload, &meta) == nil && meta.Format != "" {
			return meta.Format
		}
	}
	return ""
}

func (c *conflictsWebAccessor) appendResolutionEvent(kind acf.Kind, artifactID string, payload json.RawMessage, sourceAgent string) error {
	deviceID := ""
	if c.deps.orch != nil {
		deviceID = c.deps.orch.LocalDeviceID()
	}
	return appendConflictResolutionEvent(c.deps.store, kind, artifactID, payload, sourceAgent, deviceID)
}

// conversationsWebAccessor satisfies api.ConversationsAccessor.
type conversationsWebAccessor struct {
	deps *webAPIDeps

	// readFirstEvent overrides the lightweight canonical-store event reader
	// in tests.
	readFirstEvent func(acf.Kind, string) (acf.Event, bool, error)
}

func (c *conversationsWebAccessor) readFirstEventFn() func(acf.Kind, string) (acf.Event, bool, error) {
	if c.readFirstEvent != nil {
		return c.readFirstEvent
	}
	return c.deps.store.ReadFirstEvent
}

func (c *conversationsWebAccessor) SearchConversations(q apiweb.ConversationSearchQuery) (apiweb.ConversationSearchResponse, error) {
	out := apiweb.ConversationSearchResponse{
		Query:         strings.TrimSpace(q.Query),
		Conversations: []apiweb.ConversationSummary{},
	}
	if c.deps.store == nil {
		return out, nil
	}
	limit := q.Limit
	if limit <= 0 {
		limit = conversationSearchDefaultLimit
	}
	if limit > conversationSearchMaxLimit {
		limit = conversationSearchMaxLimit
	}
	needle := strings.ToLower(out.Query)

	arts, err := c.deps.store.ListArtifacts(acf.KindConversation)
	if err != nil {
		return out, err
	}
	sort.SliceStable(arts, func(i, j int) bool {
		if arts[i].UpdatedAt.Equal(arts[j].UpdatedAt) {
			return arts[i].ArtifactID > arts[j].ArtifactID
		}
		return arts[i].UpdatedAt.After(arts[j].UpdatedAt)
	})
	scanLimit := conversationSearchScanLimit
	if needle == "" && limit < scanLimit {
		scanLimit = limit
	}

	scannedUnique := 0
	seenSourcePaths := map[string]struct{}{}
	for _, art := range arts {
		if key := conversationSourcePathDedupeKey(art); key != "" {
			if _, seen := seenSourcePaths[key]; seen {
				continue
			}
			seenSourcePaths[key] = struct{}{}
		}
		if scannedUnique >= scanLimit {
			break
		}
		scannedUnique++
		summary, ok := c.conversationSummary(art)
		if !ok {
			continue
		}
		if needle != "" && !strings.Contains(conversationSummaryHaystack(summary), needle) {
			continue
		}
		out.Conversations = append(out.Conversations, summary)
		if len(out.Conversations) >= limit {
			break
		}
	}
	return out, nil
}

func conversationSourcePathDedupeKey(art acf.Artifact) string {
	return strings.TrimSpace(art.SourcePath)
}

func (c *conversationsWebAccessor) conversationSummary(art acf.Artifact) (apiweb.ConversationSummary, bool) {
	first, hasFirst, err := c.readFirstEventFn()(acf.KindConversation, art.ArtifactID)
	if err != nil {
		return apiweb.ConversationSummary{}, false
	}
	var events []acf.Event
	if hasFirst {
		events = []acf.Event{first}
	}
	summary := fallbackConversationSummary(art, events)
	if hasFirst && applyConversationPayloadSummary(&summary, art, first) {
		return summary, true
	}

	last, hasLast, err := c.deps.store.LastEvent(acf.KindConversation, art.ArtifactID)
	if err != nil {
		return summary, true
	}
	if hasLast && (!hasFirst || !sameConversationEvent(first, last)) {
		applyConversationPayloadSummary(&summary, art, last)
	}
	return summary, true
}

func applyConversationPayloadSummary(summary *apiweb.ConversationSummary, art acf.Artifact, ev acf.Event) bool {
	if !acf.HasPayload(ev.Payload) {
		return false
	}
	payload, err := acf.DecodeConversationPayload(ev)
	if err != nil {
		return false
	}
	turns, supported := acf.ConversationTextTurns(payload)
	if !supported || len(turns) == 0 {
		return false
	}
	title, description := conversationTitleAndDescription(turns, art.Name)
	summary.Title = title
	summary.Description = description
	summary.TurnCount = len(turns)
	summary.SearchText = conversationSearchText(turns)
	if summary.SourceAgent == "" {
		summary.SourceAgent = ev.Provenance.SourceAgent
	}
	return true
}

func sameConversationEvent(a, b acf.Event) bool {
	if a.Hash != "" && b.Hash != "" {
		return a.Hash == b.Hash
	}
	return a.EventID != "" && a.EventID == b.EventID
}

func fallbackConversationSummary(art acf.Artifact, events []acf.Event) apiweb.ConversationSummary {
	sourceAgent := ""
	if len(events) > 0 {
		sourceAgent = events[0].Provenance.SourceAgent
	}
	title := strings.TrimSpace(art.Name)
	if title == "" {
		title = art.ArtifactID
	}
	return apiweb.ConversationSummary{
		ArtifactID:     art.ArtifactID,
		Title:          title,
		SourceAgent:    sourceAgent,
		SourcePath:     redactedDisplayPath(art.SourcePath),
		CreatedAt:      art.CreatedAt,
		UpdatedAt:      art.UpdatedAt,
		BranchCount:    artifactBranchCount(art),
		MaterializedIn: materializedAgentsForArtifact(art),
	}
}

func conversationTitleAndDescription(turns []acf.TextTurn, fallback string) (string, string) {
	var firstUser, firstAssistant, latestUser string
	for _, turn := range turns {
		text := compactConversationSummaryText(turn.Text)
		if text == "" {
			continue
		}
		if turn.Role == "user" {
			if firstUser == "" {
				firstUser = text
			}
			latestUser = text
			continue
		}
		if turn.Role == "assistant" && firstAssistant == "" {
			firstAssistant = text
		}
	}
	title := firstUser
	if title == "" {
		title = firstAssistant
	}
	if title == "" {
		title = strings.TrimSpace(fallback)
	}
	if title == "" {
		title = "Conversation"
	}
	description := latestUser
	if description == title {
		description = firstAssistant
	}
	if description == title {
		description = ""
	}
	return title, description
}

func compactConversationSummaryText(text string) string {
	text = stripConversationSummaryScaffolding(text)
	return strings.Join(strings.Fields(strings.TrimSpace(text)), " ")
}

var conversationEmbeddedMediaRe = regexp.MustCompile(`(?s)<image\b.*?</image>|<video\b.*?</video>`)

func stripConversationSummaryScaffolding(text string) string {
	for _, marker := range []string{
		"## My request for Codex:",
		"My request for Codex:",
	} {
		if idx := strings.Index(text, marker); idx >= 0 {
			text = text[idx+len(marker):]
			break
		}
	}
	return conversationEmbeddedMediaRe.ReplaceAllString(text, " ")
}

func conversationSearchText(turns []acf.TextTurn) string {
	parts := make([]string, 0, conversationSummaryPreviewTurns)
	for _, turn := range turns {
		text := compactConversationSummaryText(turn.Text)
		if text == "" {
			continue
		}
		parts = append(parts, text)
		if len(parts) >= conversationSummaryPreviewTurns {
			break
		}
	}
	return strings.Join(parts, " ")
}

func artifactBranchCount(art acf.Artifact) int {
	seen := map[string]struct{}{acf.MainBranch: {}}
	for branch := range art.BranchHeads {
		if branch == "" {
			branch = acf.MainBranch
		}
		seen[branch] = struct{}{}
	}
	return len(seen)
}

func materializedAgentsForArtifact(art acf.Artifact) []string {
	if len(art.MaterializedBranchByAgent) == 0 {
		return []string{}
	}
	out := make([]string, 0, len(art.MaterializedBranchByAgent))
	for agent := range art.MaterializedBranchByAgent {
		if agent != "" {
			out = append(out, agent)
		}
	}
	sort.Strings(out)
	return out
}

func conversationSummaryHaystack(summary apiweb.ConversationSummary) string {
	return strings.ToLower(strings.Join([]string{
		summary.ArtifactID,
		summary.Title,
		summary.Description,
		summary.SearchText,
		summary.SourceAgent,
		summary.SourcePath,
		strings.Join(summary.MaterializedIn, " "),
	}, " "))
}

// conversationBranchesWebAccessor satisfies api.ConversationBranchesAccessor.
// It deliberately mirrors the CLI fork/checkout semantics so the local web UI
// is a control surface over the same durable branch model, not a parallel path.
type conversationBranchesWebAccessor struct {
	deps *webAPIDeps
}

func (c *conversationBranchesWebAccessor) ListConversationBranches(id string) (apiweb.ConversationBranchesResponse, bool, error) {
	if c.deps.store == nil {
		return apiweb.ConversationBranchesResponse{}, false, errors.New("conversation branches: canonical store not wired")
	}
	art, err := c.deps.store.ReadArtifact(acf.KindConversation, id)
	if errors.Is(err, os.ErrNotExist) {
		return apiweb.ConversationBranchesResponse{}, false, nil
	}
	if err != nil {
		return apiweb.ConversationBranchesResponse{}, false, err
	}
	branches, err := c.deps.store.ListBranches(acf.KindConversation, id, true)
	if err != nil {
		return apiweb.ConversationBranchesResponse{}, false, err
	}
	materialized := materializedAgentsByBranch(art)
	out := apiweb.ConversationBranchesResponse{
		ArtifactID: id,
		Branches:   make([]apiweb.ConversationBranch, 0, len(branches)),
	}
	for _, b := range branches {
		agents := materialized[b.Name]
		if agents == nil {
			agents = []string{}
		}
		out.Branches = append(out.Branches, apiweb.ConversationBranch{
			Name:               b.Name,
			CreatedAt:          b.CreatedAt,
			LastEventAt:        b.LastEventAt,
			Head:               b.Head,
			ForkedFrom:         b.ForkedFrom,
			ForkedFromHash:     b.ForkedFromHash,
			OriginAgent:        b.OriginAgent,
			Rationale:          b.Rationale,
			Archived:           b.Archived,
			MergedInto:         b.MergedInto,
			EventCount:         b.EventCount,
			MaterializedAgents: agents,
		})
	}
	return out, true, nil
}

func (c *conversationBranchesWebAccessor) ForkConversation(id string, req apiweb.ConversationForkRequest) (apiweb.ConversationBranchMutationResponse, error) {
	if c.deps.store == nil {
		return apiweb.ConversationBranchMutationResponse{}, errors.New("conversation branches: canonical store not wired")
	}
	if _, err := c.deps.store.ReadArtifact(acf.KindConversation, id); errors.Is(err, os.ErrNotExist) {
		return apiweb.ConversationBranchMutationResponse{}, apiweb.ErrConversationNotFound
	} else if err != nil {
		return apiweb.ConversationBranchMutationResponse{}, err
	}
	events, err := c.deps.store.ReadEvents(acf.KindConversation, id)
	if err != nil {
		return apiweb.ConversationBranchMutationResponse{}, err
	}
	var parent *acf.Event
	for i := range events {
		if events[i].EventID == req.FromEventID || events[i].Hash == req.FromEventID {
			parent = &events[i]
			break
		}
	}
	if parent == nil {
		return apiweb.ConversationBranchMutationResponse{}, fmt.Errorf("%w: event %q not found on conversation %s",
			apiweb.ErrConversationBranchNotFound, req.FromEventID, id)
	}

	branch := strings.TrimSpace(req.Branch)
	if branch == "" {
		shortHash := parent.Hash
		if len(shortHash) > shortHashLen {
			shortHash = shortHash[:shortHashLen]
		}
		branch = fmt.Sprintf("%s-%s", shortHash, req.TargetAgent)
	}
	normalized, err := acf.NormalizeBranchName(branch)
	if err != nil {
		return apiweb.ConversationBranchMutationResponse{}, err
	}
	srcBranch := parent.Branch
	if srcBranch == "" {
		srcBranch = acf.MainBranch
	}
	originAgent := parent.Provenance.SourceAgent
	if originAgent == "" {
		originAgent = "aplexica"
	}
	event := acf.Event{
		EventID:          acf.NewID(),
		ArtifactID:       id,
		Type:             acf.EventTypeForkOuter,
		Timestamp:        time.Now().UTC(),
		ParentHash:       parent.Hash,
		Branch:           normalized,
		ForkSourceBranch: srcBranch,
		ForkFromEventID:  parent.EventID,
		ForkOriginAgent:  originAgent,
		ForkRationale:    strings.TrimSpace(req.Rationale),
		Provenance: acf.Provenance{
			SourceAgent:    "aplexica:web-fork",
			AdapterVersion: "aplexica:web",
		},
	}
	if err := c.deps.store.AppendEvent(acf.KindConversation, event); err != nil {
		return apiweb.ConversationBranchMutationResponse{}, err
	}
	art, err := c.deps.store.ReadArtifact(acf.KindConversation, id)
	if err != nil {
		return apiweb.ConversationBranchMutationResponse{}, err
	}
	if art.MaterializedBranchByAgent == nil {
		art.MaterializedBranchByAgent = map[string]string{}
	}
	art.MaterializedBranchByAgent[req.TargetAgent] = normalized
	if err := c.deps.store.WriteArtifact(art); err != nil {
		return apiweb.ConversationBranchMutationResponse{}, err
	}
	if _, err := c.deps.store.RefreshBranchIndex(acf.KindConversation, id); err != nil {
		return apiweb.ConversationBranchMutationResponse{}, err
	}
	_ = journalBranchOp(c.deps.store.Root, "web-fork", map[string]any{
		"artifactId": id,
		"kind":       string(acf.KindConversation),
		"branch":     normalized,
		"from":       parent.Hash,
		"toAgent":    req.TargetAgent,
		"rationale":  req.Rationale,
	})

	out := apiweb.ConversationBranchMutationResponse{
		ArtifactID:    id,
		Branch:        normalized,
		Agent:         req.TargetAgent,
		Operation:     "fork",
		CreatedBranch: true,
	}
	return c.materializeInto(out)
}

func (c *conversationBranchesWebAccessor) CheckoutConversation(id string, req apiweb.ConversationCheckoutRequest) (apiweb.ConversationBranchMutationResponse, error) {
	if c.deps.store == nil {
		return apiweb.ConversationBranchMutationResponse{}, errors.New("conversation branches: canonical store not wired")
	}
	art, err := c.deps.store.ReadArtifact(acf.KindConversation, id)
	if errors.Is(err, os.ErrNotExist) {
		return apiweb.ConversationBranchMutationResponse{}, apiweb.ErrConversationNotFound
	}
	if err != nil {
		return apiweb.ConversationBranchMutationResponse{}, err
	}
	normalized, err := acf.NormalizeBranchName(req.Branch)
	if err != nil {
		return apiweb.ConversationBranchMutationResponse{}, err
	}
	bi, err := c.deps.store.RefreshBranchIndex(acf.KindConversation, id)
	if err != nil {
		return apiweb.ConversationBranchMutationResponse{}, err
	}
	info, ok := bi.Branches[normalized]
	if !ok {
		return apiweb.ConversationBranchMutationResponse{}, fmt.Errorf("%w: branch %q does not exist on conversation %s",
			apiweb.ErrConversationBranchNotFound, normalized, id)
	}
	if info.Archived {
		return apiweb.ConversationBranchMutationResponse{}, fmt.Errorf("conversation branch %q is archived", normalized)
	}
	if art.MaterializedBranchByAgent == nil {
		art.MaterializedBranchByAgent = map[string]string{}
	}
	previous := art.MaterializedBranchByAgent[req.Agent]
	art.MaterializedBranchByAgent[req.Agent] = normalized
	if err := c.deps.store.WriteArtifact(art); err != nil {
		return apiweb.ConversationBranchMutationResponse{}, err
	}
	_ = journalBranchOp(c.deps.store.Root, "web-checkout", map[string]any{
		"artifactId": id,
		"agent":      req.Agent,
		"branch":     normalized,
		"previous":   previous,
	})

	out := apiweb.ConversationBranchMutationResponse{
		ArtifactID: id,
		Branch:     normalized,
		Agent:      req.Agent,
		Operation:  "checkout",
	}
	return c.materializeInto(out)
}

func (c *conversationBranchesWebAccessor) materializeInto(out apiweb.ConversationBranchMutationResponse) (apiweb.ConversationBranchMutationResponse, error) {
	if c.deps.orch == nil {
		out.Warning = "daemon materializer is not wired; branch pointer was saved"
		return out, nil
	}
	path, materialized, err := c.deps.orch.MaterializeConversationBranch(out.ArtifactID, out.Agent, out.Branch)
	if err != nil {
		out.Warning = err.Error()
		return out, nil
	}
	out.Path = path
	out.Materialized = materialized
	return out, nil
}

// pendingWebAccessor satisfies api.PendingAccessor. Link adds the
// project to the registry and triggers refanout.
type pendingWebAccessor struct {
	deps *webAPIDeps
}

func (p *pendingWebAccessor) List() ([]pending.Project, error) {
	if p.deps.pendingFn != nil {
		return p.deps.pendingFn()
	}
	return []pending.Project{}, nil
}

func (p *pendingWebAccessor) Link(id, localPath string) error {
	if p.deps.projectReg == nil {
		return errors.New("pending: project registry not wired")
	}
	// Verify the project is actually pending.
	got, err := p.List()
	if err != nil {
		return err
	}
	found := false
	for _, pr := range got {
		if pr.ID == id {
			found = true
			break
		}
	}
	if !found {
		return apiweb.ErrPendingNotFound
	}
	abs, err := filepath.Abs(localPath)
	if err != nil {
		return fmt.Errorf("link: resolve path: %w", err)
	}
	if err := p.deps.projectReg.Add(project.Entry{ID: id, Path: abs, DisplayName: id}); err != nil {
		return err
	}
	if p.deps.orch != nil {
		_, _ = p.deps.orch.RefanOutByProject(id)
	}
	return nil
}

// configWebAccessor satisfies api.ConfigAccessor. PATCH applies the
// whitelisted keys onto the on-disk config.json and re-writes it
// atomically; the daemon picks up the new values on the next SIGHUP /
// control-socket reload (live reload of the listener config itself is
// V1.1).
type configWebAccessor struct {
	deps *webAPIDeps
}

func (c *configWebAccessor) Load() (*daemon.Config, error) {
	return daemon.LoadConfig(c.deps.configPath)
}

func (c *configWebAccessor) Patch(updates map[string]any) error {
	cfg, err := daemon.LoadConfig(c.deps.configPath)
	if err != nil {
		return err
	}
	for k, v := range updates {
		switch k {
		case "logLevel":
			if s, ok := v.(string); ok {
				cfg.LogLevel = s
			}
		case "storeHighWatermarkGB":
			if f, ok := v.(float64); ok {
				cfg.StoreHighWatermarkGB = f
			}
		case "snapshotCadenceConversation":
			if f, ok := v.(float64); ok {
				cfg.SnapshotCadenceConversation = int(f)
			}
		case "snapshotCadenceMemory":
			if f, ok := v.(float64); ok {
				cfg.SnapshotCadenceMemory = int(f)
			}
		case "snapshotCadenceSkill":
			if f, ok := v.(float64); ok {
				cfg.SnapshotCadenceSkill = int(f)
			}
		case "snapshotCadenceTool":
			if f, ok := v.(float64); ok {
				cfg.SnapshotCadenceTool = int(f)
			}
		case "snapshotMaxAgeConversation":
			if d, ok := durationFromAny(v); ok {
				cfg.SnapshotMaxAgeConversation = d
			}
		case "snapshotMaxAgeMemory":
			if d, ok := durationFromAny(v); ok {
				cfg.SnapshotMaxAgeMemory = d
			}
		case "snapshotMaxAgeSkill":
			if d, ok := durationFromAny(v); ok {
				cfg.SnapshotMaxAgeSkill = d
			}
		case "snapshotMaxAgeTool":
			if d, ok := durationFromAny(v); ok {
				cfg.SnapshotMaxAgeTool = d
			}
		case "hermesWatchInterval":
			if d, ok := durationFromAny(v); ok {
				cfg.HermesWatchInterval = d
			}
		case "tray":
			if m, ok := v.(map[string]any); ok {
				if en, ok := m["enabled"].(bool); ok {
					cfg.Tray.Enabled = &en
				}
			}
		case "web":
			if m, ok := v.(map[string]any); ok {
				if en, ok := m["enabled"].(bool); ok {
					cfg.Web.Enabled = &en
				}
				if p, ok := m["port"].(float64); ok {
					cfg.Web.Port = int(p)
				}
			}
		}
	}
	return daemon.WriteConfig(c.deps.configPath, cfg)
}

func (c *configWebAccessor) RawPath() string {
	return c.deps.configPath
}

// durationFromAny accepts a Go duration string ("5s", "1h") OR a JSON
// number and returns a time.Duration. Numbers are interpreted as
// nanoseconds so they match exactly the wire shape GET emits (a
// time.Duration marshals to its int64 nanosecond count); this keeps the
// natural SPA read-modify-write — GET the config, change a field, PATCH
// it back — lossless. Human-readable strings remain accepted for
// ergonomics.
func durationFromAny(v any) (time.Duration, bool) {
	switch x := v.(type) {
	case string:
		d, err := time.ParseDuration(x)
		if err != nil {
			return 0, false
		}
		return d, true
	case float64:
		// Nanoseconds, matching what GET serializes for a time.Duration.
		return time.Duration(int64(x)), true
	}
	return 0, false
}

// transportWebAccessor satisfies api.TransportAccessor. V1 OSS is
// hard-pinned to transport.LocalOnly; Set("local") is a no-op success.
type transportWebAccessor struct{}

func (transportWebAccessor) Get() transport.Info { return transport.LocalOnly }

func (transportWebAccessor) Set(mode string) error {
	if mode == string(transport.ModeLocal) {
		return nil
	}
	return transport.ErrModeUnsupported
}

func (transportWebAccessor) SetBYO(_ transport.BYORelayOpts) error {
	return transport.ErrBYONotInOSS
}

// onboardingWebAccessor satisfies api.OnboardingAccessor. Inputs are
// computed on each request from live daemon state.
type onboardingWebAccessor struct {
	deps *webAPIDeps
}

func (o *onboardingWebAccessor) State() onboarding.State {
	in := onboarding.Inputs{
		AdapterCount: len(o.deps.adapters),
	}
	if o.deps.orch != nil {
		in.LastSyncActivity = o.deps.orch.LastActivity()
	}
	return onboarding.Compute(in)
}

// daemonWebAccessor — RemoteStatusAccessor methods.
//
// These satisfy api.RemoteStatusAccessor so GET /api/daemon's response
// gains a `remote` sub-object summarising the configured remote-plugin
// state (configured, enabled, conn_state, restart_count). The handler
// casts via type-assertion; OSS-only daemons that don't construct a
// RemoteRunner still satisfy the cast (RemoteConfigured returns false
// when remoteCfg.Executable is empty).

// RemoteConfigured reports whether a remote plugin's path is set in
// the daemon's config — regardless of whether it's enabled or running.
func (d *daemonWebAccessor) RemoteConfigured() bool {
	return d.deps.remoteCfg.Executable != ""
}

// RemoteEnabled reports whether the configured plugin is enabled in
// config. False when no plugin is configured.
func (d *daemonWebAccessor) RemoteEnabled() bool {
	cfg := daemon.Config{Remote: d.deps.remoteCfg}
	return daemon.RemoteEnabled(&cfg)
}

// RemoteConnState returns the latest cached connectivity label from
// the RemoteRunner. "unknown" when no runner is wired (OSS-only daemon).
func (d *daemonWebAccessor) RemoteConnState() string {
	if d.deps.remoteRunner == nil {
		if !d.RemoteConfigured() {
			return "unconfigured"
		}
		return "unknown"
	}
	return d.deps.remoteRunner.ConnState()
}

// RemoteRestartCount returns the cumulative plugin restart count since
// daemon startup. 0 when no runner is wired.
func (d *daemonWebAccessor) RemoteRestartCount() uint64 {
	if d.deps.remoteRunner == nil {
		return 0
	}
	return d.deps.remoteRunner.RestartCount()
}

// userRulesPath resolves the user-rules file path for the daemon. The
// global rulesPath is a flag-bound variable that the daemon subcommand
// doesn't set; we fall back to ~/.aplexica/rules.toml here.
func userRulesPath() string {
	if rulesPath != "" {
		return rulesPath
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".aplexica", "rules.toml")
}

// remoteWebAccessor satisfies api.RemoteAccessor by shelling out to the
// configured remote (cloud) plugin's CLI. The OSS daemon stays plugin-
// agnostic: it knows only how to exec the binary and parse the
// documented stdout lines of its --pair / --status / --connect-check
// entry points. All cloud specifics (broker URLs, mTLS, account model)
// live behind that boundary in the plugin.
type remoteWebAccessor struct {
	deps *webAPIDeps
}

// remoteExecTimeout bounds every plugin invocation so a hung plugin
// can't wedge a web request indefinitely. Pairing in particular makes a
// network round-trip to the cloud, so this is generous.
const remoteExecTimeout = 35 * time.Second

type boundedCommandOutput struct {
	buffer bytes.Buffer
	max    int
}

func (w *boundedCommandOutput) Write(p []byte) (int, error) {
	if w.buffer.Len()+len(p) > w.max {
		remain := w.max - w.buffer.Len()
		if remain > 0 {
			_, _ = w.buffer.Write(p[:remain])
		}
		return len(p), fmt.Errorf("plugin output exceeded limit")
	}
	return w.buffer.Write(p)
}

func runRemoteCommandBounded(cmd *exec.Cmd) ([]byte, error) {
	out := &boundedCommandOutput{max: 64 << 10}
	cmd.Stdout, cmd.Stderr = out, out
	err := cmd.Run()
	return append([]byte(nil), out.buffer.Bytes()...), err
}

type preparedRemotePluginCommand interface {
	Cmd() *exec.Cmd
	Close() error
}

type remotePluginCommandPreparer func(context.Context, string, [32]byte, ...string) (preparedRemotePluginCommand, error)
type remotePluginCommandPreparerForPath func(context.Context, string, ...string) (preparedRemotePluginCommand, error)

// pairLineRe matches the plugin's success line:
//
//	paired: device_id=<id> account_id=<acct>
//
// Both IDs are opaque tokens (no whitespace). Extra trailing fields are
// tolerated.
var pairLineRe = regexp.MustCompile(`device_id=(\S+)\s+account_id=(\S+)`)

// remoteExecBitOK reports whether mode permits executing the plugin. On
// Windows there is no POSIX exec bit (files land mode 0666 via scp/copy),
// so the check is skipped there — matching cmd_remote.go's install path,
// which only warns (never fails) on a missing exec bit. Without this,
// Pair/Verify/Unpair would wrongly report the plugin "not configured" on
// Windows even though the daemon spawns it fine.
func remoteExecBitOK(mode os.FileMode) bool {
	return runtime.GOOS == "windows" || mode.Perm()&0o111 != 0
}

// remoteExecPath returns the validated plugin executable path, or
// ErrRemoteNotConfigured when no plugin is configured / it is missing /
// it isn't executable. Mirrors the stat + exec-bit checks in
// cmd_remote.go's install path.
func (a *remoteWebAccessor) remoteExecPath() (string, error) {
	execPath := a.deps.remoteCfg.Executable
	if execPath == "" {
		return "", apiweb.ErrRemoteNotConfigured
	}
	st, err := os.Stat(execPath)
	if err != nil {
		return "", fmt.Errorf("%w: stat %s: %v", apiweb.ErrRemoteNotConfigured, execPath, err)
	}
	if st.IsDir() {
		return "", fmt.Errorf("%w: %s is a directory", apiweb.ErrRemoteNotConfigured, execPath)
	}
	if !remoteExecBitOK(st.Mode()) {
		return "", fmt.Errorf("%w: %s is not executable (mode %#o)", apiweb.ErrRemoteNotConfigured, execPath, st.Mode().Perm())
	}
	verified, err := a.verifyRemotePluginRuntime(execPath)
	if err != nil {
		return "", fmt.Errorf("%w: signed plugin verification failed: %v", apiweb.ErrRemoteNotConfigured, err)
	}
	if !verified.Manifest.HasCapability(proto.CapabilityPairStdinV1) || !verified.Manifest.HasCapability(proto.CapabilityTrustProtocolV1) || !verified.Manifest.HasCapability(proto.CapabilityInboundAckV2) {
		return "", fmt.Errorf("%w: plugin upgrade required for secure pairing", apiweb.ErrRemoteNotConfigured)
	}
	return execPath, nil
}

func (a *remoteWebAccessor) verifyRemotePluginRuntime(execPath string) (proto.VerifiedRemotePlugin, error) {
	verify := a.deps.remotePluginVerifier
	if verify == nil {
		verify = verifyRemotePluginWithCompiledTrust
	}
	verified, err := verify(execPath)
	if err != nil {
		return proto.VerifiedRemotePlugin{}, err
	}
	if a.deps.remotePluginCheckpointVerifier != nil {
		if err := a.deps.remotePluginCheckpointVerifier(execPath, verified); err != nil {
			return proto.VerifiedRemotePlugin{}, err
		}
	} else {
		store := truststate.Store{Root: filepath.Join(a.deps.stateDir, "remote-plugin-trust")}
		if _, err := store.VerifyCurrent(execPath, verified, remotePluginTrustPolicy()); err != nil {
			return proto.VerifiedRemotePlugin{}, err
		}
	}
	return verified, nil
}

// prepareConfiguredRemotePluginCommand authenticates the complete signed
// runtime and checkpoint, then captures an OS launch primitive which cannot be
// redirected to different pathname bytes before process start.
func (a *remoteWebAccessor) prepareConfiguredRemotePluginCommand(ctx context.Context, execPath string, args ...string) (preparedRemotePluginCommand, error) {
	if a == nil || a.deps == nil || a.deps.remoteCfg.Executable != execPath {
		return nil, errors.New("configured remote plugin path changed before launch")
	}
	verified, err := a.verifyRemotePluginRuntime(execPath)
	if err != nil {
		return nil, err
	}
	prepare := a.deps.remotePluginCommandPreparer
	if prepare == nil {
		prepare = func(ctx context.Context, path string, digest [32]byte, args ...string) (preparedRemotePluginCommand, error) {
			return secureexec.Prepare(ctx, path, digest, args...)
		}
	}
	prepared, err := prepare(ctx, execPath, verified.Manifest.BinarySHA256, args...)
	if err != nil {
		return nil, err
	}
	if prepared == nil || prepared.Cmd() == nil {
		if prepared != nil {
			_ = prepared.Close()
		}
		return nil, errors.New("remote plugin command preparer returned no command")
	}
	hideRemotePluginWindow(prepared.Cmd())
	return prepared, nil
}

// Pair runs `<plugin> --pair <token> --device-name <name>`, parses the
// cloud-assigned identifiers from stdout, then (best-effort) restarts the
// running plugin so it re-reads the freshly written credentials and
// reconnects.
func (a *remoteWebAccessor) Pair(ctx context.Context, token, deviceName string) (string, string, error) {
	execPath, err := a.remoteExecPath()
	if err != nil {
		return "", "", err
	}
	cctx, cancel := context.WithTimeout(ctx, remoteExecTimeout)
	defer cancel()

	args := []string{"--pair-stdin"}
	if deviceName != "" {
		args = append(args, "--device-name", deviceName)
	}
	prepared, err := a.prepareConfiguredRemotePluginCommand(cctx, execPath, args...)
	if err != nil {
		return "", "", fmt.Errorf("%w: plugin identity changed before launch: %v", apiweb.ErrPairFailed, err)
	}
	defer func() { _ = prepared.Close() }()
	cmd := prepared.Cmd()
	cmd.Stdin = strings.NewReader(token)
	// Hand the plugin this device's installed-agent inventory so it can
	// report it to the cloud at pairing time (drives the portal's
	// per-device routing-rule targets). Inherit the rest of the env.
	cmd.Env = os.Environ()
	if env := a.installedAgentsEnv(); env != "" {
		cmd.Env = append(cmd.Env, env)
	}
	// Hand the plugin this device's X25519 wrap public key so it can register
	// it with the cloud at pairing time. The daemon owns the keypair
	// (the private half never leaves the secrets store); the plugin only
	// forwards the public half.
	if env := a.wrapPubKeyEnv(); env != "" {
		cmd.Env = append(cmd.Env, env)
	}
	out, err := runRemoteCommandBounded(cmd)
	for token != "" && bytes.Contains(out, []byte(token)) {
		out = bytes.ReplaceAll(out, []byte(token), []byte("[redacted]"))
	}
	if err != nil {
		trimmed := strings.TrimSpace(string(out))
		if trimmed == "" {
			trimmed = err.Error()
		}
		return "", "", fmt.Errorf("%w: %s", apiweb.ErrPairFailed, trimmed)
	}

	deviceID, accountID := parsePairOutput(string(out))
	if deviceID == "" || accountID == "" {
		return "", "", fmt.Errorf("%w: could not parse device_id/account_id from plugin output: %s",
			apiweb.ErrPairFailed, strings.TrimSpace(string(out)))
	}

	// Credentials are now on disk. Propagate the (possibly rotated) cloud
	// device id to every stamping component — runner, orchestrator, adapter
	// provenance, control-socket status — BEFORE the bounce, so no outbound
	// event authored in the gap carries the retired identity (the
	// orchestrator setter WARNs the old→new rotation). Then bounce the
	// running plugin so it re-reads the credentials and connects. A nil
	// runner (plugin configured but not started, or OSS-only build) means
	// there is nothing to bounce — the pairing still succeeded, so we don't
	// treat that as an error.
	applyCloudDeviceIdentity(deviceID, a.deps.remoteRunner, a.deps.orch, a.deps.adapters, a.deps.ctl)
	if a.deps.remoteRunner != nil {
		_ = a.deps.remoteRunner.Restart(ctx)
	}
	return deviceID, accountID, nil
}

// installedAgentsEnv builds the APLEXICA_DEVICE_AGENTS env entry the
// plugin reads at --pair time: a JSON array of {name,version} for the
// agents discovered as installed on this device. Returns "" when nothing
// is installed (or deps are unwired), in which case no env is set and
// the plugin reports an empty inventory.
func (a *remoteWebAccessor) installedAgentsEnv() string {
	if a.deps == nil || len(a.deps.adapters) == 0 {
		return ""
	}
	type agentRef struct {
		Name    string `json:"name"`
		Version string `json:"version,omitempty"`
	}
	refs := make([]agentRef, 0, len(a.deps.adapters))
	for _, ad := range a.deps.adapters {
		// Only report agents whose native storage was actually found.
		if d := a.deps.discoveryFor(ad); !d.Installed {
			continue
		}
		refs = append(refs, agentRef{Name: ad.Name(), Version: ad.Version()})
	}
	if len(refs) == 0 {
		return ""
	}
	b, err := json.Marshal(refs)
	if err != nil {
		return ""
	}
	return "APLEXICA_DEVICE_AGENTS=" + string(b)
}

// wrapPubKeyEnv builds the APLEXICA_WRAP_PUBKEY env entry (base64 of the
// device's 32-byte X25519 wrap public key) the plugin forwards to the cloud
// at pairing. Returns "" when the secrets root is unknown or the key can't be
// loaded — pairing still proceeds (the device just won't be a wrap target
// until it re-pairs once a key exists). The private half stays on disk in the
// secrets store; only the public half is emitted.
func (a *remoteWebAccessor) wrapPubKeyEnv() string {
	if a.deps == nil || a.deps.secretsRoot == "" {
		return ""
	}
	st := &secrets.Store{Root: a.deps.secretsRoot}
	if err := st.Init(); err != nil {
		return ""
	}
	_, pub, err := keys.NewDeviceKeyStore(st).LoadOrCreate()
	if err != nil {
		return ""
	}
	return "APLEXICA_WRAP_PUBKEY=" + base64.StdEncoding.EncodeToString(pub[:])
}

// parsePairOutput extracts device_id + account_id from the plugin's
// stdout. It scans every line so the success marker can be surrounded by
// other diagnostic output.
func parsePairOutput(out string) (deviceID, accountID string) {
	if m := pairLineRe.FindStringSubmatch(out); m != nil {
		return m[1], m[2]
	}
	return "", ""
}

// Unpair runs `<plugin> --unpair` (clears stored credentials + cached
// cloud rules), then restarts the running plugin so it drops its broker
// connection and returns to the unpaired path. The cloud account record
// is left untouched — removing it there is the account owner's action.
func (a *remoteWebAccessor) Unpair(ctx context.Context) error {
	execPath, err := a.remoteExecPath()
	if err != nil {
		return err
	}
	cctx, cancel := context.WithTimeout(ctx, remoteExecTimeout)
	defer cancel()
	prepared, err := a.prepareConfiguredRemotePluginCommand(cctx, execPath, "--unpair")
	if err != nil {
		return fmt.Errorf("api: remote plugin identity changed before unpair: %w", err)
	}
	defer func() { _ = prepared.Close() }()
	out, err := prepared.Cmd().CombinedOutput()
	if err != nil {
		trimmed := strings.TrimSpace(string(out))
		if trimmed == "" {
			trimmed = err.Error()
		}
		return fmt.Errorf("api: remote unpair failed: %s", trimmed)
	}
	if a.deps.remoteRunner != nil {
		_ = a.deps.remoteRunner.Restart(ctx)
	}
	return nil
}

// Status reports configured/enabled/paired plus live conn-state +
// restart-count. It never returns ErrRemoteNotConfigured: an
// unconfigured daemon reports configured=false and zeroes the rest.
func (a *remoteWebAccessor) Status(ctx context.Context) (bool, bool, bool, string, string, string, uint64, error) {
	execPath := a.deps.remoteCfg.Executable
	configured := execPath != ""
	cfg := daemon.Config{Remote: a.deps.remoteCfg}
	enabled := daemon.RemoteEnabled(&cfg)

	connState := "unknown"
	var restartCount uint64
	if a.deps.remoteRunner != nil {
		connState = a.deps.remoteRunner.ConnState()
		restartCount = a.deps.remoteRunner.RestartCount()
	}

	if !configured {
		return false, false, false, "", "", connState, restartCount, nil
	}

	// Best-effort: ask the plugin for its pairing state. A non-zero exit
	// (e.g. plugin missing, not yet paired) is tolerated — we just report
	// paired=false rather than failing the whole status call.
	paired, deviceID, accountID := a.queryPaired(ctx, execPath)
	return configured, enabled, paired, deviceID, accountID, connState, restartCount, nil
}

// statusFieldRe pulls a "<key>: <value>" pair out of one --status line,
// tolerating the column padding the plugin uses ("paired:        yes").
var statusFieldRe = regexp.MustCompile(`^(\w+):\s+(\S.*?)\s*$`)

// queryPaired runs `<plugin> --status` and parses its lines. Any failure
// (missing binary, non-zero exit, unparseable output) degrades to
// paired=false with empty IDs.
func (a *remoteWebAccessor) queryPaired(ctx context.Context, execPath string) (bool, string, string) {
	verifiedPath, err := a.remoteExecPath()
	if err != nil || verifiedPath != execPath {
		return false, "", ""
	}
	return queryPluginStatus(ctx, verifiedPath, a.prepareConfiguredRemotePluginCommand)
}

// queryPluginStatus runs `<plugin> --status` and parses the paired / device_id
// / account_id fields. Package-level so the daemon boot path (which seeds the
// RemoteRunner's DeviceID) and the web accessor share one implementation. Any
// failure (missing binary, non-zero exit, unparseable output, not yet paired)
// degrades to paired=false with empty IDs.
func queryPluginStatus(ctx context.Context, execPath string, prepare remotePluginCommandPreparerForPath) (paired bool, deviceID, accountID string) {
	paired, deviceID, accountID, err := queryPluginStatusChecked(ctx, execPath, prepare)
	if err != nil {
		return false, "", ""
	}
	return paired, deviceID, accountID
}

// queryPluginStatusChecked is queryPluginStatus without the degrade: it
// distinguishes "the plugin answered" from "the query could not be answered"
// — a missing/failed spawn, a non-zero exit, or output with no parseable
// paired field is an error, never an unpaired answer. The repair
// --check-peers guard depends on that distinction to fail closed: degrading
// a broken plugin to paired=false would wave a fleet flag-day through.
func queryPluginStatusChecked(ctx context.Context, execPath string, prepare remotePluginCommandPreparerForPath) (paired bool, deviceID, accountID string, err error) {
	if execPath == "" || prepare == nil {
		return false, "", "", errors.New("no remote plugin to query")
	}
	cctx, cancel := context.WithTimeout(ctx, remoteExecTimeout)
	defer cancel()
	prepared, err := prepare(cctx, execPath, "--status")
	if err != nil {
		return false, "", "", err
	}
	defer func() { _ = prepared.Close() }()
	out, err := prepared.Cmd().CombinedOutput()
	if err != nil {
		return false, "", "", fmt.Errorf("plugin --status: %w", err)
	}
	sawPaired := false
	for _, line := range strings.Split(string(out), "\n") {
		m := statusFieldRe.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil {
			continue
		}
		key, val := m[1], strings.TrimSpace(m[2])
		switch key {
		case "paired":
			sawPaired = true
			paired = strings.EqualFold(val, "yes") || strings.EqualFold(val, "true")
		case "device_id":
			deviceID = val
		case "account_id":
			accountID = val
		}
	}
	if !sawPaired {
		return false, "", "", errors.New("plugin --status output has no paired field")
	}
	return paired, deviceID, accountID, nil
}

// Verify runs `<plugin> --connect-check`. Exit 0 with "OK" in stdout =>
// connected; a "not paired" message maps to ErrNotPaired; anything else
// reports connected=false with the plugin's message.
func (a *remoteWebAccessor) Verify(ctx context.Context) (bool, string, error) {
	execPath, err := a.remoteExecPath()
	if err != nil {
		return false, "", err
	}
	cctx, cancel := context.WithTimeout(ctx, remoteExecTimeout)
	defer cancel()

	prepared, err := a.prepareConfiguredRemotePluginCommand(cctx, execPath, "--connect-check")
	if err != nil {
		return false, "", fmt.Errorf("api: remote plugin identity changed before connection check: %w", err)
	}
	defer func() { _ = prepared.Close() }()
	out, err := prepared.Cmd().CombinedOutput()
	msg := strings.TrimSpace(string(out))
	if strings.Contains(strings.ToLower(msg), "not paired") {
		return false, "", apiweb.ErrNotPaired
	}
	if err != nil {
		if msg == "" {
			msg = err.Error()
		}
		return false, msg, nil
	}
	connected := strings.Contains(msg, "OK")
	return connected, msg, nil
}

// rbacWebAccessor satisfies api.RBACAccessor by resolving the caller's
// per-namespace role through the daemon's RoleService (which round-trips the
// remote plugin) and deriving the capability list. It keeps the HTTP handler
// thin: a no-membership / unpaired result is reported as an empty role +
// empty capabilities with a nil error (the endpoint is total), and a
// transient resolution failure is mapped to apiweb.ErrRBACUnavailable so the
// SPA retries instead of rendering a misleading "no access".
type rbacWebAccessor struct {
	deps *webAPIDeps
}

// NamespaceRole resolves the caller's role + capabilities for namespaceID.
func (a *rbacWebAccessor) NamespaceRole(ctx context.Context, namespaceID string) (string, []string, error) {
	// No RoleService wired (OSS-only daemon / not paired): report a total
	// no-access state rather than an error, matching the remote/status seam.
	if a.deps == nil || a.deps.roleService == nil {
		return "", []string{}, nil
	}
	caps, err := a.deps.roleService.Capabilities(ctx, namespaceID)
	if err != nil {
		// A reconnecting plugin is a transient condition: surface it as
		// retryable, distinct from a genuine no-access answer.
		if errors.Is(err, daemon.ErrRemoteReconnecting) {
			return "", nil, apiweb.ErrRBACUnavailable
		}
		return "", nil, err
	}
	// Resolve the role string for display. Capabilities already collapsed a
	// no-membership caller to an empty slice with a nil error, so a parallel
	// ResolveRole here distinguishes "reader with few caps" from "no member".
	role, rerr := a.deps.roleService.ResolveRole(ctx, namespaceID)
	switch {
	case rerr == nil:
		return string(role), opsToStrings(caps), nil
	case errors.Is(rerr, rbac.ErrNoMembership):
		return "", []string{}, nil
	case errors.Is(rerr, daemon.ErrRemoteReconnecting):
		return "", nil, apiweb.ErrRBACUnavailable
	default:
		return "", nil, rerr
	}
}

// opsToStrings renders the typed capability list as the wire string slice the
// SPA consumes.
func opsToStrings(ops []rbac.Operation) []string {
	out := make([]string, 0, len(ops))
	for _, op := range ops {
		out = append(out, string(op))
	}
	return out
}
