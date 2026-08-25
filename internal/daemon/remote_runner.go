package daemon

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aplexica/aplexica/internal/plugin/proto"
	"github.com/aplexica/aplexica/internal/plugin/proxy"
	"github.com/aplexica/aplexica/internal/plugin/secureexec"
	"github.com/aplexica/aplexica/internal/plugin/truststate"
	"github.com/aplexica/aplexica/internal/syncrules"
)

const (
	remotePluginOpenTimeout              = 10 * time.Second
	remoteSyncNegotiationTimeout         = 10 * time.Second
	remoteSyncNegotiationRefreshInterval = 3 * time.Minute
)

const (
	remoteSyncModeRankLegacy = iota
	remoteSyncModeRankShadow
	remoteSyncModeRankDurableRead
	remoteSyncModeRankPreferred
	remoteSyncModeRankRequired
)

// RemoteRunner owns the lifecycle of a single remote-transport
// plugin: spawn the configured executable, open a RemoteProxy against
// its stdin/stdout pipe, restart on crash with exponential backoff,
// shut down cleanly on context cancellation.
//
// The daemon's startup path constructs one RemoteRunner per
// RemoteConfig and starts it in a goroutine. The Runner re-exports
// the proxy's methods so the sync orchestrator can call Publish /
// Fetch / Status without caring about the lifecycle dance underneath.
//
// Concurrency: Publish/Fetch/etc. callers may race with a
// reconnection (the proxy is being swapped). The Runner serializes
// access via proxyMu so callers always see a valid (or explicitly
// nil) proxy; on reconnect-in-progress, calls return
// ErrRemoteReconnecting and the caller is expected to retry on the
// next scheduler tick.
type RemoteRunner struct {
	Executable string
	// DeviceID may be populated by a keyed struct literal before the runner is
	// shared. Runtime callers must use SetDeviceID and CurrentDeviceID: pairing
	// can replace an initially-empty identity while the supervisor and inbound
	// callbacks are running.
	DeviceID      string
	Version       string
	PublisherKeys []ed25519.PublicKey
	// PluginVerifier allows the daemon entrypoint to supply its complete
	// compiled trust policy (standalone publisher manifests plus finite
	// Balanced exact-byte authorizations). Nil preserves the standalone
	// publisher-manifest verifier for package users and existing tests.
	PluginVerifier func(string) (proto.VerifiedRemotePlugin, error)
	TrustStore     truststate.Store
	TrustPolicy    truststate.Policy
	// TransferRoot is the daemon-owned state directory used only for additive
	// private-file checkpoint handoffs. It must be dedicated to this protocol;
	// ordinary remote.publish calls never touch it.
	TransferRoot string
	// ObservationSampleKey is a daemon-private persistent HMAC key used to
	// unlink content-free observation sample IDs from their local source
	// identities. It never crosses the plugin boundary.
	ObservationSampleKey [32]byte

	// envelopeCaps caches the account-level remote.envelope_caps answer
	// (2026-07-29 envelope wire-efficiency ADR D3) so the seal path never
	// hits the plugin per event. Zero value is ready to use; the cache,
	// TTLs, and fail-closed semantics live in remote_envelope_caps.go.
	envelopeCaps envelopeCapsCache

	// Logger: tolerated nil for tests.
	Logger interface {
		Info(msg string, args ...any)
		Warn(msg string, args ...any)
		Error(msg string, args ...any)
	}

	// Callbacks invoked when the plugin pushes asynchronous events.
	// Daemon wires these to its sync orchestrator. Nil callbacks are
	// silently dropped.
	OnInbound           func([]proto.RemoteEvent)
	OnInboundV2         func(proto.RemoteInboundDeliveryV2) proto.RemoteInboundAckV2
	OnInboundFinalizeV1 func(proto.RemoteInboundFinalizeV1Params) proto.RemoteInboundFinalizeV1Result
	// RetainedInboundInterval paces retained catch-up deliveries before they
	// reach OnInboundV2. Live events are never delayed. A zero value disables
	// pacing for tests and non-cloud transports.
	RetainedInboundInterval time.Duration
	OnConnState             func(state, human string)
	OnEnumerateHint         func(reason string)
	OnCheckpointNeededV1    func(proto.RemoteCheckpointNeededV1Notification)
	OnRulesUpdate           func(changeID string, rules []syncrules.Rule)
	// DurableCursorStore remains daemon-owned across plugin replacement. When
	// the negotiated runtime advertises cursor-resume support, runOnce sends
	// this store's exact stream cursor to the new plugin before durable reads.
	DurableCursorStore *DurableCursorStore
	// DurableInbox retains the exact content-free post-cloud-ACK native-finalize
	// obligation. It is handed to a replaced plugin alongside the authoritative
	// cursor so plugin-state loss cannot strand a committed but unmaterialized
	// event.
	DurableInbox *InboundInbox

	// Key-rotation callbacks. OnNamespaceKeyRotated
	// fires on the inbound key_rotated signal; OnNamespaceKeyBroadcast
	// fires when wrapped key material is pushed to this device.
	OnNamespaceKeyRotated   func(proto.RemoteNamespaceKeyRotatedNotification)
	OnNamespaceKeyBroadcast func(proto.RemoteNamespaceKeyBroadcastNotification)

	// RefreshIdentity, when non-nil, runs on its own goroutine after each
	// successful plugin (re)connection. The daemon wires it to re-query the
	// plugin's --status device id and propagate a rotated identity to every
	// stamping component. Pairing via `<plugin> --pair` on the CLI bypasses
	// the daemon's web API entirely, so the post-connect hook is the only
	// choke point where that rotation is reliably observed — without it the
	// daemon keeps stamping the RETIRED device id until restart.
	RefreshIdentity func(ctx context.Context)

	// Internal state.
	deviceMu              sync.RWMutex
	proxyMu               sync.Mutex
	proxy                 *proxy.RemoteProxy
	syncObservationSigned bool
	observationMu         sync.Mutex
	observations          *syncObservationQueue
	syncMu                sync.RWMutex
	syncMode              proto.RemoteNegotiateSyncV1Result
	// syncFinalizeSigned belongs to the exact authenticated plugin image that
	// produced syncMode. Runtime capability strings alone are untrusted.
	syncFinalizeSigned bool
	// syncStagedCheckpointSigned belongs to the exact authenticated plugin image
	// that produced syncMode. Runtime/server strings cannot grant file access.
	syncStagedCheckpointSigned bool
	// syncRedactionBatchSigned belongs to the exact authenticated plugin image
	// and is required before any multi-event durable delivery is admitted.
	syncRedactionBatchSigned bool
	transfer                 *remoteTransferSession
	cmdMu                    sync.Mutex
	cmd                      *exec.Cmd
	stopped                  atomic.Bool
	stopOnce                 sync.Once
	doneCh                   chan struct{}
	cancel                   context.CancelFunc

	// Live status counters surfaced via Snapshot().
	lastConnState atomic.Value // string
	lastConnAt    atomic.Value // time.Time
	restartCount  atomic.Uint64
}

type remoteSyncNegotiator interface {
	NegotiateSyncV1(context.Context, proto.RemoteNegotiateSyncV1Params) (proto.RemoteNegotiateSyncV1Result, error)
	ResumeCursorV1(context.Context, proto.RemoteResumeCursorV1Params) (proto.RemoteResumeCursorV1Result, error)
}

type remoteMultiStreamResumer interface {
	ResumeCursorsV1(context.Context, proto.RemoteResumeCursorsV1Params) (proto.RemoteResumeCursorsV1Result, error)
}

// ErrRemoteReconnecting is returned by Publish/Fetch/etc. when the
// plugin is in the middle of being restarted. Callers should treat
// this as a transient retryable signal.
var ErrRemoteReconnecting = errors.New("daemon: remote plugin reconnecting")

// ErrRemoteNotConfigured is returned when the runner has not yet
// observed a successful initialize handshake.
var ErrRemoteNotConfigured = errors.New("daemon: remote plugin not configured")

// ErrRemoteFinalizeRecoveryBlocked means daemon-owned terminal evidence
// survived plugin state loss, but the replacement plugin/server generation
// could not prove authority to drain that exact stream and epoch. The caller
// must keep durable sync inactive; inventing a replacement token or accepting
// a newly selected epoch would strand an already-committed cloud cursor.
var ErrRemoteFinalizeRecoveryBlocked = errors.New("daemon: pending inbound finalize recovery blocked")

// reconnectBackoffMin is the lower bound for the exponential backoff
// applied after a plugin crash or initialize failure.
const reconnectBackoffMin = 2 * time.Second

// remoteSyncCompiledMaximumMode is an independent daemon rollout ceiling.
// Preferred cutover is admitted only after the signed capability, runtime,
// all-device, checkpoint, stream, cursor, replay-batch, and staged-transfer
// gates below pass. Delta-required remains dark in this build.
const remoteSyncCompiledMaximumMode = proto.RemoteSyncModeDeltaPreferred

// reconnectBackoffMax caps the backoff at 60s — long enough that we
// don't busy-loop, short enough that recovery feels responsive once
// the upstream issue clears.
const reconnectBackoffMax = 60 * time.Second

// SetDeviceID updates the cloud-assigned identity used by future plugin
// handshakes and by daemon services that resolve identity lazily. Pairing calls
// this before Restart so the replacement child is initialized with the new
// identity.
func (r *RemoteRunner) SetDeviceID(deviceID string) {
	if r == nil {
		return
	}
	r.deviceMu.Lock()
	r.DeviceID = deviceID
	r.deviceMu.Unlock()
}

// CurrentDeviceID returns the latest cloud-assigned identity. Keeping the
// exported DeviceID field preserves keyed struct initialization compatibility;
// after a runner is shared, all production access must go through these
// methods.
func (r *RemoteRunner) CurrentDeviceID() string {
	if r == nil {
		return ""
	}
	r.deviceMu.RLock()
	deviceID := r.DeviceID
	r.deviceMu.RUnlock()
	return deviceID
}

// Start launches the runner's supervision goroutine and returns. The
// goroutine respawns the plugin on any failure until ctx is
// cancelled. Calling Start twice on the same Runner is a programmer
// error (no protection — wrap construction in your own once-guard
// when needed).
func (r *RemoteRunner) Start(ctx context.Context) {
	supervisedCtx, cancel := context.WithCancel(ctx)
	r.cancel = cancel
	r.doneCh = make(chan struct{})
	r.lastConnState.Store("starting")
	r.startSyncObservationQueue(supervisedCtx)
	go r.supervise(supervisedCtx)
}

