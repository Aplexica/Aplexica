package keyrotation_test

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/aplexica/aplexica/internal/keyrotation"
	"github.com/aplexica/aplexica/internal/keys"
)

// ---- fakes ----------------------------------------------------------------

type fakeTransport struct {
	mu        sync.Mutex
	devices   []keyrotation.Device
	listErr   error
	listCalls int

	putErr     error // set to ErrKeyAlreadyClaimed to simulate a lost CAS
	puts       []keyrotation.NamespaceKeyWrite
	broadcasts []keyrotation.NamespaceKeyBroadcast

	getState keyrotation.NamespaceKeyState
	getErr   error
	getCalls int
}

func (f *fakeTransport) ListNamespaceDevices(_ context.Context, _ string) ([]keyrotation.Device, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listCalls++
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.devices, nil
}

func (f *fakeTransport) PutNamespaceKey(_ context.Context, w keyrotation.NamespaceKeyWrite) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.putErr != nil {
		return f.putErr
	}
	f.puts = append(f.puts, w)
	return nil
}

func (f *fakeTransport) GetNamespaceKey(_ context.Context, _ string, _ int) (keyrotation.NamespaceKeyState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getCalls++
	return f.getState, f.getErr
}

func (f *fakeTransport) BroadcastNamespaceKey(_ context.Context, b keyrotation.NamespaceKeyBroadcast) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.broadcasts = append(f.broadcasts, b)
	return nil
}

type fakeContentKeys struct {
	mu   sync.Mutex
	keys map[string][]byte
}

func newFakeContentKeys() *fakeContentKeys { return &fakeContentKeys{keys: map[string][]byte{}} }

func ckKey(ns string, v int) string { return ns + "|" + string(rune('0'+v)) }

func (f *fakeContentKeys) GetContentKey(ns string, v int) ([]byte, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k, ok := f.keys[ckKey(ns, v)]
	return k, ok, nil
}

func (f *fakeContentKeys) PutContentKey(ns string, v int, key []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.keys[ckKey(ns, v)] = append([]byte(nil), key...)
	return nil
}

type fakeDeviceKeys struct {
	priv [32]byte
}

func (f fakeDeviceKeys) Private() ([32]byte, error) { return f.priv, nil }

// ---- helpers --------------------------------------------------------------

// device builds a Device with a fresh keypair and returns the private key
// alongside so the test can unwrap blobs aimed at it.
func device(t *testing.T, id string) (keyrotation.Device, [32]byte) {
	t.Helper()
	priv, pub, err := keys.NewDeviceKey()
	if err != nil {
		t.Fatalf("NewDeviceKey: %v", err)
	}
	return keyrotation.Device{DeviceID: id, PubKey: pub}, priv
}

// wrapFor builds a wrapped-key set: content key wrapped for each device.
func wrapFor(t *testing.T, ck []byte, devs ...keyrotation.Device) []keyrotation.WrappedKey {
	t.Helper()
	out := make([]keyrotation.WrappedKey, 0, len(devs))
	for _, d := range devs {
		blob, err := keys.WrapContentKey(ck, d.PubKey)
		if err != nil {
			t.Fatalf("WrapContentKey: %v", err)
		}
		out = append(out, keyrotation.WrappedKey{DeviceID: d.DeviceID, Wrapped: blob})
	}
	return out
}

func rotator(id keyrotation.Identity, tr keyrotation.Transport, cks keyrotation.ContentKeyStore, dk keyrotation.DeviceKeys) *keyrotation.Rotator {
	return &keyrotation.Rotator{Identity: id, Transport: tr, ContentKeys: cks, DeviceKeys: dk}
}

// ---- tests ----------------------------------------------------------------

