package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"

	"github.com/aplexica/aplexica/internal/plugin/proto"
	"github.com/aplexica/aplexica/internal/privatefs"
)

// clientReadTimeout bounds a single CLI control-socket request. The daemon
// control protocol is local and line-oriented, so a missing newline or wedged
// server should fail fast rather than hanging the CLI.
const (
	clientReadTimeout             = 5 * time.Second
	controlMaxRequestBytes        = 12 << 20
	deviceTransitionSubmitTimeout = 60 * time.Second
	syncEvidenceStatusTimeout     = 4 * time.Second
)

// Request is the wire shape for control commands sent from CLI to daemon.
//
// Commands:
//   - "status"              → returns StatusInfo
//   - "stop"                → schedules graceful shutdown
//   - "reload"              → re-applies file config; returns load report
//   - "refanout"            → re-runs fanOut for every artifact whose
//     Project.ID matches ProjectID. Used by
//     `aplexica project link` to materialize the
//     newly-linked project's pending artifacts.
//     v0.58.0.
//   - "materialize"         → materializes a conversation artifact branch
//     into a target local agent immediately after fork/checkout.
//   - "deferred-drop"       → drops queued/abandoned native materialization
//     retries for the given Agent/ArtifactID (empty
//     means "all"). Used by
//     `aplexica repair materialization --drop`.
//   - "web-issue-token"     → mints a bootstrap URL via the WebTokenIssuer
//     callback wired at construction. Used by
//     `aplexica web issue-token` and the tray's
//     "Open Aplexica" handler. v0.107.0.
//   - "web-revoke-sessions" → invalidates every active web session via
//     the WebSessionRevoker callback. Used by
//     `aplexica web revoke-sessions`. v0.107.0.
type Request struct {
	Command             string `json:"command"`
	ProjectID           string `json:"projectId,omitempty"`
	ArtifactID          string `json:"artifactId,omitempty"`
	Agent               string `json:"agent,omitempty"`
	Branch              string `json:"branch,omitempty"`
	BackupID            string `json:"backupId,omitempty"`
	PlanBlob            []byte `json:"planBlob,omitempty"`
	IncludeSyncEvidence bool   `json:"includeSyncEvidence,omitempty"`

	// Backfill fields (the "backfill" command). Agents narrows the target
	// set (empty = every enabled conversation-capable agent), Depth is the
	// per-agent history depth (negative = full), Scope is "local" (or the
	// reserved "cloud"), and DryRun plans without materializing.
	Agents []string `json:"agents,omitempty"`
	Depth  int      `json:"depth,omitempty"`
	Scope  string   `json:"scope,omitempty"`
	DryRun bool     `json:"dryRun,omitempty"`
}

// Response is the wire shape for daemon replies.
type Response struct {
	OK    bool   `json:"ok"`
	Data  any    `json:"data,omitempty"`
	Error string `json:"error,omitempty"`
}