// Stop signals the supervisor to shut down and waits for the plugin
// to exit. Idempotent.
func (r *RemoteRunner) Stop(ctx context.Context) error {
	r.stopOnce.Do(func() {
		r.stopped.Store(true)
		if r.cancel != nil {
			r.cancel()
		}
	})
	if r.doneCh == nil {
		return nil
	}
	select {
	case <-r.doneCh:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *RemoteRunner) supervise(ctx context.Context) {
	defer close(r.doneCh)
	backoff := reconnectBackoffMin
	for {
		select {
		case <-ctx.Done():
			r.tearDownProxy()
			return
		default:
		}

		if err := r.runOnce(ctx); err != nil {
			r.warn("remote plugin exited", "err", err, "backoff", backoff)
			r.restartCount.Add(1)
			r.lastConnState.Store("disconnected")
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff = nextBackoff(backoff)
			continue
		}
		// runOnce returned nil = clean shutdown, no restart.
		return
	}
}

func nextBackoff(cur time.Duration) time.Duration {
	next := cur * 2
	if next > reconnectBackoffMax {
		next = reconnectBackoffMax
	}
	return next
}

// runOnce spawns the plugin, opens the RemoteProxy, wires callbacks,
// and blocks until either the proxy's read pump returns OR ctx is
// cancelled. Returns nil for "clean shutdown — don't restart"; any
// non-nil error triggers the supervisor's backoff + reconnect.
func (r *RemoteRunner) runOnce(ctx context.Context) error {
	r.setSyncNegotiation(legacyRemoteSyncNegotiation("plugin not negotiated"))
	verified, err := r.verifyPlugin(r.Executable)
	if err != nil {
		return fmt.Errorf("verify remote plugin before spawn: %w", err)
	}
	if _, err := r.TrustStore.VerifyCurrent(r.Executable, verified, r.TrustPolicy); err != nil {
		return fmt.Errorf("verify remote plugin rollback checkpoint before spawn: %w", err)
	}
	manifestIdentity := verified
	manifest := verified.Manifest
	if !manifest.HasCapability(proto.CapabilityTrustProtocolV1) || !manifest.HasCapability(proto.CapabilityInboundAckV2) {
		return fmt.Errorf("remote plugin lacks required trust or inbound acknowledgement capability")
	}
	verified, err = r.verifyPlugin(r.Executable)
	if err != nil {
		return fmt.Errorf("remote plugin identity changed before launch: %w", err)
	}
	if verified.ManifestSHA256 != manifestIdentity.ManifestSHA256 ||
		verified.InventorySHA256 != manifestIdentity.InventorySHA256 ||
		verified.PublisherKeySHA256 != manifestIdentity.PublisherKeySHA256 ||
		verified.Manifest.BinarySHA256 != manifestIdentity.Manifest.BinarySHA256 {
		return fmt.Errorf("remote plugin identity changed before launch")
	}
	if _, err := r.TrustStore.VerifyCurrent(r.Executable, verified, r.TrustPolicy); err != nil {
		return fmt.Errorf("remote plugin checkpoint changed before launch: %w", err)
	}
	// Capture the authenticated image only after the final manifest, inventory,
	// publisher, and checkpoint check. On macOS this retained O_NOFOLLOW inode is
	// re-hashed and its immutable pathname identity is revalidated immediately
	// before the small in-process pipe setup and direct exec.
	prepared, err := secureexec.Prepare(ctx, r.Executable, verified.Manifest.BinarySHA256)
	if err != nil {
		return fmt.Errorf("prepare authenticated remote plugin launch: %w", err)
	}
	defer func() { _ = prepared.Close() }()
	cmd := prepared.Cmd()
	var transfer *remoteTransferSession
	if manifest.HasCapability(proto.CapabilityStagedCheckpointV1) {
		transfer, err = prepareRemoteTransferSession(r.TransferRoot)
		if err != nil {
			return fmt.Errorf("prepare private remote checkpoint transfer: %w", err)
		}
		defer func() { _ = transfer.Close() }()
	}
	configureRemoteTransferEnvironment(cmd, transfer)
	// The plugin is a console-subsystem binary; on Windows suppress its console
	// window (DETACHED_PROCESS) so each (re)spawn shows no terminal — the
	// supervisor may respawn it on backoff. No-op on non-windows.
	hideChildWindow(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	// Forward the plugin's stderr into the daemon log, one line per write
	// chunk, so failure-mode diagnostics (auth failures, crash-on-spawn,
	// flapping) surface via `aplexica daemon logs`. Setting cmd.Stderr = nil
	// would instead route it to the null device (os/exec semantics), and the
	// daemon's own stderr is not redirected to its log file, so those
	// diagnostics would vanish. Assigning an io.Writer here is independent of
	// the DETACHED_PROCESS console flag set above, so Windows console
	// suppression is unaffected. Mirrors internal/plugin/manager's stderrLogger.
	cmd.Stderr = &stderrLogWriter{logger: r.Logger}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start plugin: %w", err)
	}
	// Start returns only after the child image has been resolved. Linux has
	// executed the sealed memfd; Windows no longer needs its launch locks; macOS
	// launched from the validated privileged immutable path.
	if err := prepared.Close(); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return fmt.Errorf("release authenticated launch resources: %w", err)
	}
	r.info("remote plugin spawned", "executable", r.Executable, "pid", cmd.Process.Pid)
	r.setCmd(cmd)
	r.lastConnState.Store("connecting")

	// pipeShim adapts the separate stdin/stdout into one
	// io.ReadWriter that closes both sides on the proxy's Close.
	transport := &pipeShim{r: stdout, w: stdin, cmd: cmd}

	openCtx, cancelOpen := context.WithTimeout(ctx, remotePluginOpenTimeout)
	rp, err := proxy.OpenRemote(openCtx, transport, r.CurrentDeviceID(), r.Version)
	cancelOpen()
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return fmt.Errorf("open remote proxy: %w", err)
	}

	// Wire notification callbacks. The proxy's OnInbound/OnConnState/
	// OnEnumerateHint store the callback before the read pump dispatches
	// any notifications; since the proxy's pump started inside
	// OpenRemote and the initialize-response is the first frame, any
	// notification arrives strictly after this wiring.
	rp.OnInbound(r.OnInbound)
	inboundV2 := paceRetainedInboundV2(ctx, r.RetainedInboundInterval, r.OnInboundV2)
	if inboundV2 != nil {
		rp.OnInboundV2(func(delivery proto.RemoteInboundDeliveryV2) proto.RemoteInboundAckV2 {
			r.beginSyncObservationInboundBarrier()
			return inboundV2(delivery)
		})
		rp.OnInboundV2ResponseWritten(r.endSyncObservationInboundBarrier)
	}
	if r.OnInboundFinalizeV1 != nil {
		rp.OnInboundFinalizeV1(func(params proto.RemoteInboundFinalizeV1Params) proto.RemoteInboundFinalizeV1Result {
			r.beginSyncObservationInboundBarrier()
			return r.OnInboundFinalizeV1(params)
		})
		rp.OnInboundFinalizeV1ResponseWritten(r.endSyncObservationInboundBarrier)
	}
	rp.OnConnState(func(state, human string) {
		r.lastConnState.Store(state)
		r.lastConnAt.Store(time.Now().UTC())
		if r.OnConnState != nil {
			r.OnConnState(state, human)
		}
	})
	rp.OnEnumerateHint(r.OnEnumerateHint)
	rp.OnCheckpointNeededV1(func(notification proto.RemoteCheckpointNeededV1Notification) {
		if r.OnCheckpointNeededV1 != nil {
			r.OnCheckpointNeededV1(notification)
		}
	})
	rp.OnRulesUpdate(func(changeID string, rules []syncrules.Rule) {
		if r.OnRulesUpdate != nil {
			r.OnRulesUpdate(changeID, rules)
		}
	})
	rp.OnNamespaceKeyRotated(func(n proto.RemoteNamespaceKeyRotatedNotification) {
		if r.OnNamespaceKeyRotated != nil {
			r.OnNamespaceKeyRotated(n)
		}
	})
	rp.OnNamespaceKeyBroadcast(func(n proto.RemoteNamespaceKeyBroadcastNotification) {
		if r.OnNamespaceKeyBroadcast != nil {
			r.OnNamespaceKeyBroadcast(n)
		}
	})

	negotiated, negotiateErr := r.negotiateRemoteSyncV1(ctx, rp, manifest)
	if negotiateErr != nil {
		r.warn("remote durable sync negotiation constrained; using safe selected mode", "sync_mode", negotiated.Mode, "err", negotiateErr)
	}
	r.setSyncNegotiationWithManifest(negotiated, manifest)

	// The cloud plugin expires durable FetchV2/FetchParentV1/AckV2 authority
	// five minutes after negotiation. Refresh well inside that window. The
	// per-run context is cancelled and joined before runOnce returns, so an old
	// plugin generation can never overwrite a replacement's negotiation state.
	stopSyncRefresh := func() {}
	if manifest.HasCapability(proto.CapabilityDurableDeltaSyncV1) {
		refreshCtx, cancel := context.WithCancel(ctx)
		syncRefreshDone := make(chan struct{})
		go func() {
			defer close(syncRefreshDone)
			r.runRemoteSyncNegotiationRefresh(refreshCtx, rp, manifest)
		}()
		var stopOnce sync.Once
		stopSyncRefresh = func() {
			stopOnce.Do(func() {
				cancel()
				<-syncRefreshDone
			})
		}
		defer stopSyncRefresh()
	}

	r.swapProxy(rp, transfer, manifest.HasCapability(proto.CapabilityDurableSyncObservationV1))
	r.lastConnState.Store("connected")
	r.lastConnAt.Store(time.Now().UTC())
	r.info("remote plugin ready", "name", rp.Name(), "version", rp.Version(), "sync_mode", negotiated.Mode)
	if r.RefreshIdentity != nil {
		// Own goroutine: the hook spawns a one-shot `--status` plugin process
		// and must not delay the connected-run wait below. ctx is the
		// supervisor context, cancelled at daemon shutdown.
		go r.RefreshIdentity(ctx)
	}

	// Block until ctx cancels (clean shutdown) or the underlying
	// process exits (crash; supervisor will respawn).
	waitErr := make(chan error, 1)
	go func() { waitErr <- cmd.Wait() }()

	select {
	case <-ctx.Done():
		stopSyncRefresh()
		_ = rp.Shutdown(context.Background())
		<-waitErr
		r.tearDownProxy()
		r.lastConnState.Store("disconnected")
		return nil
	case err := <-waitErr:
		stopSyncRefresh()
		r.tearDownProxy()
		if err != nil {
			return fmt.Errorf("plugin process exited: %w", err)
		}
		return errors.New("plugin process exited cleanly without shutdown")
	}
}

func paceRetainedInboundV2(ctx context.Context, interval time.Duration, next func(proto.RemoteInboundDeliveryV2) proto.RemoteInboundAckV2) func(proto.RemoteInboundDeliveryV2) proto.RemoteInboundAckV2 {
	if next == nil || interval <= 0 {
		return next
	}
	var mu sync.Mutex
	var nextSlot time.Time
	return func(delivery proto.RemoteInboundDeliveryV2) proto.RemoteInboundAckV2 {
		if remoteDeliveryContainsLane(delivery, "retained") {
			now := time.Now()
			mu.Lock()
			slot := now
			if nextSlot.After(slot) {
				slot = nextSlot
			}
			nextSlot = slot.Add(interval)
			mu.Unlock()

			if wait := time.Until(slot); wait > 0 {
				timer := time.NewTimer(wait)
				defer timer.Stop()
				select {
				case <-ctx.Done():
					return retryableRemoteInboundAck(delivery, "daemon-shutdown")
				case <-timer.C:
				}
			}
		}
		return next(delivery)
	}
}

