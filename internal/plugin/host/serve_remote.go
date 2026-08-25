package host

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"

	"github.com/aplexica/aplexica/internal/plugin/proto"
)

// ServeRemote is the remote-plugin counterpart to Serve. It reads
// JSON-RPC requests from r, dispatches each to a RemoteHandler, and
// writes responses to w. Remote plugins MUST use this entry point
// instead of Serve — the method dispatch tables don't overlap.
//
// notifier is the plugin-side outbound channel for pushed
// notifications (remote.inbound, remote.conn_state,
// remote.enumerate_hint). The supplied implementation marshals
// each notification frame onto w under a mutex shared with the
// response writer, so plugin authors don't have to worry about
// concurrent frame interleaving.
func ServeRemote(ctx context.Context, handler RemoteHandler, r io.Reader, w io.Writer) error {
	var writeMu sync.Mutex

	fr := proto.NewFrameReader(r)
	fw := proto.NewFrameWriter(w)

	notifier := &serializingNotifier{fw: fw, mu: &writeMu}
	handler = wrapWithNotifier(handler, notifier)

	for {
		frame, err := fr.Read()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		resp := dispatchRemote(ctx, handler, frame)
		b, mErr := json.Marshal(resp)
		if mErr != nil {
			return mErr
		}
		writeMu.Lock()
		writeErr := fw.Write(b)
		writeMu.Unlock()
		if writeErr != nil {
			return writeErr
		}
		if resp.shutdown {
			return nil
		}
	}
}

// dispatchRemote decodes a frame and routes to the matching
// RemoteHandler method. Method names are namespaced under `remote.`
// so they cannot collide with the adapter dispatch table.
func dispatchRemote(ctx context.Context, handler RemoteHandler, frame []byte) dispatchedResponse {
	var req proto.Request
	if err := json.Unmarshal(frame, &req); err != nil {
		return errResp(nil, proto.CodeParseError, "parse error: "+err.Error())
	}
	switch req.Method {
	case proto.MethodInitialize:
		return call(ctx, req, handler.Initialize)
	case proto.MethodRemotePublish:
		return call(ctx, req, handler.Publish)
	case proto.MethodRemoteFetch:
		return call(ctx, req, handler.Fetch)
	case proto.MethodRemoteNegotiateSyncV1:
		if durable, ok := handler.(DurableDeltaSyncHandler); ok {
			return call(ctx, req, durable.NegotiateSyncV1)
		}
		return errResp(req.ID, proto.CodeInternal, ErrDurableDeltaSyncUnsupported.Error())
	case proto.MethodRemoteResumeCursorV1:
		if resumable, ok := handler.(DurableCursorResumeHandler); ok {
			return call(ctx, req, resumable.ResumeCursorV1)
		}
		return errResp(req.ID, proto.CodeInternal, ErrDurableDeltaSyncUnsupported.Error())
	case proto.MethodRemoteResumeCursorsV1:
		if resumable, ok := handler.(DurableMultiStreamResumeHandler); ok {
			return call(ctx, req, resumable.ResumeCursorsV1)
		}
		return errResp(req.ID, proto.CodeInternal, ErrDurableDeltaSyncUnsupported.Error())
	case proto.MethodRemoteFetchV2:
		if durable, ok := handler.(DurableDeltaSyncHandler); ok {
			return call(ctx, req, durable.FetchV2)
		}
		return errResp(req.ID, proto.CodeInternal, ErrDurableDeltaSyncUnsupported.Error())
	case proto.MethodRemoteFetchParentV1:
		if durable, ok := handler.(DurableDeltaSyncHandler); ok {
			return call(ctx, req, durable.FetchParentV1)
		}
		return errResp(req.ID, proto.CodeInternal, ErrDurableDeltaSyncUnsupported.Error())
	case proto.MethodRemoteAckV2:
		if durable, ok := handler.(DurableDeltaSyncHandler); ok {
			return call(ctx, req, durable.AckV2)
		}
		return errResp(req.ID, proto.CodeInternal, ErrDurableDeltaSyncUnsupported.Error())
	case proto.MethodRemoteRequestCheckpointV1:
		if durable, ok := handler.(DurableDeltaSyncHandler); ok {
			return call(ctx, req, durable.RequestCheckpointV1)
		}
		return errResp(req.ID, proto.CodeInternal, ErrDurableDeltaSyncUnsupported.Error())
	case proto.MethodRemoteObserveSyncV1:
		if observer, ok := handler.(DurableSyncObservationHandler); ok {
			return call(ctx, req, observer.ObserveSyncV1)
		}
		return errResp(req.ID, proto.CodeInternal, ErrDurableSyncObservationUnsupported.Error())
	case proto.MethodRemoteEnumerate:
		return call(ctx, req, handler.Enumerate)
	case proto.MethodRemoteSubscribe:
		// Subscribe returns plain error; wrap so the generic `call`
		// helper handles the empty Result by emitting null.
		return call(ctx, req, func(ctx context.Context, p proto.RemoteSubscribeParams) (struct{}, error) {
			return struct{}{}, handler.Subscribe(ctx, p)
		})
	case proto.MethodRemoteUnsubscribe:
		return call(ctx, req, func(ctx context.Context, p proto.RemoteUnsubscribeParams) (struct{}, error) {
			return struct{}{}, handler.Unsubscribe(ctx, p)
		})
	case proto.MethodRemoteStatus:
		return call(ctx, req, func(ctx context.Context, _ struct{}) (proto.RemoteStatusResult, error) {
			return handler.Status(ctx)
		})
	case proto.MethodRemoteListNamespaceDevices:
		return call(ctx, req, handler.ListNamespaceDevices)
	case proto.MethodRemotePutNamespaceKey:
		return call(ctx, req, handler.PutNamespaceKey)
	case proto.MethodRemoteGetNamespaceKey:
		return call(ctx, req, handler.GetNamespaceKey)
	case proto.MethodRemoteBroadcastNamespaceKey:
		return call(ctx, req, func(ctx context.Context, p proto.RemoteBroadcastNamespaceKeyParams) (struct{}, error) {
			return struct{}{}, handler.BroadcastNamespaceKey(ctx, p)
		})
	case proto.MethodRemoteGetNamespaceRole:
		return call(ctx, req, handler.GetNamespaceRole)
	case proto.MethodRemoteRegisterWrapKey:
		// RegisterWrapKey returns plain error; wrap so the generic `call`
		// helper handles the empty Result by emitting null.
		return call(ctx, req, func(ctx context.Context, p proto.RemoteRegisterWrapKeyParams) (struct{}, error) {
			return struct{}{}, handler.RegisterWrapKey(ctx, p)
		})
	case proto.MethodRemoteListAccountDevices:
		// Account-scoped: no params (the account is resolved server-side).
		return call(ctx, req, func(ctx context.Context, _ struct{}) (proto.RemoteListAccountDevicesResult, error) {
			return handler.ListAccountDevices(ctx)
		})
	case proto.MethodShutdown:
		r := call(ctx, req, handler.Shutdown)
		r.shutdown = true
		return r
	default:
		return errResp(req.ID, proto.CodeMethodNotFound, "method not found: "+req.Method)
	}
}