// StatusInfo is the payload returned by the "status" command.
//
// LastActivity (v0.39.0) + PendingImports (v0.44.0) + AdapterStates +
// AdapterLastErrors (v0.51.0) + PendingProjects (v0.58.0) are
// populated dynamically at status-request time from the Activity
// provider handed to NewControlServer; the construction-time
// StatusInfo's values for these fields are ignored. omitzero (time)
// / omitempty (int + maps + slices) keep the JSON wire shape tidy at
// steady-state defaults.
type StatusInfo struct {
	PID            int       `json:"pid"`
	StartedAt      time.Time `json:"startedAt"`
	WatchedDir     string    `json:"watchedDir"`
	Version        string    `json:"version,omitempty"`
	LastActivity   time.Time `json:"lastActivity,omitzero"`
	PendingImports int       `json:"pendingImports,omitempty"`

	// LocalDeviceID is the cloud device identity the daemon stamps on
	// outbound event provenance, seeded from the remote plugin at startup.
	// Empty when remote sync is disabled or the plugin is unpaired. Exposed
	// so CLI commands that author store events (import, hermes, watch) can
	// stamp the SAME identity: the outbound sweep skips any head whose
	// provenance names a different device, so an event carrying the
	// adapters' os.Hostname() fallback is never published to peers.
	LocalDeviceID string `json:"localDeviceId,omitempty"`

	// AdapterStates (ADR-0159 Candidate B; v0.51.0) maps each
	// configured adapter's name to a bucketed state string. Today's
	// alphabet: "active" / "idle". Future extensions may add
	// "quarantined" once adapter-quarantine semantics are wired up
	// (currently no such state machine in the orchestrator).
	AdapterStates map[string]string `json:"adapterStates,omitempty"`

	// AdapterLastErrors (ADR-0159 Candidate D; v0.51.0) maps each
	// adapter that errored on its most recent Export to the redacted
	// error string (paths under $HOME stripped to ~/). Cleared per
	// adapter on next successful Import/Export. Empty when no
	// adapter has errored recently.
	AdapterLastErrors map[string]string `json:"adapterLastErrors,omitempty"`

	// PendingProjects (BRD-02 §4.13; v0.58.0) lists project-scope
	// artifacts whose canonical project ID has no entry in the user's
	// project registry on this device. Surfaced to the tray menu as
	// "Pending projects (N) →" with one child per project. The user
	// resolves via `aplexica project link <id> <path>`.
	//
	// The element type is map[string]any to avoid an import cycle
	// (daemon → pending → daemon would loop because pending imports
	// project which imports atomicfile which... etc.). Field order:
	// {id, artifactCount, samplePath}.
	PendingProjects []map[string]any `json:"pendingProjects,omitempty"`

	// DeferredMaterializations lists native-session writes the orchestrator
	// could not complete and is still retrying, plus those it abandoned after
	// spending their retry budget. Element type is map[string]any for the same
	// import-cycle reason as PendingProjects. Keys: agent, artifactId, state
	// ("pending" / "overflow" / "abandoned"), attempts, firstDeferredAt,
	// nextAttemptAt, abandonedAt, originAgent, lastError. Surfaced by
	// `aplexica status` and `aplexica repair materialization`.
	DeferredMaterializations []map[string]any `json:"deferredMaterializations,omitempty"`

	// SyncSuppressions lists every declined materialization target,
	// aggregated by (agent, reason). This is the surface that answers "why is
	// nothing syncing?". Before it existed, a device whose rules engine
	// denied every target reported full health — artifacts counted, adapters
	// installed, no conflicts — while all cross-agent sync was dead
	// (2026-07-30). Element type is map[string]any for the same import-cycle
	// reason as PendingProjects. Keys: agent, reason, class
	// ("policy"/"defect"/"capability"), count, firstAt, lastAt, explain,
	// remedy, exemplars. Bounded by agents x reasons; carries no body content.
	SyncSuppressions []map[string]any `json:"syncSuppressions,omitempty"`

	// SyncDisabledReason is non-empty when this device cannot fan out AT ALL.
	// The flagship case is a non-nil rules engine holding zero rules, which
	// is what a device with no ~/.aplexica/rules.toml gets: fail-closed by
	// design, correct, and previously invisible. A single top-line field lets
	// a human or a script answer "is sync actually working?" without
	// inferring it from an empty table.
	SyncDisabledReason string `json:"syncDisabledReason,omitempty"`

	// Store-disk-pressure fields. Populated dynamically at
	// status-request time from the PressureProvider the daemon wires (nil
	// when the disk-pressure path is disabled, e.g. store_max_gb=0). They
	// mirror retention.PressureState. StoreMaxBytes == 0 means the cap is
	// disabled — `aplexica status` then renders nothing for store pressure.
	// The size is sampled by the daemon's pressure goroutine each tick, so
	// these can lag reality by up to one pressure-check interval (acceptable
	// for a last-resort backstop surfaced to a human).
	//
	// The reclaimable/pinned split is the honest accounting:
	// StoreReclaimableBytes is what retention could actually free,
	// StorePinnedBytes is what it cannot legally touch (append-only event
	// logs — StoreEventLogBytes — live artifact metadata, head-referenced
	// blobs). StoreWatermarkUnreachable means pinned bytes alone meet the
	// watermark, so no sweep can relieve the pressure.
	StoreBytes                int64 `json:"storeBytes,omitempty"`
	StoreMaxBytes             int64 `json:"storeMaxBytes,omitempty"`
	StoreHighWatermarkBytes   int64 `json:"storeHighWatermarkBytes,omitempty"`
	StoreReclaimableBytes     int64 `json:"storeReclaimableBytes,omitempty"`
	StorePinnedBytes          int64 `json:"storePinnedBytes,omitempty"`
	StoreEventLogBytes        int64 `json:"storeEventLogBytes,omitempty"`
	OverHighWatermark         bool  `json:"overHighWatermark,omitempty"`
	OverEmergency             bool  `json:"overEmergency,omitempty"`
	StoreWatermarkUnreachable bool  `json:"storeWatermarkUnreachable,omitempty"`

	// SyncEvidence is populated only for an explicit IncludeSyncEvidence status
	// request when a verified remote plugin and durable outbox are wired. It is
	// content-free and read-only, and remains absent from ordinary status/tray
	// polling and local-only installations.
	SyncEvidence *SyncEvidenceStatus `json:"syncEvidence,omitempty"`
}

