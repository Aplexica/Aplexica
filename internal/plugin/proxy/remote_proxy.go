package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aplexica/aplexica/internal/plugin/proto"
	"github.com/aplexica/aplexica/internal/syncrules"
)

// RemoteProxy is the daemon-side handle to a remote-transport plugin
// (manifest.kind="remote"). Unlike the adapter-flavored Proxy above,
// RemoteProxy talks the remote.* JSON-RPC surface and handles
// asynchronous notifications (remote.inbound, remote.conn_state,
// remote.enumerate_hint) by dispatching them onto caller-supplied
// callback channels.
//
// Concurrency model: one background goroutine reads from the
// transport. Request responses get matched to pending callers by JSON-
// RPC ID; notifications get routed to the callback functions. A single
// write mutex protects outbound framing.
type RemoteProxy struct {
	fr *proto.FrameReader
	fw *proto.FrameWriter

	writeMu sync.Mutex
	nextID  atomic.Int64

	pendingMu sync.Mutex
	pending   map[int64]chan proto.Response

	manifest proto.InitializeResult
	deviceID string

	// cbMu guards the callback funcs below: the OnXxx setters run on the
	// caller goroutine while handleNotification reads them on the read-pump
	// goroutine (started by OpenRemote before callbacks are registered).
	cbMu sync.RWMutex
	// Callback funcs for inbound notifications. Set via OnXxx
	// methods before any calls. nil = the notification is silently
	// dropped (logged at debug level by the daemon wiring).
	onInbound                          func([]proto.RemoteEvent)
	onInboundV2                        func(proto.RemoteInboundDeliveryV2) proto.RemoteInboundAckV2
	onInboundV2ResponseWritten         func()
	onInboundFinalizeV1                func(proto.RemoteInboundFinalizeV1Params) proto.RemoteInboundFinalizeV1Result
	onInboundFinalizeV1ResponseWritten func()
	onConnState                        func(state, human string)
	onEnumerateHint                    func(reason string)
	onCheckpointNeededV1               func(proto.RemoteCheckpointNeededV1Notification)
	onRulesUpdate                      func(changeID string, rules []syncrules.Rule)
	onNamespaceKeyRotated              func(proto.RemoteNamespaceKeyRotatedNotification)
	onNamespaceKeyBroadcast            func(proto.RemoteNamespaceKeyBroadcastNotification)

	readDoneCh chan struct{}
	readErr    error

	closer       io.Closer
	inboundCh    chan inboundDelivery
	finalizeCh   chan inboundFinalizeRequest
	inboundBytes atomic.Int64
}
type inboundDelivery struct {
	delivery proto.RemoteInboundDeliveryV2
	id       json.RawMessage
	legacy   bool
	bytes    int64
}

type inboundFinalizeRequest struct {
	params proto.RemoteInboundFinalizeV1Params
	id     json.RawMessage
}

// OpenRemote performs the initialize handshake against a remote plugin
// and returns a RemoteProxy ready for use. The caller must register
// notification callbacks via OnInbound/OnConnState/OnEnumerateHint
// before any namespace subscription, otherwise inbound events get
// dropped silently.
//
// transport carries JSON-RPC frames; the proxy reads and writes on the
// SAME reader/writer (typical: a *exec.Cmd stdin+stdout pair wrapped
// in an io.ReadWriter shim).
func OpenRemote(ctx context.Context, transport io.ReadWriter, deviceID, daemonVersion string) (*RemoteProxy, error) {
	p := &RemoteProxy{
		fr:         proto.NewFrameReader(transport),
		fw:         proto.NewFrameWriter(transport),
		pending:    map[int64]chan proto.Response{},
		readDoneCh: make(chan struct{}),
		deviceID:   deviceID,
		inboundCh:  make(chan inboundDelivery, 64),
		finalizeCh: make(chan inboundFinalizeRequest, 64),
	}
	if c, ok := transport.(io.Closer); ok {
		p.closer = c
	}

	// Start the read pump BEFORE the initialize call so the
	// response can be received.
	go p.readPump()
	go p.inboundWorker()
	go p.inboundFinalizeWorker()

	var result proto.InitializeResult
	if err := p.call(ctx, proto.MethodInitialize, proto.InitializeParams{
		ABIVersion:    proto.ABIVersion,
		DaemonVersion: daemonVersion,
		DeviceID:      deviceID,
	}, &result); err != nil {
		return nil, fmt.Errorf("plugin/proxy: remote initialize: %w", err)
	}
	if result.ABIVersion != proto.ABIVersion {
		return nil, fmt.Errorf("plugin/proxy: remote abi_version mismatch — plugin %q, daemon %q", result.ABIVersion, proto.ABIVersion)
	}
	p.manifest = result
	return p, nil
}

// Name returns the plugin's name from its initialize response.
func (p *RemoteProxy) Name() string { return p.manifest.PluginName }

// Version returns the plugin's semver from its initialize response.
func (p *RemoteProxy) Version() string { return p.manifest.PluginVersion }

// OnInbound registers a callback invoked whenever the plugin pushes
// a remote.inbound notification with one or more events. The daemon
// passes these into its sync orchestrator's import path.
//
// Set this BEFORE calling Subscribe — inbound events arrive
// asynchronously and a nil callback drops them.
func (p *RemoteProxy) OnInbound(fn func([]proto.RemoteEvent)) {
	p.cbMu.Lock()
	p.onInbound = fn
	p.cbMu.Unlock()
}

func (p *RemoteProxy) OnInboundV2(fn func(proto.RemoteInboundDeliveryV2) proto.RemoteInboundAckV2) {
	p.cbMu.Lock()
	p.onInboundV2 = fn
	p.cbMu.Unlock()
}

// OnInboundV2ResponseWritten installs a post-response hook. It runs only after
// the terminal/retry ACK frame has been written, allowing the runner to release
// daemon-to-plugin observation RPCs without creating a bidirectional RPC wait.
func (p *RemoteProxy) OnInboundV2ResponseWritten(fn func()) {
	p.cbMu.Lock()
	p.onInboundV2ResponseWritten = fn
	p.cbMu.Unlock()
}