func TestHandleSignal_RemovedUser_Skips(t *testing.T) {
	tr := &fakeTransport{}
	cks := newFakeContentKeys()
	r := rotator(keyrotation.Identity{DeviceID: "dev-a", UserID: "user-bob"}, tr, cks, fakeDeviceKeys{})

	err := r.HandleSignal(context.Background(), keyrotation.Signal{
		NamespaceID: "ns-1", NewVersion: 2, RemovedUserID: "user-bob",
	})
	if err != nil {
		t.Fatalf("HandleSignal: %v", err)
	}
	if tr.listCalls != 0 {
		t.Errorf("removed user must not even list devices; listCalls=%d", tr.listCalls)
	}
	if len(tr.puts) != 0 || len(tr.broadcasts) != 0 || len(cks.keys) != 0 {
		t.Errorf("removed user must not write, broadcast, or store")
	}
}

func TestHandleSignal_NonMemberDevice_Skips(t *testing.T) {
	d1, _ := device(t, "dev-x")
	d2, _ := device(t, "dev-y")
	tr := &fakeTransport{devices: []keyrotation.Device{d1, d2}}
	cks := newFakeContentKeys()
	r := rotator(keyrotation.Identity{DeviceID: "dev-not-listed"}, tr, cks, fakeDeviceKeys{})

	if err := r.HandleSignal(context.Background(), keyrotation.Signal{NamespaceID: "ns-1", NewVersion: 1}); err != nil {
		t.Fatalf("HandleSignal: %v", err)
	}
	if len(tr.puts) != 0 || len(tr.broadcasts) != 0 || len(cks.keys) != 0 {
		t.Errorf("a device not in the surviving list must do nothing")
	}
}

// Every surviving device attempts; the one whose conditional write lands
// generates the key, persists its own plaintext, and broadcasts.
func TestHandleSignal_Survivor_WinsCAS_GeneratesWrapsWritesBroadcasts(t *testing.T) {
	me, mePriv := device(t, "dev-bbb") // NOT the lowest id — election is gone
	peer, peerPriv := device(t, "dev-aaa")
	tr := &fakeTransport{devices: []keyrotation.Device{peer, me}}
	cks := newFakeContentKeys()
	r := rotator(keyrotation.Identity{DeviceID: "dev-bbb"}, tr, cks, fakeDeviceKeys{priv: mePriv})

	if err := r.HandleSignal(context.Background(), keyrotation.Signal{NamespaceID: "ns-9", NewVersion: 5}); err != nil {
		t.Fatalf("HandleSignal: %v", err)
	}

	stored, ok, _ := cks.GetContentKey("ns-9", 5)
	if !ok {
		t.Fatal("winner did not persist the content key locally")
	}
	if len(tr.puts) != 1 || tr.puts[0].KeyVersion != 5 {
		t.Fatalf("expected 1 write for v5, got %+v", tr.puts)
	}
	if len(tr.broadcasts) != 1 {
		t.Fatalf("expected 1 broadcast, got %d", len(tr.broadcasts))
	}
	if len(tr.puts[0].Wrapped) != 2 {
		t.Fatalf("expected a blob per device, got %d", len(tr.puts[0].Wrapped))
	}
	byDev := map[string][]byte{}
	for _, w := range tr.puts[0].Wrapped {
		byDev[w.DeviceID] = w.Wrapped
	}
	gotMe, err := keys.UnwrapContentKey(byDev["dev-bbb"], mePriv)
	if err != nil {
		t.Fatalf("own blob did not unwrap: %v", err)
	}
	gotPeer, err := keys.UnwrapContentKey(byDev["dev-aaa"], peerPriv)
	if err != nil {
		t.Fatalf("peer blob did not unwrap: %v", err)
	}
	if !bytes.Equal(gotMe, stored) || !bytes.Equal(gotPeer, stored) {
		t.Fatal("wrapped blobs must all decrypt to the stored content key")
	}
}