func remoteDeliveryContainsLane(delivery proto.RemoteInboundDeliveryV2, lane string) bool {
	for _, event := range delivery.Events {
		if event.Lane == lane {
			return true
		}
	}
	return false
}

func retryableRemoteInboundAck(delivery proto.RemoteInboundDeliveryV2, reason string) proto.RemoteInboundAckV2 {
	ack := proto.RemoteInboundAckV2{DeliveryID: delivery.DeliveryID, Outcomes: make([]proto.RemoteInboundEventOutcomeV2, len(delivery.Events))}
	for i := range ack.Outcomes {
		ack.Outcomes[i] = proto.RemoteInboundEventOutcomeV2{Index: uint32(i), Disposition: "retryable", ReasonCode: reason}
	}
	return ack
}

func (r *RemoteRunner) verifyPlugin(execPath string) (proto.VerifiedRemotePlugin, error) {
	if r.PluginVerifier != nil {
		return r.PluginVerifier(execPath)
	}
	return proto.VerifyRemotePluginDetailed(execPath, r.PublisherKeys)
}

func (r *RemoteRunner) swapProxy(p *proxy.RemoteProxy, transfer *remoteTransferSession, syncObservationSigned bool) {
	r.proxyMu.Lock()
	defer r.proxyMu.Unlock()
	r.proxy = p
	r.transfer = transfer
	r.syncObservationSigned = syncObservationSigned
}

func (r *RemoteRunner) tearDownProxy() {
	r.proxyMu.Lock()
	r.proxy = nil
	r.transfer = nil
	r.syncObservationSigned = false
	r.proxyMu.Unlock()
	r.resetSyncObservationInboundBarrier()
	r.setSyncNegotiation(legacyRemoteSyncNegotiation("plugin disconnected"))
}

func legacyRemoteSyncNegotiation(reason string) proto.RemoteNegotiateSyncV1Result {
	return proto.RemoteNegotiateSyncV1Result{Mode: proto.RemoteSyncModeLegacy, Reason: reason}
}

func validateRemoteSyncNegotiation(candidate proto.RemoteNegotiateSyncV1Result, signedCapability, signedCursorResumeCapability, signedFinalizeCapability, signedMultiStreamCapability bool, signedBatchAndStaged ...bool) proto.RemoteNegotiateSyncV1Result {
	legacy := legacyRemoteSyncNegotiation("durable sync safety gate unavailable")
	if !signedCapability || candidate.Mode == "" || candidate.Mode == proto.RemoteSyncModeLegacy {
		return legacy
	}
	if candidate.SelectedProtocol != 1 ||
		!candidate.FeatureGateEnabled ||
		!remoteStringSetContains(candidate.ServerCapabilities, proto.CapabilityDurableDeltaSyncV1) ||
		!remoteStringSetContains(candidate.ServerCapabilities, proto.CapabilityInboundAckV2) ||
		candidate.StreamID == "" ||
		candidate.StreamEpoch == "" ||
		candidate.MaxEventBytes == 0 ||
		candidate.MaxPageEvents == 0 ||
		candidate.MaxPageBytes == 0 {
		return legacy
	}
	if len(candidate.Streams) != 0 && !validRemoteStreamDescriptors(candidate, false) {
		return legacy
	}
	if len(candidate.Streams) != 0 && (!signedMultiStreamCapability || !remoteStringSetContains(candidate.ServerCapabilities, proto.CapabilityDurableMultiStreamV1)) {
		return legacy
	}
	if remoteSyncModeRank(candidate.Mode) > remoteSyncModeRank(remoteSyncCompiledMaximumMode) {
		return legacy
	}
	switch candidate.Mode {
	case proto.RemoteSyncModeShadow:
		return candidate
	case proto.RemoteSyncModeDurableRead, proto.RemoteSyncModeDeltaPreferred, proto.RemoteSyncModeDeltaRequired:
		if durableCutoverCapabilitiesReady(candidate, signedCursorResumeCapability, signedFinalizeCapability, signedMultiStreamCapability, signedBatchAndStaged...) {
			return candidate
		}
	}
	return legacy
}

func durableCutoverCapabilitiesReady(candidate proto.RemoteNegotiateSyncV1Result, signedCursorResumeCapability, signedFinalizeCapability, signedMultiStreamCapability bool, signedBatchAndStaged ...bool) bool {
	signedBatch, signedStaged := len(signedBatchAndStaged) == 2 && signedBatchAndStaged[0], len(signedBatchAndStaged) == 2 && signedBatchAndStaged[1]
	return candidate.AllActiveDevicesCapable && candidate.CheckpointReady && signedCursorResumeCapability && signedFinalizeCapability && signedMultiStreamCapability && signedBatch && signedStaged &&
		remoteStringSetContains(candidate.ServerCapabilities, proto.CapabilityDurableCursorResumeV1) &&
		remoteStringSetContains(candidate.ServerCapabilities, proto.CapabilityInboundFinalizeV1) &&
		remoteStringSetContains(candidate.ServerCapabilities, proto.CapabilityDurableMultiStreamV1) &&
		remoteStringSetContains(candidate.ServerCapabilities, proto.CapabilityRedactionSafeBatchV1) &&
		remoteStringSetContains(candidate.ServerCapabilities, proto.CapabilityStagedCheckpointV1) &&
		validRemoteStreamDescriptors(candidate, true)
}

func validRemoteStreamDescriptors(candidate proto.RemoteNegotiateSyncV1Result, requireCheckpoint bool) bool {
	if len(candidate.Streams) == 0 || len(candidate.Streams) > 128 {
		return false
	}
	streamIDs := make(map[string]struct{}, len(candidate.Streams))
	for index, descriptor := range candidate.Streams {
		if !validDurableCursorOpaque(descriptor.StreamID, durableCursorMaxIdentity) ||
			!validDurableCursorOpaque(descriptor.StreamEpoch, durableCursorMaxIdentity) ||
			(descriptor.NamespaceID != "" && !validDurableCursorOpaque(descriptor.NamespaceID, durableCursorMaxIdentity)) ||
			(descriptor.MinAvailableCursor != "" && !validDurableCursorOpaque(descriptor.MinAvailableCursor, proto.MaxDurableCursorBytes)) ||
			(descriptor.TipCursor != "" && !validDurableCursorOpaque(descriptor.TipCursor, proto.MaxDurableCursorBytes)) ||
			(descriptor.TipCursor == "" && descriptor.TipPosition != 0) ||
			(requireCheckpoint && (descriptor.MinAvailableCursor == "" || descriptor.TipCursor == "")) ||
			descriptor.MaxEventBytes == 0 || descriptor.MaxEventBytes > candidate.MaxEventBytes ||
			descriptor.MaxPageEvents == 0 || descriptor.MaxPageEvents > candidate.MaxPageEvents ||
			descriptor.MaxPageBytes == 0 || descriptor.MaxPageBytes > candidate.MaxPageBytes ||
			(requireCheckpoint && !descriptor.CheckpointReady) {
			return false
		}
		if _, duplicate := streamIDs[descriptor.StreamID]; duplicate {
			return false
		}
		if index == 0 {
			if descriptor.NamespaceID != "" || descriptor.StreamID != candidate.StreamID || descriptor.StreamEpoch != candidate.StreamEpoch {
				return false
			}
		} else if descriptor.NamespaceID == "" || candidate.Streams[index-1].NamespaceID >= descriptor.NamespaceID {
			return false
		}
		streamIDs[descriptor.StreamID] = struct{}{}
	}
	return true
}

func remoteSyncNegotiationParams() proto.RemoteNegotiateSyncV1Params {
	return proto.RemoteNegotiateSyncV1Params{
		ProtocolMin:          1,
		ProtocolMax:          1,
		DaemonCapabilities:   []string{proto.CapabilityDurableCursorResumeV1, proto.CapabilityDurableDeltaSyncV1, proto.CapabilityDurableMultiStreamV1, proto.CapabilityInboundAckV2, proto.CapabilityInboundFinalizeV1, proto.CapabilityRedactionSafeBatchV1, proto.CapabilityStagedCheckpointV1},
		RequestedMaximumMode: remoteSyncCompiledMaximumMode,
	}
}

// pendingFinalizeEvidenceForNegotiation finds the single daemon-owned
// terminal obligation across every retained stream epoch. The durable inbox is
// authoritative for whether native finalization is outstanding; the cursor
// store must independently prove that the same obligation is the exact
// committed cloud cursor. Any ambiguity fails closed before a replacement
// plugin is allowed to ask the server for a new generation.
func (r *RemoteRunner) pendingFinalizeEvidenceForNegotiation() (*proto.RemoteInboundFinalizeEvidenceV1, error) {
	if r == nil || r.DurableInbox == nil {
		return nil, nil
	}
	completed, err := r.DurableInbox.CompletedDurable()
	if err != nil {
		return nil, fmt.Errorf("%w: durable inbox scan failed: %v", ErrRemoteFinalizeRecoveryBlocked, err)
	}
	var obligation *InboundDurableCompletion
	for index := range completed {
		completion := &completed[index]
		if completion.NativeFinalized {
			continue
		}
		if obligation != nil {
			return nil, fmt.Errorf("%w: multiple unfinalized terminal obligations", ErrRemoteFinalizeRecoveryBlocked)
		}
		if completion.Ack.FinalizeEvidence == nil {
			return nil, fmt.Errorf("%w: terminal obligation has no exact evidence", ErrRemoteFinalizeRecoveryBlocked)
		}
		obligation = completion
	}
	if obligation == nil {
		return nil, nil
	}
	if r.DurableCursorStore == nil {
		return nil, fmt.Errorf("%w: durable cursor store unavailable", ErrRemoteFinalizeRecoveryBlocked)
	}
	remoteIdentity := r.CurrentDeviceID()
	evidence := obligation.Ack.FinalizeEvidence
	if evidence == nil || evidence.RemoteIdentity != remoteIdentity || obligation.RemoteIdentity != remoteIdentity {
		return nil, fmt.Errorf("%w: terminal obligation device identity mismatch", ErrRemoteFinalizeRecoveryBlocked)
	}
	key := DurableCursorKey{RemoteIdentity: remoteIdentity, StreamID: obligation.StreamID, StreamEpoch: obligation.StreamEpoch}
	if validateDurableCursorKey(key) != nil {
		return nil, fmt.Errorf("%w: terminal obligation cursor key malformed", ErrRemoteFinalizeRecoveryBlocked)
	}
	// CompleteDurable precedes cursor CAS by design. A process crash in that
	// narrow window leaves the exact terminal record naming either a missing
	// genesis cursor or the still-current adjacent predecessor. Repair that
	// authenticated transition before deciding recovery is corrupt.
	state, err := r.DurableCursorStore.RepairFromCompletedDurable(*obligation)
	if err != nil {
		return nil, fmt.Errorf("%w: terminal obligation cursor repair unavailable: %v", ErrRemoteFinalizeRecoveryBlocked, err)
	}
	if validateDurableCursorValue(state, true) != nil || state.Position == 0 ||
		state.Cursor != obligation.Cursor || state.CursorDigest != obligation.CursorDigest || state.Position != obligation.Position {
		return nil, fmt.Errorf("%w: terminal obligation does not match daemon cursor", ErrRemoteFinalizeRecoveryBlocked)
	}
	exact, err := r.DurableInbox.PendingFinalizeEvidence(key, state)
	if err != nil || exact == nil || *exact != *evidence {
		if err != nil {
			return nil, fmt.Errorf("%w: exact terminal evidence unavailable: %v", ErrRemoteFinalizeRecoveryBlocked, err)
		}
		return nil, fmt.Errorf("%w: exact terminal evidence mismatch", ErrRemoteFinalizeRecoveryBlocked)
	}
	copyEvidence := *exact
	return &copyEvidence, nil
}

