package daemon

import (
	"context"
	"errors"
	"testing"

	"github.com/aplexica/aplexica/internal/plugin/proto"
	"github.com/aplexica/aplexica/internal/rbac"
)

// The write-gate (RoleService.Authorize / AuthorizeArtifact) is desync-safe by
// construction: its ONLY non-nil return is a definitive permission denial
// wrapping rbac.ErrForbidden, computed over the existing ResolveRole tri-state
// with zero side effects. Every error branch of ResolveRole — no-membership,
// unpaired, transport(offline) — yields nil (PROCEED), so the gate can never
// block AFTER a commit and therefore cannot desync the local hash-chain from
// peers. The server stays authoritative; this only refuses earlier.

// Test 1: a KNOWN reader role is a definitive deny for every artifact-mutating
// operation, and the denial is a clean typed permission error (ErrForbidden),
// distinguishable from a no-membership or transport answer.
func TestRoleService_Authorize_KnownReaderDeniesMutations(t *testing.T) {
	caller := &countingRBACCaller{result: proto.RemoteGetNamespaceRoleResult{Role: "reader", Found: true}}
	svc := NewRoleService(context.Background(), caller, pairedIdentity("dev-1"), nil)

	for _, op := range []rbac.Operation{rbac.OpCreateArtifact, rbac.OpEditArtifact, rbac.OpDeleteArtifact} {
		err := svc.Authorize(context.Background(), "ns-1", op)
		if err == nil {
			t.Fatalf("Authorize(%q) = nil, want a definitive deny", op)
		}
		if !errors.Is(err, rbac.ErrForbidden) {
			t.Errorf("Authorize(%q) error = %v, want it to wrap rbac.ErrForbidden", op, err)
		}
		// A deny must NOT masquerade as the proceed-causing branches.
		if errors.Is(err, rbac.ErrNoMembership) {
			t.Errorf("Authorize(%q) deny must not be ErrNoMembership", op)
		}
		if errors.Is(err, ErrRemoteReconnecting) {
			t.Errorf("Authorize(%q) deny must not be a transport error", op)
		}
	}
}

// Test 2: a contributor+ role with the capability PROCEEDS (nil error).
func TestRoleService_Authorize_ContributorPlusProceeds(t *testing.T) {
	contrib := &countingRBACCaller{result: proto.RemoteGetNamespaceRoleResult{Role: "contributor", Found: true}}
	csvc := NewRoleService(context.Background(), contrib, pairedIdentity("dev-1"), nil)
	if err := csvc.Authorize(context.Background(), "ns-1", rbac.OpCreateArtifact); err != nil {
		t.Errorf("contributor create: Authorize = %v, want nil", err)
	}
	if err := csvc.Authorize(context.Background(), "ns-1", rbac.OpEditArtifact); err != nil {
		t.Errorf("contributor edit: Authorize = %v, want nil", err)
	}
	// A contributor may NOT delete (Editor+); that is a definitive deny.
	if err := csvc.Authorize(context.Background(), "ns-1", rbac.OpDeleteArtifact); !errors.Is(err, rbac.ErrForbidden) {
		t.Errorf("contributor delete: Authorize = %v, want ErrForbidden", err)
	}

	editor := &countingRBACCaller{result: proto.RemoteGetNamespaceRoleResult{Role: "editor", Found: true}}
	esvc := NewRoleService(context.Background(), editor, pairedIdentity("dev-1"), nil)
	if err := esvc.Authorize(context.Background(), "ns-1", rbac.OpDeleteArtifact); err != nil {
		t.Errorf("editor delete: Authorize = %v, want nil", err)
	}
}

// Test 2 (ownership variant): AuthorizeArtifact encodes the Contributor
// own-artifact rule — editing an OWN artifact proceeds, editing someone
// else's is a definitive deny.
func TestRoleService_AuthorizeArtifact_ContributorOwnership(t *testing.T) {
	caller := &countingRBACCaller{result: proto.RemoteGetNamespaceRoleResult{Role: "contributor", Found: true}}
	svc := NewRoleService(context.Background(), caller, pairedIdentity("dev-1"), nil)

	if err := svc.AuthorizeArtifact(context.Background(), "ns-1", rbac.OpEditArtifact, true); err != nil {
		t.Errorf("contributor editing OWN artifact: AuthorizeArtifact = %v, want nil", err)
	}
	err := svc.AuthorizeArtifact(context.Background(), "ns-1", rbac.OpEditArtifact, false)
	if !errors.Is(err, rbac.ErrForbidden) {
		t.Errorf("contributor editing NON-OWN artifact: AuthorizeArtifact = %v, want ErrForbidden", err)
	}
}

// Test 3a: no-membership (Found=false => ResolveRole yields ErrNoMembership)
// must PROCEED — the unknown-role case is never a block.
func TestRoleService_Authorize_NoMembershipProceeds(t *testing.T) {
	caller := &countingRBACCaller{result: proto.RemoteGetNamespaceRoleResult{Found: false}}
	svc := NewRoleService(context.Background(), caller, pairedIdentity("dev-1"), nil)

	if err := svc.Authorize(context.Background(), "ns-1", rbac.OpCreateArtifact); err != nil {
		t.Errorf("no-membership: Authorize = %v, want nil (PROCEED — server is authoritative)", err)
	}
	if err := svc.AuthorizeArtifact(context.Background(), "ns-1", rbac.OpEditArtifact, false); err != nil {
		t.Errorf("no-membership: AuthorizeArtifact = %v, want nil (PROCEED)", err)
	}
}

// Test 3b: an unpaired identity PROCEEDS and never reaches the transport.
func TestRoleService_Authorize_UnpairedProceeds(t *testing.T) {
	caller := &countingRBACCaller{result: proto.RemoteGetNamespaceRoleResult{Role: "reader", Found: true}}
	svc := NewRoleService(context.Background(), caller, pairedIdentity(""), nil)

	if err := svc.Authorize(context.Background(), "ns-1", rbac.OpCreateArtifact); err != nil {
		t.Errorf("unpaired: Authorize = %v, want nil (PROCEED)", err)
	}
	if n := caller.callCount(); n != 0 {
		t.Errorf("unpaired Authorize hit the transport %d times, want 0", n)
	}
}

// Test 3c: a transport/offline error PROCEEDS — offline never blocks, and the
// error is swallowed into a proceed, not surfaced as a deny.
func TestRoleService_Authorize_TransportErrorProceeds(t *testing.T) {
	caller := &countingRBACCaller{err: ErrRemoteReconnecting}
	svc := NewRoleService(context.Background(), caller, pairedIdentity("dev-1"), nil)

	if err := svc.Authorize(context.Background(), "ns-1", rbac.OpCreateArtifact); err != nil {
		t.Errorf("transport error: Authorize = %v, want nil (offline never blocks)", err)
	}
	if err := svc.AuthorizeArtifact(context.Background(), "ns-1", rbac.OpDeleteArtifact, false); err != nil {
		t.Errorf("transport error: AuthorizeArtifact = %v, want nil (offline never blocks)", err)
	}
}