// A survivor whose conditional write loses the race adopts the winner's key
// (read back + unwrap own blob) instead of minting a conflicting one.
func TestHandleSignal_Survivor_LosesCAS_AdoptsWinnerKey(t *testing.T) {
	me, mePriv := device(t, "dev-me")
	peer, _ := device(t, "dev-peer")

	winnerKey, _ := keys.NewContentKey()
	tr := &fakeTransport{
		devices:  []keyrotation.Device{me, peer},
		putErr:   keyrotation.ErrKeyAlreadyClaimed, // we lose the CAS
		getState: keyrotation.NamespaceKeyState{Found: true, Wrapped: nil},
	}
	// The winner's wrapped set (includes a blob for us).
	tr.getState.Wrapped = wrapFor(t, winnerKey, me, peer)

	cks := newFakeContentKeys()
	r := rotator(keyrotation.Identity{DeviceID: "dev-me"}, tr, cks, fakeDeviceKeys{priv: mePriv})

	if err := r.HandleSignal(context.Background(), keyrotation.Signal{NamespaceID: "ns-1", NewVersion: 3}); err != nil {
		t.Fatalf("HandleSignal: %v", err)
	}

	got, ok, _ := cks.GetContentKey("ns-1", 3)
	if !ok {
		t.Fatal("loser did not adopt/persist the winner's key")
	}
	if !bytes.Equal(got, winnerKey) {
		t.Fatal("adopted key does not match the winner's content key")
	}
	if len(tr.puts) != 0 {
		t.Error("a CAS loser must not record a successful write")
	}
	if len(tr.broadcasts) != 0 {
		t.Error("a CAS loser must not broadcast (the winner already did)")
	}
	if tr.getCalls != 1 {
		t.Errorf("expected exactly one read-back to adopt, got %d", tr.getCalls)
	}
}

// CAS lost AND the winner's set has no blob for this device (e.g. the device
// roster shifted): no key stored, no error — await the broadcast/fetch.
func TestHandleSignal_LosesCAS_NoBlobForMe_AwaitsBroadcast(t *testing.T) {
	me, mePriv := device(t, "dev-me")
	peer, _ := device(t, "dev-peer")
	winnerKey, _ := keys.NewContentKey()

	tr := &fakeTransport{
		devices: []keyrotation.Device{me, peer},
		putErr:  keyrotation.ErrKeyAlreadyClaimed,
		getState: keyrotation.NamespaceKeyState{
			Found:   true,
			Wrapped: wrapFor(t, winnerKey, peer), // no blob for dev-me
		},
	}
	cks := newFakeContentKeys()
	r := rotator(keyrotation.Identity{DeviceID: "dev-me"}, tr, cks, fakeDeviceKeys{priv: mePriv})

	if err := r.HandleSignal(context.Background(), keyrotation.Signal{NamespaceID: "ns-1", NewVersion: 3}); err != nil {
		t.Fatalf("HandleSignal should not error when no blob targets us: %v", err)
	}
	if _, ok, _ := cks.GetContentKey("ns-1", 3); ok {
		t.Error("must not store a key when the winner's set has no blob for us")
	}
}

func TestHandleSignal_AlreadyHasKey_Idempotent(t *testing.T) {
	me, mePriv := device(t, "dev-me")
	tr := &fakeTransport{devices: []keyrotation.Device{me}}
	cks := newFakeContentKeys()
	existing, _ := keys.NewContentKey()
	_ = cks.PutContentKey("ns-1", 4, existing)

	r := rotator(keyrotation.Identity{DeviceID: "dev-me"}, tr, cks, fakeDeviceKeys{priv: mePriv})
	if err := r.HandleSignal(context.Background(), keyrotation.Signal{NamespaceID: "ns-1", NewVersion: 4}); err != nil {
		t.Fatalf("HandleSignal: %v", err)
	}
	if tr.listCalls != 0 || len(tr.puts) != 0 || len(tr.broadcasts) != 0 {
		t.Error("already holding the key must short-circuit before any transport call")
	}
	got, _, _ := cks.GetContentKey("ns-1", 4)
	if !bytes.Equal(got, existing) {
		t.Error("must not overwrite an already-held key")
	}
}