func samePendingFinalizeEvidence(left, right *proto.RemoteInboundFinalizeEvidenceV1) bool {
	return (left == nil) == (right == nil) && (left == nil || *left == *right)
}

func remoteNegotiationHasStream(result proto.RemoteNegotiateSyncV1Result, streamID, streamEpoch string) bool {
	if len(result.Streams) == 0 {
		return result.StreamID == streamID && result.StreamEpoch == streamEpoch
	}
	for _, descriptor := range result.Streams {
		if descriptor.StreamID == streamID && descriptor.StreamEpoch == streamEpoch {
			return true
		}
	}
	return false
}

func remoteNegotiationHasScopedStream(result proto.RemoteNegotiateSyncV1Result, streamID, streamEpoch, namespaceID string) bool {
	if len(result.Streams) == 0 {
		return result.StreamID == streamID && result.StreamEpoch == streamEpoch
	}
	for _, descriptor := range result.Streams {
		if descriptor.StreamID == streamID && descriptor.StreamEpoch == streamEpoch && descriptor.NamespaceID == namespaceID {
			return true
		}
	}
	return false
}

func remoteNegotiationAuthorizesEvidenceStream(result proto.RemoteNegotiateSyncV1Result, evidence *proto.RemoteInboundFinalizeEvidenceV1) bool {
	return evidence != nil && remoteNegotiationHasScopedStream(result, evidence.StreamID, evidence.StreamEpoch, evidence.NamespaceID)
}

func recoveryNegotiationAuthorizesEvidence(candidate, negotiated proto.RemoteNegotiateSyncV1Result, evidence *proto.RemoteInboundFinalizeEvidenceV1, signedResume, signedFinalize bool, signedBatch ...bool) bool {
	batchAuthorized := evidence != nil && (evidence.BatchEventCount == 0 || len(signedBatch) == 1 && signedBatch[0] && remoteStringSetContains(candidate.ServerCapabilities, proto.CapabilityRedactionSafeBatchV1) && remoteStringSetContains(negotiated.ServerCapabilities, proto.CapabilityRedactionSafeBatchV1))
	return evidence != nil && batchAuthorized && signedResume && signedFinalize && negotiated.Mode != proto.RemoteSyncModeLegacy &&
		candidate.SelectedProtocol == evidence.ProtocolVersion && remoteNegotiationAuthorizesEvidenceStream(candidate, evidence) &&
		remoteNegotiationAuthorizesEvidenceStream(negotiated, evidence) &&
		remoteStringSetContains(candidate.ServerCapabilities, proto.CapabilityDurableCursorResumeV1) &&
		remoteStringSetContains(candidate.ServerCapabilities, proto.CapabilityInboundFinalizeV1)
}

// validatedFinalizedPluginProposal treats replacement-plugin evidence only as
// a proposal. The daemon accepts it when its own current cursor and retained
// terminal record independently prove the identical full tuple was already
// native-finalized. This permits a lost cloud-ACK response to be retried and
// answered AlreadyFinalized without reopening materialization authority.
func (r *RemoteRunner) validatedFinalizedPluginProposal(candidate proto.RemoteNegotiateSyncV1Result) (*proto.RemoteInboundFinalizeEvidenceV1, error) {
	proposal := candidate.PendingFinalizeEvidence
	if r == nil || proposal == nil || r.DurableCursorStore == nil || r.DurableInbox == nil {
		return nil, fmt.Errorf("%w: plugin terminal proposal lacks daemon state", ErrRemoteFinalizeRecoveryBlocked)
	}
	remoteIdentity := r.CurrentDeviceID()
	if proposal.RemoteIdentity != remoteIdentity || proposal.ProtocolVersion != candidate.SelectedProtocol ||
		!remoteNegotiationAuthorizesEvidenceStream(candidate, proposal) {
		return nil, fmt.Errorf("%w: plugin terminal proposal generation mismatch", ErrRemoteFinalizeRecoveryBlocked)
	}
	key := DurableCursorKey{RemoteIdentity: remoteIdentity, StreamID: proposal.StreamID, StreamEpoch: proposal.StreamEpoch}
	if validateDurableCursorKey(key) != nil {
		return nil, fmt.Errorf("%w: plugin terminal proposal cursor key malformed", ErrRemoteFinalizeRecoveryBlocked)
	}
	state, err := r.DurableCursorStore.Load(key)
	if err != nil || validateDurableCursorValue(state, true) != nil || state.Position == 0 ||
		state.Cursor != proposal.Cursor || state.CursorDigest != proposal.CursorDigest || state.Position != proposal.Position {
		if err != nil {
			return nil, fmt.Errorf("%w: plugin terminal proposal cursor unavailable: %v", ErrRemoteFinalizeRecoveryBlocked, err)
		}
		return nil, fmt.Errorf("%w: plugin terminal proposal is not daemon current cursor", ErrRemoteFinalizeRecoveryBlocked)
	}
	retained, finalized, err := r.DurableInbox.RetainedFinalizeEvidenceAtCursor(key, state)
	if err != nil || retained == nil || !finalized || *retained != *proposal {
		if err != nil {
			return nil, fmt.Errorf("%w: plugin terminal proposal has no exact retained completion: %v", ErrRemoteFinalizeRecoveryBlocked, err)
		}
		return nil, fmt.Errorf("%w: plugin terminal proposal is not exact already-finalized evidence", ErrRemoteFinalizeRecoveryBlocked)
	}
	copyEvidence := *retained
	return &copyEvidence, nil
}

// negotiateRemoteSyncV1 performs one complete authenticated negotiation
// generation. The signed manifest gates every runtime capability; a cursor
// handoff is part of the same generation and must finish before an active
// durable mode can be selected. The returned result is always safe to install,
// even when err reports why it was constrained to shadow or legacy.
func (r *RemoteRunner) negotiateRemoteSyncV1(ctx context.Context, remote remoteSyncNegotiator, manifest proto.RemotePluginManifestUnsignedV1) (proto.RemoteNegotiateSyncV1Result, error) {
	pending, pendingErr := r.pendingFinalizeEvidenceForNegotiation()
	if pendingErr != nil {
		return legacyRemoteSyncNegotiation(proto.RemoteSyncReasonTerminalFinalizeRecoveryStateInvalid), pendingErr
	}
	signedResume := manifest.HasCapability(proto.CapabilityDurableCursorResumeV1)
	signedFinalize := manifest.HasCapability(proto.CapabilityInboundFinalizeV1)
	signedMultiStream := manifest.HasCapability(proto.CapabilityDurableMultiStreamV1)
	signedBatch := manifest.HasCapability(proto.CapabilityRedactionSafeBatchV1)
	signedStaged := manifest.HasCapability(proto.CapabilityStagedCheckpointV1)
	if pending != nil && (remote == nil || !manifest.HasCapability(proto.CapabilityDurableDeltaSyncV1) || !signedResume || !signedFinalize) {
		return legacyRemoteSyncNegotiation(proto.RemoteSyncReasonTerminalFinalizeCapabilityUnavailable), fmt.Errorf("%w: signed plugin recovery capability unavailable", ErrRemoteFinalizeRecoveryBlocked)
	}
	if pending != nil && pending.BatchEventCount != 0 && !signedBatch {
		return legacyRemoteSyncNegotiation(proto.RemoteSyncReasonTerminalFinalizeCapabilityUnavailable), fmt.Errorf("%w: signed plugin batch recovery capability unavailable", ErrRemoteFinalizeRecoveryBlocked)
	}
	if remote == nil || !manifest.HasCapability(proto.CapabilityDurableDeltaSyncV1) {
		return legacyRemoteSyncNegotiation("signed durable capability unavailable"), nil
	}
	negotiationParams := remoteSyncNegotiationParams()
	negotiationParams.PendingFinalizeEvidence = pending
	negotiateCtx, cancelNegotiate := context.WithTimeout(ctx, remoteSyncNegotiationTimeout)
	candidate, err := remote.NegotiateSyncV1(negotiateCtx, negotiationParams)
	cancelNegotiate()
	if err != nil {
		if pending != nil {
			return legacyRemoteSyncNegotiation(proto.RemoteSyncReasonTerminalFinalizeRecoveryBlocked), fmt.Errorf("%w: fresh server negotiation unavailable: %v", ErrRemoteFinalizeRecoveryBlocked, err)
		}
		return legacyRemoteSyncNegotiation("runtime negotiation unavailable"), err
	}
	if pending != nil && !samePendingFinalizeEvidence(candidate.PendingFinalizeEvidence, pending) {
		return legacyRemoteSyncNegotiation(proto.RemoteSyncReasonTerminalFinalizeRecoveryStateInvalid), fmt.Errorf("%w: replacement plugin did not echo exact daemon terminal evidence", ErrRemoteFinalizeRecoveryBlocked)
	}
	negotiated := validateRemoteSyncNegotiation(candidate, true, signedResume, signedFinalize, signedMultiStream, signedBatch, signedStaged)
	recoveryEvidence := pending
	if pending == nil && candidate.PendingFinalizeEvidence != nil {
		if !signedResume || !signedFinalize {
			return legacyRemoteSyncNegotiation(proto.RemoteSyncReasonTerminalFinalizeCapabilityUnavailable), fmt.Errorf("%w: plugin terminal proposal lacks signed recovery capability", ErrRemoteFinalizeRecoveryBlocked)
		}
		recoveryEvidence, err = r.validatedFinalizedPluginProposal(candidate)
		if err != nil {
			return legacyRemoteSyncNegotiation(proto.RemoteSyncReasonTerminalFinalizeRecoveryStateInvalid), err
		}
	}
	if recoveryEvidence != nil && !recoveryNegotiationAuthorizesEvidence(candidate, negotiated, recoveryEvidence, signedResume, signedFinalize, signedBatch) {
		reason := proto.RemoteSyncReasonTerminalFinalizeRecoveryBlocked
		if candidate.Mode != "" && candidate.Mode != proto.RemoteSyncModeLegacy && remoteNegotiationAuthorizesEvidenceStream(candidate, recoveryEvidence) &&
			(!remoteStringSetContains(candidate.ServerCapabilities, proto.CapabilityDurableCursorResumeV1) ||
				!remoteStringSetContains(candidate.ServerCapabilities, proto.CapabilityInboundFinalizeV1)) {
			reason = proto.RemoteSyncReasonTerminalFinalizeCapabilityUnavailable
		}
		return legacyRemoteSyncNegotiation(reason), fmt.Errorf("%w: fresh server authority does not match terminal stream epoch", ErrRemoteFinalizeRecoveryBlocked)
	}
	if candidate.Mode != proto.RemoteSyncModeLegacy && negotiated.Mode == proto.RemoteSyncModeLegacy {
		return negotiated, fmt.Errorf("remote durable sync mode %q rejected by daemon safety gates", candidate.Mode)
	}
	if negotiated.Mode == proto.RemoteSyncModeLegacy || !signedResume || !remoteStringSetContains(negotiated.ServerCapabilities, proto.CapabilityDurableCursorResumeV1) {
		return negotiated, nil
	}
	if len(negotiated.Streams) != 0 {
		return r.resumeNegotiatedMultiStream(ctx, remote, negotiated, pending, recoveryEvidence, signedFinalize)
	}
	return r.resumeNegotiatedSingleStream(ctx, remote, negotiated, pending, recoveryEvidence, signedFinalize)
}

