package host

import (
	"context"
	"errors"

	"github.com/aplexica/aplexica/internal/plugin/proto"
)

// ErrRemoteRotationUnsupported is returned by the BaseRemoteHandler
// defaults for the namespace key-rotation methods. A remote transport that
// has no team/namespace-membership concept (a single-user BYO transport,
// say) never receives a rotation signal and so never needs to implement
// them; if one is ever called against such a transport, this surfaces a
// clear "not supported" rather than a silent no-op that would drop key
// material.
var ErrRemoteRotationUnsupported = errors.New("host: namespace key rotation not supported by this remote transport")

// ErrRBACUnsupported is returned by the BaseRemoteHandler default for
// GetNamespaceRole. A remote transport with no team/membership concept (a
// single-user BYO transport) has no roles to report; surfacing a clear "not
// supported" lets the daemon distinguish that from a genuine "no membership"
// answer rather than silently treating the caller as role-less.
var ErrRBACUnsupported = errors.New("host: namespace roles not supported by this remote transport")

// ErrRemoteAccountUnsupported is returned by the BaseRemoteHandler defaults
// for the account-scoped wrap-key methods (RegisterWrapKey,
// ListAccountDevices). A remote transport with no account concept (a
// single-user BYO transport, say) has no account-wide device directory to
// register against or enumerate; surfacing a clear "not supported" lets the
// daemon distinguish that from an empty recipient set rather than silently
// dropping E2E key material.
var ErrRemoteAccountUnsupported = errors.New("host: account-scoped wrap keys not supported by this remote transport")

// ErrDurableDeltaSyncUnsupported is the fail-safe default for the additive
// durable-delta RPC surface. A legacy plugin remains a valid RemoteHandler and
// continues to use remote.publish/remote.fetch; callers may attempt the new
// methods only after verifying the signed capability and negotiating runtime
// support.
var ErrDurableDeltaSyncUnsupported = errors.New("host: durable delta sync not supported by this remote transport")

// ErrDurableSyncObservationUnsupported is the fail-safe default for the
// separately signed, content-free durable-sync observation method.
var ErrDurableSyncObservationUnsupported = errors.New("host: durable sync observations not supported by this remote transport")

