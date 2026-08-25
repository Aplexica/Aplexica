// Package rbac implements client-side checks for Aplexica's per-namespace
// role-based access control. The daemon can refuse team operations the
// caller's role does not permit before a round trip. The remote service
// remains authoritative; this layer only refuses operations earlier.
//
// Trust posture: roles are PLAINTEXT authorization metadata, not key
// material. Nothing in this package touches content keys or the zero-
// knowledge crypto path; it reasons purely over (role, operation) pairs.
//
// Fail-safe: an unknown/empty role, or an operation outside the matrix, is
// denied for everyone (closed-world). The matrix below is the SINGLE source
// of truth — Authorize, Can, Capabilities, and CanArtifact all derive from
// it, so the UI's capability list and the gate can never drift apart.
//
// This package has no imports beyond the standard library.
package rbac

import (
	"errors"
	"fmt"
)

// Role is a namespace membership role. A user may hold different roles in
// different namespaces. The canonical vocabulary is
// owner/admin/editor/contributor/reader.
type Role string

const (
	// RoleOwner has full control: manage members, roles, and policies, and
	// delete the namespace.
	RoleOwner Role = "owner"
	// RoleAdmin manages members and policies but cannot delete the namespace.
	RoleAdmin Role = "admin"
	// RoleEditor creates, updates, and deletes artifacts in the namespace.
	RoleEditor Role = "editor"
	// RoleContributor creates and updates artifacts but cannot delete
	// others' artifacts (only its own — see CanArtifact).
	RoleContributor Role = "contributor"
	// RoleReader has read-only access and can materialize into local agents.
	RoleReader Role = "reader"
)

// Role rank tiers for hierarchy comparisons ("X or higher"). Higher rank =
// strictly more capability. Declared as named consts (FR-10.6) and kept dense
// + ascending so a new tier can be slotted in without renumbering. An unknown
// role has no entry (effective rank below rankReader) and so is treated as
// below every requirement (deny-by-default).
const (
	rankReader      = iota + 1 // 1
	rankContributor            // 2
	rankEditor                 // 3
	rankAdmin                  // 4
	rankOwner                  // 5
)

// rank orders the roles for hierarchy comparisons. An unknown role has no
// entry; meetsRequirement treats a missing entry as deny-by-default.
var rank = map[Role]int{
	RoleReader:      rankReader,
	RoleContributor: rankContributor,
	RoleEditor:      rankEditor,
	RoleAdmin:       rankAdmin,
	RoleOwner:       rankOwner,
}

// Operation is a discrete namespace operation the matrix gates.
type Operation string

const (
	// OpRead reads artifacts in the namespace.
	OpRead Operation = "read"
	// OpMaterialize materializes namespace artifacts into local agents.
	OpMaterialize Operation = "materialize"
	// OpCreateArtifact creates a new artifact in the namespace.
	OpCreateArtifact Operation = "create_artifact"
	// OpEditArtifact edits an existing artifact. For a Contributor this is
	// permitted only for its OWN artifacts (see CanArtifact); Editor+ may
	// edit any.
	OpEditArtifact Operation = "edit_artifact"
	// OpDeleteArtifact deletes an artifact (Editor+).
	OpDeleteArtifact Operation = "delete_artifact"
	// OpForkConversation forks a conversation into the user's Personal
	// namespace; Reader+ suffices because the fork is not written back into
	// the shared namespace.
	OpForkConversation Operation = "fork_conversation"
	// OpAddMember adds a member to the namespace (Admin+).
	OpAddMember Operation = "add_member"
	// OpRemoveMember removes a member (Admin+).
	OpRemoveMember Operation = "remove_member"
	// OpChangeMemberRole changes another member's role (Admin+).
	OpChangeMemberRole Operation = "change_member_role"
	// OpSetNamespacePolicy sets namespace policy: retention, DLP, etc.
	// (Admin+).
	OpSetNamespacePolicy Operation = "set_namespace_policy"
	// OpDeleteNamespace deletes the namespace (Owner only).
	OpDeleteNamespace Operation = "delete_namespace"
)