func (r *RemoteRunner) resumeNegotiatedSingleStream(ctx context.Context, remote remoteSyncNegotiator, negotiated proto.RemoteNegotiateSyncV1Result, pending, recoveryEvidence *proto.RemoteInboundFinalizeEvidenceV1, signedFinalize bool) (proto.RemoteNegotiateSyncV1Result, error) {
	streamID, streamEpoch := negotiated.StreamID, negotiated.StreamEpoch
	if recoveryEvidence != nil && (streamID != recoveryEvidence.StreamID || streamEpoch != recoveryEvidence.StreamEpoch) {
		if !remoteNegotiationAuthorizesEvidenceStream(negotiated, recoveryEvidence) {
			return legacyRemoteSyncNegotiation(proto.RemoteSyncReasonTerminalFinalizeRecoveryStateInvalid), fmt.Errorf("%w: terminal stream is not negotiated", ErrRemoteFinalizeRecoveryBlocked)
		}
		streamID, streamEpoch = recoveryEvidence.StreamID, recoveryEvidence.StreamEpoch
	}
	params, err := r.authoritativeResumeCursorParamsForStream(streamID, streamEpoch)
	if recoveryEvidence != nil && err != nil {
		return legacyRemoteSyncNegotiation(proto.RemoteSyncReasonTerminalFinalizeRecoveryStateInvalid), fmt.Errorf("%w: authoritative evidence handoff unavailable: %v", ErrRemoteFinalizeRecoveryBlocked, err)
	}
	if pending != nil && !samePendingFinalizeEvidence(params.PendingFinalizeEvidence, pending) {
		return legacyRemoteSyncNegotiation(proto.RemoteSyncReasonTerminalFinalizeRecoveryStateInvalid), fmt.Errorf("%w: authoritative evidence changed during negotiation", ErrRemoteFinalizeRecoveryBlocked)
	}
	if pending == nil && recoveryEvidence != nil {
		if params.PendingFinalizeEvidence != nil {
			return legacyRemoteSyncNegotiation(proto.RemoteSyncReasonTerminalFinalizeRecoveryStateInvalid), fmt.Errorf("%w: daemon terminal state changed while validating plugin proposal", ErrRemoteFinalizeRecoveryBlocked)
		}
		evidence := *recoveryEvidence
		params.PendingFinalizeEvidence = &evidence
	}
	if recoveryEvidence == nil && err == nil && params.PendingFinalizeEvidence != nil {
		return legacyRemoteSyncNegotiation(proto.RemoteSyncReasonTerminalFinalizeRecoveryStateInvalid), fmt.Errorf("%w: unannounced terminal evidence appeared during negotiation", ErrRemoteFinalizeRecoveryBlocked)
	}
	if err == nil && params.PendingFinalizeEvidence != nil &&
		(!signedFinalize || !remoteStringSetContains(negotiated.ServerCapabilities, proto.CapabilityInboundFinalizeV1)) {
		return legacyRemoteSyncNegotiation(proto.RemoteSyncReasonTerminalFinalizeCapabilityUnavailable), fmt.Errorf("%w: replacement plugin cannot accept terminal evidence", ErrRemoteFinalizeRecoveryBlocked)
	}
	if err == nil {
		resumeCtx, cancelResume := context.WithTimeout(ctx, remoteSyncNegotiationTimeout)
		var resumed proto.RemoteResumeCursorV1Result
		resumed, err = remote.ResumeCursorV1(resumeCtx, params)
		cancelResume()
		if err == nil && !validAuthoritativeResumeCursorResult(params, resumed) {
			err = errors.New("remote plugin did not install exact daemon cursor")
		}
	}
	if err != nil && recoveryEvidence != nil {
		return legacyRemoteSyncNegotiation(proto.RemoteSyncReasonTerminalFinalizeRecoveryStateInvalid), fmt.Errorf("%w: replacement plugin did not confirm exact terminal evidence: %v", ErrRemoteFinalizeRecoveryBlocked, err)
	}
	if err != nil && negotiated.Mode != proto.RemoteSyncModeShadow {
		return legacyRemoteSyncNegotiation("authoritative cursor handoff unavailable"), err
	}
	return negotiated, err
}

func (r *RemoteRunner) resumeNegotiatedMultiStream(ctx context.Context, remote remoteSyncNegotiator, negotiated proto.RemoteNegotiateSyncV1Result, pending, recoveryEvidence *proto.RemoteInboundFinalizeEvidenceV1, signedFinalize bool) (proto.RemoteNegotiateSyncV1Result, error) {
	multi, ok := remote.(remoteMultiStreamResumer)
	if !ok {
		return legacyRemoteSyncNegotiation("atomic multistream cursor handoff unavailable"), errors.New("remote plugin does not implement atomic multistream cursor resume")
	}
	params, err := r.authoritativeResumeCursorsParams(negotiated)
	if err != nil {
		if recoveryEvidence != nil {
			return legacyRemoteSyncNegotiation(proto.RemoteSyncReasonTerminalFinalizeRecoveryStateInvalid), fmt.Errorf("%w: authoritative multistream evidence handoff unavailable: %v", ErrRemoteFinalizeRecoveryBlocked, err)
		}
		return legacyRemoteSyncNegotiation("authoritative multistream cursor handoff unavailable"), err
	}
	pendingIndex, installedPending, ambiguous := resumeCursorsPendingFinalize(params)
	if ambiguous {
		return legacyRemoteSyncNegotiation(proto.RemoteSyncReasonTerminalFinalizeRecoveryStateInvalid), fmt.Errorf("%w: multiple terminal obligations in cursor handoff", ErrRemoteFinalizeRecoveryBlocked)
	}
	if pending != nil && !samePendingFinalizeEvidence(installedPending, pending) {
		return legacyRemoteSyncNegotiation(proto.RemoteSyncReasonTerminalFinalizeRecoveryStateInvalid), fmt.Errorf("%w: authoritative multistream evidence changed during negotiation", ErrRemoteFinalizeRecoveryBlocked)
	}
	if pending == nil && recoveryEvidence != nil {
		if installedPending != nil {
			return legacyRemoteSyncNegotiation(proto.RemoteSyncReasonTerminalFinalizeRecoveryStateInvalid), fmt.Errorf("%w: daemon terminal state changed while validating plugin proposal", ErrRemoteFinalizeRecoveryBlocked)
		}
		pendingIndex = -1
		for index := range params.Cursors {
			cursor := params.Cursors[index]
			descriptor := negotiated.Streams[index]
			if cursor.StreamID == recoveryEvidence.StreamID && cursor.StreamEpoch == recoveryEvidence.StreamEpoch && descriptor.NamespaceID == recoveryEvidence.NamespaceID {
				if !cursor.CursorPresent || cursor.Cursor != recoveryEvidence.Cursor || cursor.CursorDigest != recoveryEvidence.CursorDigest || cursor.Position != recoveryEvidence.Position {
					return legacyRemoteSyncNegotiation(proto.RemoteSyncReasonTerminalFinalizeRecoveryStateInvalid), fmt.Errorf("%w: proposed terminal evidence is not the exact resumed cursor", ErrRemoteFinalizeRecoveryBlocked)
				}
				pendingIndex = index
				break
			}
		}
		if pendingIndex < 0 {
			return legacyRemoteSyncNegotiation(proto.RemoteSyncReasonTerminalFinalizeRecoveryStateInvalid), fmt.Errorf("%w: proposed terminal stream is absent from cursor handoff", ErrRemoteFinalizeRecoveryBlocked)
		}
		evidence := *recoveryEvidence
		params.Cursors[pendingIndex].PendingFinalizeEvidence = &evidence
		installedPending = &evidence
	}
	if recoveryEvidence == nil && installedPending != nil {
		return legacyRemoteSyncNegotiation(proto.RemoteSyncReasonTerminalFinalizeRecoveryStateInvalid), fmt.Errorf("%w: unannounced terminal evidence appeared during multistream negotiation", ErrRemoteFinalizeRecoveryBlocked)
	}
	if installedPending != nil && (!signedFinalize || !remoteStringSetContains(negotiated.ServerCapabilities, proto.CapabilityInboundFinalizeV1)) {
		return legacyRemoteSyncNegotiation(proto.RemoteSyncReasonTerminalFinalizeCapabilityUnavailable), fmt.Errorf("%w: replacement plugin cannot accept multistream terminal evidence", ErrRemoteFinalizeRecoveryBlocked)
	}
	resumeCtx, cancelResume := context.WithTimeout(ctx, remoteSyncNegotiationTimeout)
	resumed, err := multi.ResumeCursorsV1(resumeCtx, params)
	cancelResume()
	if err == nil && !validAuthoritativeResumeCursorsResult(params, resumed) {
		err = errors.New("remote plugin did not atomically install exact daemon cursor set")
	}
	if err != nil && recoveryEvidence != nil {
		return legacyRemoteSyncNegotiation(proto.RemoteSyncReasonTerminalFinalizeRecoveryStateInvalid), fmt.Errorf("%w: replacement plugin did not confirm exact multistream terminal evidence: %v", ErrRemoteFinalizeRecoveryBlocked, err)
	}
	if err != nil && negotiated.Mode != proto.RemoteSyncModeShadow {
		return legacyRemoteSyncNegotiation("authoritative multistream cursor handoff unavailable"), err
	}
	return negotiated, err
}

