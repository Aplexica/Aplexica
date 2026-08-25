package daemon

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/aplexica/aplexica/internal/rbac"
)

// roleCacheTTL bounds how long a resolved role is reused before a re-fetch.
// It must stay well under the 60-second role-propagation budget so a role
// change reaches an online device promptly even without an explicit
// membership-change hint to invalidate the entry; the hint path
// (Invalidate/InvalidateAll) tightens this further to near-immediate.
const roleCacheTTL = 30 * time.Second

// RoleService is the daemon-side wiring for client-side RBAC. It resolves the
// caller's role for a namespace over the remote plugin (via roleTransport)
// and caches it per-namespace with a short TTL, so the local UI and any
// client-side gate can consult the role without a round-trip on every check.
//
// Deny-safe by construction:
//   - An unpaired identity (empty DeviceID) never reaches the transport and
//     resolves to rbac.ErrNoMembership.
//   - A negative ("not a member") answer is cached so the daemon doesn't
//     hammer the server for a namespace the user isn't in.
//   - A transport error is surfaced but NOT cached, so a connectivity blip
//     does not pin a deny; the next call retries.
//
// The server stays authoritative; this layer only refuses earlier and paints
// the UI. identityFn is consulted on each resolve so a device id known only
// after pairing is picked up without a restart.
type RoleService struct {
	transport  rbac.Transport
	identityFn func() rbac.Identity
	logger     rbac.Logger
	ttl        time.Duration

	mu    sync.Mutex
	cache map[string]roleCacheEntry
}

type roleCacheEntry struct {
	role      rbac.Role
	noMember  bool // true => the caller holds no membership in this namespace
	expiresAt time.Time
}

// NewRoleService builds the service over a remote caller (the RemoteRunner)
// and an identity provider. The daemon-lifetime ctx is accepted for call-site
// symmetry with NewKeyRotationService (resolution uses each call's own
// context). identityFn must not be nil; logger may be nil.
func NewRoleService(
	_ context.Context,
	caller rbacRoleCaller,
	identityFn func() rbac.Identity,
	logger rbac.Logger,
) *RoleService {
	return &RoleService{
		transport:  newRoleTransport(caller),
		identityFn: identityFn,
		logger:     logger,
		ttl:        roleCacheTTL,
		cache:      map[string]roleCacheEntry{},
	}
}

// ResolveRole returns the caller's role in namespaceID, using the cache when
// fresh. Returns rbac.ErrNoMembership when the device is unpaired or the
// caller holds no membership; propagates a transport error otherwise.
func (s *RoleService) ResolveRole(ctx context.Context, namespaceID string) (rbac.Role, error) {
	// Unpaired device: no identity to resolve against — deny-safe without a
	// transport call.
	if !s.identityFn().Paired() {
		return rbac.Role(""), rbac.ErrNoMembership
	}

	if role, noMember, ok := s.cached(namespaceID); ok {
		if noMember {
			return rbac.Role(""), rbac.ErrNoMembership
		}
		return role, nil
	}

	role, err := s.transport.ResolveRole(ctx, namespaceID)
	switch {
	case err == nil:
		s.store(namespaceID, roleCacheEntry{role: role, expiresAt: s.now().Add(s.ttl)})
		return role, nil
	case errors.Is(err, rbac.ErrNoMembership):
		// Cache the negative answer too (bounded by the same TTL).
		s.store(namespaceID, roleCacheEntry{noMember: true, expiresAt: s.now().Add(s.ttl)})
		return rbac.Role(""), rbac.ErrNoMembership
	default:
		// Transport / parse error: surface it, do NOT cache (avoid pinning a
		// deny on a transient failure).
		s.warn("rbac: resolve role failed", "namespace", namespaceID, "err", err)
		return rbac.Role(""), err
	}
}

