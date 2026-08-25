package daemon

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/keys"
	"github.com/aplexica/aplexica/internal/plugin/proto"
)

// fakeDeviceLister is a stub accountDeviceLister capturing call count so we can
// assert caching.
type fakeDeviceLister struct {
	calls   atomic.Int64
	devices []proto.RemoteAccountDevice
	err     error
}

func (f *fakeDeviceLister) ListAccountDevices(_ context.Context) (proto.RemoteListAccountDevicesResult, error) {
	f.calls.Add(1)
	if f.err != nil {
		return proto.RemoteListAccountDevicesResult{}, f.err
	}
	return proto.RemoteListAccountDevicesResult{Devices: f.devices}, nil
}

// flakyDeviceLister fails its first `failFirst` calls with a reconnecting error,
// then returns devices. Models a cloud plugin that is momentarily reconnecting
// and then recovers — the exact condition that produced the self-only seal.
type flakyDeviceLister struct {
	calls     atomic.Int64
	failFirst int64
	devices   []proto.RemoteAccountDevice
}

func (f *flakyDeviceLister) ListAccountDevices(_ context.Context) (proto.RemoteListAccountDevicesResult, error) {
	n := f.calls.Add(1)
	if n <= f.failFirst {
		return proto.RemoteListAccountDevicesResult{}, errors.New("reconnecting")
	}
	return proto.RemoteListAccountDevicesResult{Devices: f.devices}, nil
}

func mkPub(t *testing.T) []byte {
	t.Helper()
	_, pub, err := keys.NewDeviceKey()
	if err != nil {
		t.Fatal(err)
	}
	return pub[:]
}

// TestRecipientResolver_IncludesPeersAndSelf verifies the resolver returns the
// account's devices PLUS always this device (so the sender decrypts its own
// re-imports), de-duplicated.
func TestRecipientResolver_IncludesPeersAndSelf(t *testing.T) {
	peerPub := mkPub(t)
	lister := &fakeDeviceLister{devices: []proto.RemoteAccountDevice{
		{DeviceID: "peer-1", PubKey: peerPub},
	}}
	_, selfPub, err := keys.NewDeviceKey()
	if err != nil {
		t.Fatal(err)
	}
	r := newRecipientResolver(
		context.Background(),
		lister,
		func() string { return "self-dev" },
		func() ([keys.X25519KeySize]byte, error) { return selfPub, nil },
		nil,
	)

	got, err := r.Recipients("ns-1")
	if err != nil {
		t.Fatalf("Recipients: %v", err)
	}
	ids := map[string]bool{}
	for _, g := range got {
		ids[g.DeviceID] = true
	}
	if !ids["peer-1"] || !ids["self-dev"] {
		t.Fatalf("expected peer-1 + self-dev, got %v", ids)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 recipients, got %d", len(got))
	}
}

// TestRecipientResolver_Caches verifies the device list is cached within the
// TTL (one underlying call for repeated Recipients in-window).
func TestRecipientResolver_Caches(t *testing.T) {
	lister := &fakeDeviceLister{devices: []proto.RemoteAccountDevice{{DeviceID: "peer-1", PubKey: mkPub(t)}}}
	_, selfPub, _ := keys.NewDeviceKey()
	r := newRecipientResolver(context.Background(), lister,
		func() string { return "self-dev" },
		func() ([keys.X25519KeySize]byte, error) { return selfPub, nil }, nil)
	r.ttl = time.Hour // force cache hit

	for i := 0; i < 5; i++ {
		if _, err := r.Recipients("ns-1"); err != nil {
			t.Fatalf("Recipients: %v", err)
		}
	}
	if c := lister.calls.Load(); c != 1 {
		t.Fatalf("expected 1 underlying ListAccountDevices call (cached), got %d", c)
	}
}

// A failed legacy device-list refresh must pause publication. Self-only
// fallback would permanently exclude peers while still draining the outbox.
func TestRecipientResolver_FailsClosedWhenListFails(t *testing.T) {
	lister := &fakeDeviceLister{err: errors.New("reconnecting")}
	_, selfPub, _ := keys.NewDeviceKey()
	r := newRecipientResolver(context.Background(), lister,
		func() string { return "self-dev" },
		func() ([keys.X25519KeySize]byte, error) { return selfPub, nil }, nil)

	if got, err := r.Recipients("ns-1"); err == nil || len(got) != 0 {
		t.Fatalf("refresh failure must return no recipients and an error, got=%v err=%v", got, err)
	}
}