func resumeCursorsPendingFinalize(params proto.RemoteResumeCursorsV1Params) (int, *proto.RemoteInboundFinalizeEvidenceV1, bool) {
	index := -1
	var pending *proto.RemoteInboundFinalizeEvidenceV1
	for cursorIndex := range params.Cursors {
		if params.Cursors[cursorIndex].PendingFinalizeEvidence == nil {
			continue
		}
		if pending != nil {
			return -1, nil, true
		}
		index = cursorIndex
		pending = params.Cursors[cursorIndex].PendingFinalizeEvidence
	}
	return index, pending, false
}

func (r *RemoteRunner) runRemoteSyncNegotiationRefresh(ctx context.Context, remote remoteSyncNegotiator, manifest proto.RemotePluginManifestUnsignedV1) {
	ticker := time.NewTicker(remoteSyncNegotiationRefreshInterval)
	defer ticker.Stop()
	r.refreshRemoteSyncNegotiation(ctx, remote, manifest, ticker.C)
}

// refreshRemoteSyncNegotiation is split from ticker construction so tests can
// deterministically prove cancellation, plugin-generation replacement, and
// failure fallback without relying on wall-clock sleeps.
func (r *RemoteRunner) refreshRemoteSyncNegotiation(ctx context.Context, remote remoteSyncNegotiator, manifest proto.RemotePluginManifestUnsignedV1, ticks <-chan time.Time) {
	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-ticks:
			if !ok {
				return
			}
			negotiated, err := r.negotiateRemoteSyncV1(ctx, remote, manifest)
			r.setSyncNegotiationWithManifest(negotiated, manifest)
			if err != nil {
				r.warn("remote durable sync refresh constrained; using safe selected mode", "sync_mode", negotiated.Mode, "err", err)
			}
		}
	}
}

func (r *RemoteRunner) authoritativeResumeCursorParams(negotiated proto.RemoteNegotiateSyncV1Result) (proto.RemoteResumeCursorV1Params, error) {
	return r.authoritativeResumeCursorParamsForStream(negotiated.StreamID, negotiated.StreamEpoch)
}

func (r *RemoteRunner) authoritativeResumeCursorParamsForStream(streamID, streamEpoch string) (proto.RemoteResumeCursorV1Params, error) {
	params := proto.RemoteResumeCursorV1Params{Authoritative: true, StreamID: streamID, StreamEpoch: streamEpoch}
	if r == nil || r.DurableCursorStore == nil {
		return proto.RemoteResumeCursorV1Params{}, ErrRemoteNotConfigured
	}
	remoteIdentity := r.CurrentDeviceID()
	if !validDurableCursorOpaque(remoteIdentity, durableCursorMaxIdentity) ||
		!validDurableCursorOpaque(streamID, durableCursorMaxIdentity) || !validDurableCursorOpaque(streamEpoch, durableCursorMaxIdentity) {
		return proto.RemoteResumeCursorV1Params{}, ErrRemoteNotConfigured
	}
	key := DurableCursorKey{RemoteIdentity: remoteIdentity, StreamID: streamID, StreamEpoch: streamEpoch}
	state, err := r.DurableCursorStore.Load(key)
	if errors.Is(err, ErrDurableCursorNotFound) {
		return params, nil
	}
	if err != nil || validateDurableCursorValue(state, true) != nil || state.Position == 0 {
		if err != nil {
			return proto.RemoteResumeCursorV1Params{}, err
		}
		return proto.RemoteResumeCursorV1Params{}, ErrDurableCursorInvalid
	}
	params.CursorPresent = true
	params.Cursor = state.Cursor
	params.CursorDigest = state.CursorDigest
	params.Position = state.Position
	if r.DurableInbox == nil {
		return proto.RemoteResumeCursorV1Params{}, ErrRemoteNotConfigured
	}
	pending, err := r.DurableInbox.PendingFinalizeEvidence(
		key,
		state,
	)
	if errors.Is(err, ErrInboundFinalizeEvidenceNotFound) {
		seeded, seedErr := r.DurableCursorStore.IsCurrentCheckpointSeed(
			key,
			state,
		)
		if seedErr != nil {
			return proto.RemoteResumeCursorV1Params{}, seedErr
		}
		if !seeded {
			return proto.RemoteResumeCursorV1Params{}, err
		}
		// An authenticated checkpoint covers this stream position but was
		// fetched outside the ordinary delivery log, so there is deliberately
		// no cloud ACK/native-finalize obligation to transfer.
		pending = nil
	} else if err != nil {
		return proto.RemoteResumeCursorV1Params{}, err
	}
	params.PendingFinalizeEvidence = pending
	return params, nil
}

func (r *RemoteRunner) authoritativeResumeCursorsParams(negotiated proto.RemoteNegotiateSyncV1Result) (proto.RemoteResumeCursorsV1Params, error) {
	requireCheckpoint := remoteSyncModeRank(negotiated.Mode) > remoteSyncModeRankShadow
	if !validRemoteStreamDescriptors(negotiated, requireCheckpoint) {
		return proto.RemoteResumeCursorsV1Params{}, ErrRemoteNotConfigured
	}
	params := proto.RemoteResumeCursorsV1Params{Cursors: make([]proto.RemoteResumeCursorV1Params, 0, len(negotiated.Streams))}
	pendingCount := 0
	for _, descriptor := range negotiated.Streams {
		cursor, err := r.authoritativeResumeCursorParamsForStream(descriptor.StreamID, descriptor.StreamEpoch)
		if err != nil {
			return proto.RemoteResumeCursorsV1Params{}, err
		}
		if cursor.PendingFinalizeEvidence != nil {
			if cursor.PendingFinalizeEvidence.NamespaceID != descriptor.NamespaceID ||
				cursor.PendingFinalizeEvidence.StreamID != descriptor.StreamID || cursor.PendingFinalizeEvidence.StreamEpoch != descriptor.StreamEpoch {
				return proto.RemoteResumeCursorsV1Params{}, ErrRemoteFinalizeRecoveryBlocked
			}
			pendingCount++
			if pendingCount > 1 {
				return proto.RemoteResumeCursorsV1Params{}, ErrRemoteFinalizeRecoveryBlocked
			}
		}
		params.Cursors = append(params.Cursors, cursor)
	}
	return params, nil
}

func validAuthoritativeResumeCursorResult(params proto.RemoteResumeCursorV1Params, result proto.RemoteResumeCursorV1Result) bool {
	if !params.Authoritative || !result.Accepted || result.StreamID != params.StreamID || result.StreamEpoch != params.StreamEpoch || result.CursorPresent != params.CursorPresent {
		return false
	}
	if (params.PendingFinalizeEvidence == nil) != (result.PendingFinalizeEvidence == nil) ||
		(params.PendingFinalizeEvidence != nil && *params.PendingFinalizeEvidence != *result.PendingFinalizeEvidence) {
		return false
	}
	if !params.CursorPresent {
		return params.Cursor == "" && params.CursorDigest == "" && params.Position == 0 && params.PendingFinalizeEvidence == nil &&
			result.Cursor == "" && result.CursorDigest == "" && result.Position == 0
	}
	return result.Cursor == params.Cursor && result.CursorDigest == params.CursorDigest && result.Position == params.Position
}

func validAuthoritativeResumeCursorsResult(params proto.RemoteResumeCursorsV1Params, result proto.RemoteResumeCursorsV1Result) bool {
	if !result.Accepted || len(params.Cursors) == 0 || len(result.Cursors) != len(params.Cursors) {
		return false
	}
	requested := make(map[string]struct{}, len(params.Cursors))
	echoed := make(map[string]struct{}, len(result.Cursors))
	for index := range params.Cursors {
		requestCursor := params.Cursors[index]
		resultCursor := result.Cursors[index]
		requestKey := requestCursor.StreamID
		resultKey := resultCursor.StreamID
		if _, duplicate := requested[requestKey]; duplicate {
			return false
		}
		if _, duplicate := echoed[resultKey]; duplicate {
			return false
		}
		requested[requestKey] = struct{}{}
		echoed[resultKey] = struct{}{}
		if !validAuthoritativeResumeCursorResult(requestCursor, resultCursor) {
			return false
		}
	}
	return true
}

func remoteSyncModeRank(mode string) int {
	switch mode {
	case proto.RemoteSyncModeShadow:
		return remoteSyncModeRankShadow
	case proto.RemoteSyncModeDurableRead:
		return remoteSyncModeRankDurableRead
	case proto.RemoteSyncModeDeltaPreferred:
		return remoteSyncModeRankPreferred
	case proto.RemoteSyncModeDeltaRequired:
		return remoteSyncModeRankRequired
	default:
		return remoteSyncModeRankLegacy
	}
}

func remoteStringSetContains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func (r *RemoteRunner) setSyncNegotiation(result proto.RemoteNegotiateSyncV1Result) {
	r.setSyncNegotiationSignedCapabilities(result, false, false, false)
}

func (r *RemoteRunner) setSyncNegotiationWithManifest(result proto.RemoteNegotiateSyncV1Result, manifest proto.RemotePluginManifestUnsignedV1) {
	r.setSyncNegotiationSignedCapabilities(result,
		manifest.HasCapability(proto.CapabilityInboundFinalizeV1),
		manifest.HasCapability(proto.CapabilityStagedCheckpointV1),
		manifest.HasCapability(proto.CapabilityRedactionSafeBatchV1))
}

func (r *RemoteRunner) setSyncNegotiationSignedFinalize(result proto.RemoteNegotiateSyncV1Result, signedFinalize bool) {
	r.setSyncNegotiationSignedCapabilities(result, signedFinalize, false, false)
}

func (r *RemoteRunner) setSyncNegotiationSignedCapabilities(result proto.RemoteNegotiateSyncV1Result, signedFinalize, signedStagedCheckpoint, signedRedactionBatch bool) {
	result.ServerCapabilities = append([]string(nil), result.ServerCapabilities...)
	result.Streams = append([]proto.RemoteStreamDescriptorV1(nil), result.Streams...)
	if result.PendingFinalizeEvidence != nil {
		evidence := *result.PendingFinalizeEvidence
		result.PendingFinalizeEvidence = &evidence
	}
	r.syncMu.Lock()
	r.syncMode = result
	r.syncFinalizeSigned = signedFinalize
	r.syncStagedCheckpointSigned = signedStagedCheckpoint
	r.syncRedactionBatchSigned = signedRedactionBatch
	r.syncMu.Unlock()
}

// SignedRedactionSafeBatchReady proves the additive batch ABI was present in
// the exact signed plugin image and selected by the current server response.
func (r *RemoteRunner) SignedRedactionSafeBatchReady() bool {
	if r == nil {
		return false
	}
	r.syncMu.RLock()
	result, signed := r.syncMode, r.syncRedactionBatchSigned
	r.syncMu.RUnlock()
	return signed && result.SelectedProtocol == 1 && result.FeatureGateEnabled &&
		remoteStringSetContains(result.ServerCapabilities, proto.CapabilityRedactionSafeBatchV1)
}

