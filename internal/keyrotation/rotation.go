// Package keyrotation implements daemon-side namespace content-key rotation
// after a member removal.
//
// The control plane never sees key material. When a member is removed it
// only bumps the namespace's key_version counter and emits a
// namespace.key_rotated audit signal. Surviving daemons receive
// that signal and do all the cryptographic work locally:
//
//  1. Confirm this device is a SURVIVING member (not the removed user's
//     device, and still present in the namespace's device list).
//  2. If we don't already hold the key for this version, ATTEMPT to be the
//     writer: generate a fresh content key, wrap it for every surviving
//     device's registered X25519 public key, and CONDITIONALLY write the
//     wrapped blobs back (compare-and-swap: the server only accepts the
//     write if WrappedForDevicePubkeyIDs is still empty for this version).
//  3. Exactly one attempt wins the CAS. The winner persists its own
//     plaintext copy and broadcasts the wrapped blobs to surviving devices.
//     Every loser ADOPTS the winner's key — reads back the wrapped set,
//     unwraps the blob addressed to its device, and persists that.
//  4. Online survivors also install the key from the inbound broadcast (a
//     fast path that short-circuits their own attempt).
//
// There is no leader election: every online survivor races, and the CAS
// makes concurrent writers safe (first-writer-wins, no clobber). Liveness
// therefore holds as long as ANY one survivor is online — no single device
// can stall the rotation by being offline.
//
// Forward-erasure: only new writes use the new key; old key
// versions are preserved so survivors can still read prior artifacts. The
// removed member, having lost membership, is excluded from the device list
// and therefore from the new key — it can no longer decrypt anything
// written after the rotation.
package keyrotation

import (
	"context"
	"errors"
	"fmt"

	"github.com/aplexica/aplexica/internal/keys"
)

// ErrKeyAlreadyClaimed is returned by Transport.PutNamespaceKey when the
// conditional write loses the compare-and-swap — another surviving device
// already populated the wrapped key material for this version. The rotator
// treats it as "adopt the winner's key", not as a failure.
var ErrKeyAlreadyClaimed = errors.New("keyrotation: namespace key version already claimed by another device")

// Signal is the daemon's view of a namespace.key_rotated audit event. The
// fields mirror the control plane's emitted payload (namespace_id,
// new_version, removed_user_id).
type Signal struct {
	NamespaceID   string
	NewVersion    int
	RemovedUserID string
}

// Device is one surviving member device and its registered wrap key.
type Device struct {
	DeviceID string
	PubKey   [keys.X25519KeySize]byte
}

// WrappedKey is a content key wrapped for a single device's public key.
type WrappedKey struct {
	DeviceID string
	Wrapped  []byte
}

// NamespaceKeyWrite is the durable write-back of wrapped key material to
// the namespace_keys row for a given version (populates the server's
// WrappedForDevicePubkeyIDs without ever exposing plaintext).
type NamespaceKeyWrite struct {
	NamespaceID string
	KeyVersion  int
	Wrapped     []WrappedKey
}

// NamespaceKeyBroadcast is the live push of wrapped key material to
// surviving devices so online members can decrypt new artifacts without
// waiting for a fetch. Carries one blob per device; each device picks the
// blob addressed to it.
type NamespaceKeyBroadcast struct {
	NamespaceID string
	KeyVersion  int
	Wrapped     []WrappedKey
}

// NamespaceKeyState is the wrapped key material already written for a
// version, returned by Transport.GetNamespaceKey so a CAS loser can adopt
// the winner's key. Found is false when no write has landed yet.
type NamespaceKeyState struct {
	Found   bool
	Wrapped []WrappedKey
}

// Transport is the narrow slice of the remote plugin the rotator needs.
// The real implementation calls the plugin over JSON-RPC; tests use a fake.
type Transport interface {
	// ListNamespaceDevices returns the namespace's surviving member
	// devices and their registered public keys. It is called AFTER the
	// server-side membership soft-delete, so the removed user's devices
	// are already excluded.
	ListNamespaceDevices(ctx context.Context, namespaceID string) ([]Device, error)
	// PutNamespaceKey conditionally writes the wrapped blobs for a key
	// version (compare-and-swap). It returns ErrKeyAlreadyClaimed when
	// another device already populated this version — the signal to adopt
	// rather than retry.
	PutNamespaceKey(ctx context.Context, w NamespaceKeyWrite) error
	// GetNamespaceKey reads back the wrapped material already written for a
	// version, so a CAS loser can adopt the winner's key.
	GetNamespaceKey(ctx context.Context, namespaceID string, version int) (NamespaceKeyState, error)
	// BroadcastNamespaceKey pushes the wrapped blobs to surviving devices.
	BroadcastNamespaceKey(ctx context.Context, b NamespaceKeyBroadcast) error
}

