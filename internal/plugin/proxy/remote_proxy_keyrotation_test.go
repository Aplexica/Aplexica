package proxy

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/plugin/host"
	"github.com/aplexica/aplexica/internal/plugin/proto"
)

// krStub extends the base remoteStub with the key-rotation surface:
// it records the outbound calls and canned-returns a device list.
type krStub struct {
	*remoteStub
	krMu       sync.Mutex
	devices    []proto.RemoteDevice
	listedNS   string
	puts       []proto.RemotePutNamespaceKeyParams
	putClaimed bool
	gotState   proto.RemoteGetNamespaceKeyResult
	gets       []proto.RemoteGetNamespaceKeyParams
	broadcasts []proto.RemoteBroadcastNamespaceKeyParams
}

func (s *krStub) ListNamespaceDevices(_ context.Context, p proto.RemoteListNamespaceDevicesParams) (proto.RemoteListNamespaceDevicesResult, error) {
	s.krMu.Lock()
	defer s.krMu.Unlock()
	s.listedNS = p.NamespaceID
	return proto.RemoteListNamespaceDevicesResult{Devices: s.devices}, nil
}

func (s *krStub) PutNamespaceKey(_ context.Context, p proto.RemotePutNamespaceKeyParams) (proto.RemotePutNamespaceKeyResult, error) {
	s.krMu.Lock()
	defer s.krMu.Unlock()
	s.puts = append(s.puts, p)
	return proto.RemotePutNamespaceKeyResult{Claimed: s.putClaimed}, nil
}

func (s *krStub) GetNamespaceKey(_ context.Context, p proto.RemoteGetNamespaceKeyParams) (proto.RemoteGetNamespaceKeyResult, error) {
	s.krMu.Lock()
	defer s.krMu.Unlock()
	s.gets = append(s.gets, p)
	return s.gotState, nil
}

func (s *krStub) BroadcastNamespaceKey(_ context.Context, p proto.RemoteBroadcastNamespaceKeyParams) error {
	s.krMu.Lock()
	defer s.krMu.Unlock()
	s.broadcasts = append(s.broadcasts, p)
	return nil
}