// SyncNegotiation returns the last runtime selection made against the exact
// signed plugin image. The zero value is normalized to legacy.
func (r *RemoteRunner) SyncNegotiation() proto.RemoteNegotiateSyncV1Result {
	r.syncMu.RLock()
	result := r.syncMode
	r.syncMu.RUnlock()
	if result.Mode == "" {
		result = legacyRemoteSyncNegotiation("plugin not negotiated")
	}
	result.ServerCapabilities = append([]string(nil), result.ServerCapabilities...)
	result.Streams = append([]proto.RemoteStreamDescriptorV1(nil), result.Streams...)
	if result.PendingFinalizeEvidence != nil {
		evidence := *result.PendingFinalizeEvidence
		result.PendingFinalizeEvidence = &evidence
	}
	return result
}

// SignedInboundFinalizeReady proves both halves of the finalize authority:
// the exact launched plugin image signed the ABI capability, and its current
// runtime negotiation selected the same capability. Shadow mode may use this
// only to drain an existing post-cloud-ACK obligation; it still cannot admit a
// new durable delivery.
func (r *RemoteRunner) SignedInboundFinalizeReady() bool {
	if r == nil {
		return false
	}
	r.syncMu.RLock()
	result := r.syncMode
	signed := r.syncFinalizeSigned
	r.syncMu.RUnlock()
	return signed && result.SelectedProtocol == 1 && result.FeatureGateEnabled &&
		remoteStringSetContains(result.ServerCapabilities, proto.CapabilityInboundFinalizeV1)
}

// DurableReceiptRequired is consumed by the outbox publisher. Shadow and
// durable-read modes keep today's retained MQTT authority; in delta modes a
// live-lane outbox intent may retire only after an authenticated committed
// receipt. Retained checkpoint/suppression outcomes remain trusted plugin
// decisions, and laneless compatibility events keep legacy acknowledgement
// semantics.
func (r *RemoteRunner) DurableReceiptRequired() bool {
	mode := r.SyncNegotiation().Mode
	return mode == proto.RemoteSyncModeDeltaPreferred || mode == proto.RemoteSyncModeDeltaRequired
}

func (r *RemoteRunner) setCmd(cmd *exec.Cmd) {
	r.cmdMu.Lock()
	defer r.cmdMu.Unlock()
	r.cmd = cmd
}

// Restart forces the currently-running plugin child process to exit so
// the supervisor goroutine respawns it (re-exec'ing r.Executable with a
// fresh proxy handshake). This is the path the local web API uses after
// a successful pairing so the plugin re-reads its on-disk credentials
// and reconnects without a full daemon reload.
//
// Safety wrt Stop/Start:
//   - Restart does NOT call r.cancel and does NOT trip stopOnce, so the
//     supervisor's ctx stays live and its loop treats the killed child
//     as a respawnable crash (the same path as any plugin exit): it
//     increments restartCount, applies backoff, then re-execs.
//   - Restart is a no-op once Stop has run (r.stopped is set): we must
//     not kill a process the shutdown path is already winding down, and
//     there is nothing to respawn into.
//   - Killing the process makes the in-flight runOnce's cmd.Wait()
//     return a non-nil error, which is exactly the "respawn" signal —
//     not the clean ctx.Done() shutdown signal.
//
// Returns nil when a kill signal was delivered (or when there is no live
// process yet — the supervisor is already (re)spawning). Never blocks on
// the respawn; the caller observes the new connection via Status/ConnState.
func (r *RemoteRunner) Restart(_ context.Context) error {
	if r.stopped.Load() {
		return nil
	}
	r.cmdMu.Lock()
	cmd := r.cmd
	r.cmdMu.Unlock()
	if cmd == nil || cmd.Process == nil {
		// Nothing live to kill: either the runner hasn't spawned yet or
		// it is mid-respawn. The supervisor will (re)spawn on its own and
		// pick up the freshly-written credentials, so this is a no-op
		// success rather than an error.
		return nil
	}
	r.info("remote plugin restart requested", "pid", cmd.Process.Pid)
	// Best-effort kill; if the process already exited the supervisor is
	// already respawning, so a kill error here is benign.
	_ = cmd.Process.Kill()
	return nil
}

// ---------------------------------------------------------------------------
// Outbound delegating wrappers.
// ---------------------------------------------------------------------------

func (r *RemoteRunner) Publish(ctx context.Context, events []proto.RemoteEvent) (proto.RemotePublishResult, error) {
	r.proxyMu.Lock()
	p := r.proxy
	transfer := r.transfer
	acquired := len(events) == 1 && stagedRemoteCheckpointCandidate(events[0]) && transfer != nil && transfer.acquire()
	r.proxyMu.Unlock()
	if p == nil {
		if acquired {
			transfer.release()
		}
		return proto.RemotePublishResult{}, ErrRemoteReconnecting
	}
	if acquired {
		defer transfer.release()
		if streamID, streamEpoch, authorized := r.stagedRemoteCheckpointAuthority(events[0]); authorized {
			params, jobKey, err := transfer.stageOrReuse(ctx, events[0], streamID, streamEpoch)
			if err != nil {
				return proto.RemotePublishResult{}, err
			}
			result, err := p.PublishStagedV1(ctx, params)
			if err != nil {
				// The exact file/descriptor stays retained. A daemon outbox retry
				// reuses it, while a plugin/session restart reconstructs it from
				// the still-durable source intent.
				return proto.RemotePublishResult{}, err
			}
			if stagedRemotePublishTerminal(result, events[0].EventID) {
				if err := transfer.complete(jobKey, params.Transfer.TransferID); err != nil {
					return proto.RemotePublishResult{}, fmt.Errorf("daemon: clean accepted staged checkpoint: %w", err)
				}
			}
			return result, nil
		}
	}
	return p.Publish(ctx, events)
}

func stagedRemotePublishTerminal(result proto.RemotePublishResult, eventID string) bool {
	if len(result.Outcomes) != 1 || result.Outcomes[0].EventID != eventID {
		return false
	}
	outcome := result.Outcomes[0]
	return outcome.Accepted || !outcome.Retryable && outcome.Error != ""
}

func (r *RemoteRunner) Fetch(ctx context.Context, params proto.RemoteFetchParams) (proto.RemoteFetchResult, error) {
	r.proxyMu.Lock()
	p := r.proxy
	r.proxyMu.Unlock()
	if p == nil {
		return proto.RemoteFetchResult{}, ErrRemoteReconnecting
	}
	return p.Fetch(ctx, params)
}

func (r *RemoteRunner) FetchV2(ctx context.Context, params proto.RemoteFetchV2Params) (proto.RemoteFetchV2Result, error) {
	negotiated := r.SyncNegotiation()
	mode := negotiated.Mode
	if mode != proto.RemoteSyncModeDurableRead && mode != proto.RemoteSyncModeDeltaPreferred && mode != proto.RemoteSyncModeDeltaRequired {
		return proto.RemoteFetchV2Result{}, ErrRemoteNotConfigured
	}
	if !remoteNegotiationHasStream(negotiated, params.StreamID, params.StreamEpoch) {
		return proto.RemoteFetchV2Result{}, ErrRemoteNotConfigured
	}
	r.proxyMu.Lock()
	p := r.proxy
	r.proxyMu.Unlock()
	if p == nil {
		return proto.RemoteFetchV2Result{}, ErrRemoteReconnecting
	}
	return p.FetchV2(ctx, params)
}

func (r *RemoteRunner) FetchParentV1(ctx context.Context, params proto.RemoteFetchParentV1Params) (proto.RemoteFetchParentV1Result, error) {
	negotiated := r.SyncNegotiation()
	mode := negotiated.Mode
	if mode != proto.RemoteSyncModeDurableRead && mode != proto.RemoteSyncModeDeltaPreferred && mode != proto.RemoteSyncModeDeltaRequired {
		return proto.RemoteFetchParentV1Result{}, ErrRemoteNotConfigured
	}
	if !remoteNegotiationHasScopedStream(negotiated, params.StreamID, params.StreamEpoch, params.NamespaceID) {
		return proto.RemoteFetchParentV1Result{}, ErrRemoteNotConfigured
	}
	r.proxyMu.Lock()
	p := r.proxy
	r.proxyMu.Unlock()
	if p == nil {
		return proto.RemoteFetchParentV1Result{}, ErrRemoteReconnecting
	}
	return p.FetchParentV1(ctx, params)
}

func (r *RemoteRunner) AckV2(ctx context.Context, params proto.RemoteAckV2Params) (proto.RemoteAckV2Result, error) {
	negotiated := r.SyncNegotiation()
	mode := negotiated.Mode
	if mode != proto.RemoteSyncModeDurableRead && mode != proto.RemoteSyncModeDeltaPreferred && mode != proto.RemoteSyncModeDeltaRequired {
		return proto.RemoteAckV2Result{}, ErrRemoteNotConfigured
	}
	if !remoteNegotiationHasStream(negotiated, params.StreamID, params.StreamEpoch) {
		return proto.RemoteAckV2Result{}, ErrRemoteNotConfigured
	}
	r.proxyMu.Lock()
	p := r.proxy
	r.proxyMu.Unlock()
	if p == nil {
		return proto.RemoteAckV2Result{}, ErrRemoteReconnecting
	}
	return p.AckV2(ctx, params)
}

func (r *RemoteRunner) RequestCheckpointV1(ctx context.Context, params proto.RemoteRequestCheckpointV1Params) (proto.RemoteRequestCheckpointV1Result, error) {
	negotiated := r.SyncNegotiation()
	if negotiated.Mode == proto.RemoteSyncModeLegacy ||
		!remoteNegotiationHasScopedStream(negotiated, params.StreamID, params.StreamEpoch, params.NamespaceID) {
		return proto.RemoteRequestCheckpointV1Result{}, ErrRemoteNotConfigured
	}
	r.proxyMu.Lock()
	p := r.proxy
	r.proxyMu.Unlock()
	if p == nil {
		return proto.RemoteRequestCheckpointV1Result{}, ErrRemoteReconnecting
	}
	return p.RequestCheckpointV1(ctx, params)
}

func (r *RemoteRunner) Enumerate(ctx context.Context, params proto.RemoteEnumerateParams) (proto.RemoteEnumerateResult, error) {
	r.proxyMu.Lock()
	p := r.proxy
	r.proxyMu.Unlock()
	if p == nil {
		return proto.RemoteEnumerateResult{}, ErrRemoteReconnecting
	}
	return p.Enumerate(ctx, params)
}

func (r *RemoteRunner) Subscribe(ctx context.Context, namespaceID string) error {
	r.proxyMu.Lock()
	p := r.proxy
	r.proxyMu.Unlock()
	if p == nil {
		return ErrRemoteReconnecting
	}
	return p.Subscribe(ctx, namespaceID)
}

func (r *RemoteRunner) Unsubscribe(ctx context.Context, namespaceID string) error {
	r.proxyMu.Lock()
	p := r.proxy
	r.proxyMu.Unlock()
	if p == nil {
		return ErrRemoteReconnecting
	}
	return p.Unsubscribe(ctx, namespaceID)
}