// Capabilities returns the operations the caller may perform in namespaceID,
// derived from the resolved role (the shape the web API serializes for the
// UI). A no-membership caller yields an empty (non-nil) capability slice and
// a nil error, so the UI can render "no access" uniformly rather than
// treating it as a failure.
func (s *RoleService) Capabilities(ctx context.Context, namespaceID string) ([]rbac.Operation, error) {
	role, err := s.ResolveRole(ctx, namespaceID)
	if errors.Is(err, rbac.ErrNoMembership) {
		return []rbac.Operation{}, nil
	}
	if err != nil {
		return nil, err
	}
	return rbac.Capabilities(role), nil
}

// Authorize is the DESYNC-SAFE client-side write-gate. It returns a non-nil
// error ONLY when there is a DEFINITIVE local deny — the caller holds a KNOWN
// role for namespaceID and that role lacks the capability op requires — and
// that error wraps rbac.ErrForbidden so callers map it to a clean permission
// refusal. In every other case it returns nil (PROCEED):
//
//   - no membership / unpaired (ResolveRole => rbac.ErrNoMembership): the role
//     is unknown, so we must not block — the server remains authoritative and
//     is the backstop.
//   - transport / offline error (ResolveRole => any other error): a
//     connectivity blip must never deny; we proceed and let the server decide.
//
// This is the WHOLE point of the gate: the only non-nil return is computable
// with zero side effects from a KNOWN role, so a caller can consult it BEFORE
// performing any local mutation/commit. Because the unknown/offline paths
// proceed, the gate can never refuse AFTER a commit and therefore can never
// desync the local hash-chain from peers. It only refuses earlier than the
// server would; it never grants anything the server would refuse.
//
// FOLLOW-UP (out of scope here): full offline write-consistency — reconciling
// a server-side POST-HOC rejection of a write made while offline (unknown
// role) — is a separate effort. This gate deliberately does not attempt it;
// it only fast-paths the definitive-deny case.
func (s *RoleService) Authorize(ctx context.Context, namespaceID string, op rbac.Operation) error {
	role, err := s.ResolveRole(ctx, namespaceID)
	if err != nil {
		// Unknown role (no-membership/unpaired) or a transport error: PROCEED.
		// The server stays authoritative; we never block on a non-definitive
		// answer, so we can never block after a commit (no desync).
		return nil
	}
	// Known role: this is the ONLY branch that may return a definitive deny.
	return rbac.Authorize(role, op)
}

// AuthorizeArtifact is the ownership-aware companion to Authorize for the
// Contributor own-artifact rule (rbac.CanArtifact): a Contributor may edit its
// OWN artifact but not someone else's. ownArtifact reports whether the caller
// owns the target artifact. Its desync-safety contract is identical to
// Authorize — the only non-nil return is a definitive deny from a KNOWN role.
func (s *RoleService) AuthorizeArtifact(ctx context.Context, namespaceID string, op rbac.Operation, ownArtifact bool) error {
	role, err := s.ResolveRole(ctx, namespaceID)
	if err != nil {
		return nil // unknown/offline => PROCEED (server authoritative)
	}
	return rbac.CanArtifact(role, op, ownArtifact)
}

// Invalidate drops the cached role for one namespace, forcing the next
// resolve to re-fetch. Wire this to a membership/role-change hint so a role
// change reaches this device within the required window.
func (s *RoleService) Invalidate(namespaceID string) {
	s.mu.Lock()
	delete(s.cache, namespaceID)
	s.mu.Unlock()
}

// InvalidateAll drops every cached role. Used when the plugin reconnects or a
// broad membership_changed hint arrives without a specific namespace.
func (s *RoleService) InvalidateAll() {
	s.mu.Lock()
	s.cache = map[string]roleCacheEntry{}
	s.mu.Unlock()
}

func (s *RoleService) cached(namespaceID string) (rbac.Role, bool, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.cache[namespaceID]
	if !ok || !s.now().Before(e.expiresAt) {
		return rbac.Role(""), false, false
	}
	return e.role, e.noMember, true
}

func (s *RoleService) store(namespaceID string, e roleCacheEntry) {
	s.mu.Lock()
	s.cache[namespaceID] = e
	s.mu.Unlock()
}

func (s *RoleService) now() time.Time { return time.Now() }

func (s *RoleService) warn(msg string, args ...any) {
	if s.logger != nil {
		s.logger.Warn(msg, args...)
	}
}