// ContentKeyStore persists this device's plaintext content keys locally
// (e.g. the secrets store). Never leaves the device.
type ContentKeyStore interface {
	GetContentKey(namespaceID string, version int) (key []byte, ok bool, err error)
	PutContentKey(namespaceID string, version int, key []byte) error
}

// DeviceKeys exposes this device's X25519 private key for the unwrap path.
type DeviceKeys interface {
	Private() ([keys.X25519KeySize]byte, error)
}

// Identity is this device's identity for the survivor check.
type Identity struct {
	DeviceID string
	UserID   string // optional; "" when the daemon doesn't know its own user id
}

// Logger is the optional structured logger; nil is tolerated.
type Logger interface {
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
}

// Rotator carries out client-side namespace key rotation.
type Rotator struct {
	Identity    Identity
	Transport   Transport
	ContentKeys ContentKeyStore
	DeviceKeys  DeviceKeys
	Logger      Logger
}

// HandleSignal processes one namespace.key_rotated signal. Safe to call on
// every surviving daemon concurrently: each races to write, the CAS makes
// concurrent writers safe, and it is idempotent under at-least-once signal
// redelivery.
func (r *Rotator) HandleSignal(ctx context.Context, sig Signal) error {
	if sig.NamespaceID == "" {
		return fmt.Errorf("keyrotation: signal missing namespace id")
	}

	// (1) If we positively know we ARE the removed user, do nothing.
	// Forward-erasure: the removed member must not participate.
	if r.Identity.UserID != "" && r.Identity.UserID == sig.RemovedUserID {
		r.info("ignoring key-rotation signal for our own removal", "namespace", sig.NamespaceID)
		return nil
	}

	// (2) Idempotency: if we already hold the key for this version (we won
	// earlier, adopted it, or installed a broadcast), there is nothing to do
	// — short-circuit before any transport call.
	if _, have, err := r.ContentKeys.GetContentKey(sig.NamespaceID, sig.NewVersion); err != nil {
		return fmt.Errorf("keyrotation: read local content key: %w", err)
	} else if have {
		return nil
	}

	devices, err := r.Transport.ListNamespaceDevices(ctx, sig.NamespaceID)
	if err != nil {
		return fmt.Errorf("keyrotation: list devices for %s: %w", sig.NamespaceID, err)
	}

	// (3) If our device is not among the survivors, we were the one removed
	// (or never a member). Nothing to do.
	if !containsDevice(devices, r.Identity.DeviceID) {
		r.info("not a surviving member; skipping rotation", "namespace", sig.NamespaceID)
		return nil
	}

	return r.attempt(ctx, sig, devices)
}

// attempt generates a candidate content key, wraps it for every surviving
// device, and tries the conditional write. The candidate is persisted only
// if the write WINS the CAS; a loser adopts the winner's key instead, so no
// device ever keeps a content key that didn't become authoritative.
func (r *Rotator) attempt(ctx context.Context, sig Signal, devices []Device) error {
	contentKey, err := keys.NewContentKey()
	if err != nil {
		return fmt.Errorf("keyrotation: generate content key: %w", err)
	}

	wrapped := make([]WrappedKey, 0, len(devices))
	for _, d := range devices {
		blob, werr := keys.WrapContentKey(contentKey, d.PubKey)
		if werr != nil {
			return fmt.Errorf("keyrotation: wrap for device %s: %w", d.DeviceID, werr)
		}
		wrapped = append(wrapped, WrappedKey{DeviceID: d.DeviceID, Wrapped: blob})
	}

	putErr := r.Transport.PutNamespaceKey(ctx, NamespaceKeyWrite{
		NamespaceID: sig.NamespaceID,
		KeyVersion:  sig.NewVersion,
		Wrapped:     wrapped,
	})
	switch {
	case putErr == nil:
		// We won the CAS: this content key is authoritative. Persist our
		// plaintext copy and broadcast the wrapped set to surviving devices.
		if err := r.ContentKeys.PutContentKey(sig.NamespaceID, sig.NewVersion, contentKey); err != nil {
			return fmt.Errorf("keyrotation: persist content key: %w", err)
		}
		if err := r.Transport.BroadcastNamespaceKey(ctx, NamespaceKeyBroadcast{
			NamespaceID: sig.NamespaceID,
			KeyVersion:  sig.NewVersion,
			Wrapped:     wrapped,
		}); err != nil {
			return fmt.Errorf("keyrotation: broadcast wrapped keys: %w", err)
		}
		r.info("rotated namespace content key",
			"namespace", sig.NamespaceID, "version", sig.NewVersion, "devices", len(devices))
		return nil
	case errors.Is(putErr, ErrKeyAlreadyClaimed):
		// A peer won the race; discard our candidate and adopt theirs.
		return r.adopt(ctx, sig)
	default:
		return fmt.Errorf("keyrotation: write back wrapped keys: %w", putErr)
	}
}