type OutboxEvidenceStatus struct {
	Available               bool   `json:"available"`
	Pending                 uint64 `json:"pending"`
	OldestPendingPresent    bool   `json:"oldestPendingPresent"`
	OldestPendingAgeSeconds uint64 `json:"oldestPendingAgeSeconds"`
}

type SyncEvidenceStatus struct {
	RemoteAvailable bool                      `json:"remoteAvailable"`
	Remote          *proto.RemoteStatusResult `json:"remote,omitempty"`
	Outbox          OutboxEvidenceStatus      `json:"outbox"`
}

type SyncEvidenceProvider func(context.Context) SyncEvidenceStatus

// StorePressure is the store-disk-pressure snapshot a PressureProvider
// supplies at status time. It mirrors the daemon's cached
// retention.PressureState field-for-field — including the honest
// reclaimable-vs-pinned split — declared here rather than imported so the
// daemon package stays free of an import on internal/retention.
type StorePressure struct {
	StoreBytes              int64
	StoreMaxBytes           int64
	StoreHighWatermarkBytes int64
	StoreReclaimableBytes   int64
	StorePinnedBytes        int64
	StoreEventLogBytes      int64
	OverHighWatermark       bool
	OverEmergency           bool
	WatermarkUnreachable    bool
}

// PressureProvider is the optional callback the daemon wires so the "status"
// command can report live store-disk-pressure (FR-03.21). A nil provider
// means the disk-pressure path is disabled; the status response then carries
// zero StoreBytes/StoreMaxBytes and the renderer prints nothing.
type PressureProvider func() StorePressure

// Activity is the optional live-data provider that ControlServer
// consults at "status"-request time. The interface is intentionally
// named for the field's purpose (not the type) so future live fields
// can extend it without renaming.
//
// Fields:
//   - LastActivity      (v0.39.0): wall-clock time of last successful work
//   - PendingImports    (v0.44.0): debouncer queue depth — ADR-0159 Cand A
//   - AdapterStates     (v0.51.0): per-adapter "active"/"idle" bucket — ADR-0159 Cand B
//   - AdapterLastErrors (v0.51.0): per-adapter redacted error string — ADR-0159 Cand D
//   - PendingProjects   (v0.58.0): project-scope artifacts whose project isn't linked locally
//   - RefanOutByProject (v0.58.0): re-run fanOut for a project ID
//     (called by control-socket "refanout")
//   - MaterializeConversationBranch: write one conversation branch into one
//     local agent immediately (called by control-socket "materialize")
//   - DeferredMaterializations / DropDeferredMaterializations: read and drain
//     the native-materialization retry queue (control-socket "status" and
//     "deferred-drop"; surfaced by `aplexica repair materialization`)
//
// Pass nil to NewControlServer when no live tracking is wired up
// (tests, ad-hoc invocations). The status response then carries the
// construction-time StatusInfo verbatim.
type Activity interface {
	LastActivity() time.Time
	PendingImports() int
	AdapterStates() map[string]string
	AdapterLastErrors() map[string]string
	PendingProjects() []map[string]any
	RefanOutByProject(projectID string) (int, error)
	MaterializeConversationBranch(artifactID, agent, branch string) (path string, materialized bool, err error)
	DeferredMaterializations() []map[string]any
	DropDeferredMaterializations(agent, artifactID string) (int, error)
	// SyncSuppressions reports declined materialization targets aggregated by
	// (agent, reason); SyncStructurallyDisabled reports the device-wide
	// "nothing can fan out" state. Both are read-only status surfaces.
	SyncSuppressions() []map[string]any
	SyncStructurallyDisabled() bool
}

// Reloader is the synchronous callback the ControlServer invokes for
// the "reload" command. Returns an opaque report (Data field of the
// Response) describing what changed, or an error if the reload failed.
// Wired by the daemon at startup so the control-socket reload triggers
// the same code path SIGHUP does on Unix.
type Reloader func() (any, error)

