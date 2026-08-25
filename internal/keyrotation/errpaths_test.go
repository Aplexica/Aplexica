package keyrotation_test

// Error-branch coverage for the cryptographic / rotation control paths in
// internal/keyrotation, per NFR-09.6. These complement rotation_test.go and
// contentkeystore_test.go by driving the error arms the happy-path tests skip:
// empty-namespace guards, store read/write failures, the CAS-loser adopt
// failure modes, the unwrap-failure path on install, and the optional logger
// branch.
//
// The fakes here are local to this file so the existing rotation_test.go fakes
// (which deliberately never error) stay simple.

import (
	"context"
	"errors"
	"testing"

	"github.com/aplexica/aplexica/internal/keyrotation"
	"github.com/aplexica/aplexica/internal/keys"
)

var errBoom = errors.New("boom")

// errContentKeys is a ContentKeyStore whose Get/Put can be made to fail, to
// drive the local-store error arms of HandleSignal / attempt / installWrapped.
type errContentKeys struct {
	getErr error
	putErr error
	stored map[string][]byte
}

func newErrContentKeys() *errContentKeys { return &errContentKeys{stored: map[string][]byte{}} }

func eckKey(ns string, v int) string { return ns + "|" + string(rune('0'+v)) }

func (f *errContentKeys) GetContentKey(ns string, v int) ([]byte, bool, error) {
	if f.getErr != nil {
		return nil, false, f.getErr
	}
	k, ok := f.stored[eckKey(ns, v)]
	return k, ok, nil
}

func (f *errContentKeys) PutContentKey(ns string, v int, key []byte) error {
	if f.putErr != nil {
		return f.putErr
	}
	f.stored[eckKey(ns, v)] = append([]byte(nil), key...)
	return nil
}

// errDeviceKeys is a DeviceKeys whose Private() fails, to drive the
// load-device-private-key error arm of installWrapped.
type errDeviceKeys struct{ err error }

func (f errDeviceKeys) Private() ([keys.X25519KeySize]byte, error) {
	var z [keys.X25519KeySize]byte
	return z, f.err
}

// okDeviceKeys returns a fixed real private key.
type okDeviceKeys struct{ priv [keys.X25519KeySize]byte }

func (f okDeviceKeys) Private() ([keys.X25519KeySize]byte, error) { return f.priv, nil }

// fullTransport is a configurable Transport for the rotation error paths.
type fullTransport struct {
	devices  []keyrotation.Device
	listErr  error
	putErr   error
	getState keyrotation.NamespaceKeyState
	getErr   error
	bcastErr error

	puts       []keyrotation.NamespaceKeyWrite
	broadcasts []keyrotation.NamespaceKeyBroadcast
}

func (f *fullTransport) ListNamespaceDevices(_ context.Context, _ string) ([]keyrotation.Device, error) {
	return f.devices, f.listErr
}

func (f *fullTransport) PutNamespaceKey(_ context.Context, w keyrotation.NamespaceKeyWrite) error {
	if f.putErr != nil {
		return f.putErr
	}
	f.puts = append(f.puts, w)
	return nil
}

func (f *fullTransport) GetNamespaceKey(_ context.Context, _ string, _ int) (keyrotation.NamespaceKeyState, error) {
	return f.getState, f.getErr
}

func (f *fullTransport) BroadcastNamespaceKey(_ context.Context, b keyrotation.NamespaceKeyBroadcast) error {
	if f.bcastErr != nil {
		return f.bcastErr
	}
	f.broadcasts = append(f.broadcasts, b)
	return nil
}

// recordingLogger captures Info calls so the optional-logger branch in
// Rotator.info is exercised (the nil-logger arm is covered by every other
// test, which leaves Logger unset).
type recordingLogger struct{ infos int }

func (l *recordingLogger) Info(string, ...any) { l.infos++ }
func (l *recordingLogger) Warn(string, ...any) {}

// --- HandleSignal guard branches -------------------------------------------

func TestHandleSignal_EmptyNamespace_Errors(t *testing.T) {
	r := &keyrotation.Rotator{Identity: keyrotation.Identity{DeviceID: "dev-a"}}
	if err := r.HandleSignal(context.Background(), keyrotation.Signal{NamespaceID: "", NewVersion: 1}); err == nil {
		t.Fatal("expected error for a signal with an empty namespace id")
	}
}