func (r *RemoteRunner) Status(ctx context.Context) (proto.RemoteStatusResult, error) {
	r.proxyMu.Lock()
	p := r.proxy
	r.proxyMu.Unlock()
	if p == nil {
		state, _ := r.lastConnState.Load().(string)
		return proto.RemoteStatusResult{ConnState: state}, ErrRemoteReconnecting
	}
	return p.Status(ctx)
}

// ListNamespaceDevices delegates to the live proxy; returns
// ErrRemoteReconnecting when the plugin is mid-restart.
func (r *RemoteRunner) ListNamespaceDevices(ctx context.Context, namespaceID string) (proto.RemoteListNamespaceDevicesResult, error) {
	r.proxyMu.Lock()
	p := r.proxy
	r.proxyMu.Unlock()
	if p == nil {
		return proto.RemoteListNamespaceDevicesResult{}, ErrRemoteReconnecting
	}
	return p.ListNamespaceDevices(ctx, namespaceID)
}

// RegisterWrapKey registers this device's X25519 wrap public key with the
// control plane (account-scoped end-to-end encryption). Returns
// ErrRemoteReconnecting when the plugin is mid-restart.
func (r *RemoteRunner) RegisterWrapKey(ctx context.Context, pub []byte) error {
	r.proxyMu.Lock()
	p := r.proxy
	r.proxyMu.Unlock()
	if p == nil {
		return ErrRemoteReconnecting
	}
	return p.RegisterWrapKey(ctx, pub)
}

// ListAccountDevices returns every active device in the caller's account with a
// registered wrap pubkey (includes self). Account-scoped recipient discovery
// for outbound E2E encryption. Returns ErrRemoteReconnecting when the plugin is
// mid-restart.
func (r *RemoteRunner) ListAccountDevices(ctx context.Context) (proto.RemoteListAccountDevicesResult, error) {
	r.proxyMu.Lock()
	p := r.proxy
	r.proxyMu.Unlock()
	if p == nil {
		return proto.RemoteListAccountDevicesResult{}, ErrRemoteReconnecting
	}
	return p.ListAccountDevices(ctx)
}

// PutNamespaceKey delegates to the live proxy.
func (r *RemoteRunner) PutNamespaceKey(ctx context.Context, params proto.RemotePutNamespaceKeyParams) (proto.RemotePutNamespaceKeyResult, error) {
	r.proxyMu.Lock()
	p := r.proxy
	r.proxyMu.Unlock()
	if p == nil {
		return proto.RemotePutNamespaceKeyResult{}, ErrRemoteReconnecting
	}
	return p.PutNamespaceKey(ctx, params)
}

// GetNamespaceKey delegates to the live proxy.
func (r *RemoteRunner) GetNamespaceKey(ctx context.Context, params proto.RemoteGetNamespaceKeyParams) (proto.RemoteGetNamespaceKeyResult, error) {
	r.proxyMu.Lock()
	p := r.proxy
	r.proxyMu.Unlock()
	if p == nil {
		return proto.RemoteGetNamespaceKeyResult{}, ErrRemoteReconnecting
	}
	return p.GetNamespaceKey(ctx, params)
}

// BroadcastNamespaceKey delegates to the live proxy.
func (r *RemoteRunner) BroadcastNamespaceKey(ctx context.Context, params proto.RemoteBroadcastNamespaceKeyParams) error {
	r.proxyMu.Lock()
	p := r.proxy
	r.proxyMu.Unlock()
	if p == nil {
		return ErrRemoteReconnecting
	}
	return p.BroadcastNamespaceKey(ctx, params)
}

// GetNamespaceRole delegates to the live proxy; returns ErrRemoteReconnecting
// when the plugin is mid-restart (client-side RBAC).
func (r *RemoteRunner) GetNamespaceRole(ctx context.Context, params proto.RemoteGetNamespaceRoleParams) (proto.RemoteGetNamespaceRoleResult, error) {
	r.proxyMu.Lock()
	p := r.proxy
	r.proxyMu.Unlock()
	if p == nil {
		return proto.RemoteGetNamespaceRoleResult{}, ErrRemoteReconnecting
	}
	return p.GetNamespaceRole(ctx, params)
}

func (r *RemoteRunner) SecurityEpochPrepare(ctx context.Context, params proto.RemoteSecurityEpochPrepareParams) (proto.RemoteSecurityEpochStatusResult, error) {
	r.proxyMu.Lock()
	p := r.proxy
	r.proxyMu.Unlock()
	if p == nil {
		return proto.RemoteSecurityEpochStatusResult{}, ErrRemoteReconnecting
	}
	return p.SecurityEpochPrepare(ctx, params)
}
func (r *RemoteRunner) SecurityEpochCommit(ctx context.Context, params proto.RemoteSecurityEpochCommandParams) (proto.RemoteSecurityEpochStatusResult, error) {
	r.proxyMu.Lock()
	p := r.proxy
	r.proxyMu.Unlock()
	if p == nil {
		return proto.RemoteSecurityEpochStatusResult{}, ErrRemoteReconnecting
	}
	return p.SecurityEpochCommit(ctx, params)
}
func (r *RemoteRunner) SecurityEpochActivate(ctx context.Context, params proto.RemoteSecurityEpochCommandParams) (proto.RemoteSecurityEpochStatusResult, error) {
	r.proxyMu.Lock()
	p := r.proxy
	r.proxyMu.Unlock()
	if p == nil {
		return proto.RemoteSecurityEpochStatusResult{}, ErrRemoteReconnecting
	}
	return p.SecurityEpochActivate(ctx, params)
}
func (r *RemoteRunner) SecurityEpochStatus(ctx context.Context, params proto.RemoteSecurityEpochCommandParams) (proto.RemoteSecurityEpochStatusResult, error) {
	r.proxyMu.Lock()
	p := r.proxy
	r.proxyMu.Unlock()
	if p == nil {
		return proto.RemoteSecurityEpochStatusResult{}, ErrRemoteReconnecting
	}
	return p.SecurityEpochStatus(ctx, params)
}

func (r *RemoteRunner) SubmitDeviceTransitionPlan(ctx context.Context, params proto.RemoteSubmitDeviceTransitionPlanParams) error {
	r.proxyMu.Lock()
	p := r.proxy
	r.proxyMu.Unlock()
	if p == nil {
		return ErrRemoteReconnecting
	}
	return p.SubmitDeviceTransitionPlan(ctx, params)
}

func (r *RemoteRunner) GetDeviceTransitionPlans(ctx context.Context, params proto.RemoteGetDeviceTransitionPlansParams) (proto.RemoteGetDeviceTransitionPlansResult, error) {
	r.proxyMu.Lock()
	p := r.proxy
	r.proxyMu.Unlock()
	if p == nil {
		return proto.RemoteGetDeviceTransitionPlansResult{}, ErrRemoteReconnecting
	}
	return p.GetDeviceTransitionPlans(ctx, params)
}

// RestartCount exposes the cumulative number of plugin restarts.
// Used by the /api/daemon health endpoint to surface flapping plugins.
func (r *RemoteRunner) RestartCount() uint64 {
	return r.restartCount.Load()
}

// ConnState returns the latest cached connectivity label, without
// RPC'ing the plugin. Cheap to call from hot paths.
func (r *RemoteRunner) ConnState() string {
	if s, ok := r.lastConnState.Load().(string); ok {
		return s
	}
	return "unknown"
}

// ---------------------------------------------------------------------------
// Internal helpers.
// ---------------------------------------------------------------------------

func (r *RemoteRunner) info(msg string, args ...any) {
	if r.Logger != nil {
		r.Logger.Info(msg, args...)
	}
}

func (r *RemoteRunner) warn(msg string, args ...any) {
	if r.Logger != nil {
		r.Logger.Warn(msg, args...)
	}
}

// stderrLogWriter is an io.Writer assigned to the spawned plugin's
// cmd.Stderr so its diagnostics flow into the daemon log (one log line per
// write chunk) instead of the null device. A single trailing newline is
// trimmed for tidier lines and blank chunks are dropped. The logger is
// nil-tolerated (matching RemoteRunner.Logger). Mirrors
// internal/plugin/manager.stderrLogger.
type stderrLogWriter struct {
	logger interface {
		Info(msg string, args ...any)
	}
	mu                 sync.Mutex
	retryWindowStarted time.Time
	retrySuppressed    uint64
	now                func() time.Time
	retryWindow        time.Duration
}

func (w *stderrLogWriter) Write(p []byte) (int, error) {
	msg := string(p)
	if n := len(msg); n > 0 && msg[n-1] == '\n' {
		msg = msg[:n-1]
	}
	if msg == "" || w.logger == nil {
		return len(p), nil
	}
	if class := remoteStderrDiagnosticClass(msg); class != "" {
		now := time.Now()
		if w.now != nil {
			now = w.now()
		}
		window := time.Minute
		if w.retryWindow > 0 {
			window = w.retryWindow
		}

		w.mu.Lock()
		started := w.retryWindowStarted
		if started.IsZero() || now.Before(started) || now.Sub(started) >= window {
			suppressed := w.retrySuppressed
			w.retryWindowStarted = now
			w.retrySuppressed = 0
			w.mu.Unlock()
			if suppressed > 0 {
				w.logger.Info("remote plugin stderr messages suppressed",
					"class", class, "count", suppressed)
			}
			w.logger.Info("remote plugin stderr", "line", msg)
			return len(p), nil
		}
		w.retrySuppressed++
		w.mu.Unlock()
		return len(p), nil
	}
	w.logger.Info("remote plugin stderr", "line", msg)
	return len(p), nil
}

func remoteStderrDiagnosticClass(msg string) string {
	if isRemoteInboundRetryDiagnostic(msg) {
		return "mqtt-inbound-delivery-retry"
	}
	if strings.HasPrefix(msg, "eventsync: scheduler: realtime inbound ") {
		return "mqtt-inbound-event"
	}
	return ""
}

func isRemoteInboundRetryDiagnostic(msg string) bool {
	return strings.HasPrefix(msg, "eventsync: mqtt inbound push failed ") &&
		strings.Contains(msg, "err=remote: daemon requested delivery retry")
}

// compile-time assertion: stderrLogWriter is a valid Stderr sink.
var _ io.Writer = (*stderrLogWriter)(nil)

// pipeShim glues exec.Cmd's separate stdin/stdout pipes into a single
// io.ReadWriter for the RemoteProxy. Close kills the underlying
// process when the proxy closes the transport (typical shutdown
// path).
type pipeShim struct {
	r   io.ReadCloser
	w   io.WriteCloser
	cmd *exec.Cmd
}

func (s *pipeShim) Read(p []byte) (int, error)  { return s.r.Read(p) }
func (s *pipeShim) Write(p []byte) (int, error) { return s.w.Write(p) }
func (s *pipeShim) Close() error {
	_ = s.w.Close()
	_ = s.r.Close()
	if s.cmd != nil && s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
	}
	return nil
}