// BackfillRunner is the callback for the "backfill" command: the daemon
// validates the scope against its configuration, plans (and, unless dryRun,
// starts) a LOCAL conversation backfill, and returns the plan. Wired via
// SetBackfillRunner; nil means the daemon predates the command.
type BackfillRunner func(agents []string, depth int, scope string, dryRun bool) (any, error)

// ValidateBackfillScope decides whether a requested backfill scope may run.
// "local" (or empty) is always allowed — a local backfill only materializes
// canonical history into this device's agents and never publishes to the
// relay. "cloud" is RESERVED: it is refused today regardless of
// configuration, and the sync.cloudBackfill key only selects which gate the
// error names, so the future cross-device implementation can ship behind a
// key whose name is already stable.
func ValidateBackfillScope(scope string, cloudEnabled bool) error {
	switch scope {
	case "", "local":
		return nil
	case "cloud":
		if !cloudEnabled {
			return errors.New("cloud backfill is disabled (sync.cloudBackfill=false) and is reserved for a future release; only --scope local is available")
		}
		return errors.New("sync.cloudBackfill is enabled, but cloud backfill is not implemented in this daemon version; only --scope local is available")
	default:
		return fmt.Errorf("unknown backfill scope %q (expected local or cloud)", scope)
	}
}

// WebTokenIssuer is the daemon-side callback invoked by the
// "web-issue-token" control command. Returns the full bootstrap URL
// for the local web UI ("http://127.0.0.1:<port>/?bootstrap=…") or
// an error when the web listener isn't running. Wired by
// SetWebTokenIssuer; nil callback means the command returns "web UI
// not running".
type WebTokenIssuer func() (string, error)
type WebBootstrapFileIssuer func() (string, error)

// WebSessionRevoker is the daemon-side callback invoked by the
// "web-revoke-sessions" control command. Returns the number of
// sessions invalidated. Wired by SetWebSessionRevoker; nil callback
// means the command returns "web UI not running".
type WebSessionRevoker func() int
type ProjectRemover func(projectID string) error
type NativeRestorer func(context.Context, string, string) (any, error)
type GenerationActivationRequester func()
type DeviceTransitionSubmitter func(context.Context, []byte) error

// ControlServer accepts requests on a Unix-domain-socket and handles
// them. v0.8.0 supports `status` and `stop`; v0.75.0 adds `reload`;
// v0.107.0 adds `web-issue-token` / `web-revoke-sessions` for the
// local-web-UI control surface.
type ControlServer struct {
	sockPath                      string
	info                          *StatusInfo
	activity                      Activity       // optional; nil = no live overlay
	reloader                      Reloader       // optional; nil = `reload` returns OK:false
	webTokenIssuer                WebTokenIssuer // optional; nil = `web-issue-token` returns OK:false
	webBootstrapFileIssuer        WebBootstrapFileIssuer
	webSessionRevoker             WebSessionRevoker // optional; nil = `web-revoke-sessions` returns OK:false
	projectRemover                ProjectRemover
	nativeRestorer                NativeRestorer
	generationActivationRequester GenerationActivationRequester
	deviceTransitionSubmitter     DeviceTransitionSubmitter
	pressureProvider              PressureProvider // optional; nil = no store-pressure overlay on status
	syncEvidenceProvider          SyncEvidenceProvider
	backfillRunner                BackfillRunner // optional; nil = `backfill` returns OK:false
	listener                      net.Listener

	mu      sync.Mutex
	stopped bool
	done    chan struct{}

	// localDeviceID, when non-empty, overrides the construction-time
	// StatusInfo.LocalDeviceID in every status response. Pairing — and
	// RE-pairing, which retires the previous cloud device id — rotates the
	// identity while the daemon runs, and CLI commands adopt whatever the
	// status response reports (cliCloudDeviceID) before authoring events: a
	// status response frozen at the boot seed would hand every CLI import
	// the RETIRED identity, resurrecting the exact unpublishable-head bug
	// the field exists to fix. Guarded by mu.
	localDeviceID string
}

// SetLocalDeviceID updates the cloud device identity reported by future
// status responses. An empty id is a no-op: an unpaired/unreachable plugin
// must not erase a known identity mid-flight (mirrors the daemon-side
// applyCloudDeviceIdentity contract).
func (s *ControlServer) SetLocalDeviceID(id string) {
	if id == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.localDeviceID = id
}

// SetReloader wires the reload callback. Safe to call before Start.
// Calling SetReloader(nil) after wiring disables the reload command.
func (s *ControlServer) SetReloader(r Reloader) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reloader = r
}