// OnInboundFinalizeV1 registers the split-phase durable receive finalizer.
// Unlike a notification, this is a plugin->daemon JSON-RPC request and always
// receives a bounded result so the plugin can retain or clear its obligation.
func (p *RemoteProxy) OnInboundFinalizeV1(fn func(proto.RemoteInboundFinalizeV1Params) proto.RemoteInboundFinalizeV1Result) {
	p.cbMu.Lock()
	p.onInboundFinalizeV1 = fn
	p.cbMu.Unlock()
}

// OnInboundFinalizeV1ResponseWritten mirrors OnInboundV2ResponseWritten for
// the split-phase finalize reverse request.
func (p *RemoteProxy) OnInboundFinalizeV1ResponseWritten(fn func()) {
	p.cbMu.Lock()
	p.onInboundFinalizeV1ResponseWritten = fn
	p.cbMu.Unlock()
}

// OnConnState registers a callback invoked on connectivity
// transitions. Daemon uses this to paint /api/daemon and the tray.
func (p *RemoteProxy) OnConnState(fn func(state, human string)) {
	p.cbMu.Lock()
	p.onConnState = fn
	p.cbMu.Unlock()
}

// OnEnumerateHint registers a callback invoked when the plugin
// asks the daemon to schedule a re-enumeration.
func (p *RemoteProxy) OnEnumerateHint(fn func(reason string)) {
	p.cbMu.Lock()
	p.onEnumerateHint = fn
	p.cbMu.Unlock()
}

// OnCheckpointNeededV1 registers a callback for a cloud checkpoint request.
// Registering the callback does not enable durable mode; callers must first
// verify the signed capability and complete runtime negotiation.
func (p *RemoteProxy) OnCheckpointNeededV1(fn func(proto.RemoteCheckpointNeededV1Notification)) {
	p.cbMu.Lock()
	p.onCheckpointNeededV1 = fn
	p.cbMu.Unlock()
}

// OnRulesUpdate registers a callback invoked when the plugin pushes a
// cloud-authored selective-sync ruleset (remote.rules_update). The
// daemon rebuilds its syncrules.Engine from the supplied rules and
// swaps it into the live orchestrator so routing changes made in the
// portal apply without a restart.
//
// Set this BEFORE Subscribe — like the other notifications, a nil
// callback drops the push silently.
func (p *RemoteProxy) OnRulesUpdate(fn func(changeID string, rules []syncrules.Rule)) {
	p.cbMu.Lock()
	p.onRulesUpdate = fn
	p.cbMu.Unlock()
}

// OnNamespaceKeyRotated registers a callback invoked when the plugin
// forwards a namespace.key_rotated audit signal. The
// daemon's keyrotation.Rotator turns it into client-side key work.
//
// Set this BEFORE Subscribe — a nil callback drops the push silently.
func (p *RemoteProxy) OnNamespaceKeyRotated(fn func(proto.RemoteNamespaceKeyRotatedNotification)) {
	p.cbMu.Lock()
	p.onNamespaceKeyRotated = fn
	p.cbMu.Unlock()
}

// OnNamespaceKeyBroadcast registers a callback invoked when the plugin
// pushes freshly-wrapped key material to this device. The daemon unwraps
// the blob addressed to its device key and installs the new content key.
func (p *RemoteProxy) OnNamespaceKeyBroadcast(fn func(proto.RemoteNamespaceKeyBroadcastNotification)) {
	p.cbMu.Lock()
	p.onNamespaceKeyBroadcast = fn
	p.cbMu.Unlock()
}

// Publish RPCs the plugin to send a batch of events to the remote.
// Returns per-event outcomes; daemon retries entries with
// Accepted=false subject to Retryable/RetryAfter hints.
func (p *RemoteProxy) Publish(ctx context.Context, events []proto.RemoteEvent) (proto.RemotePublishResult, error) {
	var result proto.RemotePublishResult
	err := p.call(ctx, proto.MethodRemotePublish, proto.RemotePublishParams{Events: events}, &result)
	return result, err
}

// PublishStagedV1 sends one exceptional retained checkpoint by private-file
// descriptor. The sealed body never enters the JSON-RPC frame; ordinary
// Publish calls and their frozen wire shape are unchanged.
func (p *RemoteProxy) PublishStagedV1(ctx context.Context, params proto.RemotePublishStagedV1Params) (proto.RemotePublishResult, error) {
	var result proto.RemotePublishResult
	err := p.call(ctx, proto.MethodRemotePublishStagedV1, params, &result)
	return result, err
}

// Fetch pulls events the local store doesn't have yet.
func (p *RemoteProxy) Fetch(ctx context.Context, params proto.RemoteFetchParams) (proto.RemoteFetchResult, error) {
	var result proto.RemoteFetchResult
	err := p.call(ctx, proto.MethodRemoteFetch, params, &result)
	return result, err
}

// NegotiateSyncV1 selects a fail-safe runtime sync mode. Callers must gate
// this additive RPC on CapabilityDurableDeltaSyncV1 from the verified signed
// plugin manifest; a method error means legacy mode.
func (p *RemoteProxy) NegotiateSyncV1(ctx context.Context, params proto.RemoteNegotiateSyncV1Params) (proto.RemoteNegotiateSyncV1Result, error) {
	var result proto.RemoteNegotiateSyncV1Result
	err := p.call(ctx, proto.MethodRemoteNegotiateSyncV1, params, &result)
	return result, err
}

// ResumeCursorV1 transfers the daemon-owned authoritative durable cursor to a
// freshly started plugin before durable reads can begin.
func (p *RemoteProxy) ResumeCursorV1(ctx context.Context, params proto.RemoteResumeCursorV1Params) (proto.RemoteResumeCursorV1Result, error) {
	var result proto.RemoteResumeCursorV1Result
	err := p.call(ctx, proto.MethodRemoteResumeCursorV1, params, &result)
	return result, err
}

