package proto

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"testing"
)

// The plugin sends wrap_pubkey as base64-std of the raw 32-byte X25519 key.
// encoding/json's []byte default is base64-std, so the daemon's params struct
// must produce exactly that wire shape.
func TestRemoteRegisterWrapKeyParams_WireShape(t *testing.T) {
	pub := bytes.Repeat([]byte{0x2A}, 32)
	in := RemoteRegisterWrapKeyParams{WrapPubKey: pub}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"wrap_pubkey":"` + base64.StdEncoding.EncodeToString(pub) + `"}`
	if string(b) != want {
		t.Fatalf("wire shape mismatch:\n got %s\nwant %s", b, want)
	}
	var out RemoteRegisterWrapKeyParams
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !bytes.Equal(out.WrapPubKey, pub) {
		t.Fatalf("round-trip mismatch: %x", out.WrapPubKey)
	}
}

// list_account_devices result: device_id + pubkey (base64-std 32 bytes),
// matching the plugin's wireRemoteDevice shape.
func TestRemoteListAccountDevicesResult_RoundTrip(t *testing.T) {
	raw := `{"devices":[{"device_id":"dev-a","pubkey":"` +
		base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0xAB}, 32)) + `"},` +
		`{"device_id":"dev-b","pubkey":"` +
		base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0xCD}, 32)) + `"}]}`
	var out RemoteListAccountDevicesResult
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(out.Devices) != 2 {
		t.Fatalf("want 2 devices, got %d", len(out.Devices))
	}
	if out.Devices[0].DeviceID != "dev-a" || len(out.Devices[0].PubKey) != 32 {
		t.Fatalf("device 0 mismatch: %+v", out.Devices[0])
	}
	if out.Devices[1].DeviceID != "dev-b" || len(out.Devices[1].PubKey) != 32 {
		t.Fatalf("device 1 mismatch: %+v", out.Devices[1])
	}

	// Re-marshal and confirm the key names stay device_id/pubkey.
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !bytes.Contains(b, []byte(`"device_id":"dev-a"`)) || !bytes.Contains(b, []byte(`"pubkey":"`)) {
		t.Fatalf("wire key names drifted: %s", b)
	}
}

func TestAccountDeviceMethodNames(t *testing.T) {
	// Lock the wire names so a rename can't silently break the plugin contract.
	if MethodRemoteRegisterWrapKey != "remote.register_wrap_key" {
		t.Errorf("MethodRemoteRegisterWrapKey = %q", MethodRemoteRegisterWrapKey)
	}
	if MethodRemoteListAccountDevices != "remote.list_account_devices" {
		t.Errorf("MethodRemoteListAccountDevices = %q", MethodRemoteListAccountDevices)
	}
}
