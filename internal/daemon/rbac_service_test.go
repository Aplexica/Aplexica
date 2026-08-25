package daemon

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/plugin/proto"
	"github.com/aplexica/aplexica/internal/rbac"
)

// countingRBACCaller records how many times the transport was hit so cache
// behaviour can be asserted.
type countingRBACCaller struct {
	mu     sync.Mutex
	result proto.RemoteGetNamespaceRoleResult
	err    error
	calls  int
	perNS  map[string]int
}

func (c *countingRBACCaller) GetNamespaceRole(_ context.Context, p proto.RemoteGetNamespaceRoleParams) (proto.RemoteGetNamespaceRoleResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	if c.perNS == nil {
		c.perNS = map[string]int{}
	}
	c.perNS[p.NamespaceID]++
	return c.result, c.err
}

func (c *countingRBACCaller) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

func pairedIdentity(deviceID string) func() rbac.Identity {
	return func() rbac.Identity { return rbac.Identity{DeviceID: deviceID} }
}

func TestRoleService_ResolveCachesPerNamespace(t *testing.T) {
	caller := &countingRBACCaller{result: proto.RemoteGetNamespaceRoleResult{Role: "editor", Found: true}}
	svc := NewRoleService(context.Background(), caller, pairedIdentity("dev-1"), nil)

	role, err := svc.ResolveRole(context.Background(), "ns-1")
	if err != nil {
		t.Fatalf("ResolveRole: %v", err)
	}
	if role != rbac.RoleEditor {
		t.Errorf("role = %q, want editor", role)
	}

	// Second resolve within TTL must reuse the cache (no extra transport hit).
	if _, err := svc.ResolveRole(context.Background(), "ns-1"); err != nil {
		t.Fatalf("ResolveRole(2): %v", err)
	}
	if n := caller.callCount(); n != 1 {
		t.Errorf("transport hit %d times, want 1 (cache miss only once)", n)
	}

	// A different namespace is a distinct cache entry => one more hit.
	if _, err := svc.ResolveRole(context.Background(), "ns-2"); err != nil {
		t.Fatalf("ResolveRole(ns-2): %v", err)
	}
	if n := caller.callCount(); n != 2 {
		t.Errorf("transport hit %d times, want 2 (per-namespace cache)", n)
	}
}

func TestRoleService_UnknownIdentityReturnsNoRole(t *testing.T) {
	caller := &countingRBACCaller{result: proto.RemoteGetNamespaceRoleResult{Role: "admin", Found: true}}
	// Empty device id == not paired: deny-safe, and we must not even ask the
	// transport (no identity to resolve against).
	svc := NewRoleService(context.Background(), caller, pairedIdentity(""), nil)

	role, err := svc.ResolveRole(context.Background(), "ns-1")
	if !errors.Is(err, rbac.ErrNoMembership) {
		t.Fatalf("unknown identity should yield ErrNoMembership, got role=%q err=%v", role, err)
	}
	if role != rbac.Role("") {
		t.Errorf("role = %q, want empty", role)
	}
	if n := caller.callCount(); n != 0 {
		t.Errorf("transport must not be called for an unpaired identity, got %d calls", n)
	}
}

func TestRoleService_Capabilities_DerivesFromRole(t *testing.T) {
	caller := &countingRBACCaller{result: proto.RemoteGetNamespaceRoleResult{Role: "contributor", Found: true}}
	svc := NewRoleService(context.Background(), caller, pairedIdentity("dev-1"), nil)

	caps, err := svc.Capabilities(context.Background(), "ns-1")
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	want := rbac.Capabilities(rbac.RoleContributor)
	if len(caps) != len(want) {
		t.Fatalf("caps = %v, want %v", caps, want)
	}
	// A contributor can create but not delete.
	if !containsOp(caps, rbac.OpCreateArtifact) || containsOp(caps, rbac.OpDeleteArtifact) {
		t.Errorf("contributor caps wrong: %v", caps)
	}
}

func TestRoleService_NotAMember_ReturnsNoMembershipAndCaches(t *testing.T) {
	caller := &countingRBACCaller{result: proto.RemoteGetNamespaceRoleResult{Found: false}}
	svc := NewRoleService(context.Background(), caller, pairedIdentity("dev-1"), nil)

	if _, err := svc.ResolveRole(context.Background(), "ns-1"); !errors.Is(err, rbac.ErrNoMembership) {
		t.Fatalf("want ErrNoMembership, got %v", err)
	}
	// The negative result is cached too, so we don't hammer the server for a
	// namespace the user isn't in.
	if _, err := svc.ResolveRole(context.Background(), "ns-1"); !errors.Is(err, rbac.ErrNoMembership) {
		t.Fatalf("want cached ErrNoMembership, got %v", err)
	}
	if n := caller.callCount(); n != 1 {
		t.Errorf("negative result not cached: transport hit %d times, want 1", n)
	}
}