// ResumeCursorsV1 atomically transfers every daemon-owned cursor in the
// negotiated multistream generation. The caller validates the complete echo
// before enabling durable reads.
func (p *RemoteProxy) ResumeCursorsV1(ctx context.Context, params proto.RemoteResumeCursorsV1Params) (proto.RemoteResumeCursorsV1Result, error) {
	var result proto.RemoteResumeCursorsV1Result
	err := p.call(ctx, proto.MethodRemoteResumeCursorsV1, params, &result)
	return result, err
}

// FetchV2 pulls durable events after an opaque contiguous cloud cursor.
func (p *RemoteProxy) FetchV2(ctx context.Context, params proto.RemoteFetchV2Params) (proto.RemoteFetchV2Result, error) {
	var result proto.RemoteFetchV2Result
	err := p.call(ctx, proto.MethodRemoteFetchV2, params, &result)
	if err == nil && result.StagedCheckpoint != nil && len(result.Events) == 1 {
		normalizeStagedWirePayload(&result.Events[0])
	}
	return result, err
}

// FetchParentV1 retrieves one canonical parent by opaque event hash for gap
// recovery. Its authenticated log cursor is evidence only and is never a main
// stream acknowledgement.
func (p *RemoteProxy) FetchParentV1(ctx context.Context, params proto.RemoteFetchParentV1Params) (proto.RemoteFetchParentV1Result, error) {
	var result proto.RemoteFetchParentV1Result
	err := p.call(ctx, proto.MethodRemoteFetchParentV1, params, &result)
	if err == nil && result.Record != nil && result.Record.StagedCheckpoint != nil {
		normalizeStagedWirePayload(&result.Record.Event)
	}
	return result, err
}

// AckV2 monotonically acknowledges the daemon's highest contiguous,
// durably-applied cloud cursor.
func (p *RemoteProxy) AckV2(ctx context.Context, params proto.RemoteAckV2Params) (proto.RemoteAckV2Result, error) {
	var result proto.RemoteAckV2Result
	err := p.call(ctx, proto.MethodRemoteAckV2, params, &result)
	return result, err
}

// RequestCheckpointV1 asks the remote to coordinate a fresh client-produced
// encrypted checkpoint.
func (p *RemoteProxy) RequestCheckpointV1(ctx context.Context, params proto.RemoteRequestCheckpointV1Params) (proto.RemoteRequestCheckpointV1Result, error) {
	var result proto.RemoteRequestCheckpointV1Result
	err := p.call(ctx, proto.MethodRemoteRequestCheckpointV1, params, &result)
	if err == nil && result.Checkpoint != nil && result.Checkpoint.StagedCheckpoint != nil {
		normalizeStagedWirePayload(&result.Checkpoint.Event)
	}
	return result, err
}

// ObserveSyncV1 transfers one closed, content-free client observation. The
// caller must independently gate this method on the exact signed plugin
// manifest capability.
func (p *RemoteProxy) ObserveSyncV1(ctx context.Context, params proto.RemoteSyncObservationV1Params) (proto.RemoteSyncObservationV1Result, error) {
	var result proto.RemoteSyncObservationV1Result
	if err := params.Validate(); err != nil {
		return result, err
	}
	err := p.call(ctx, proto.MethodRemoteObserveSyncV1, params, &result)
	return result, err
}

func normalizeStagedWirePayload(event *proto.RemoteEvent) {
	if event != nil && string(event.Bytes) == "null" {
		event.Bytes = nil
	}
}

// Enumerate lists the plugin's view of remote namespaces + branch tips.
func (p *RemoteProxy) Enumerate(ctx context.Context, params proto.RemoteEnumerateParams) (proto.RemoteEnumerateResult, error) {
	var result proto.RemoteEnumerateResult
	err := p.call(ctx, proto.MethodRemoteEnumerate, params, &result)
	return result, err
}

// Subscribe registers interest in a namespace's inbound stream.
func (p *RemoteProxy) Subscribe(ctx context.Context, namespaceID string) error {
	return p.call(ctx, proto.MethodRemoteSubscribe, proto.RemoteSubscribeParams{NamespaceID: namespaceID}, nil)
}

// Unsubscribe withdraws interest.
func (p *RemoteProxy) Unsubscribe(ctx context.Context, namespaceID string) error {
	return p.call(ctx, proto.MethodRemoteUnsubscribe, proto.RemoteUnsubscribeParams{NamespaceID: namespaceID}, nil)
}

// Status polls the plugin's connectivity + counter view.
func (p *RemoteProxy) Status(ctx context.Context) (proto.RemoteStatusResult, error) {
	var result proto.RemoteStatusResult
	err := p.call(ctx, proto.MethodRemoteStatus, struct{}{}, &result)
	return result, err
}

// ListNamespaceDevices asks the plugin for the namespace's surviving
// member devices and their wrap keys.
func (p *RemoteProxy) ListNamespaceDevices(ctx context.Context, namespaceID string) (proto.RemoteListNamespaceDevicesResult, error) {
	var result proto.RemoteListNamespaceDevicesResult
	err := p.call(ctx, proto.MethodRemoteListNamespaceDevices, proto.RemoteListNamespaceDevicesParams{NamespaceID: namespaceID}, &result)
	return result, err
}

// RegisterWrapKey persists this device's X25519 wrap public key with the
// control plane (account-scoped end-to-end encryption). No result payload.
func (p *RemoteProxy) RegisterWrapKey(ctx context.Context, pub []byte) error {
	return p.call(ctx, proto.MethodRemoteRegisterWrapKey, proto.RemoteRegisterWrapKeyParams{WrapPubKey: pub}, nil)
}

// ListAccountDevices asks the plugin for every active device in the caller's
// account that has a registered wrap pubkey (includes self). Account-scoped, so
// no namespace id is needed for recipient discovery.
func (p *RemoteProxy) ListAccountDevices(ctx context.Context) (proto.RemoteListAccountDevicesResult, error) {
	var result proto.RemoteListAccountDevicesResult
	err := p.call(ctx, proto.MethodRemoteListAccountDevices, struct{}{}, &result)
	return result, err
}

