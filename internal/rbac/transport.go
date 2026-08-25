package rbac

import (
	"context"
	"errors"
)

// ErrNoMembership is returned by a Transport when the caller holds no role
// in the namespace. It is deliberately distinct from ErrForbidden: a
// caller with no membership has no role at all (the daemon surfaces an empty
// role / empty capability set), whereas ErrForbidden means a known role
// lacks a specific capability. The role-resolution service treats both as
// deny-safe, but distinguishing them keeps the UI's messaging accurate.
var ErrNoMembership = errors.New("rbac: caller holds no membership in namespace")

// Transport is the narrow port the role-resolution layer needs: resolve the
// caller's role for a namespace. The real implementation (in the daemon
// package) calls the remote plugin over JSON-RPC and translates the proto
// result; tests use a fake. It returns ErrNoMembership when the caller is
// not a member of the namespace.
//
// Keeping this port in the domain package (proto-free, stdlib-only) lets the
// daemon adapter assert `var _ rbac.Transport = ...` at compile time and
// keeps the resolution logic independent of the wire format.
type Transport interface {
	ResolveRole(ctx context.Context, namespaceID string) (Role, error)
}

// Identity is the daemon's view of who is asking. For client-side RBAC the
// only fact the deny-safe path needs is whether this device is paired
// (DeviceID known); the caller's role itself is resolved server-side via the
// authenticated transport. A UserID field is reserved for future use but is
// not required to resolve a role.
type Identity struct {
	DeviceID string
	UserID   string // optional; "" when the daemon does not know its own user id
}

// Paired reports whether the device has a known identity. An unpaired device
// (empty DeviceID) cannot have a role resolved and is treated as having no
// membership (deny-by-default).
func (i Identity) Paired() bool { return i.DeviceID != "" }

// Logger is the optional structured logger the role-resolution layer uses to
// record transport failures. nil is tolerated (no-op). Mirrors the optional
// logger shape used elsewhere in the daemon.
type Logger interface {
	Warn(msg string, args ...any)
}
