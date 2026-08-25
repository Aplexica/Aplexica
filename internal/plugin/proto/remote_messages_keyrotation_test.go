package proto

import (
	"bytes"
	"encoding/json"
	"testing"
)

// The control plane emits the namespace.key_rotated audit payload as
// {namespace_id, new_version, removed_user_id}. The plugin
// forwards exactly that as the notification params; the daemon must
// unmarshal it field-for-field.
func TestRemoteNamespaceKeyRotatedNotification_UnmarshalsAuditPayload(t *testing.T) {
	raw := `{"namespace_id":"ns-1","new_version":7,"removed_user_id":"user-bob"}`
	var n RemoteNamespaceKeyRotatedNotification
	if err := json.Unmarshal([]byte(raw), &n); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if n.NamespaceID != "ns-1" {
		t.Errorf("NamespaceID = %q", n.NamespaceID)
	}
	if n.NewVersion != 7 {
		t.Errorf("NewVersion = %d", n.NewVersion)
	}
	if n.RemovedUserID != "user-bob" {
		t.Errorf("RemovedUserID = %q", n.RemovedUserID)
	}
}

func TestRemoteWrappedKey_BytesRoundTripAsBase64(t *testing.T) {
	in := RemoteWrappedKey{DeviceID: "dev-1", Wrapped: []byte{0x00, 0x01, 0xFE, 0xFF}}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// encoding/json base64-encodes []byte; verify the wire is a string.
	if !bytes.Contains(b, []byte(`"wrapped":"`)) {
		t.Errorf("wrapped not base64-string-encoded: %s", b)
	}
	var out RemoteWrappedKey
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.DeviceID != in.DeviceID || !bytes.Equal(out.Wrapped, in.Wrapped) {
		t.Errorf("round-trip mismatch: %+v vs %+v", out, in)
	}
}

func TestRemoteNamespaceKeyBroadcastNotification_RoundTrip(t *testing.T) {
	in := RemoteNamespaceKeyBroadcastNotification{
		NamespaceID: "ns-9",
		KeyVersion:  4,
		Wrapped: []RemoteWrappedKey{
			{DeviceID: "dev-a", Wrapped: []byte{1, 2, 3}},
			{DeviceID: "dev-b", Wrapped: []byte{4, 5, 6}},
		},
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out RemoteNamespaceKeyBroadcastNotification
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.NamespaceID != in.NamespaceID || out.KeyVersion != in.KeyVersion || len(out.Wrapped) != 2 {
		t.Fatalf("round-trip mismatch: %+v", out)
	}
}

func TestRemoteListNamespaceDevicesResult_RoundTrip(t *testing.T) {
	in := RemoteListNamespaceDevicesResult{
		Devices: []RemoteDevice{
			{DeviceID: "dev-a", PubKey: bytes.Repeat([]byte{0xAB}, 32)},
		},
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out RemoteListNamespaceDevicesResult
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(out.Devices) != 1 || out.Devices[0].DeviceID != "dev-a" || len(out.Devices[0].PubKey) != 32 {
		t.Fatalf("round-trip mismatch: %+v", out)
	}
}

func TestKeyRotationMethodAndNotificationNames(t *testing.T) {
	// Lock the wire names so a rename can't silently break the cloud
	// plugin contract.
	cases := map[string]string{
		MethodRemoteListNamespaceDevices:        "remote.list_namespace_devices",
		MethodRemotePutNamespaceKey:             "remote.put_namespace_key",
		MethodRemoteGetNamespaceKey:             "remote.get_namespace_key",
		MethodRemoteBroadcastNamespaceKey:       "remote.broadcast_namespace_key",
		NotificationRemoteNamespaceKeyRotated:   "remote.namespace_key_rotated",
		NotificationRemoteNamespaceKeyBroadcast: "remote.namespace_key_broadcast",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("wire name = %q, want %q", got, want)
		}
	}
}