// EnvelopeCaps asks the plugin for the server-asserted account-level envelope
// capability switch (2026-07-29 envelope wire-efficiency ADR D3). Fail-closed
// contract: the plugin answers {"v3_enabled": false, "source": "absent"} for
// a missing entitlement, a missing envelope_caps block, or any upstream
// error; an older plugin that does not know the method surfaces a JSON-RPC
// error here, which the caller likewise treats as disabled.
func (p *RemoteProxy) EnvelopeCaps(ctx context.Context) (proto.RemoteEnvelopeCapsResult, error) {
	var result proto.RemoteEnvelopeCapsResult
	err := p.call(ctx, proto.MethodRemoteEnvelopeCaps, struct{}{}, &result)
	return result, err
}

// PutNamespaceKey conditionally writes wrapped key material for a key
// version back to the namespace_keys row. The result's Claimed reports
// whether this write won the compare-and-swap.
func (p *RemoteProxy) PutNamespaceKey(ctx context.Context, params proto.RemotePutNamespaceKeyParams) (proto.RemotePutNamespaceKeyResult, error) {
	var result proto.RemotePutNamespaceKeyResult
	err := p.call(ctx, proto.MethodRemotePutNamespaceKey, params, &result)
	return result, err
}

// GetNamespaceKey reads back the wrapped material already written for a key
// version, so a compare-and-swap loser can adopt the winner's key.
func (p *RemoteProxy) GetNamespaceKey(ctx context.Context, params proto.RemoteGetNamespaceKeyParams) (proto.RemoteGetNamespaceKeyResult, error) {
	var result proto.RemoteGetNamespaceKeyResult
	err := p.call(ctx, proto.MethodRemoteGetNamespaceKey, params, &result)
	return result, err
}

// BroadcastNamespaceKey asks the plugin to push wrapped key material to
// surviving devices over the live transport.
func (p *RemoteProxy) BroadcastNamespaceKey(ctx context.Context, params proto.RemoteBroadcastNamespaceKeyParams) error {
	return p.call(ctx, proto.MethodRemoteBroadcastNamespaceKey, params, nil)
}

// GetNamespaceRole asks the plugin for the caller's role in a namespace, so
// the daemon can gate team operations client-side. The
// plugin's authenticated transport identifies the caller server-side.
func (p *RemoteProxy) GetNamespaceRole(ctx context.Context, params proto.RemoteGetNamespaceRoleParams) (proto.RemoteGetNamespaceRoleResult, error) {
	var result proto.RemoteGetNamespaceRoleResult
	err := p.call(ctx, proto.MethodRemoteGetNamespaceRole, params, &result)
	return result, err
}

func (p *RemoteProxy) GetTrustAnchor(ctx context.Context) (proto.RemoteGetTrustAnchorResult, error) {
	var result proto.RemoteGetTrustAnchorResult
	err := p.call(ctx, proto.MethodRemoteGetTrustAnchor, struct{}{}, &result)
	return result, err
}

func (p *RemoteProxy) SubmitTrustAnchor(ctx context.Context, object proto.RemoteOpaqueSignedObject) (proto.RemoteSubmitTrustAnchorResult, error) {
	var result proto.RemoteSubmitTrustAnchorResult
	err := p.call(ctx, proto.MethodRemoteSubmitTrustAnchor, proto.RemoteSubmitSignedObjectParams{Object: object}, &result)
	return result, err
}

func (p *RemoteProxy) GetServiceTrustConfig(ctx context.Context) (proto.RemoteGetServiceTrustConfigResult, error) {
	var result proto.RemoteGetServiceTrustConfigResult
	err := p.call(ctx, proto.MethodRemoteGetServiceTrustConfig, struct{}{}, &result)
	return result, err
}

func (p *RemoteProxy) GetAuthorityTransitions(ctx context.Context, fromExclusive uint64) (proto.RemoteGetAuthorityTransitionsResult, error) {
	var result proto.RemoteGetAuthorityTransitionsResult
	err := p.call(ctx, proto.MethodRemoteGetAuthorityTransitions, proto.RemoteGetAuthorityTransitionsParams{FromExclusive: fromExclusive}, &result)
	return result, err
}

func (p *RemoteProxy) SubmitAuthorityTransition(ctx context.Context, object proto.RemoteOpaqueSignedObject) error {
	return p.call(ctx, proto.MethodRemoteSubmitAuthorityTransition, proto.RemoteSubmitSignedObjectParams{Object: object}, nil)
}

func (p *RemoteProxy) GetSignedRoster(ctx context.Context, params proto.RemoteGetSignedRosterParams) (proto.RemoteGetSignedRosterResult, error) {
	var result proto.RemoteGetSignedRosterResult
	err := p.call(ctx, proto.MethodRemoteGetSignedRoster, params, &result)
	return result, err
}

func (p *RemoteProxy) SubmitRosterTransition(ctx context.Context, object proto.RemoteOpaqueSignedObject) error {
	return p.call(ctx, proto.MethodRemoteSubmitRosterTransition, proto.RemoteSubmitSignedObjectParams{Object: object}, nil)
}

func (p *RemoteProxy) SubmitAtomicAuthorityRosterTransition(ctx context.Context, object proto.RemoteOpaqueSignedObject) error {
	return p.call(ctx, proto.MethodRemoteSubmitAtomicAuthorityRosterTransition, proto.RemoteSubmitSignedObjectParams{Object: object}, nil)
}

func (p *RemoteProxy) RegisterDeviceCredential(ctx context.Context, params proto.RemoteRegisterDeviceCredentialParams) error {
	return p.call(ctx, proto.MethodRemoteRegisterDeviceCredential, params, nil)
}

func (p *RemoteProxy) ActivateSyncGeneration(ctx context.Context, params proto.RemoteActivateSyncGenerationParams) (proto.RemoteActivateSyncGenerationResult, error) {
	var result proto.RemoteActivateSyncGenerationResult
	err := p.call(ctx, proto.MethodRemoteActivateSyncGeneration, params, &result)
	return result, err
}