func TestHandleSignal_Redelivery_NoDoubleMint(t *testing.T) {
	me, mePriv := device(t, "dev-me")
	tr := &fakeTransport{devices: []keyrotation.Device{me}}
	cks := newFakeContentKeys()
	r := rotator(keyrotation.Identity{DeviceID: "dev-me"}, tr, cks, fakeDeviceKeys{priv: mePriv})

	sig := keyrotation.Signal{NamespaceID: "ns-9", NewVersion: 5}
	if err := r.HandleSignal(context.Background(), sig); err != nil {
		t.Fatalf("first: %v", err)
	}
	first, _, _ := cks.GetContentKey("ns-9", 5)
	if err := r.HandleSignal(context.Background(), sig); err != nil {
		t.Fatalf("second: %v", err)
	}
	second, _, _ := cks.GetContentKey("ns-9", 5)
	if !bytes.Equal(first, second) {
		t.Fatal("redelivery minted a new content key for the same version")
	}
	if len(tr.puts) != 1 {
		t.Errorf("redelivery should not write again; puts=%d", len(tr.puts))
	}
}

func TestHandleSignal_ListError_Propagates(t *testing.T) {
	tr := &fakeTransport{listErr: errors.New("boom")}
	r := rotator(keyrotation.Identity{DeviceID: "dev-a"}, tr, newFakeContentKeys(), fakeDeviceKeys{})
	if err := r.HandleSignal(context.Background(), keyrotation.Signal{NamespaceID: "ns-1", NewVersion: 1}); err == nil {
		t.Fatal("expected ListNamespaceDevices error to propagate")
	}
}

// ---- InstallBroadcast (unchanged fast path) -------------------------------

func TestInstallBroadcast_UnwrapsAndStores(t *testing.T) {
	me, mePriv := device(t, "dev-me")
	other, _ := device(t, "dev-other")
	cks := newFakeContentKeys()
	r := rotator(keyrotation.Identity{DeviceID: "dev-me"}, &fakeTransport{}, cks, fakeDeviceKeys{priv: mePriv})

	ck, _ := keys.NewContentKey()
	b := keyrotation.NamespaceKeyBroadcast{
		NamespaceID: "ns-1", KeyVersion: 7,
		Wrapped: wrapFor(t, ck, other, me),
	}
	if err := r.InstallBroadcast(context.Background(), b); err != nil {
		t.Fatalf("InstallBroadcast: %v", err)
	}
	got, ok, _ := cks.GetContentKey("ns-1", 7)
	if !ok || !bytes.Equal(got, ck) {
		t.Fatal("InstallBroadcast did not install the wrapped key")
	}
}

func TestInstallBroadcast_Idempotent(t *testing.T) {
	me, mePriv := device(t, "dev-me")
	cks := newFakeContentKeys()
	existing := bytes.Repeat([]byte{0xAB}, keys.ContentKeySize)
	_ = cks.PutContentKey("ns-1", 7, existing)
	r := rotator(keyrotation.Identity{DeviceID: "dev-me"}, &fakeTransport{}, cks, fakeDeviceKeys{priv: mePriv})

	ck, _ := keys.NewContentKey()
	b := keyrotation.NamespaceKeyBroadcast{NamespaceID: "ns-1", KeyVersion: 7, Wrapped: wrapFor(t, ck, me)}
	if err := r.InstallBroadcast(context.Background(), b); err != nil {
		t.Fatalf("InstallBroadcast: %v", err)
	}
	got, _, _ := cks.GetContentKey("ns-1", 7)
	if !bytes.Equal(got, existing) {
		t.Fatal("InstallBroadcast overwrote an already-installed version")
	}
}

func TestInstallBroadcast_NoBlobForMe_Skips(t *testing.T) {
	_, mePriv := device(t, "dev-me")
	other, _ := device(t, "dev-other")
	cks := newFakeContentKeys()
	r := rotator(keyrotation.Identity{DeviceID: "dev-me"}, &fakeTransport{}, cks, fakeDeviceKeys{priv: mePriv})

	ck, _ := keys.NewContentKey()
	b := keyrotation.NamespaceKeyBroadcast{NamespaceID: "ns-1", KeyVersion: 7, Wrapped: wrapFor(t, ck, other)}
	if err := r.InstallBroadcast(context.Background(), b); err != nil {
		t.Fatalf("InstallBroadcast: %v", err)
	}
	if _, ok, _ := cks.GetContentKey("ns-1", 7); ok {
		t.Fatal("stored a key even though none was for me")
	}
}