// requiredRole maps each operation to the MINIMUM role that may perform it,
// encoding 07-role-to-capability-mapping.md. This is the single source of
// truth for the whole package.
//
// For OpEditArtifact the minimum here is RoleContributor (a Contributor may
// edit its own artifacts); the ownership refinement — a Contributor may NOT
// edit another member's artifact — lives in CanArtifact, not in this table,
// because it depends on per-artifact ownership the matrix alone cannot see.
var requiredRole = map[Operation]Role{
	OpRead:               RoleReader,
	OpMaterialize:        RoleReader,
	OpForkConversation:   RoleReader,
	OpCreateArtifact:     RoleContributor,
	OpEditArtifact:       RoleContributor,
	OpDeleteArtifact:     RoleEditor,
	OpAddMember:          RoleAdmin,
	OpRemoveMember:       RoleAdmin,
	OpChangeMemberRole:   RoleAdmin,
	OpSetNamespacePolicy: RoleAdmin,
	OpDeleteNamespace:    RoleOwner,
}

// operationOrder fixes the deterministic order Capabilities returns
// operations in (lowest required role first, then a stable intra-tier
// order). Listing it explicitly keeps the UI's capability list stable
// across releases without sorting strings at call time.
var operationOrder = []Operation{
	OpRead,
	OpMaterialize,
	OpForkConversation,
	OpCreateArtifact,
	OpEditArtifact,
	OpDeleteArtifact,
	OpAddMember,
	OpRemoveMember,
	OpChangeMemberRole,
	OpSetNamespacePolicy,
	OpDeleteNamespace,
}

// ErrForbidden is the sentinel wrapped by every denial Authorize/CanArtifact
// returns. Callers map it to HTTP 403 (web) or a clear refusal (CLI/UI). It
// is deliberately distinct from a transport error so a denial is never
// confused with "couldn't reach the server".
var ErrForbidden = errors.New("rbac: operation forbidden for role")

// ErrUnknownRole is wrapped by ParseRole when the input is not one of the
// five canonical roles.
var ErrUnknownRole = errors.New("rbac: unknown role")

// Authorize returns nil when role may perform op, or an error wrapping
// ErrForbidden otherwise. An unknown role or an operation outside the matrix
// is denied (fail-safe). The error names the role and operation so callers
// can surface a precise message.
func Authorize(role Role, op Operation) error {
	if meetsRequirement(role, op) {
		return nil
	}
	return fmt.Errorf("%w: role %q may not perform %q", ErrForbidden, string(role), string(op))
}

// Can is the boolean form of Authorize: true iff role may perform op.
func Can(role Role, op Operation) bool {
	return meetsRequirement(role, op)
}

// CanArtifact authorizes an artifact-scoped operation, accounting for the
// Contributor own-artifact rule: a Contributor may edit its OWN artifacts
// but not others'. ownArtifact reports whether the caller owns the target
// artifact. For every role/operation other than a Contributor editing a
// non-owned artifact, it behaves exactly like Authorize.
func CanArtifact(role Role, op Operation, ownArtifact bool) error {
	// The only ownership-sensitive relaxation in the matrix: a Contributor
	// editing its own artifact is allowed even though general edit authority
	// is Editor+. A Contributor editing someone else's artifact is denied.
	if op == OpEditArtifact && role == RoleContributor && !ownArtifact {
		return fmt.Errorf("%w: contributor may only edit its own artifacts", ErrForbidden)
	}
	return Authorize(role, op)
}

// Capabilities returns the operations role may perform, in a stable
// deterministic order. This is the shape the daemon web API serializes for
// the local UI. It derives from the same matrix Authorize uses, so it can
// never list a capability the gate would refuse. An unknown role yields an
// empty (non-nil) slice.
func Capabilities(role Role) []Operation {
	out := make([]Operation, 0, len(operationOrder))
	for _, op := range operationOrder {
		if meetsRequirement(role, op) {
			out = append(out, op)
		}
	}
	return out
}

// ParseRole converts a canonical role string to a Role, rejecting anything
// outside the five-role vocabulary (case-sensitive: the wire form is always
// lowercase). The returned error wraps ErrUnknownRole.
func ParseRole(s string) (Role, error) {
	switch Role(s) {
	case RoleOwner, RoleAdmin, RoleEditor, RoleContributor, RoleReader:
		return Role(s), nil
	default:
		return Role(""), fmt.Errorf("%w: %q", ErrUnknownRole, s)
	}
}

// meetsRequirement is the core check: role's rank is at least the operation's
// required-role rank. Unknown role (rank 0) or unknown operation (no matrix
// entry) => false.
func meetsRequirement(role Role, op Operation) bool {
	need, ok := requiredRole[op]
	if !ok {
		return false // operation outside the matrix: closed-world deny
	}
	have, ok := rank[role]
	if !ok {
		return false // unknown role: deny-by-default
	}
	return have >= rank[need]
}
