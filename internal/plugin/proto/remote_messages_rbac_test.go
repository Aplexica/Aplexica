package proto

import (
	"encoding/json"
	"testing"
)

// The daemon asks the cloud "what is my role in namespace X" over the
// remote plugin. The plugin's authenticated transport identifies the caller
// server-side (the same way remote.list_namespace_devices carries no caller
// id), so the request is just the namespace id; the result is the resolved
// role string plus a Found flag distinguishing "Reader" from "not a member".
func TestRemoteGetNamespaceRoleParams_RoundTrip(t *testing.T) {
	in := RemoteGetNamespaceRoleParams{NamespaceID: "ns-1"}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out RemoteGetNamespaceRoleParams
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.NamespaceID != in.NamespaceID {
		t.Errorf("round-trip mismatch: %+v vs %+v", out, in)
	}
}

func TestRemoteGetNamespaceRoleResult_RoundTrip(t *testing.T) {
	in := RemoteGetNamespaceRoleResult{Role: "admin", Found: true}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out RemoteGetNamespaceRoleResult
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Role != in.Role || out.Found != in.Found {
		t.Errorf("round-trip mismatch: %+v vs %+v", out, in)
	}
}

// Found=false is the "no membership" wire signal: the role is empty and the
// daemon must treat it as deny-by-default.
func TestRemoteGetNamespaceRoleResult_NotFound(t *testing.T) {
	raw := `{"found":false,"role":""}`
	var out RemoteGetNamespaceRoleResult
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Found || out.Role != "" {
		t.Errorf("not-found result = %+v", out)
	}
}

// Lock the wire name so a rename can't silently break the cloud-plugin
// contract (mirrors TestKeyRotationMethodAndNotificationNames).
func TestRBACMethodName(t *testing.T) {
	if MethodRemoteGetNamespaceRole != "remote.get_namespace_role" {
		t.Errorf("wire name = %q, want %q", MethodRemoteGetNamespaceRole, "remote.get_namespace_role")
	}
}