// adopt installs the winning device's key after we lose the CAS: read back
// the wrapped set, unwrap the blob addressed to this device, and persist it.
// A no-op (await the broadcast) when the winning set carries no blob for us.
func (r *Rotator) adopt(ctx context.Context, sig Signal) error {
	state, err := r.Transport.GetNamespaceKey(ctx, sig.NamespaceID, sig.NewVersion)
	if err != nil {
		return fmt.Errorf("keyrotation: read back claimed key: %w", err)
	}
	if !state.Found {
		// Claimed-but-not-yet-readable: a transient race. Surface it so the
		// caller can retry on the next signal/tick; the broadcast will also
		// install it.
		return fmt.Errorf("keyrotation: version %d claimed but not yet readable", sig.NewVersion)
	}
	return r.installWrapped(ctx, sig.NamespaceID, sig.NewVersion, state.Wrapped, "adopt")
}

// InstallBroadcast handles an inbound namespace key broadcast on a surviving
// device: find the blob addressed to this device, unwrap it, and persist the
// plaintext content key locally. Idempotent and a no-op when no blob targets
// this device. This is the fast path that lets an online survivor install
// the winner's key without running its own attempt.
func (r *Rotator) InstallBroadcast(ctx context.Context, b NamespaceKeyBroadcast) error {
	if b.NamespaceID == "" {
		return fmt.Errorf("keyrotation: broadcast missing namespace id")
	}
	return r.installWrapped(ctx, b.NamespaceID, b.KeyVersion, b.Wrapped, "broadcast")
}

// installWrapped is the shared install path for both adoption (CAS loser
// reading back the winner's set) and broadcast (live push): if we don't
// already hold the version, find the blob addressed to this device, unwrap
// it with our device key, and persist. Idempotent; a no-op when no blob
// targets this device (we await a later broadcast/fetch). source is a label
// for logging only.
func (r *Rotator) installWrapped(_ context.Context, namespaceID string, version int, wrapped []WrappedKey, source string) error {
	if _, have, err := r.ContentKeys.GetContentKey(namespaceID, version); err != nil {
		return fmt.Errorf("keyrotation: read local content key: %w", err)
	} else if have {
		return nil // already installed
	}

	var blob []byte
	for _, w := range wrapped {
		if w.DeviceID == r.Identity.DeviceID {
			blob = w.Wrapped
			break
		}
	}
	if blob == nil {
		r.info("wrapped key set carried no blob for this device; skipping",
			"namespace", namespaceID, "version", version, "source", source)
		return nil
	}

	priv, err := r.DeviceKeys.Private()
	if err != nil {
		return fmt.Errorf("keyrotation: load device private key: %w", err)
	}
	contentKey, err := keys.UnwrapContentKey(blob, priv)
	if err != nil {
		return fmt.Errorf("keyrotation: unwrap %s key: %w", source, err)
	}
	if err := r.ContentKeys.PutContentKey(namespaceID, version, contentKey); err != nil {
		return fmt.Errorf("keyrotation: persist installed key: %w", err)
	}
	r.info("installed rotated content key", "namespace", namespaceID, "version", version, "source", source)
	return nil
}

func containsDevice(devices []Device, id string) bool {
	for _, d := range devices {
		if d.DeviceID == id {
			return true
		}
	}
	return false
}

func (r *Rotator) info(msg string, args ...any) {
	if r.Logger != nil {
		r.Logger.Info(msg, args...)
	}
}