func (p *RemoteProxy) GetSyncGenerationStatus(ctx context.Context, params proto.RemoteGetSyncGenerationStatusParams) (proto.RemoteGetSyncGenerationStatusResult, error) {
	var result proto.RemoteGetSyncGenerationStatusResult
	err := p.call(ctx, proto.MethodRemoteGetSyncGenerationStatus, params, &result)
	return result, err
}

func (p *RemoteProxy) GetRosterConsistency(ctx context.Context, params proto.RemoteGetRosterConsistencyParams) (proto.RemoteGetRosterConsistencyResult, error) {
	var result proto.RemoteGetRosterConsistencyResult
	err := p.call(ctx, proto.MethodRemoteGetRosterConsistency, params, &result)
	return result, err
}

func (p *RemoteProxy) SecurityEpochPrepare(ctx context.Context, params proto.RemoteSecurityEpochPrepareParams) (proto.RemoteSecurityEpochStatusResult, error) {
	var result proto.RemoteSecurityEpochStatusResult
	err := p.call(ctx, proto.MethodRemoteSecurityEpochPrepare, params, &result)
	return result, err
}
func (p *RemoteProxy) SecurityEpochCommit(ctx context.Context, params proto.RemoteSecurityEpochCommandParams) (proto.RemoteSecurityEpochStatusResult, error) {
	var result proto.RemoteSecurityEpochStatusResult
	err := p.call(ctx, proto.MethodRemoteSecurityEpochCommit, params, &result)
	return result, err
}
func (p *RemoteProxy) SecurityEpochActivate(ctx context.Context, params proto.RemoteSecurityEpochCommandParams) (proto.RemoteSecurityEpochStatusResult, error) {
	var result proto.RemoteSecurityEpochStatusResult
	err := p.call(ctx, proto.MethodRemoteSecurityEpochActivate, params, &result)
	return result, err
}
func (p *RemoteProxy) SecurityEpochStatus(ctx context.Context, params proto.RemoteSecurityEpochCommandParams) (proto.RemoteSecurityEpochStatusResult, error) {
	var result proto.RemoteSecurityEpochStatusResult
	err := p.call(ctx, proto.MethodRemoteSecurityEpochStatus, params, &result)
	return result, err
}

func (p *RemoteProxy) SubmitDeviceTransitionPlan(ctx context.Context, params proto.RemoteSubmitDeviceTransitionPlanParams) error {
	return p.call(ctx, proto.MethodRemoteSubmitDeviceTransitionPlan, params, nil)
}

func (p *RemoteProxy) GetDeviceTransitionPlans(ctx context.Context, params proto.RemoteGetDeviceTransitionPlansParams) (proto.RemoteGetDeviceTransitionPlansResult, error) {
	var result proto.RemoteGetDeviceTransitionPlansResult
	err := p.call(ctx, proto.MethodRemoteGetDeviceTransitionPlans, params, &result)
	return result, err
}

// Shutdown sends the plugin a graceful shutdown signal and waits for
// the read pump to drain. After Shutdown returns, no further methods
// are valid.
func (p *RemoteProxy) Shutdown(ctx context.Context) error {
	_ = p.call(ctx, proto.MethodShutdown, proto.ShutdownParams{}, nil)
	// Closing the underlying transport tears down the read pump.
	if p.closer != nil {
		_ = p.closer.Close()
	}
	<-p.readDoneCh
	return nil
}

// call performs one JSON-RPC round-trip against the remote plugin.
// Concurrent callers are safe — outbound writes serialize via
// writeMu and responses correlate by JSON-RPC ID through the pending
// table.
func (p *RemoteProxy) call(ctx context.Context, method string, params any, out any) error {
	id := p.nextID.Add(1)
	idJSON, _ := json.Marshal(id)

	paramsJSON, err := json.Marshal(params)
	if err != nil {
		return fmt.Errorf("encode params: %w", err)
	}
	reqBytes, err := json.Marshal(proto.Request{
		JSONRPC: "2.0",
		ID:      idJSON,
		Method:  method,
		Params:  paramsJSON,
	})
	if err != nil {
		return fmt.Errorf("encode request: %w", err)
	}

	respCh := make(chan proto.Response, 1)
	p.pendingMu.Lock()
	p.pending[id] = respCh
	p.pendingMu.Unlock()

	defer func() {
		p.pendingMu.Lock()
		delete(p.pending, id)
		p.pendingMu.Unlock()
	}()

	p.writeMu.Lock()
	werr := p.fw.Write(reqBytes)
	p.writeMu.Unlock()
	if werr != nil {
		return fmt.Errorf("write request: %w", werr)
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case resp := <-respCh:
		if resp.Error != nil {
			return &proto.RPCError{Code: resp.Error.Code, Message: resp.Error.Message}
		}
		if out != nil && len(resp.Result) > 0 {
			if err := json.Unmarshal(resp.Result, out); err != nil {
				return fmt.Errorf("decode result: %w", err)
			}
		}
		return nil
	case <-p.readDoneCh:
		if p.readErr != nil {
			return fmt.Errorf("transport closed: %w", p.readErr)
		}
		return errors.New("transport closed")
	}
}

