package daemon

import (
	"context"
	"fmt"

	"github.com/aplexica/aplexica/internal/keyrotation"
	"github.com/aplexica/aplexica/internal/keys"
	"github.com/aplexica/aplexica/internal/plugin/proto"
)

// remoteKeyRotationCaller is the slice of the RemoteRunner that the
// key-rotation transport adapter needs. *RemoteRunner satisfies it; tests
// inject a fake.
type remoteKeyRotationCaller interface {
	ListNamespaceDevices(ctx context.Context, namespaceID string) (proto.RemoteListNamespaceDevicesResult, error)
	PutNamespaceKey(ctx context.Context, params proto.RemotePutNamespaceKeyParams) (proto.RemotePutNamespaceKeyResult, error)
	GetNamespaceKey(ctx context.Context, params proto.RemoteGetNamespaceKeyParams) (proto.RemoteGetNamespaceKeyResult, error)
	BroadcastNamespaceKey(ctx context.Context, params proto.RemoteBroadcastNamespaceKeyParams) error
}

// keyRotationTransport adapts the remote plugin's proto surface to the
// proto-free keyrotation.Transport interface, translating wire types
// (base64 byte slices) to/from the rotator's domain types (fixed-size key
// arrays).
type keyRotationTransport struct {
	caller remoteKeyRotationCaller
}

func newKeyRotationTransport(c remoteKeyRotationCaller) *keyRotationTransport {
	return &keyRotationTransport{caller: c}
}

// Compile-time assertion that the adapter satisfies the rotator's port.
var _ keyrotation.Transport = (*keyRotationTransport)(nil)

func (t *keyRotationTransport) ListNamespaceDevices(ctx context.Context, namespaceID string) ([]keyrotation.Device, error) {
	res, err := t.caller.ListNamespaceDevices(ctx, namespaceID)
	if err != nil {
		return nil, err
	}
	out := make([]keyrotation.Device, 0, len(res.Devices))
	for _, d := range res.Devices {
		if len(d.PubKey) != keys.X25519KeySize {
			return nil, fmt.Errorf("daemon: device %s has %d-byte pubkey, want %d", d.DeviceID, len(d.PubKey), keys.X25519KeySize)
		}
		var pub [keys.X25519KeySize]byte
		copy(pub[:], d.PubKey)
		out = append(out, keyrotation.Device{DeviceID: d.DeviceID, PubKey: pub})
	}
	return out, nil
}

func (t *keyRotationTransport) PutNamespaceKey(ctx context.Context, w keyrotation.NamespaceKeyWrite) error {
	res, err := t.caller.PutNamespaceKey(ctx, proto.RemotePutNamespaceKeyParams{
		NamespaceID: w.NamespaceID,
		KeyVersion:  w.KeyVersion,
		Wrapped:     toProtoWrapped(w.Wrapped),
	})
	if err != nil {
		return err
	}
	// A lost compare-and-swap is not an error to the transport contract —
	// translate it to the sentinel the rotator adopts on.
	if !res.Claimed {
		return keyrotation.ErrKeyAlreadyClaimed
	}
	return nil
}

func (t *keyRotationTransport) GetNamespaceKey(ctx context.Context, namespaceID string, version int) (keyrotation.NamespaceKeyState, error) {
	res, err := t.caller.GetNamespaceKey(ctx, proto.RemoteGetNamespaceKeyParams{
		NamespaceID: namespaceID,
		KeyVersion:  version,
	})
	if err != nil {
		return keyrotation.NamespaceKeyState{}, err
	}
	return keyrotation.NamespaceKeyState{
		Found:   res.Found,
		Wrapped: fromProtoWrapped(res.Wrapped),
	}, nil
}

func (t *keyRotationTransport) BroadcastNamespaceKey(ctx context.Context, b keyrotation.NamespaceKeyBroadcast) error {
	return t.caller.BroadcastNamespaceKey(ctx, proto.RemoteBroadcastNamespaceKeyParams{
		NamespaceID: b.NamespaceID,
		KeyVersion:  b.KeyVersion,
		Wrapped:     toProtoWrapped(b.Wrapped),
	})
}

func toProtoWrapped(in []keyrotation.WrappedKey) []proto.RemoteWrappedKey {
	out := make([]proto.RemoteWrappedKey, 0, len(in))
	for _, w := range in {
		out = append(out, proto.RemoteWrappedKey{DeviceID: w.DeviceID, Wrapped: w.Wrapped})
	}
	return out
}

func fromProtoWrapped(in []proto.RemoteWrappedKey) []keyrotation.WrappedKey {
	out := make([]keyrotation.WrappedKey, 0, len(in))
	for _, w := range in {
		out = append(out, keyrotation.WrappedKey{DeviceID: w.DeviceID, Wrapped: w.Wrapped})
	}
	return out
}

// keyRotationSignalFromProto converts the inbound rotated-signal
// notification to the rotator's domain Signal.
func keyRotationSignalFromProto(n proto.RemoteNamespaceKeyRotatedNotification) keyrotation.Signal {
	return keyrotation.Signal{
		NamespaceID:   n.NamespaceID,
		NewVersion:    n.NewVersion,
		RemovedUserID: n.RemovedUserID,
	}
}

// keyRotationBroadcastFromProto converts the inbound broadcast
// notification to the rotator's domain broadcast.
func keyRotationBroadcastFromProto(n proto.RemoteNamespaceKeyBroadcastNotification) keyrotation.NamespaceKeyBroadcast {
	wrapped := make([]keyrotation.WrappedKey, 0, len(n.Wrapped))
	for _, w := range n.Wrapped {
		wrapped = append(wrapped, keyrotation.WrappedKey{DeviceID: w.DeviceID, Wrapped: w.Wrapped})
	}
	return keyrotation.NamespaceKeyBroadcast{
		NamespaceID: n.NamespaceID,
		KeyVersion:  n.KeyVersion,
		Wrapped:     wrapped,
	}
}