// TestRecipientResolver_EmptyWhenNoSelfAndNoPeers verifies the resolver returns
// empty (caller drops) when there is no self device id and the list is empty —
// the zero-knowledge drop path.
func TestRecipientResolver_EmptyWhenNoSelfAndNoPeers(t *testing.T) {
	lister := &fakeDeviceLister{devices: nil}
	_, selfPub, _ := keys.NewDeviceKey()
	r := newRecipientResolver(context.Background(), lister,
		func() string { return "" }, // unpaired: no device id
		func() ([keys.X25519KeySize]byte, error) { return selfPub, nil }, nil)

	got, err := r.Recipients("ns-1")
	if err != nil {
		t.Fatalf("Recipients: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty recipient set (drop), got %v", got)
	}
}

// TestRecipientResolver_IsAccountScoped verifies the resolver ignores the
// namespace id entirely (the account-scoped list works for Personal tier, which
// never knows a namespace id): different namespace ids must yield the SAME set
// and share the cache (one underlying call).
func TestRecipientResolver_IsAccountScoped(t *testing.T) {
	lister := &fakeDeviceLister{devices: []proto.RemoteAccountDevice{
		{DeviceID: "peer-1", PubKey: mkPub(t)},
	}}
	_, selfPub, _ := keys.NewDeviceKey()
	r := newRecipientResolver(context.Background(), lister,
		func() string { return "self-dev" },
		func() ([keys.X25519KeySize]byte, error) { return selfPub, nil }, nil)
	r.ttl = time.Hour

	a, err := r.Recipients("") // empty namespace
	if err != nil {
		t.Fatalf("Recipients(empty): %v", err)
	}
	b, err := r.Recipients("ns-other") // different namespace
	if err != nil {
		t.Fatalf("Recipients(ns-other): %v", err)
	}
	if len(a) != 2 || len(b) != 2 {
		t.Fatalf("account-scoped set should be identical for any namespace: %d vs %d", len(a), len(b))
	}
	if c := lister.calls.Load(); c != 1 {
		t.Fatalf("account-scoped cache must be shared across namespaces (want 1 call, got %d)", c)
	}
	// The peer's 32-byte pubkey must map through intact.
	for _, g := range a {
		if g.DeviceID == "peer-1" && len(g.PubKey) != keys.X25519KeySize {
			t.Fatalf("peer pubkey length = %d, want %d", len(g.PubKey), keys.X25519KeySize)
		}
	}
}

// TestRecipientResolver_RecipientsAreCrossDeviceUsable proves the resolver's
// output is end-to-end usable: a content key wrapped to the PEER recipient the
// resolver returned can be UNWRAPPED with that peer's private key. This is the
// daemon-side guarantee that, once both devices register, device A encrypts to
// device B and B can decrypt.
func TestRecipientResolver_RecipientsAreCrossDeviceUsable(t *testing.T) {
	// Device B (the "peer") has a real keypair; its pubkey is what the account
	// device list returns.
	privB, pubB, err := keys.NewDeviceKey()
	if err != nil {
		t.Fatal(err)
	}
	lister := &fakeDeviceLister{devices: []proto.RemoteAccountDevice{
		{DeviceID: "device-B", PubKey: pubB[:]},
	}}
	// Device A is "self".
	_, selfPub, _ := keys.NewDeviceKey()
	r := newRecipientResolver(context.Background(), lister,
		func() string { return "device-A" },
		func() ([keys.X25519KeySize]byte, error) { return selfPub, nil }, nil)

	recips, err := r.Recipients("")
	if err != nil {
		t.Fatalf("Recipients: %v", err)
	}
	// Find device-B's recipient pubkey as the resolver mapped it.
	var bPub [keys.X25519KeySize]byte
	foundB := false
	for _, rec := range recips {
		if rec.DeviceID == "device-B" {
			bPub = rec.PubKey
			foundB = true
		}
	}
	if !foundB {
		t.Fatalf("resolver did not include device-B; got %+v", recips)
	}

	// Wrap a fresh content key to device-B's resolver-provided pubkey, then
	// unwrap with device-B's actual private key — the exact path the
	// orchestrator + receiving daemon take.
	ck, err := keys.NewContentKey()
	if err != nil {
		t.Fatal(err)
	}
	wrapped, err := keys.WrapContentKey(ck, bPub)
	if err != nil {
		t.Fatalf("wrap: %v", err)
	}
	got, err := keys.UnwrapContentKey(wrapped, privB)
	if err != nil {
		t.Fatalf("device-B could not unwrap a key sealed to the resolver-provided pubkey: %v", err)
	}
	if len(got) != keys.ContentKeySize {
		t.Fatalf("unwrapped key length = %d", len(got))
	}
	for i := range ck {
		if got[i] != ck[i] {
			t.Fatal("unwrapped content key mismatch")
		}
	}
}