// readPump runs in a goroutine for the lifetime of the RemoteProxy.
// It frame-decodes inbound JSON, routes responses to pending callers,
// and dispatches notifications to the registered callbacks.
func (p *RemoteProxy) readPump() {
	defer close(p.readDoneCh)
	if p.inboundCh != nil {
		defer close(p.inboundCh)
	}
	if p.finalizeCh != nil {
		defer close(p.finalizeCh)
	}
	for {
		frame, err := p.fr.Read()
		if err != nil {
			if !errors.Is(err, io.EOF) {
				p.readErr = err
			}
			return
		}

		// JSON-RPC frame: either a response (has id+result/error) or
		// a notification (has method, no id).
		var probe struct {
			ID     json.RawMessage `json:"id,omitempty"`
			Method string          `json:"method,omitempty"`
		}
		if err := json.Unmarshal(frame, &probe); err != nil {
			// Malformed frame — log and continue (daemon will surface
			// repeated parse errors via the orchestrator).
			continue
		}

		if probe.Method == proto.NotificationRemoteInbound || probe.Method == proto.MethodRemoteInboundDeliveryV2 {
			if err := p.enqueueInbound(frame, probe.Method == proto.NotificationRemoteInbound); err != nil {
				p.readErr = err
				if p.closer != nil {
					_ = p.closer.Close()
				}
				return
			}
			continue
		}
		if probe.Method == proto.MethodRemoteInboundFinalizeV1 {
			if err := p.enqueueInboundFinalize(frame); err != nil {
				p.readErr = err
				if p.closer != nil {
					_ = p.closer.Close()
				}
				return
			}
			continue
		}
		if probe.Method != "" {
			p.handleNotification(probe.Method, frame)
			continue
		}

		// Response path: parse the ID, find the pending channel.
		var resp proto.Response
		if err := json.Unmarshal(frame, &resp); err != nil {
			continue
		}
		var id int64
		if err := json.Unmarshal(resp.ID, &id); err != nil {
			continue
		}
		p.pendingMu.Lock()
		ch, ok := p.pending[id]
		p.pendingMu.Unlock()
		if ok {
			ch <- resp
		}
	}
}