// serializingNotifier marshals notification frames to a shared writer
// under a mutex. Plugins are free to call its methods from any
// goroutine; the writer's framing stays well-formed.
type serializingNotifier struct {
	fw *proto.FrameWriter
	mu *sync.Mutex
}

func (n *serializingNotifier) writeFrame(method string, params any) error {
	body, err := json.Marshal(params)
	if err != nil {
		return err
	}
	// JSON-RPC notification: no ID field.
	notif := struct {
		JSONRPC string          `json:"jsonrpc"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params"`
	}{
		JSONRPC: "2.0",
		Method:  method,
		Params:  body,
	}
	frame, err := json.Marshal(notif)
	if err != nil {
		return err
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.fw.Write(frame)
}

// Inbound sends one or more events to the daemon. Plugin code typically
// calls this from a goroutine handling the underlying transport (MQTT
// subscription callback, BYO poll loop, etc.).
func (n *serializingNotifier) Inbound(events []proto.RemoteEvent) error {
	return n.writeFrame(proto.NotificationRemoteInbound, proto.RemoteInboundNotification{Events: events})
}

// ConnState announces a connectivity transition. Plugins emit on
// connect/disconnect; the host throttles by collapsing duplicates
// downstream.
func (n *serializingNotifier) ConnState(state string, human string) error {
	return n.writeFrame(proto.NotificationRemoteConnState, proto.RemoteConnStateNotification{
		ConnState:           state,
		HumanReadableStatus: human,
	})
}

// EnumerateHint nudges the daemon to re-enumerate. Plugins SHOULD
// throttle: at most one hint per 5 minutes per cause.
func (n *serializingNotifier) EnumerateHint(reason string) error {
	return n.writeFrame(proto.NotificationRemoteEnumerateHint, proto.RemoteEnumerateHintNotification{
		Reason: reason,
	})
}

// NamespaceKeyRotated forwards the control plane's key_rotated audit
// signal to the daemon.
func (n *serializingNotifier) NamespaceKeyRotated(notif proto.RemoteNamespaceKeyRotatedNotification) error {
	return n.writeFrame(proto.NotificationRemoteNamespaceKeyRotated, notif)
}

// NamespaceKeyBroadcast pushes freshly-wrapped key material to surviving
// devices.
func (n *serializingNotifier) NamespaceKeyBroadcast(notif proto.RemoteNamespaceKeyBroadcastNotification) error {
	return n.writeFrame(proto.NotificationRemoteNamespaceKeyBroadcast, notif)
}

// RulesUpdate pushes a cloud-originated selective-sync ruleset to the daemon
// (the daemon side consumes it in RemoteProxy.handleNotification ->
// OnRulesUpdate).
func (n *serializingNotifier) RulesUpdate(notif proto.RemoteRulesUpdateNotification) error {
	return n.writeFrame(proto.NotificationRemoteRulesUpdate, notif)
}

// CheckpointNeeded asks the daemon to create a client-produced encrypted
// checkpoint for an opaque artifact route.
func (n *serializingNotifier) CheckpointNeeded(notif proto.RemoteCheckpointNeededV1Notification) error {
	return n.writeFrame(proto.NotificationRemoteCheckpointNeededV1, notif)
}

// notifierAttachable is an optional interface a RemoteHandler MAY
// implement to receive the Notifier reference at startup. The host
// calls AttachNotifier once before the first dispatched method.
//
// Plugins that don't implement notifierAttachable simply can't push
// asynchronous events — they're publish/fetch/poll-only.
type notifierAttachable interface {
	AttachNotifier(Notifier)
}

func wrapWithNotifier(handler RemoteHandler, n Notifier) RemoteHandler {
	if att, ok := handler.(notifierAttachable); ok {
		att.AttachNotifier(n)
	}
	return handler
}