// RemoteHandler is the plugin-author-facing interface for a
// remote-transport plugin (manifest.kind="remote"). The daemon's
// JSON-RPC host dispatches each `remote.*` method to one of these
// callbacks. Plugins implement THIS interface instead of the adapter-
// flavored Handler above.
//
// Plugins MAY embed Notifier (passed via NotifierContext at
// Initialize time) and call its Inbound / ConnState methods to push
// asynchronous events back to the daemon. The notifier is set up
// during Initialize and remains valid until Shutdown.
//
// Initialize and Shutdown are shared with the adapter ABI and reuse
// the same params/result types; the remote-specific ABI sits between
// them. Note that the InitializeResult.Kinds field MUST be empty for
// a remote plugin — the manifest validator enforces this.
//
// See remote_messages.go for the full payload shapes.
type RemoteHandler interface {
	Initialize(ctx context.Context, params proto.InitializeParams) (proto.InitializeResult, error)

	Publish(ctx context.Context, params proto.RemotePublishParams) (proto.RemotePublishResult, error)
	Fetch(ctx context.Context, params proto.RemoteFetchParams) (proto.RemoteFetchResult, error)
	Enumerate(ctx context.Context, params proto.RemoteEnumerateParams) (proto.RemoteEnumerateResult, error)
	Subscribe(ctx context.Context, params proto.RemoteSubscribeParams) error
	Unsubscribe(ctx context.Context, params proto.RemoteUnsubscribeParams) error
	Status(ctx context.Context) (proto.RemoteStatusResult, error)

	// Namespace key rotation. Optional capability —
	// transports without a team concept can embed BaseRemoteHandler and
	// leave these defaulted. ListNamespaceDevices returns the surviving
	// member devices (post-removal) + their wrap keys; PutNamespaceKey
	// durably stores wrapped key material; BroadcastNamespaceKey pushes it
	// to surviving devices over the live transport.
	ListNamespaceDevices(ctx context.Context, params proto.RemoteListNamespaceDevicesParams) (proto.RemoteListNamespaceDevicesResult, error)
	PutNamespaceKey(ctx context.Context, params proto.RemotePutNamespaceKeyParams) (proto.RemotePutNamespaceKeyResult, error)
	GetNamespaceKey(ctx context.Context, params proto.RemoteGetNamespaceKeyParams) (proto.RemoteGetNamespaceKeyResult, error)
	BroadcastNamespaceKey(ctx context.Context, params proto.RemoteBroadcastNamespaceKeyParams) error

	// Account-scoped wrap-key registration and recipient discovery (end-to-end
	// encryption). RemoteProxy issues both (remote.register_wrap_key,
	// remote.list_account_devices). RegisterWrapKey persists THIS device's
	// X25519 wrap public key with the control plane so a device that paired
	// before it had a wrap key becomes a valid encryption recipient without
	// re-pairing; ListAccountDevices returns every active device in the
	// caller's account that has a registered wrap pubkey (includes self), so
	// the daemon can seal each outbound event to all of them without a
	// namespace id. Optional capability — transports without an account
	// concept can embed BaseRemoteHandler and leave these defaulted to
	// ErrRemoteAccountUnsupported.
	RegisterWrapKey(ctx context.Context, params proto.RemoteRegisterWrapKeyParams) error
	ListAccountDevices(ctx context.Context) (proto.RemoteListAccountDevicesResult, error)

	// GetNamespaceRole reports the caller's role in a namespace (client-
	// side RBAC). Optional capability — transports without a team concept can
	// embed BaseRemoteHandler and leave this defaulted to ErrRBACUnsupported.
	GetNamespaceRole(ctx context.Context, params proto.RemoteGetNamespaceRoleParams) (proto.RemoteGetNamespaceRoleResult, error)

	Shutdown(ctx context.Context, params proto.ShutdownParams) (proto.ShutdownResult, error)
}

// DurableDeltaSyncHandler is an additive extension to RemoteHandler. Keeping
// it separate preserves Go source compatibility for legacy/BYO handlers while
// allowing ServeRemote to recognize the new methods. BaseRemoteHandler
// implements conservative unsupported defaults, so plugins can opt in one
// method at a time while their signed manifest continues to omit
// durable_delta_sync_v1.
type DurableDeltaSyncHandler interface {
	NegotiateSyncV1(ctx context.Context, params proto.RemoteNegotiateSyncV1Params) (proto.RemoteNegotiateSyncV1Result, error)
	FetchV2(ctx context.Context, params proto.RemoteFetchV2Params) (proto.RemoteFetchV2Result, error)
	FetchParentV1(ctx context.Context, params proto.RemoteFetchParentV1Params) (proto.RemoteFetchParentV1Result, error)
	AckV2(ctx context.Context, params proto.RemoteAckV2Params) (proto.RemoteAckV2Result, error)
	RequestCheckpointV1(ctx context.Context, params proto.RemoteRequestCheckpointV1Params) (proto.RemoteRequestCheckpointV1Result, error)
}

// DurableCursorResumeHandler is separate from DurableDeltaSyncHandler so the
// daemon can add authoritative cursor handoff without breaking existing v1
// durable handlers. A plugin opts in only after its signed manifest and runtime
// negotiation both advertise durable_cursor_resume_v1.
type DurableCursorResumeHandler interface {
	ResumeCursorV1(ctx context.Context, params proto.RemoteResumeCursorV1Params) (proto.RemoteResumeCursorV1Result, error)
}

// DurableMultiStreamResumeHandler is a separately negotiated additive
// extension. Keeping it distinct lets an older plugin continue to serve the
// singular shadow-mode handoff while a higher mode fails closed unless the
// signed durable_multistream_v1 capability and this atomic RPC are both real.
type DurableMultiStreamResumeHandler interface {
	ResumeCursorsV1(ctx context.Context, params proto.RemoteResumeCursorsV1Params) (proto.RemoteResumeCursorsV1Result, error)
}