func (p *RemoteProxy) enqueueInboundFinalize(frame []byte) error {
	var envelope struct {
		ID     json.RawMessage `json:"id,omitempty"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(frame, &envelope); err != nil {
		return err
	}
	if len(envelope.ID) == 0 || string(envelope.ID) == "null" {
		return fmt.Errorf("plugin/proxy: inbound finalize request id required")
	}
	var params proto.RemoteInboundFinalizeV1Params
	if err := json.Unmarshal(envelope.Params, &params); err != nil {
		return err
	}
	if !validInboundFinalizeEvidence(params.Evidence) {
		return fmt.Errorf("plugin/proxy: invalid inbound finalize evidence")
	}
	request := inboundFinalizeRequest{params: params, id: append(json.RawMessage(nil), envelope.ID...)}
	t := time.NewTimer(100 * time.Millisecond)
	defer t.Stop()
	select {
	case p.finalizeCh <- request:
		return nil
	case <-t.C:
		return fmt.Errorf("plugin/proxy: inbound finalize queue saturated")
	}
}

func (p *RemoteProxy) enqueueInbound(frame []byte, legacy bool) error {
	var envelope struct {
		ID     json.RawMessage `json:"id,omitempty"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(frame, &envelope); err != nil {
		return err
	}
	var delivery proto.RemoteInboundDeliveryV2
	if legacy {
		var stagedProbe struct {
			StagedCheckpoint json.RawMessage `json:"staged_checkpoint"`
		}
		if err := json.Unmarshal(envelope.Params, &stagedProbe); err != nil {
			return err
		}
		if len(stagedProbe.StagedCheckpoint) != 0 && string(stagedProbe.StagedCheckpoint) != "null" {
			return fmt.Errorf("plugin/proxy: staged checkpoint requires inbound v2")
		}
		var n proto.RemoteInboundNotification
		if err := json.Unmarshal(envelope.Params, &n); err != nil {
			return err
		}
		delivery.Events = n.Events
	} else {
		if len(envelope.ID) == 0 || string(envelope.ID) == "null" {
			return fmt.Errorf("plugin/proxy: inbound v2 request id required")
		}
		if err := json.Unmarshal(envelope.Params, &delivery); err != nil {
			return err
		}
		if !validOpaqueDeliveryValue(delivery.DeliveryID, proto.MaxDeliveryIDBytes) ||
			!validOpaqueDeliveryValue(delivery.Cursor, proto.MaxDurableCursorBytes) {
			return fmt.Errorf("plugin/proxy: invalid inbound delivery identity")
		}
		if remoteDeliveryHasDurableMetadata(delivery) && !validDurableDeliveryAdjacency(delivery) {
			return fmt.Errorf("plugin/proxy: invalid durable inbound adjacency")
		}
	}
	if len(delivery.Events) == 0 || len(delivery.Events) > proto.MaxInboundEvents {
		return fmt.Errorf("plugin/proxy: inbound page exceeds event limit")
	}
	// RemoteEvent.Bytes intentionally preserves the frozen v1 JSON field, so a
	// descriptor-only staged event crosses JSON-RPC as bytes:null. Normalize
	// only that exact token to the in-memory absence used by the staged ABI.
	if delivery.StagedCheckpoint != nil && len(delivery.Events) == 1 {
		normalizeStagedWirePayload(&delivery.Events[0])
	}
	if delivery.StagedCheckpoint != nil && (legacy || !validStagedInboundDelivery(delivery)) {
		return fmt.Errorf("plugin/proxy: invalid staged inbound checkpoint")
	}
	var total int64
	for _, e := range delivery.Events {
		if delivery.StagedCheckpoint != nil {
			continue
		}
		if len(e.Bytes) > proto.MaxSealedEventBytes {
			return fmt.Errorf("plugin/proxy: inbound event exceeds byte limit")
		}
		total += int64(len(e.Bytes))
		if total > proto.MaxInboundBytes {
			return fmt.Errorf("plugin/proxy: inbound page exceeds byte limit")
		}
	}
	for {
		cur := p.inboundBytes.Load()
		if cur+total > 64<<20 {
			return fmt.Errorf("plugin/proxy: inbound queue byte limit reached")
		}
		if p.inboundBytes.CompareAndSwap(cur, cur+total) {
			break
		}
	}
	t := time.NewTimer(100 * time.Millisecond)
	defer t.Stop()
	select {
	case p.inboundCh <- inboundDelivery{delivery: delivery, id: append(json.RawMessage(nil), envelope.ID...), legacy: legacy, bytes: total}:
		return nil
	case <-t.C:
		p.inboundBytes.Add(-total)
		return fmt.Errorf("plugin/proxy: inbound queue saturated")
	}
}

func validStagedInboundDelivery(delivery proto.RemoteInboundDeliveryV2) bool {
	if len(delivery.Events) != 1 || delivery.StagedCheckpoint == nil {
		return false
	}
	event, staged := delivery.Events[0], delivery.StagedCheckpoint
	return len(event.Bytes) == 0 && event.Lane == "retained" && !event.Clear &&
		event.CheckpointCoverage > 0 && validRemoteCursorDigest(event.CheckpointGeneration) && validRemoteCursorDigest(event.CheckpointAlignmentHash) &&
		staged.ProtocolVersion == proto.RemoteStagedTransferProtocolV1 && validRemoteCursorDigest(staged.TransferID) &&
		staged.SealedBytes > proto.MaxSealedEventBytes && staged.SealedBytes <= proto.MaxRemoteStagedCheckpointBytes &&
		validRemoteCursorDigest(staged.BodyDigest) && staged.BodyDigest == event.BodyDigest &&
		staged.StreamID == delivery.StreamID && staged.StreamEpoch == delivery.StreamEpoch &&
		validRemoteCursorDigest(staged.BindingDigest) && staged.BindingDigest == proto.RemoteStagedBindingDigest(event, *staged)
}

func (p *RemoteProxy) inboundWorker() {
	for d := range p.inboundCh {
		p.cbMu.RLock()
		fn := p.onInbound
		fnV2 := p.onInboundV2
		afterV2 := p.onInboundV2ResponseWritten
		p.cbMu.RUnlock()
		if d.legacy {
			if fn != nil {
				fn(d.delivery.Events)
			}
		} else {
			ack := proto.RemoteInboundAckV2{DeliveryID: d.delivery.DeliveryID, Outcomes: make([]proto.RemoteInboundEventOutcomeV2, len(d.delivery.Events))}
			if fnV2 != nil {
				ack = fnV2(d.delivery)
			} else {
				for i := range ack.Outcomes {
					ack.Outcomes[i] = proto.RemoteInboundEventOutcomeV2{Index: uint32(i), Disposition: "retryable", ReasonCode: "handler-unavailable"}
				}
			}
			_ = p.writeInboundAck(d.id, ack)
			if afterV2 != nil {
				afterV2()
			}
		}
		p.inboundBytes.Add(-d.bytes)
	}
}

func (p *RemoteProxy) inboundFinalizeWorker() {
	for request := range p.finalizeCh {
		p.cbMu.RLock()
		fn := p.onInboundFinalizeV1
		after := p.onInboundFinalizeV1ResponseWritten
		p.cbMu.RUnlock()
		result := proto.RemoteInboundFinalizeV1Result{ReasonCode: "handler-unavailable"}
		if fn != nil {
			result = fn(request.params)
		}
		if !validInboundFinalizeResult(request.params.Evidence, result) {
			result = proto.RemoteInboundFinalizeV1Result{ReasonCode: "handler-invalid-result"}
		}
		_ = p.writeInboundFinalizeResult(request.id, result)
		if after != nil {
			after()
		}
	}
}

func validOpaqueDeliveryValue(value string, maximum int) bool {
	if value == "" || maximum <= 0 || len(value) > maximum {
		return false
	}
	for _, r := range value {
		if r < 0x21 || r > 0x7e {
			return false
		}
	}
	return true
}

func remoteDeliveryHasDurableMetadata(delivery proto.RemoteInboundDeliveryV2) bool {
	return delivery.ProtocolVersion != 0 || delivery.StreamID != "" || delivery.StreamEpoch != "" ||
		delivery.PredecessorCursor != "" || delivery.PredecessorPosition != 0 ||
		delivery.Position != 0 || delivery.CursorDigest != ""
}

func validDurableDeliveryAdjacency(delivery proto.RemoteInboundDeliveryV2) bool {
	const maxStreamIdentityBytes = 512
	if delivery.ProtocolVersion != 1 || len(delivery.Events) != 1 ||
		!validOpaqueDeliveryValue(delivery.StreamID, maxStreamIdentityBytes) ||
		!validOpaqueDeliveryValue(delivery.StreamEpoch, maxStreamIdentityBytes) ||
		delivery.Position == 0 || !validRemoteCursorDigest(delivery.CursorDigest) {
		return false
	}
	wantDigest := sha256.Sum256([]byte(delivery.Cursor))
	if delivery.CursorDigest != hex.EncodeToString(wantDigest[:]) {
		return false
	}
	return delivery.PredecessorPosition == delivery.Position-1 &&
		validOpaqueDeliveryValue(delivery.PredecessorCursor, proto.MaxDurableCursorBytes) &&
		delivery.PredecessorCursor != delivery.Cursor
}

func validRemoteCursorDigest(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size && hex.EncodeToString(decoded) == value
}

func validInboundFinalizeEvidence(e proto.RemoteInboundFinalizeEvidenceV1) bool {
	if e.ProtocolVersion != 1 || e.Position == 0 ||
		!validOpaqueDeliveryValue(e.RemoteIdentity, 512) ||
		!validOpaqueDeliveryValue(e.DeliveryID, proto.MaxDeliveryIDBytes) ||
		!validOpaqueDeliveryValue(e.StreamID, 512) ||
		!validOpaqueDeliveryValue(e.StreamEpoch, 512) ||
		!validOpaqueDeliveryValue(e.Cursor, proto.MaxDurableCursorBytes) ||
		!validRemoteCursorDigest(e.CursorDigest) ||
		!validOptionalOpaqueDeliveryValue(e.NamespaceID, 512) ||
		!validOptionalOpaqueDeliveryValue(e.BranchID, 512) ||
		!validOpaqueDeliveryValue(e.Kind, 128) ||
		!validOpaqueDeliveryValue(e.ArtifactID, 512) ||
		!validOpaqueDeliveryValue(e.WireEventID, 512) ||
		!validRemoteCursorDigest(e.BodyDigest) ||
		!validOptionalRemoteCursorDigest(e.ParentHash) ||
		!validOpaqueDeliveryValue(e.EventType, 128) ||
		!validOpaqueDeliveryValue(e.Origin, 512) ||
		!validOptionalOpaqueDeliveryValue(e.SourceAgent, 512) ||
		!validOptionalOpaqueDeliveryValue(e.Lane, 128) {
		return false
	}
	if e.Lane == "retained" {
		if !validRemoteCursorDigest(e.CheckpointAlignmentHash) {
			return false
		}
	} else if e.CheckpointAlignmentHash != "" {
		return false
	}
	want := sha256.Sum256([]byte(e.Cursor))
	if e.CursorDigest != hex.EncodeToString(want[:]) {
		return false
	}
	switch e.FinalizeKind {
	case proto.InboundFinalizeCanonicalMaterialize:
		return !e.Clear && validRemoteCursorDigest(e.WireEventHash) &&
			validOpaqueDeliveryValue(e.CanonicalEventID, 512) && validRemoteCursorDigest(e.CanonicalHash) &&
			e.NoopReason == "" && e.AuthenticatedHeaderDigest == "" && e.AuthenticatedSignerIdentity == ""
	case proto.InboundFinalizeAuthenticatedNoop:
		if e.Clear || e.CanonicalEventID != "" || e.CanonicalHash != "" ||
			!validRemoteCursorDigest(e.AuthenticatedHeaderDigest) || !validProxyAuthenticatedSigner(e.AuthenticatedSignerIdentity) {
			return false
		}
		switch e.NoopReason {
		case proto.InboundFinalizeNoopNotRecipient:
			return validRemoteCursorDigest(e.WireEventHash)
		default:
			return false
		}
	default:
		return false
	}
}

func validOptionalRemoteCursorDigest(value string) bool {
	return value == "" || validRemoteCursorDigest(value)
}

func validProxyAuthenticatedSigner(value string) bool {
	separator := strings.LastIndexByte(value, ':')
	return separator > 0 && validOpaqueDeliveryValue(value[:separator], 512) && validRemoteCursorDigest(value[separator+1:])
}

func validInboundFinalizeResult(evidence proto.RemoteInboundFinalizeEvidenceV1, result proto.RemoteInboundFinalizeV1Result) bool {
	successes := 0
	for _, success := range []bool{result.Materialized, result.NoopFinalized, result.AlreadyFinalized} {
		if success {
			successes++
		}
	}
	if result.Accepted {
		if successes != 1 || result.ReasonCode != "" {
			return false
		}
		if result.AlreadyFinalized {
			return evidence.FinalizeKind == proto.InboundFinalizeCanonicalMaterialize || evidence.FinalizeKind == proto.InboundFinalizeAuthenticatedNoop
		}
		return (evidence.FinalizeKind == proto.InboundFinalizeCanonicalMaterialize && result.Materialized) ||
			(evidence.FinalizeKind == proto.InboundFinalizeAuthenticatedNoop && result.NoopFinalized)
	}
	return successes == 0 && validOpaqueDeliveryValue(result.ReasonCode, 96)
}

func validOptionalOpaqueDeliveryValue(value string, maximum int) bool {
	return value == "" || validOpaqueDeliveryValue(value, maximum)
}

func (p *RemoteProxy) writeInboundAck(id json.RawMessage, ack proto.RemoteInboundAckV2) error {
	result, err := json.Marshal(ack)
	if err != nil {
		return err
	}
	frame, err := json.Marshal(proto.Response{JSONRPC: "2.0", ID: id, Result: result})
	if err != nil {
		return err
	}
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	return p.fw.Write(frame)
}

func (p *RemoteProxy) writeInboundFinalizeResult(id json.RawMessage, result proto.RemoteInboundFinalizeV1Result) error {
	encoded, err := json.Marshal(result)
	if err != nil {
		return err
	}
	frame, err := json.Marshal(proto.Response{JSONRPC: "2.0", ID: id, Result: encoded})
	if err != nil {
		return err
	}
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	return p.fw.Write(frame)
}

func (p *RemoteProxy) handleNotification(method string, frame []byte) {
	var envelope struct {
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(frame, &envelope); err != nil {
		return
	}
	// Snapshot the callbacks under the lock, then invoke outside it: the
	// setters run concurrently on the caller goroutine (see cbMu).
	p.cbMu.RLock()
	onConnState := p.onConnState
	onEnumerateHint := p.onEnumerateHint
	onCheckpointNeededV1 := p.onCheckpointNeededV1
	onRulesUpdate := p.onRulesUpdate
	onNamespaceKeyRotated := p.onNamespaceKeyRotated
	onNamespaceKeyBroadcast := p.onNamespaceKeyBroadcast
	p.cbMu.RUnlock()
	switch method {
	case proto.NotificationRemoteConnState:
		var n proto.RemoteConnStateNotification
		if err := json.Unmarshal(envelope.Params, &n); err == nil && onConnState != nil {
			onConnState(n.ConnState, n.HumanReadableStatus)
		}
	case proto.NotificationRemoteEnumerateHint:
		var n proto.RemoteEnumerateHintNotification
		if err := json.Unmarshal(envelope.Params, &n); err == nil && onEnumerateHint != nil {
			onEnumerateHint(n.Reason)
		}
	case proto.NotificationRemoteCheckpointNeededV1:
		var n proto.RemoteCheckpointNeededV1Notification
		if err := json.Unmarshal(envelope.Params, &n); err == nil && onCheckpointNeededV1 != nil {
			onCheckpointNeededV1(n)
		}
	case proto.NotificationRemoteRulesUpdate:
		var n proto.RemoteRulesUpdateNotification
		if err := json.Unmarshal(envelope.Params, &n); err == nil && onRulesUpdate != nil {
			onRulesUpdate(n.ChangeID, n.Rules)
		}
	case proto.NotificationRemoteNamespaceKeyRotated:
		var n proto.RemoteNamespaceKeyRotatedNotification
		if err := json.Unmarshal(envelope.Params, &n); err == nil && onNamespaceKeyRotated != nil {
			onNamespaceKeyRotated(n)
		}
	case proto.NotificationRemoteNamespaceKeyBroadcast:
		var n proto.RemoteNamespaceKeyBroadcastNotification
		if err := json.Unmarshal(envelope.Params, &n); err == nil && onNamespaceKeyBroadcast != nil {
			onNamespaceKeyBroadcast(n)
		}
	}
}
