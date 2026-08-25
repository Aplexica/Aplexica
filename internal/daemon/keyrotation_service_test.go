package daemon

import (
	"bytes"
	"context"
	"testing"

	"github.com/aplexica/aplexica/internal/keyrotation"
	"github.com/aplexica/aplexica/internal/keys"
	"github.com/aplexica/aplexica/internal/plugin/proto"
	"github.com/aplexica/aplexica/internal/secrets"
)

func staticIdentity(id keyrotation.Identity) func() keyrotation.Identity {
	return func() keyrotation.Identity { return id }
}

func tempSecrets(t *testing.T) *secrets.Store {
	t.Helper()
	s := &secrets.Store{Root: t.TempDir()}
	if err := s.Init(); err != nil {
		t.Fatalf("secrets init: %v", err)
	}
	return s
}

// Full daemon-side leader flow: a proto rotated-signal arrives, this device
// is the elected leader, and the service generates + wraps + writes back +
// broadcasts, persisting its own plaintext locally — all through the
// proto/domain boundary.
func TestKeyRotationService_LeaderFlowEndToEnd(t *testing.T) {
	store := tempSecrets(t)

	// This device's keypair (its pubkey is what the plugin would report).
	myPriv, myPub, err := keys.NewDeviceKeyStore(store).LoadOrCreate()
	if err != nil {
		t.Fatalf("device key: %v", err)
	}
	_, peerPub, _ := keys.NewDeviceKey()

	caller := &fakeKRCaller{devices: proto.RemoteListNamespaceDevicesResult{
		Devices: []proto.RemoteDevice{
			{DeviceID: "dev-aaa", PubKey: myPub[:]}, // lowest id => leader => us
			{DeviceID: "dev-bbb", PubKey: peerPub[:]},
		},
	}}

	svc := NewKeyRotationService(context.Background(), caller, store,
		staticIdentity(keyrotation.Identity{DeviceID: "dev-aaa", UserID: "user-me"}), nil)

	svc.HandleSignal(proto.RemoteNamespaceKeyRotatedNotification{
		NamespaceID: "ns-1", NewVersion: 2, RemovedUserID: "user-bob",
	})

	// Local plaintext content key persisted.
	stored, ok, err := keyrotation.NewSecretsContentKeyStore(store).GetContentKey("ns-1", 2)
	if err != nil || !ok {
		t.Fatalf("content key not persisted: ok=%v err=%v", ok, err)
	}

	if len(caller.puts) != 1 {
		t.Fatalf("expected 1 write-back, got %d", len(caller.puts))
	}
	if len(caller.broadcasts) != 1 {
		t.Fatalf("expected 1 broadcast, got %d", len(caller.broadcasts))
	}

	// Our own wrapped blob unwraps to the stored content key.
	var myBlob []byte
	for _, w := range caller.puts[0].Wrapped {
		if w.DeviceID == "dev-aaa" {
			myBlob = w.Wrapped
		}
	}
	if myBlob == nil {
		t.Fatal("no blob wrapped for our own device")
	}
	got, err := keys.UnwrapContentKey(myBlob, myPriv)
	if err != nil {
		t.Fatalf("unwrap own blob: %v", err)
	}
	if !bytes.Equal(got, stored) {
		t.Fatal("wrapped blob does not match the stored content key")
	}
}

// A removed user's own daemon must not rotate (forward-erasure).
func TestKeyRotationService_RemovedUserDoesNothing(t *testing.T) {
	store := tempSecrets(t)
	caller := &fakeKRCaller{}
	svc := NewKeyRotationService(context.Background(), caller, store,
		staticIdentity(keyrotation.Identity{DeviceID: "dev-x", UserID: "user-bob"}), nil)

	svc.HandleSignal(proto.RemoteNamespaceKeyRotatedNotification{
		NamespaceID: "ns-1", NewVersion: 2, RemovedUserID: "user-bob",
	})
	if len(caller.puts) != 0 || len(caller.broadcasts) != 0 {
		t.Error("removed user's daemon must not write or broadcast")
	}
}

// Full broadcast-install path: a surviving non-leader receives the wrapped
// key and installs it by unwrapping with its device key.
func TestKeyRotationService_InstallBroadcastEndToEnd(t *testing.T) {
	store := tempSecrets(t)
	_, myPub, err := keys.NewDeviceKeyStore(store).LoadOrCreate()
	if err != nil {
		t.Fatalf("device key: %v", err)
	}

	ck, _ := keys.NewContentKey()
	wrappedForMe, _ := keys.WrapContentKey(ck, myPub)

	svc := NewKeyRotationService(context.Background(), &fakeKRCaller{}, store,
		staticIdentity(keyrotation.Identity{DeviceID: "dev-me"}), nil)

	svc.HandleBroadcast(proto.RemoteNamespaceKeyBroadcastNotification{
		NamespaceID: "ns-1", KeyVersion: 5,
		Wrapped: []proto.RemoteWrappedKey{{DeviceID: "dev-me", Wrapped: wrappedForMe}},
	})

	got, ok, err := keyrotation.NewSecretsContentKeyStore(store).GetContentKey("ns-1", 5)
	if err != nil || !ok {
		t.Fatalf("broadcast key not installed: ok=%v err=%v", ok, err)
	}
	if !bytes.Equal(got, ck) {
		t.Fatal("installed key does not match the broadcast original")
	}
}

// The identity provider is consulted at signal time, so a device id that
// becomes known only after construction (the pairing flow) is picked
// up without restarting the daemon.
func TestKeyRotationService_IdentityResolvedLazily(t *testing.T) {
	store := tempSecrets(t)
	_, myPub, _ := keys.NewDeviceKeyStore(store).LoadOrCreate()
	caller := &fakeKRCaller{devices: proto.RemoteListNamespaceDevicesResult{
		Devices: []proto.RemoteDevice{{DeviceID: "dev-aaa", PubKey: myPub[:]}},
	}}

	deviceID := "" // unknown at first (not yet paired)
	svc := NewKeyRotationService(context.Background(), caller, store,
		func() keyrotation.Identity { return keyrotation.Identity{DeviceID: deviceID} }, nil)

	// Unknown identity: fail-safe — an empty device id is never in the
	// device list, so the daemon does nothing.
	svc.HandleSignal(proto.RemoteNamespaceKeyRotatedNotification{NamespaceID: "ns-1", NewVersion: 1})
	if len(caller.puts) != 0 {
		t.Fatal("must not rotate while device identity is unknown")
	}

	// Identity becomes known (pairing completed) — next signal acts.
	deviceID = "dev-aaa"
	svc.HandleSignal(proto.RemoteNamespaceKeyRotatedNotification{NamespaceID: "ns-1", NewVersion: 1})
	if len(caller.puts) != 1 {
		t.Fatalf("expected rotation after identity resolved, got %d puts", len(caller.puts))
	}
}