func TestRemoteProxy_KeyRotation_OutboundAndNotifications(t *testing.T) {
	proxySide, hostSide := newPipePair()
	stub := &krStub{
		remoteStub: &remoteStub{},
		devices: []proto.RemoteDevice{
			{DeviceID: "dev-a", PubKey: bytes.Repeat([]byte{0x01}, 32)},
			{DeviceID: "dev-b", PubKey: bytes.Repeat([]byte{0x02}, 32)},
		},
		putClaimed: true,
		gotState: proto.RemoteGetNamespaceKeyResult{
			Found:   true,
			Wrapped: []proto.RemoteWrappedKey{{DeviceID: "dev-a", Wrapped: []byte{1, 2}}},
		},
	}

	hostDone := make(chan error, 1)
	go func() { hostDone <- host.ServeRemote(context.Background(), stub, hostSide, hostSide) }()

	rp, err := OpenRemote(context.Background(), proxySide, "dev-a", "v0.0.0-test")
	if err != nil {
		t.Fatalf("OpenRemote: %v", err)
	}

	// Wire notification callbacks BEFORE provoking any push.
	var nMu sync.Mutex
	var gotRotated []proto.RemoteNamespaceKeyRotatedNotification
	var gotBroadcast []proto.RemoteNamespaceKeyBroadcastNotification
	rp.OnNamespaceKeyRotated(func(n proto.RemoteNamespaceKeyRotatedNotification) {
		nMu.Lock()
		gotRotated = append(gotRotated, n)
		nMu.Unlock()
	})
	rp.OnNamespaceKeyBroadcast(func(n proto.RemoteNamespaceKeyBroadcastNotification) {
		nMu.Lock()
		gotBroadcast = append(gotBroadcast, n)
		nMu.Unlock()
	})

	// --- Outbound: list devices ---
	listRes, err := rp.ListNamespaceDevices(context.Background(), "ns-1")
	if err != nil {
		t.Fatalf("ListNamespaceDevices: %v", err)
	}
	if len(listRes.Devices) != 2 || listRes.Devices[0].DeviceID != "dev-a" {
		t.Fatalf("ListNamespaceDevices result = %+v", listRes)
	}
	if len(listRes.Devices[0].PubKey) != 32 {
		t.Errorf("pubkey not 32 bytes after round-trip")
	}

	// --- Outbound: put + broadcast ---
	put := proto.RemotePutNamespaceKeyParams{
		NamespaceID: "ns-1", KeyVersion: 3,
		Wrapped: []proto.RemoteWrappedKey{{DeviceID: "dev-a", Wrapped: []byte{9, 9, 9}}},
	}
	putRes, err := rp.PutNamespaceKey(context.Background(), put)
	if err != nil {
		t.Fatalf("PutNamespaceKey: %v", err)
	}
	if !putRes.Claimed {
		t.Errorf("PutNamespaceKey result Claimed = false, want true")
	}

	// --- Outbound: get (read-back for adoption) ---
	getRes, err := rp.GetNamespaceKey(context.Background(), proto.RemoteGetNamespaceKeyParams{NamespaceID: "ns-1", KeyVersion: 3})
	if err != nil {
		t.Fatalf("GetNamespaceKey: %v", err)
	}
	if !getRes.Found || len(getRes.Wrapped) != 1 || getRes.Wrapped[0].DeviceID != "dev-a" {
		t.Errorf("GetNamespaceKey result = %+v", getRes)
	}

	bc := proto.RemoteBroadcastNamespaceKeyParams{
		NamespaceID: "ns-1", KeyVersion: 3,
		Wrapped: []proto.RemoteWrappedKey{{DeviceID: "dev-a", Wrapped: []byte{9, 9, 9}}},
	}
	if err := rp.BroadcastNamespaceKey(context.Background(), bc); err != nil {
		t.Fatalf("BroadcastNamespaceKey: %v", err)
	}

	// Verify the host recorded the outbound calls.
	stub.krMu.Lock()
	if stub.listedNS != "ns-1" {
		t.Errorf("host listedNS = %q", stub.listedNS)
	}
	if len(stub.puts) != 1 || stub.puts[0].KeyVersion != 3 {
		t.Errorf("host puts = %+v", stub.puts)
	}
	if len(stub.broadcasts) != 1 {
		t.Errorf("host broadcasts = %+v", stub.broadcasts)
	}
	stub.krMu.Unlock()

	// --- Inbound notifications pushed from the plugin side ---
	waitNotifier(t, stub.remoteStub)
	if err := stub.remoteStub.notifier.NamespaceKeyRotated(proto.RemoteNamespaceKeyRotatedNotification{
		NamespaceID: "ns-1", NewVersion: 3, RemovedUserID: "user-bob",
	}); err != nil {
		t.Fatalf("notifier.NamespaceKeyRotated: %v", err)
	}
	if err := stub.remoteStub.notifier.NamespaceKeyBroadcast(proto.RemoteNamespaceKeyBroadcastNotification{
		NamespaceID: "ns-1", KeyVersion: 3,
		Wrapped: []proto.RemoteWrappedKey{{DeviceID: "dev-a", Wrapped: []byte{7, 7}}},
	}); err != nil {
		t.Fatalf("notifier.NamespaceKeyBroadcast: %v", err)
	}

	waitFor(t, func() bool {
		nMu.Lock()
		defer nMu.Unlock()
		return len(gotRotated) == 1 && len(gotBroadcast) == 1
	})
	nMu.Lock()
	if gotRotated[0].RemovedUserID != "user-bob" || gotRotated[0].NewVersion != 3 {
		t.Errorf("rotated notification = %+v", gotRotated[0])
	}
	if gotBroadcast[0].NamespaceID != "ns-1" || len(gotBroadcast[0].Wrapped) != 1 {
		t.Errorf("broadcast notification = %+v", gotBroadcast[0])
	}
	nMu.Unlock()

	if err := rp.Shutdown(context.Background()); err != nil {
		t.Errorf("Shutdown: %v", err)
	}
	select {
	case <-hostDone:
	case <-time.After(2 * time.Second):
		t.Error("host did not return")
	}
}

func waitNotifier(t *testing.T, s *remoteStub) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		n := s.notifier
		s.mu.Unlock()
		if n != nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("notifier never attached")
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met within deadline")
}
