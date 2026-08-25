package daemon

import (
	"context"

	"github.com/aplexica/aplexica/internal/plugin/proto"
	"github.com/aplexica/aplexica/internal/rbac"
)

// rbacRoleCaller is the slice of the RemoteRunner the RBAC transport adapter
// needs. *RemoteRunner satisfies it; tests inject a fake.
type rbacRoleCaller interface {
	GetNamespaceRole(ctx context.Context, params proto.RemoteGetNamespaceRoleParams) (proto.RemoteGetNamespaceRoleResult, error)
}

// roleTransport adapts the remote plugin's proto surface to the proto-free
// rbac.Transport port, translating the wire result (role string + Found
// flag) into a domain rbac.Role and mapping "not a member" to the
// rbac.ErrNoMembership sentinel.
type roleTransport struct {
	caller rbacRoleCaller
}

func newRoleTransport(c rbacRoleCaller) *roleTransport {
	return &roleTransport{caller: c}
}

// Compile-time assertion that the adapter satisfies the domain port.
var _ rbac.Transport = (*roleTransport)(nil)

// ResolveRole asks the plugin for the caller's role in namespaceID and
// translates the result. A Found=false result becomes rbac.ErrNoMembership;
// a role string outside the canonical vocabulary is a contract violation and
// surfaces as an error (via rbac.ParseRole) rather than a silent unknown
// role. Transport errors propagate unchanged.
func (t *roleTransport) ResolveRole(ctx context.Context, namespaceID string) (rbac.Role, error) {
	res, err := t.caller.GetNamespaceRole(ctx, proto.RemoteGetNamespaceRoleParams{NamespaceID: namespaceID})
	if err != nil {
		return rbac.Role(""), err
	}
	if !res.Found {
		return rbac.Role(""), rbac.ErrNoMembership
	}
	return rbac.ParseRole(res.Role)
}