// GetContentKey failing in the idempotency probe must propagate (rotation.go
// ~line 170), before any transport call.
func TestHandleSignal_LocalReadError_Propagates(t *testing.T) {
	cks := newErrContentKeys()
	cks.getErr = errBoom
	tr := &fullTransport{}
	r := &keyrotation.Rotator{
		Identity:    keyrotation.Identity{DeviceID: "dev-a"},
		Transport:   tr,
		ContentKeys: cks,
		DeviceKeys:  okDeviceKeys{},
	}
	if err := r.HandleSignal(context.Background(), keyrotation.Signal{NamespaceID: "ns-1", NewVersion: 1}); err == nil {
		t.Fatal("expected local content-key read error to propagate")
	}
	if len(tr.puts) != 0 {
		t.Error("must not write when the idempotency read failed")
	}
}

// --- attempt() error branches ----------------------------------------------

// A CAS win followed by a failed local persist must surface the persist error
// (rotation.go ~line 219).
func TestHandleSignal_WinCAS_PersistFails(t *testing.T) {
	me, mePriv := device(t, "dev-me")
	tr := &fullTransport{devices: []keyrotation.Device{me}}
	cks := newErrContentKeys()
	cks.putErr = errBoom
	r := &keyrotation.Rotator{
		Identity:    keyrotation.Identity{DeviceID: "dev-me"},
		Transport:   tr,
		ContentKeys: cks,
		DeviceKeys:  okDeviceKeys{priv: mePriv},
	}
	if err := r.HandleSignal(context.Background(), keyrotation.Signal{NamespaceID: "ns-1", NewVersion: 2}); err == nil {
		t.Fatal("expected persist error after winning the CAS")
	}
}

// A CAS win whose broadcast fails must surface the broadcast error
// (rotation.go ~line 226).
func TestHandleSignal_WinCAS_BroadcastFails(t *testing.T) {
	me, mePriv := device(t, "dev-me")
	tr := &fullTransport{devices: []keyrotation.Device{me}, bcastErr: errBoom}
	cks := newErrContentKeys()
	r := &keyrotation.Rotator{
		Identity:    keyrotation.Identity{DeviceID: "dev-me"},
		Transport:   tr,
		ContentKeys: cks,
		DeviceKeys:  okDeviceKeys{priv: mePriv},
	}
	if err := r.HandleSignal(context.Background(), keyrotation.Signal{NamespaceID: "ns-1", NewVersion: 2}); err == nil {
		t.Fatal("expected broadcast error after winning the CAS")
	}
}

// A PutNamespaceKey failure that is NOT ErrKeyAlreadyClaimed must surface as a
// write-back error (rotation.go default arm, ~line 235), not trigger adoption.
func TestHandleSignal_PutNonClaimError_Propagates(t *testing.T) {
	me, mePriv := device(t, "dev-me")
	tr := &fullTransport{devices: []keyrotation.Device{me}, putErr: errBoom}
	cks := newErrContentKeys()
	r := &keyrotation.Rotator{
		Identity:    keyrotation.Identity{DeviceID: "dev-me"},
		Transport:   tr,
		ContentKeys: cks,
		DeviceKeys:  okDeviceKeys{priv: mePriv},
	}
	if err := r.HandleSignal(context.Background(), keyrotation.Signal{NamespaceID: "ns-1", NewVersion: 2}); err == nil {
		t.Fatal("expected non-claim write-back error to propagate")
	}
}

// --- adopt() error branches ------------------------------------------------

// Losing the CAS and then failing the read-back must surface that error
// (rotation.go ~line 245).
func TestHandleSignal_LoseCAS_ReadbackFails(t *testing.T) {
	me, mePriv := device(t, "dev-me")
	tr := &fullTransport{
		devices: []keyrotation.Device{me},
		putErr:  keyrotation.ErrKeyAlreadyClaimed,
		getErr:  errBoom,
	}
	cks := newErrContentKeys()
	r := &keyrotation.Rotator{
		Identity:    keyrotation.Identity{DeviceID: "dev-me"},
		Transport:   tr,
		ContentKeys: cks,
		DeviceKeys:  okDeviceKeys{priv: mePriv},
	}
	if err := r.HandleSignal(context.Background(), keyrotation.Signal{NamespaceID: "ns-1", NewVersion: 2}); err == nil {
		t.Fatal("expected read-back error after losing the CAS")
	}
}