// SetBackfillRunner wires the "backfill" command's handler.
func (s *ControlServer) SetBackfillRunner(r BackfillRunner) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.backfillRunner = r
}

// SetPressureProvider wires the store-disk-pressure provider consulted at
// status-request time (FR-03.21). Safe to call after Start (the daemon's
// pressure goroutine starts later than the control server binds). Passing nil
// disables the store-pressure overlay.
func (s *ControlServer) SetPressureProvider(p PressureProvider) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pressureProvider = p
}

// SetSyncEvidenceProvider wires one bounded read-only snapshot for the local
// control socket. The provider owns no network authority: the signed plugin's
// existing device-proof status call continues to enforce account/scope access.
func (s *ControlServer) SetSyncEvidenceProvider(p SyncEvidenceProvider) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.syncEvidenceProvider = p
}

// SetWebTokenIssuer wires the bootstrap-URL minting callback for the
// "web-issue-token" command. Called by the daemon after the local
// web server has bound its listener (so IssueTokenURL can succeed).
func (s *ControlServer) SetWebTokenIssuer(f WebTokenIssuer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.webTokenIssuer = f
}
func (s *ControlServer) SetWebBootstrapFileIssuer(f WebBootstrapFileIssuer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.webBootstrapFileIssuer = f
}

// SetWebSessionRevoker wires the session-invalidation callback for
// the "web-revoke-sessions" command. Called by the daemon after the
// local web server has constructed its session store.
func (s *ControlServer) SetWebSessionRevoker(f WebSessionRevoker) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.webSessionRevoker = f
}

func (s *ControlServer) SetProjectRemover(f ProjectRemover) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.projectRemover = f
}
func (s *ControlServer) SetNativeRestorer(f NativeRestorer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nativeRestorer = f
}

// SetGenerationActivationRequester wires a content-free local wake-up for the
// generation activation driver. The request is intentionally scope-less: the
// driver re-enumerates only already-provisioned local identity state.
func (s *ControlServer) SetGenerationActivationRequester(f GenerationActivationRequester) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.generationActivationRequester = f
}

// SetDeviceTransitionSubmitter wires the private local ingress for a signed,
// bounded transition plan. The callback owns all cryptographic validation;
// the control server only provides a size-bounded byte transport.
func (s *ControlServer) SetDeviceTransitionSubmitter(f DeviceTransitionSubmitter) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deviceTransitionSubmitter = f
}

// NewControlServer creates a ControlServer. info is the
// construction-time StatusInfo (pid / startedAt / watchedDir /
// version). activity is an optional live-data provider whose
// LastActivity() is overlaid onto the status response on each
// request — pass nil to disable the overlay.
func NewControlServer(sockPath string, info *StatusInfo, activity Activity) *ControlServer {
	return &ControlServer{
		sockPath: sockPath,
		info:     info,
		activity: activity,
		done:     make(chan struct{}),
	}
}

// Start binds the Unix socket and begins accepting connections in the
// background. Returns once the socket is bound; the actual serve loop
// runs in a goroutine.
//
// If the socket path already exists (from a crashed previous daemon),
// it is removed before binding.
func (s *ControlServer) Start() error {
	if err := privatefs.EnsureDir(filepathDir(s.sockPath), privatefs.DirPolicy{
		Access:        privatefs.AccessPrivate,
		RepairOwned:   true,
		AllowExisting: true,
	}); err != nil {
		return fmt.Errorf("daemon: mkdir control dir: %w", err)
	}
	// Best-effort remove of stale socket file.
	_ = os.Remove(s.sockPath)

	ln, err := net.Listen("unix", s.sockPath)
	if err != nil {
		return fmt.Errorf("daemon: bind control socket: %w", err)
	}
	if err := privatefs.HardenOwnedPrivateSocket(s.sockPath); err != nil {
		ln.Close()
		_ = os.Remove(s.sockPath)
		return fmt.Errorf("daemon: protect control socket: %w", err)
	}
	s.listener = ln
	go s.serve()
	return nil
}

// Stop closes the listener and waits for in-flight connections to drain.
// Safe to call multiple times.
func (s *ControlServer) Stop() error {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return nil
	}
	s.stopped = true
	s.mu.Unlock()

	if s.listener != nil {
		_ = s.listener.Close()
	}
	_ = os.Remove(s.sockPath)
	close(s.done)
	return nil
}

