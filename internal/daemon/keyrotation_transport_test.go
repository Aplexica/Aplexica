package daemon

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/aplexica/aplexica/internal/keyrotation"
	"github.com/aplexica/aplexica/internal/plugin/proto"
)

type fakeKRCaller struct {
	devices    proto.RemoteListNamespaceDevicesResult
	listErr    error
	puts       []proto.RemotePutNamespaceKeyParams
	putLost    bool // when true, report the CAS as lost (Claimed=false)
	putErr     error
	gets       []proto.RemoteGetNamespaceKeyParams
	getState   proto.RemoteGetNamespaceKeyResult
	getErr     error
	broadcasts []proto.RemoteBroadcastNamespaceKeyParams
}

func (f *fakeKRCaller) ListNamespaceDevices(_ context.Context, _ string) (proto.RemoteListNamespaceDevicesResult, error) {
	return f.devices, f.listErr
}
func (f *fakeKRCaller) PutNamespaceKey(_ context.Context, p proto.RemotePutNamespaceKeyParams) (proto.RemotePutNamespaceKeyResult, error) {
	if f.putErr != nil {
		return proto.RemotePutNamespaceKeyResult{}, f.putErr
	}
	f.puts = append(f.puts, p)
	return proto.RemotePutNamespaceKeyResult{Claimed: !f.putLost}, nil
}
func (f *fakeKRCaller) GetNamespaceKey(_ context.Context, p proto.RemoteGetNamespaceKeyParams) (proto.RemoteGetNamespaceKeyResult, error) {
	f.gets = append(f.gets, p)
	return f.getState, f.getErr
}
func (f *fakeKRCaller) BroadcastNamespaceKey(_ context.Context, p proto.RemoteBroadcastNamespaceKeyParams) error {
	f.broadcasts = append(f.broadcasts, p)
	return nil
}

func TestKeyRotationTransport_ListConvertsDevicesAndValidatesPubkey(t *testing.T) {
	caller := &fakeKRCaller{devices: proto.RemoteListNamespaceDevicesResult{
		Devices: []proto.RemoteDevice{
			{DeviceID: "dev-a", PubKey: bytes.Repeat([]byte{0x01}, 32)},
		},
	}}
	tr := newKeyRotationTransport(caller)

	devices, err := tr.ListNamespaceDevices(context.Background(), "ns-1")
	if err != nil {
		t.Fatalf("ListNamespaceDevices: %v", err)
	}
	if len(devices) != 1 || devices[0].DeviceID != "dev-a" {
		t.Fatalf("devices = %+v", devices)
	}
	if devices[0].PubKey != [32]byte(bytes.Repeat([]byte{0x01}, 32)) {
		t.Errorf("pubkey not copied into fixed array")
	}
}

func TestKeyRotationTransport_RejectsWrongPubkeySize(t *testing.T) {
	caller := &fakeKRCaller{devices: proto.RemoteListNamespaceDevicesResult{
		Devices: []proto.RemoteDevice{{DeviceID: "dev-bad", PubKey: []byte{0x01, 0x02}}},
	}}
	tr := newKeyRotationTransport(caller)
	if _, err := tr.ListNamespaceDevices(context.Background(), "ns-1"); err == nil {
		t.Fatal("expected error for a pubkey that is not 32 bytes")
	}
}

func TestKeyRotationTransport_PutAndBroadcastConvertWrappedKeys(t *testing.T) {
	caller := &fakeKRCaller{}
	tr := newKeyRotationTransport(caller)

	wrapped := []keyrotation.WrappedKey{
		{DeviceID: "dev-a", Wrapped: []byte{1, 2, 3}},
		{DeviceID: "dev-b", Wrapped: []byte{4, 5, 6}},
	}
	if err := tr.PutNamespaceKey(context.Background(), keyrotation.NamespaceKeyWrite{
		NamespaceID: "ns-1", KeyVersion: 4, Wrapped: wrapped,
	}); err != nil {
		t.Fatalf("PutNamespaceKey: %v", err)
	}
	if err := tr.BroadcastNamespaceKey(context.Background(), keyrotation.NamespaceKeyBroadcast{
		NamespaceID: "ns-1", KeyVersion: 4, Wrapped: wrapped,
	}); err != nil {
		t.Fatalf("BroadcastNamespaceKey: %v", err)
	}

	if len(caller.puts) != 1 || caller.puts[0].KeyVersion != 4 || len(caller.puts[0].Wrapped) != 2 {
		t.Fatalf("put conversion wrong: %+v", caller.puts)
	}
	if caller.puts[0].Wrapped[0].DeviceID != "dev-a" || !bytes.Equal(caller.puts[0].Wrapped[0].Wrapped, []byte{1, 2, 3}) {
		t.Errorf("wrapped key not converted: %+v", caller.puts[0].Wrapped[0])
	}
	if len(caller.broadcasts) != 1 || len(caller.broadcasts[0].Wrapped) != 2 {
		t.Errorf("broadcast conversion wrong: %+v", caller.broadcasts)
	}
}