// Losing the CAS where the read-back reports the version not-yet-readable must
// surface the transient error (rotation.go ~line 248).
func TestHandleSignal_LoseCAS_ClaimedButNotReadable(t *testing.T) {
	me, mePriv := device(t, "dev-me")
	tr := &fullTransport{
		devices:  []keyrotation.Device{me},
		putErr:   keyrotation.ErrKeyAlreadyClaimed,
		getState: keyrotation.NamespaceKeyState{Found: false},
	}
	cks := newErrContentKeys()
	r := &keyrotation.Rotator{
		Identity:    keyrotation.Identity{DeviceID: "dev-me"},
		Transport:   tr,
		ContentKeys: cks,
		DeviceKeys:  okDeviceKeys{priv: mePriv},
	}
	if err := r.HandleSignal(context.Background(), keyrotation.Signal{NamespaceID: "ns-1", NewVersion: 2}); err == nil {
		t.Fatal("expected 'claimed but not yet readable' error")
	}
}

// --- installWrapped error branches -----------------------------------------

// InstallBroadcast with an empty namespace id must error (rotation.go ~line
// 263).
func TestInstallBroadcast_EmptyNamespace_Errors(t *testing.T) {
	r := &keyrotation.Rotator{Identity: keyrotation.Identity{DeviceID: "dev-me"}}
	if err := r.InstallBroadcast(context.Background(), keyrotation.NamespaceKeyBroadcast{NamespaceID: ""}); err == nil {
		t.Fatal("expected error for a broadcast with an empty namespace id")
	}
}

// A local read failure inside installWrapped must propagate (rotation.go ~line
// 276).
func TestInstallBroadcast_LocalReadError_Propagates(t *testing.T) {
	me, mePriv := device(t, "dev-me")
	cks := newErrContentKeys()
	cks.getErr = errBoom
	r := &keyrotation.Rotator{
		Identity:    keyrotation.Identity{DeviceID: "dev-me"},
		Transport:   &fullTransport{},
		ContentKeys: cks,
		DeviceKeys:  okDeviceKeys{priv: mePriv},
	}
	b := keyrotation.NamespaceKeyBroadcast{NamespaceID: "ns-1", KeyVersion: 7, Wrapped: wrapFor(t, mustKey(t), me)}
	if err := r.InstallBroadcast(context.Background(), b); err == nil {
		t.Fatal("expected local read error to propagate from installWrapped")
	}
}

// A DeviceKeys.Private() failure (with a blob addressed to us) must propagate
// (rotation.go ~line 296).
func TestInstallBroadcast_PrivateKeyError_Propagates(t *testing.T) {
	me, _ := device(t, "dev-me")
	cks := newErrContentKeys()
	r := &keyrotation.Rotator{
		Identity:    keyrotation.Identity{DeviceID: "dev-me"},
		Transport:   &fullTransport{},
		ContentKeys: cks,
		DeviceKeys:  errDeviceKeys{err: errBoom},
	}
	b := keyrotation.NamespaceKeyBroadcast{NamespaceID: "ns-1", KeyVersion: 7, Wrapped: wrapFor(t, mustKey(t), me)}
	if err := r.InstallBroadcast(context.Background(), b); err == nil {
		t.Fatal("expected device-private-key error to propagate")
	}
}

// A blob addressed to us that is corrupt must surface an unwrap error
// (rotation.go ~line 300). We use the right device id but a private key that
// does not match the pub the blob was wrapped for.
func TestInstallBroadcast_UnwrapFails(t *testing.T) {
	me, _ := device(t, "dev-me")
	wrongPriv, _, err := keys.NewDeviceKey()
	if err != nil {
		t.Fatalf("NewDeviceKey: %v", err)
	}
	cks := newErrContentKeys()
	r := &keyrotation.Rotator{
		Identity:    keyrotation.Identity{DeviceID: "dev-me"},
		Transport:   &fullTransport{},
		ContentKeys: cks,
		DeviceKeys:  okDeviceKeys{priv: wrongPriv}, // can't unwrap the blob wrapped for me's pub
	}
	b := keyrotation.NamespaceKeyBroadcast{NamespaceID: "ns-1", KeyVersion: 7, Wrapped: wrapFor(t, mustKey(t), me)}
	if err := r.InstallBroadcast(context.Background(), b); err == nil {
		t.Fatal("expected unwrap error for a blob this device cannot decrypt")
	}
}

// A successful unwrap whose local persist fails must surface that error
// (rotation.go ~line 303).
func TestInstallBroadcast_PersistFails(t *testing.T) {
	me, mePriv := device(t, "dev-me")
	cks := newErrContentKeys()
	cks.putErr = errBoom
	r := &keyrotation.Rotator{
		Identity:    keyrotation.Identity{DeviceID: "dev-me"},
		Transport:   &fullTransport{},
		ContentKeys: cks,
		DeviceKeys:  okDeviceKeys{priv: mePriv},
	}
	b := keyrotation.NamespaceKeyBroadcast{NamespaceID: "ns-1", KeyVersion: 7, Wrapped: wrapFor(t, mustKey(t), me)}
	if err := r.InstallBroadcast(context.Background(), b); err == nil {
		t.Fatal("expected persist error after a successful unwrap")
	}
}