// Done returns a channel closed when the server has fully stopped.
func (s *ControlServer) Done() <-chan struct{} { return s.done }

func (s *ControlServer) serve() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			s.mu.Lock()
			stopped := s.stopped
			s.mu.Unlock()
			if stopped {
				return
			}
			continue
		}
		go s.handleConn(conn)
	}
}

func (s *ControlServer) handleConn(c net.Conn) {
	defer c.Close()

	rd := bufio.NewReader(io.LimitReader(c, controlMaxRequestBytes+1))
	line, err := rd.ReadBytes('\n')
	if len(line) > controlMaxRequestBytes {
		_ = writeResponse(c, Response{OK: false, Error: "control request exceeds size limit"})
		return
	}
	if err != nil {
		return
	}

	var req Request
	if err := json.Unmarshal(line, &req); err != nil {
		_ = writeResponse(c, Response{OK: false, Error: fmt.Sprintf("parse request: %v", err)})
		return
	}

	switch req.Command {
	case "status":
		// Clone the construction-time StatusInfo by value and overlay
		// the live fields from the Activity provider when one is wired.
		// Construction-time fields (PID/StartedAt/WatchedDir/Version)
		// stay immutable; the dynamic fields all come from Activity.
		out := *s.info
		// A (re-)pair may have rotated the cloud device identity since the
		// construction-time snapshot was taken; the rotating override wins so
		// CLI adapter construction never adopts a retired id.
		s.mu.Lock()
		if s.localDeviceID != "" {
			out.LocalDeviceID = s.localDeviceID
		}
		s.mu.Unlock()
		if s.activity != nil {
			out.LastActivity = s.activity.LastActivity()
			out.PendingImports = s.activity.PendingImports()
			out.AdapterStates = s.activity.AdapterStates()
			out.AdapterLastErrors = s.activity.AdapterLastErrors()
			out.PendingProjects = s.activity.PendingProjects()
			out.DeferredMaterializations = s.activity.DeferredMaterializations()
			out.SyncSuppressions = s.activity.SyncSuppressions()
			if s.activity.SyncStructurallyDisabled() {
				// Name the consequence, not the mechanism. "rules engine
				// rebuilt (0 rules)" tells the operator nothing about what is
				// broken or what to do about it.
				out.SyncDisabledReason = "no sync rules are configured, so nothing is copied between agents on this device"
			}
		}
		// Store-disk-pressure overlay (FR-03.21). The provider is wired by
		// the daemon's pressure goroutine and reads the cached size sampled
		// each tick; a nil provider leaves the zero values (cap disabled).
		s.mu.Lock()
		pp := s.pressureProvider
		s.mu.Unlock()
		if pp != nil {
			sp := pp()
			out.StoreBytes = sp.StoreBytes
			out.StoreMaxBytes = sp.StoreMaxBytes
			out.StoreHighWatermarkBytes = sp.StoreHighWatermarkBytes
			out.StoreReclaimableBytes = sp.StoreReclaimableBytes
			out.StorePinnedBytes = sp.StorePinnedBytes
			out.StoreEventLogBytes = sp.StoreEventLogBytes
			out.OverHighWatermark = sp.OverHighWatermark
			out.OverEmergency = sp.OverEmergency
			out.StoreWatermarkUnreachable = sp.WatermarkUnreachable
		}
		s.mu.Lock()
		evidenceProvider := s.syncEvidenceProvider
		s.mu.Unlock()
		// Remote sync evidence is deliberately opt-in. Ordinary CLI status,
		// tray polling, and watch mode remain local-only and must not turn a
		// cheap liveness request into a remote plugin call.
		out.SyncEvidence = nil
		if req.IncludeSyncEvidence && evidenceProvider != nil {
			ctx, cancel := context.WithTimeout(context.Background(), syncEvidenceStatusTimeout)
			evidence := evidenceProvider(ctx)
			cancel()
			out.SyncEvidence = &evidence
		}
		_ = writeResponse(c, Response{OK: true, Data: out})
	case "stop":
		_ = writeResponse(c, Response{OK: true, Data: "shutting down"})
		// Schedule Stop after the response is sent.
		go s.Stop()
	case "reload":
		// v0.75.0 FR-10.8: cross-platform hot-reload trigger. Unix has
		// SIGHUP; Windows doesn't. The daemon registers a Reloader
		// callback at construction time which we invoke synchronously
		// here so the response carries the load result.
		if s.reloader == nil {
			_ = writeResponse(c, Response{OK: false, Error: "no reloader wired"})
			break
		}
		report, err := s.reloader()
		if err != nil {
			_ = writeResponse(c, Response{OK: false, Error: err.Error()})
			break
		}
		_ = writeResponse(c, Response{OK: true, Data: report})
	case "generation-activation-request":
		s.mu.Lock()
		request := s.generationActivationRequester
		s.mu.Unlock()
		if request == nil {
			_ = writeResponse(c, Response{OK: false, Error: "generation activation driver unavailable"})
			break
		}
		request()
		_ = writeResponse(c, Response{OK: true, Data: "generation activation requested"})
	case "device-transition-submit":
		s.mu.Lock()
		submit := s.deviceTransitionSubmitter
		s.mu.Unlock()
		if submit == nil {
			_ = writeResponse(c, Response{OK: false, Error: "device transition service unavailable"})
			break
		}
		if len(req.PlanBlob) == 0 {
			_ = writeResponse(c, Response{OK: false, Error: "signed transition plan is required"})
			break
		}
		ctx, cancel := context.WithTimeout(context.Background(), deviceTransitionSubmitTimeout)
		err := submit(ctx, append([]byte(nil), req.PlanBlob...))
		cancel()
		if err != nil {
			_ = writeResponse(c, Response{OK: false, Error: err.Error()})
			break
		}
		_ = writeResponse(c, Response{OK: true, Data: "device transition accepted"})
	case "web-issue-token":
		// v0.107.0: mint a one-time bootstrap URL for the local
		// web UI. Wired by the daemon's startup path after the web
		// listener binds; a nil callback signals "web UI not
		// running" (config disabled, or construction failure).
		s.mu.Lock()
		issuer := s.webTokenIssuer
		s.mu.Unlock()
		if issuer == nil {
			_ = writeResponse(c, Response{OK: false, Error: "web UI not running"})
			break
		}
		url, err := issuer()
		if err != nil {
			_ = writeResponse(c, Response{OK: false, Error: err.Error()})
			break
		}
		_ = writeResponse(c, Response{OK: true, Data: map[string]any{"url": url}})
	case "web-issue-bootstrap-file":
		s.mu.Lock()
		issuer := s.webBootstrapFileIssuer
		s.mu.Unlock()
		if issuer == nil {
			_ = writeResponse(c, Response{OK: false, Error: "web UI not running"})
			break
		}
		path, err := issuer()
		if err != nil {
			_ = writeResponse(c, Response{OK: false, Error: err.Error()})
			break
		}
		_ = writeResponse(c, Response{OK: true, Data: map[string]any{"path": path}})
	case "web-revoke-sessions":
		// v0.107.0: invalidate every active web session in one shot.
		s.mu.Lock()
		revoker := s.webSessionRevoker
		s.mu.Unlock()
		if revoker == nil {
			_ = writeResponse(c, Response{OK: false, Error: "web UI not running"})
			break
		}
		n := revoker()
		_ = writeResponse(c, Response{OK: true, Data: map[string]any{"revoked": n}})
	case "refanout":
		// v0.58.0: BRD-02 §4.13 materialize-on-link path. Called by
		// `aplexica project link` after it persists the registry
		// entry. Re-runs fanOut for every artifact whose Project.ID
		// matches, so the newly-linked project's pending artifacts
		// materialize to the agent native paths without waiting for
		// a fresh edit.
		if s.activity == nil {
			_ = writeResponse(c, Response{OK: false, Error: "no activity provider wired (daemon may be in a degraded state)"})
			break
		}
		n, ferr := s.activity.RefanOutByProject(req.ProjectID)
		if ferr != nil {
			_ = writeResponse(c, Response{OK: false, Error: ferr.Error()})
			break
		}
		_ = writeResponse(c, Response{OK: true, Data: map[string]any{"refanouted": n}})
	case "backfill":
		// Forced LOCAL conversation backfill (v1.0.57). The runner owns scope
		// validation (the reserved cloud gate) and the plan/start decision;
		// this arm only carries the wire fields across.
		s.mu.Lock()
		runner := s.backfillRunner
		s.mu.Unlock()
		if runner == nil {
			_ = writeResponse(c, Response{OK: false, Error: "backfill is not wired (daemon may be in a degraded state)"})
			break
		}
		data, berr := runner(req.Agents, req.Depth, req.Scope, req.DryRun)
		if berr != nil {
			_ = writeResponse(c, Response{OK: false, Error: berr.Error()})
			break
		}
		_ = writeResponse(c, Response{OK: true, Data: data})
	case "deferred-drop":
		// Operator drain for the native-materialization retry queue. An empty
		// Agent/ArtifactID widens the selection to every target/artifact, so
		// the CLI is responsible for making the scope explicit to the user.
		if s.activity == nil {
			_ = writeResponse(c, Response{OK: false, Error: "no activity provider wired (daemon may be in a degraded state)"})
			break
		}
		n, derr := s.activity.DropDeferredMaterializations(req.Agent, req.ArtifactID)
		if derr != nil {
			_ = writeResponse(c, Response{OK: false, Error: derr.Error()})
			break
		}
		_ = writeResponse(c, Response{OK: true, Data: map[string]any{"dropped": n}})
	case "project-remove":
		s.mu.Lock()
		remove := s.projectRemover
		s.mu.Unlock()
		if remove == nil {
			_ = writeResponse(c, Response{OK: false, Error: "project controller unavailable"})
			break
		}
		if req.ProjectID == "" {
			_ = writeResponse(c, Response{OK: false, Error: "project id is required"})
			break
		}
		if err := remove(req.ProjectID); err != nil {
			_ = writeResponse(c, Response{OK: false, Error: err.Error()})
			break
		}
		_ = writeResponse(c, Response{OK: true, Data: map[string]any{"removed": req.ProjectID}})
	case "native-restore":
		s.mu.Lock()
		restore := s.nativeRestorer
		s.mu.Unlock()
		if restore == nil {
			_ = writeResponse(c, Response{OK: false, Error: "native restore controller unavailable"})
			break
		}
		result, err := restore(context.Background(), req.BackupID, req.Agent)
		if err != nil {
			_ = writeResponse(c, Response{OK: false, Error: err.Error()})
			break
		}
		_ = writeResponse(c, Response{OK: true, Data: result})
	case "materialize":
		if s.activity == nil {
			_ = writeResponse(c, Response{OK: false, Error: "no activity provider wired (daemon may be in a degraded state)"})
			break
		}
		path, materialized, merr := s.activity.MaterializeConversationBranch(req.ArtifactID, req.Agent, req.Branch)
		if merr != nil {
			_ = writeResponse(c, Response{OK: false, Error: merr.Error()})
			break
		}
		_ = writeResponse(c, Response{OK: true, Data: map[string]any{
			"path":         path,
			"materialized": materialized,
		}})
	default:
		_ = writeResponse(c, Response{OK: false, Error: fmt.Sprintf("unknown command: %s", req.Command)})
	}
}