func TestRoleService_RefreshAfterInvalidate(t *testing.T) {
	caller := &countingRBACCaller{result: proto.RemoteGetNamespaceRoleResult{Role: "reader", Found: true}}
	svc := NewRoleService(context.Background(), caller, pairedIdentity("dev-1"), nil)

	if _, err := svc.ResolveRole(context.Background(), "ns-1"); err != nil {
		t.Fatalf("ResolveRole: %v", err)
	}
	if n := caller.callCount(); n != 1 {
		t.Fatalf("want 1 call, got %d", n)
	}

	// Simulate a role change: the role becomes admin, and a membership-change
	// hint invalidates the cache so the next resolve re-fetches (a
	// role change must reach online devices within 60s).
	caller.mu.Lock()
	caller.result = proto.RemoteGetNamespaceRoleResult{Role: "admin", Found: true}
	caller.mu.Unlock()
	svc.Invalidate("ns-1")

	role, err := svc.ResolveRole(context.Background(), "ns-1")
	if err != nil {
		t.Fatalf("ResolveRole after invalidate: %v", err)
	}
	if role != rbac.RoleAdmin {
		t.Errorf("role = %q, want admin (cache should have been invalidated)", role)
	}
	if n := caller.callCount(); n != 2 {
		t.Errorf("transport hit %d times, want 2 (re-fetch after invalidate)", n)
	}
}

// InvalidateAll drops every cached role (used when the plugin reconnects or a
// broad membership_changed hint arrives without a specific namespace).
func TestRoleService_InvalidateAll(t *testing.T) {
	caller := &countingRBACCaller{result: proto.RemoteGetNamespaceRoleResult{Role: "reader", Found: true}}
	svc := NewRoleService(context.Background(), caller, pairedIdentity("dev-1"), nil)

	_, _ = svc.ResolveRole(context.Background(), "ns-1")
	_, _ = svc.ResolveRole(context.Background(), "ns-2")
	if n := caller.callCount(); n != 2 {
		t.Fatalf("want 2 calls, got %d", n)
	}
	svc.InvalidateAll()
	_, _ = svc.ResolveRole(context.Background(), "ns-1")
	if n := caller.callCount(); n != 3 {
		t.Errorf("InvalidateAll should force a re-fetch: hit %d, want 3", n)
	}
}

// A transport error is deny-safe but NOT cached (so connectivity blips don't
// pin a deny), and it is distinguishable from ErrNoMembership.
func TestRoleService_TransportError_NotCached(t *testing.T) {
	caller := &countingRBACCaller{err: ErrRemoteReconnecting}
	svc := NewRoleService(context.Background(), caller, pairedIdentity("dev-1"), nil)

	if _, err := svc.ResolveRole(context.Background(), "ns-1"); !errors.Is(err, ErrRemoteReconnecting) {
		t.Fatalf("want ErrRemoteReconnecting, got %v", err)
	}
	// Recovery: transport now succeeds; the error must not have been cached.
	caller.mu.Lock()
	caller.err = nil
	caller.result = proto.RemoteGetNamespaceRoleResult{Role: "owner", Found: true}
	caller.mu.Unlock()
	role, err := svc.ResolveRole(context.Background(), "ns-1")
	if err != nil {
		t.Fatalf("ResolveRole after recovery: %v", err)
	}
	if role != rbac.RoleOwner {
		t.Errorf("role = %q, want owner", role)
	}
}

// Identity is resolved lazily at resolve time, so a device id that becomes
// known only after pairing is picked up without a restart.
func TestRoleService_IdentityResolvedLazily(t *testing.T) {
	caller := &countingRBACCaller{result: proto.RemoteGetNamespaceRoleResult{Role: "editor", Found: true}}
	deviceID := "" // unpaired at first
	svc := NewRoleService(context.Background(), caller, func() rbac.Identity {
		return rbac.Identity{DeviceID: deviceID}
	}, nil)

	if _, err := svc.ResolveRole(context.Background(), "ns-1"); !errors.Is(err, rbac.ErrNoMembership) {
		t.Fatalf("unpaired: want ErrNoMembership, got %v", err)
	}
	deviceID = "dev-1" // pairing completes
	role, err := svc.ResolveRole(context.Background(), "ns-1")
	if err != nil {
		t.Fatalf("ResolveRole after pairing: %v", err)
	}
	if role != rbac.RoleEditor {
		t.Errorf("role = %q, want editor", role)
	}
}

func containsOp(ops []rbac.Operation, want rbac.Operation) bool {
	for _, o := range ops {
		if o == want {
			return true
		}
	}
	return false
}

// guard against an accidentally-huge default TTL making the cache effectively
// permanent in a long-running daemon: it must be a sane, short-ish window.
func TestRoleService_CacheTTLIsBounded(t *testing.T) {
	if roleCacheTTL <= 0 || roleCacheTTL > time.Minute {
		t.Errorf("roleCacheTTL = %v; want a positive value <= 60s", roleCacheTTL)
	}
}