// --- Rotator.info logger branch --------------------------------------------

// With a non-nil Logger set, an info-logging code path must actually call into
// the logger (rotation.go info, ~line 320 — the Logger != nil arm). The
// "removed user" early-return logs exactly once and touches nothing else.
func TestRotator_Info_UsesLoggerWhenSet(t *testing.T) {
	lg := &recordingLogger{}
	r := &keyrotation.Rotator{
		Identity: keyrotation.Identity{DeviceID: "dev-me", UserID: "user-bob"},
		Logger:   lg,
	}
	if err := r.HandleSignal(context.Background(), keyrotation.Signal{
		NamespaceID: "ns-1", NewVersion: 1, RemovedUserID: "user-bob",
	}); err != nil {
		t.Fatalf("HandleSignal: %v", err)
	}
	if lg.infos == 0 {
		t.Fatal("expected the configured logger to receive at least one Info call")
	}
}

// mustKey returns a fresh content key or fails the test.
func mustKey(t *testing.T) []byte {
	t.Helper()
	ck, err := keys.NewContentKey()
	if err != nil {
		t.Fatalf("NewContentKey: %v", err)
	}
	return ck
}

// --- attempt() wrap-failure branch -----------------------------------------

// A surviving device whose registered public key is the all-zero (low-order)
// point makes keys.WrapContentKey error inside attempt, which must surface as a
// wrap error (rotation.go ~line 204). This models a corrupt/hostile pubkey
// registration rather than a rand failure (which is unreachable).
func TestHandleSignal_WrapFailsForBadDevicePub(t *testing.T) {
	var zeroPub [keys.X25519KeySize]byte // low-order point: WrapContentKey rejects it
	me := keyrotation.Device{DeviceID: "dev-me", PubKey: zeroPub}
	tr := &fullTransport{devices: []keyrotation.Device{me}}
	cks := newErrContentKeys()
	r := &keyrotation.Rotator{
		Identity:    keyrotation.Identity{DeviceID: "dev-me"},
		Transport:   tr,
		ContentKeys: cks,
		DeviceKeys:  okDeviceKeys{},
	}
	if err := r.HandleSignal(context.Background(), keyrotation.Signal{NamespaceID: "ns-1", NewVersion: 2}); err == nil {
		t.Fatal("expected a wrap error when a surviving device has an all-zero pubkey")
	}
	if len(tr.puts) != 0 {
		t.Error("must not write back when wrapping failed")
	}
}

// --- SecretsContentKeyStore error branches ---------------------------------

// failBackend is a secrets backend (Get/Put) whose Get can fail with an
// arbitrary (non-os.ErrNotExist) error, return a chosen raw value, or whose
// Put can fail — driving the store's read/decode/persist error arms.
type failBackend struct {
	getVal string
	getErr error
	putErr error
}

func (f *failBackend) Get(artifactID, key string) (string, error) { return f.getVal, f.getErr }
func (f *failBackend) Put(artifactID, key, value string) error    { return f.putErr }

// A backend Get error that is not os.ErrNotExist must surface as a read error
// (contentkeystore.go ~line 45), not as ok=false.
func TestSecretsContentKeyStore_GetBackendError_Propagates(t *testing.T) {
	st := keyrotation.NewSecretsContentKeyStore(&failBackend{getErr: errBoom})
	if _, ok, err := st.GetContentKey("ns-1", 1); err == nil {
		t.Fatalf("expected backend Get error to propagate, got ok=%v", ok)
	}
}

// A stored value that is not valid base64 must surface as a decode error
// (contentkeystore.go ~line 48).
func TestSecretsContentKeyStore_UndecodableValue_Errors(t *testing.T) {
	st := keyrotation.NewSecretsContentKeyStore(&failBackend{getVal: "!!!not base64!!!"})
	if _, _, err := st.GetContentKey("ns-1", 1); err == nil {
		t.Fatal("expected a base64 decode error for a corrupt stored content key")
	}
}

// A backend Put failure must surface as a persist error (contentkeystore.go
// ~line 64).
func TestSecretsContentKeyStore_PutBackendError_Propagates(t *testing.T) {
	st := keyrotation.NewSecretsContentKeyStore(&failBackend{putErr: errBoom})
	if err := st.PutContentKey("ns-1", 1, mustKey(t)); err == nil {
		t.Fatal("expected backend Put error to propagate")
	}
}