func writeResponse(c net.Conn, resp Response) error {
	b, err := json.Marshal(resp)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	_, err = c.Write(b)
	return err
}

// SendCommand connects to the daemon's control socket, sends req, reads
// one line of response, and returns it. Convenience helper for CLI
// clients.
func SendCommand(sockPath string, req Request) (Response, error) {
	return SendCommandWithTimeout(sockPath, req, clientReadTimeout)
}

// SendCommandWithTimeout is used by bounded long-running local operations
// such as an authenticated device transition. It applies one deadline to the
// complete write/response exchange, including a multi-megabyte plan payload.
func SendCommandWithTimeout(sockPath string, req Request, timeout time.Duration) (Response, error) {
	if timeout <= 0 {
		return Response{}, fmt.Errorf("control command timeout must be positive")
	}
	c, err := net.DialTimeout("unix", sockPath, 2*time.Second)
	if err != nil {
		return Response{}, fmt.Errorf("connect to %s: %w", sockPath, err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(timeout))

	reqBytes, err := json.Marshal(req)
	if err != nil {
		return Response{}, err
	}
	reqBytes = append(reqBytes, '\n')
	if _, err := io.Copy(c, bytes.NewReader(reqBytes)); err != nil {
		return Response{}, fmt.Errorf("write request: %w", err)
	}
	rd := bufio.NewReader(c)
	line, err := rd.ReadBytes('\n')
	if err != nil {
		return Response{}, fmt.Errorf("read response: %w", err)
	}
	var resp Response
	if err := json.Unmarshal(line, &resp); err != nil {
		return Response{}, fmt.Errorf("parse response: %w", err)
	}
	return resp, nil
}

// filepathDir avoids importing path/filepath only for one Dir call.
func filepathDir(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' || p[i] == '\\' {
			return p[:i]
		}
	}
	return "."
}