func TestKeyRotationTransport_PutClaimed_NoError(t *testing.T) {
	tr := newKeyRotationTransport(&fakeKRCaller{}) // putLost=false → Claimed=true
	if err := tr.PutNamespaceKey(context.Background(), keyrotation.NamespaceKeyWrite{NamespaceID: "ns-1", KeyVersion: 1}); err != nil {
		t.Fatalf("winning CAS should return nil, got %v", err)
	}
}

func TestKeyRotationTransport_PutNotClaimed_MapsToErrKeyAlreadyClaimed(t *testing.T) {
	tr := newKeyRotationTransport(&fakeKRCaller{putLost: true})
	err := tr.PutNamespaceKey(context.Background(), keyrotation.NamespaceKeyWrite{NamespaceID: "ns-1", KeyVersion: 1})
	if !errors.Is(err, keyrotation.ErrKeyAlreadyClaimed) {
		t.Fatalf("lost CAS should map to ErrKeyAlreadyClaimed, got %v", err)
	}
}

func TestKeyRotationTransport_GetNamespaceKey_Converts(t *testing.T) {
	caller := &fakeKRCaller{getState: proto.RemoteGetNamespaceKeyResult{
		Found:   true,
		Wrapped: []proto.RemoteWrappedKey{{DeviceID: "dev-a", Wrapped: []byte{1, 2, 3}}},
	}}
	tr := newKeyRotationTransport(caller)
	st, err := tr.GetNamespaceKey(context.Background(), "ns-1", 4)
	if err != nil {
		t.Fatalf("GetNamespaceKey: %v", err)
	}
	if !st.Found || len(st.Wrapped) != 1 || st.Wrapped[0].DeviceID != "dev-a" || !bytes.Equal(st.Wrapped[0].Wrapped, []byte{1, 2, 3}) {
		t.Fatalf("converted state wrong: %+v", st)
	}
	if len(caller.gets) != 1 || caller.gets[0].KeyVersion != 4 {
		t.Errorf("get params not forwarded: %+v", caller.gets)
	}
}

func TestKeyRotationSignalFromProto(t *testing.T) {
	sig := keyRotationSignalFromProto(proto.RemoteNamespaceKeyRotatedNotification{
		NamespaceID: "ns-1", NewVersion: 9, RemovedUserID: "user-x",
	})
	if sig.NamespaceID != "ns-1" || sig.NewVersion != 9 || sig.RemovedUserID != "user-x" {
		t.Errorf("signal = %+v", sig)
	}
}

func TestKeyRotationBroadcastFromProto(t *testing.T) {
	b := keyRotationBroadcastFromProto(proto.RemoteNamespaceKeyBroadcastNotification{
		NamespaceID: "ns-1", KeyVersion: 2,
		Wrapped: []proto.RemoteWrappedKey{{DeviceID: "dev-a", Wrapped: []byte{7, 8}}},
	})
	if b.NamespaceID != "ns-1" || b.KeyVersion != 2 || len(b.Wrapped) != 1 {
		t.Fatalf("broadcast = %+v", b)
	}
	if b.Wrapped[0].DeviceID != "dev-a" || !bytes.Equal(b.Wrapped[0].Wrapped, []byte{7, 8}) {
		t.Errorf("wrapped = %+v", b.Wrapped[0])
	}
}
