package rbac

import (
	"context"
	"errors"
	"testing"
)

// The domain exposes a narrow Transport port (the daemon adapter satisfies
// it) and an ErrNoMembership sentinel the adapter returns when the caller
// holds no role in the namespace. These are the seam the role-resolution
// service depends on; assert they exist and behave as a deny-safe contract.
func TestTransport_PortShape(t *testing.T) {
	// A nil-returning fake must satisfy the interface at compile time.
	var _ Transport = transportFunc(func(context.Context, string) (Role, error) {
		return RoleReader, nil
	})
}

// Identity is the daemon's view of who is asking. For RBAC the only thing
// that matters for the deny-safe path is whether the device is paired
// (DeviceID known); an empty DeviceID means "not paired", and the resolver
// must refuse to surface a role.
func TestIdentity_Paired(t *testing.T) {
	if (Identity{DeviceID: "dev-1"}).Paired() != true {
		t.Error("a non-empty DeviceID should be Paired")
	}
	if (Identity{}).Paired() != false {
		t.Error("an empty DeviceID should not be Paired")
	}
}

func TestErrNoMembership_IsDistinctSentinel(t *testing.T) {
	if errors.Is(ErrNoMembership, ErrForbidden) {
		t.Error("ErrNoMembership must be distinct from ErrForbidden (no membership != denied capability)")
	}
	wrapped := errors.Join(ErrNoMembership, errors.New("ns-1"))
	if !errors.Is(wrapped, ErrNoMembership) {
		t.Error("ErrNoMembership must be matchable through wrapping")
	}
}

// transportFunc adapts a function to the Transport interface for tests.
type transportFunc func(context.Context, string) (Role, error)

func (f transportFunc) ResolveRole(ctx context.Context, namespaceID string) (Role, error) {
	return f(ctx, namespaceID)
}