// DurableSyncObservationHandler is additive so existing RemoteHandler
// implementations remain source compatible. The daemon invokes it only for an
// executable whose exact verified manifest signs durable_sync_observation_v1.
type DurableSyncObservationHandler interface {
	ObserveSyncV1(ctx context.Context, params proto.RemoteSyncObservationV1Params) (proto.RemoteSyncObservationV1Result, error)
}

// Notifier is what a RemoteHandler uses to push asynchronous events
// back to the daemon. The host wires this up during Initialize so the
// plugin captures a reference at startup; calling Inbound from any
// goroutine is safe.
//
// Implementations are responsible for marshalling + transport. The
// daemon's host implements this by writing the notification frame
// to stdout; tests can use a stub Notifier that records calls.
type Notifier interface {
	Inbound(events []proto.RemoteEvent) error
	ConnState(state string, humanStatus string) error
	EnumerateHint(reason string) error
	// NamespaceKeyRotated forwards the control plane's key_rotated audit
	// signal to the daemon. NamespaceKeyBroadcast pushes freshly-wrapped
	// key material from the rotation leader to surviving devices.
	NamespaceKeyRotated(n proto.RemoteNamespaceKeyRotatedNotification) error
	NamespaceKeyBroadcast(n proto.RemoteNamespaceKeyBroadcastNotification) error
	// RulesUpdate pushes a cloud-originated selective-sync ruleset to the
	// daemon. The remote (relay / cloud plugin) emits this when the
	// account's routing rules change in the portal so the daemon can
	// rebuild its syncrules.Engine live (RemoteProxy.handleNotification
	// routes it to OnRulesUpdate).
	RulesUpdate(n proto.RemoteRulesUpdateNotification) error
}

// DurableDeltaSyncNotifier is an additive extension to Notifier. Keeping the
// checkpoint method out of Notifier preserves Go source compatibility for
// third-party plugins whose notifier test doubles implement the legacy
// interface. Plugins that need checkpoint notifications can type-assert the
// Notifier supplied at startup to this interface.
type DurableDeltaSyncNotifier interface {
	// CheckpointNeeded asks the daemon to produce a client-side encrypted
	// checkpoint. The notification carries routing/security metadata only.
	CheckpointNeeded(n proto.RemoteCheckpointNeededV1Notification) error
}

// BaseRemoteHandler is an embeddable stub providing safe defaults
// for the optional methods. Plugins that don't have anything to do
// at Enumerate time, for example, can embed this and override only
// what's needed.
//
// The required methods (Initialize, Publish, Fetch, Status,
// Subscribe, Unsubscribe, Shutdown) are NOT defaulted because they
// represent semantic commitments — a Publish that drops events
// silently is worse than a clear "not implemented" error.
type BaseRemoteHandler struct {
	PluginName string
}

// Enumerate default: empty manifest. Plugins that don't track a
// persistent server-side state (e.g. an SSH-rsync transport) can
// rely on this.
func (b BaseRemoteHandler) Enumerate(_ context.Context, _ proto.RemoteEnumerateParams) (proto.RemoteEnumerateResult, error) {
	return proto.RemoteEnumerateResult{}, nil
}

// NegotiateSyncV1 defaults to unsupported, which keeps legacy two-lane sync
// authoritative unless a handler explicitly implements the complete durable
// extension and advertises its signed capability.
func (b BaseRemoteHandler) NegotiateSyncV1(_ context.Context, _ proto.RemoteNegotiateSyncV1Params) (proto.RemoteNegotiateSyncV1Result, error) {
	return proto.RemoteNegotiateSyncV1Result{Mode: proto.RemoteSyncModeLegacy}, ErrDurableDeltaSyncUnsupported
}

func (b BaseRemoteHandler) ResumeCursorV1(_ context.Context, _ proto.RemoteResumeCursorV1Params) (proto.RemoteResumeCursorV1Result, error) {
	return proto.RemoteResumeCursorV1Result{}, ErrDurableDeltaSyncUnsupported
}

