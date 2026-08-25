package daemon

import (
	"context"
	"errors"
	"testing"

	"github.com/aplexica/aplexica/internal/plugin/proto"
	"github.com/aplexica/aplexica/internal/rbac"
)

type fakeRBACCaller struct {
	result proto.RemoteGetNamespaceRoleResult
	err    error
	calls  []string
}

func (f *fakeRBACCaller) GetNamespaceRole(_ context.Context, params proto.RemoteGetNamespaceRoleParams) (proto.RemoteGetNamespaceRoleResult, error) {
	f.calls = append(f.calls, params.NamespaceID)
	return f.result, f.err
}

func TestRBACTransport_ResolveRole_ConvertsResult(t *testing.T) {
	caller := &fakeRBACCaller{result: proto.RemoteGetNamespaceRoleResult{Role: "admin", Found: true}}
	tr := newRoleTransport(caller)

	role, err := tr.ResolveRole(context.Background(), "ns-1")
	if err != nil {
		t.Fatalf("ResolveRole: %v", err)
	}
	if role != rbac.RoleAdmin {
		t.Errorf("role = %q, want admin", role)
	}
	if len(caller.calls) != 1 || caller.calls[0] != "ns-1" {
		t.Errorf("namespace not forwarded: %+v", caller.calls)
	}
}

func TestRBACTransport_NotFound_ReturnsErrNoMembership(t *testing.T) {
	caller := &fakeRBACCaller{result: proto.RemoteGetNamespaceRoleResult{Found: false}}
	tr := newRoleTransport(caller)

	_, err := tr.ResolveRole(context.Background(), "ns-1")
	if !errors.Is(err, rbac.ErrNoMembership) {
		t.Fatalf("Found=false should map to ErrNoMembership, got %v", err)
	}
}

// A Found=true result carrying a role string outside the canonical
// vocabulary is a contract violation by the server/plugin; the transport
// surfaces it as an error rather than silently treating it as a known role.
func TestRBACTransport_UnknownRole_Errors(t *testing.T) {
	caller := &fakeRBACCaller{result: proto.RemoteGetNamespaceRoleResult{Role: "superadmin", Found: true}}
	tr := newRoleTransport(caller)

	if _, err := tr.ResolveRole(context.Background(), "ns-1"); err == nil {
		t.Fatal("expected error for an unknown role string")
	}
}

// A transport error (e.g. ErrRemoteReconnecting) propagates unchanged so the
// caller can distinguish "couldn't ask" from "no membership".
func TestRBACTransport_CallerError_Propagates(t *testing.T) {
	sentinel := errors.New("boom")
	caller := &fakeRBACCaller{err: sentinel}
	tr := newRoleTransport(caller)

	if _, err := tr.ResolveRole(context.Background(), "ns-1"); !errors.Is(err, sentinel) {
		t.Fatalf("transport error should propagate, got %v", err)
	}
}

// Compile-time: the adapter satisfies the domain port.
var _ rbac.Transport = (*roleTransport)(nil)