// TestRecipientResolver_DoesNotCacheListFailure is the regression test for the
// cross-device sync outage: a transient ListAccountDevices failure (plugin
// reconnecting) must NOT be cached. The resolver fails closed for THAT call,
// but the very next call must re-fetch and pick up the now-available
// peer. The bug was that Recipients() cached the self-only fallback for the
// full TTL, so every event published in that window was sealed for THIS device
// only — peers received ciphertext they could not decrypt and dropped it, and
// because a self-only publish "succeeds" (drains the outbox) the events were
// never re-sent. ttl is set huge on purpose: a FAILED fetch must not be cached
// regardless of TTL.
func TestRecipientResolver_DoesNotCacheListFailure(t *testing.T) {
	peerPub := mkPub(t)
	lister := &flakyDeviceLister{
		failFirst: 1,
		devices:   []proto.RemoteAccountDevice{{DeviceID: "peer-1", PubKey: peerPub}},
	}
	_, selfPub, _ := keys.NewDeviceKey()
	r := newRecipientResolver(context.Background(), lister,
		func() string { return "self-dev" },
		func() ([keys.X25519KeySize]byte, error) { return selfPub, nil }, nil)
	r.ttl = time.Hour // prove it is the failure (not TTL expiry) that forces the re-fetch

	// First call: list fails closed and must NOT be cached.
	if got1, err := r.Recipients("ns-1"); err == nil || len(got1) != 0 {
		t.Fatalf("first call must fail closed, got=%v err=%v", got1, err)
	}

	// Second call: list now succeeds -> MUST include the peer (no poisoned cache).
	got2, err := r.Recipients("ns-1")
	if err != nil {
		t.Fatalf("second Recipients: %v", err)
	}
	ids := map[string]bool{}
	for _, g := range got2 {
		ids[g.DeviceID] = true
	}
	if !ids["peer-1"] {
		t.Fatalf("second call must re-fetch and include peer-1 (a failed list must NOT be cached), got %v", ids)
	}
	if !ids["self-dev"] {
		t.Fatalf("second call must still include self, got %v", ids)
	}

	// A subsequent (third) call should now serve from cache: the successful
	// result IS cacheable, so no further underlying list call.
	before := lister.calls.Load()
	if _, err := r.Recipients("ns-1"); err != nil {
		t.Fatalf("third Recipients: %v", err)
	}
	if after := lister.calls.Load(); after != before {
		t.Fatalf("a SUCCESSFUL resolution must be cached (want no extra list call, got %d->%d)", before, after)
	}
}

func TestRecipientResolver_DoesNotUseStaleRosterWhenRefreshFails(t *testing.T) {
	peerPub := mkPub(t)
	lister := &fakeDeviceLister{devices: []proto.RemoteAccountDevice{{DeviceID: "peer-1", PubKey: peerPub}}}
	_, selfPub, _ := keys.NewDeviceKey()
	r := newRecipientResolver(context.Background(), lister,
		func() string { return "self-dev" },
		func() ([keys.X25519KeySize]byte, error) { return selfPub, nil }, nil)
	r.ttl = time.Nanosecond

	got1, err := r.Recipients("ns-1")
	if err != nil {
		t.Fatalf("first Recipients: %v", err)
	}
	if len(got1) != 2 {
		t.Fatalf("first resolution should include self + peer, got %v", got1)
	}

	time.Sleep(time.Millisecond)
	lister.err = errors.New("plugin busy")
	if got2, err := r.Recipients("ns-1"); err == nil || len(got2) != 0 {
		t.Fatalf("refresh failure must not reuse stale roster, got=%v err=%v", got2, err)
	}
}