func (b BaseRemoteHandler) ResumeCursorsV1(_ context.Context, _ proto.RemoteResumeCursorsV1Params) (proto.RemoteResumeCursorsV1Result, error) {
	return proto.RemoteResumeCursorsV1Result{}, ErrDurableDeltaSyncUnsupported
}

func (b BaseRemoteHandler) FetchV2(_ context.Context, _ proto.RemoteFetchV2Params) (proto.RemoteFetchV2Result, error) {
	return proto.RemoteFetchV2Result{}, ErrDurableDeltaSyncUnsupported
}

func (b BaseRemoteHandler) FetchParentV1(_ context.Context, _ proto.RemoteFetchParentV1Params) (proto.RemoteFetchParentV1Result, error) {
	return proto.RemoteFetchParentV1Result{}, ErrDurableDeltaSyncUnsupported
}

func (b BaseRemoteHandler) AckV2(_ context.Context, _ proto.RemoteAckV2Params) (proto.RemoteAckV2Result, error) {
	return proto.RemoteAckV2Result{}, ErrDurableDeltaSyncUnsupported
}

func (b BaseRemoteHandler) RequestCheckpointV1(_ context.Context, _ proto.RemoteRequestCheckpointV1Params) (proto.RemoteRequestCheckpointV1Result, error) {
	return proto.RemoteRequestCheckpointV1Result{}, ErrDurableDeltaSyncUnsupported
}

func (b BaseRemoteHandler) ObserveSyncV1(_ context.Context, _ proto.RemoteSyncObservationV1Params) (proto.RemoteSyncObservationV1Result, error) {
	return proto.RemoteSyncObservationV1Result{}, ErrDurableSyncObservationUnsupported
}

// ListNamespaceDevices default: rotation unsupported. Override on
// team-capable transports.
func (b BaseRemoteHandler) ListNamespaceDevices(_ context.Context, _ proto.RemoteListNamespaceDevicesParams) (proto.RemoteListNamespaceDevicesResult, error) {
	return proto.RemoteListNamespaceDevicesResult{}, ErrRemoteRotationUnsupported
}

// PutNamespaceKey default: rotation unsupported.
func (b BaseRemoteHandler) PutNamespaceKey(_ context.Context, _ proto.RemotePutNamespaceKeyParams) (proto.RemotePutNamespaceKeyResult, error) {
	return proto.RemotePutNamespaceKeyResult{}, ErrRemoteRotationUnsupported
}

// GetNamespaceKey default: rotation unsupported.
func (b BaseRemoteHandler) GetNamespaceKey(_ context.Context, _ proto.RemoteGetNamespaceKeyParams) (proto.RemoteGetNamespaceKeyResult, error) {
	return proto.RemoteGetNamespaceKeyResult{}, ErrRemoteRotationUnsupported
}

// BroadcastNamespaceKey default: rotation unsupported.
func (b BaseRemoteHandler) BroadcastNamespaceKey(_ context.Context, _ proto.RemoteBroadcastNamespaceKeyParams) error {
	return ErrRemoteRotationUnsupported
}

// RegisterWrapKey default: account-scoped wrap keys unsupported. Override on
// account-capable transports.
func (b BaseRemoteHandler) RegisterWrapKey(_ context.Context, _ proto.RemoteRegisterWrapKeyParams) error {
	return ErrRemoteAccountUnsupported
}

// ListAccountDevices default: account-scoped wrap keys unsupported. Override
// on account-capable transports.
func (b BaseRemoteHandler) ListAccountDevices(_ context.Context) (proto.RemoteListAccountDevicesResult, error) {
	return proto.RemoteListAccountDevicesResult{}, ErrRemoteAccountUnsupported
}

// GetNamespaceRole default: RBAC unsupported. Override on team-capable
// transports.
func (b BaseRemoteHandler) GetNamespaceRole(_ context.Context, _ proto.RemoteGetNamespaceRoleParams) (proto.RemoteGetNamespaceRoleResult, error) {
	return proto.RemoteGetNamespaceRoleResult{}, ErrRBACUnsupported
}
