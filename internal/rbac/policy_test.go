package rbac

import (
	"errors"
	"sort"
	"testing"
)

// allOps is every operation the matrix knows about. Kept here (not derived
// from the production table) so the test is an independent enumeration that
// would catch an operation silently dropped from the matrix.
var allOps = []Operation{
	OpRead,
	OpMaterialize,
	OpCreateArtifact,
	OpEditArtifact,
	OpDeleteArtifact,
	OpForkConversation,
	OpAddMember,
	OpRemoveMember,
	OpChangeMemberRole,
	OpSetNamespacePolicy,
	OpDeleteNamespace,
}

// TestAuthorize_RoleHierarchy is the authoritative table test for the remote
// role-to-capability contract. For every (role, operation) pair it asserts
// Authorize returns nil
// iff the role meets the matrix minimum.
func TestAuthorize_RoleHierarchy(t *testing.T) {
	// want[role] is the exact set of operations that role may perform.
	// Edit/Delete here mean "for artifacts the matrix allows that role" —
	// the Contributor own-artifact nuance is captured by Can/CanArtifact
	// and tested separately in TestAuthorize_ContributorOwnArtifact.
	want := map[Role]map[Operation]bool{
		RoleReader: {
			OpRead:             true,
			OpMaterialize:      true,
			OpForkConversation: true,
		},
		RoleContributor: {
			OpRead:             true,
			OpMaterialize:      true,
			OpForkConversation: true,
			OpCreateArtifact:   true,
			OpEditArtifact:     true, // own artifacts; see CanArtifact for the ownership gate
		},
		RoleEditor: {
			OpRead:             true,
			OpMaterialize:      true,
			OpForkConversation: true,
			OpCreateArtifact:   true,
			OpEditArtifact:     true,
			OpDeleteArtifact:   true,
		},
		RoleAdmin: {
			OpRead:               true,
			OpMaterialize:        true,
			OpForkConversation:   true,
			OpCreateArtifact:     true,
			OpEditArtifact:       true,
			OpDeleteArtifact:     true,
			OpAddMember:          true,
			OpRemoveMember:       true,
			OpChangeMemberRole:   true,
			OpSetNamespacePolicy: true,
		},
		RoleOwner: {
			OpRead:               true,
			OpMaterialize:        true,
			OpForkConversation:   true,
			OpCreateArtifact:     true,
			OpEditArtifact:       true,
			OpDeleteArtifact:     true,
			OpAddMember:          true,
			OpRemoveMember:       true,
			OpChangeMemberRole:   true,
			OpSetNamespacePolicy: true,
			OpDeleteNamespace:    true,
		},
	}

	for role, allowed := range want {
		for _, op := range allOps {
			err := Authorize(role, op)
			gotOK := err == nil
			wantOK := allowed[op]
			if gotOK != wantOK {
				t.Errorf("Authorize(%s, %s) ok=%v, want %v (err=%v)", role, op, gotOK, wantOK, err)
			}
			// Can must agree with Authorize.
			if Can(role, op) != wantOK {
				t.Errorf("Can(%s, %s) = %v, want %v", role, op, Can(role, op), wantOK)
			}
		}
	}
}

// TestAuthorize_UnknownRoleDeniesAll: an empty or garbage role fails-safe —
// every operation is forbidden.
func TestAuthorize_UnknownRoleDeniesAll(t *testing.T) {
	for _, role := range []Role{Role(""), Role("superadmin"), Role("ADMIN"), Role("root")} {
		for _, op := range allOps {
			if err := Authorize(role, op); err == nil {
				t.Errorf("Authorize(%q, %s) = nil, want ErrForbidden (fail-safe)", role, op)
			}
			if Can(role, op) {
				t.Errorf("Can(%q, %s) = true, want false", role, op)
			}
		}
		if caps := Capabilities(role); len(caps) != 0 {
			t.Errorf("Capabilities(%q) = %v, want empty", role, caps)
		}
	}
}

// TestAuthorize_UnknownOperationDenies: an operation outside the matrix is
// denied for everyone (closed-world).
func TestAuthorize_UnknownOperationDenies(t *testing.T) {
	for _, role := range []Role{RoleOwner, RoleAdmin, RoleEditor, RoleContributor, RoleReader} {
		if err := Authorize(role, Operation("teleport")); err == nil {
			t.Errorf("Authorize(%s, teleport) = nil, want forbidden", role)
		}
	}
}

func TestParseRole_AcceptsCanonicalRejectsUnknown(t *testing.T) {
	canonical := map[string]Role{
		"owner":       RoleOwner,
		"admin":       RoleAdmin,
		"editor":      RoleEditor,
		"contributor": RoleContributor,
		"reader":      RoleReader,
	}
	for s, want := range canonical {
		got, err := ParseRole(s)
		if err != nil {
			t.Errorf("ParseRole(%q) error: %v", s, err)
		}
		if got != want {
			t.Errorf("ParseRole(%q) = %s, want %s", s, got, want)
		}
	}
	for _, s := range []string{"", "superadmin", "Owner", "READER", "guest", " admin"} {
		if _, err := ParseRole(s); err == nil {
			t.Errorf("ParseRole(%q) = nil error, want rejection", s)
		}
	}
}

// TestCapabilities_ForRole asserts Capabilities returns exactly the set of
// ops a role satisfies (the shape the web API serializes for the UI), and
// that it derives from the same matrix Authorize uses (no drift).
func TestCapabilities_ForRole(t *testing.T) {
	cases := map[Role][]Operation{
		RoleReader: {OpRead, OpMaterialize, OpForkConversation},
		RoleContributor: {
			OpRead, OpMaterialize, OpForkConversation, OpCreateArtifact, OpEditArtifact,
		},
		RoleEditor: {
			OpRead, OpMaterialize, OpForkConversation, OpCreateArtifact, OpEditArtifact, OpDeleteArtifact,
		},
		RoleAdmin: {
			OpRead, OpMaterialize, OpForkConversation, OpCreateArtifact, OpEditArtifact, OpDeleteArtifact,
			OpAddMember, OpRemoveMember, OpChangeMemberRole, OpSetNamespacePolicy,
		},
		RoleOwner: {
			OpRead, OpMaterialize, OpForkConversation, OpCreateArtifact, OpEditArtifact, OpDeleteArtifact,
			OpAddMember, OpRemoveMember, OpChangeMemberRole, OpSetNamespacePolicy, OpDeleteNamespace,
		},
	}
	for role, wantOps := range cases {
		got := Capabilities(role)
		if !sameOpSet(got, wantOps) {
			t.Errorf("Capabilities(%s) = %v, want %v", role, got, wantOps)
		}
		// Every capability returned must actually authorize.
		for _, op := range got {
			if err := Authorize(role, op); err != nil {
				t.Errorf("Capabilities(%s) lists %s but Authorize denies it: %v", role, op, err)
			}
		}
	}
}

// TestAuthorize_ReturnsErrForbiddenNotSilent: the deny path yields a typed,
// inspectable error carrying the role + operation, so callers can map it to
// HTTP 403 / a clear CLI message — never a silent allow.
func TestAuthorize_ReturnsErrForbiddenNotSilent(t *testing.T) {
	err := Authorize(RoleReader, OpDeleteNamespace)
	if err == nil {
		t.Fatal("expected denial")
	}
	if !errors.Is(err, ErrForbidden) {
		t.Errorf("error %v is not ErrForbidden", err)
	}
	// The message must name the role and the operation for diagnostics.
	msg := err.Error()
	if !contains(msg, string(RoleReader)) || !contains(msg, string(OpDeleteNamespace)) {
		t.Errorf("error %q does not mention role+op", msg)
	}
}

// TestAuthorize_ContributorOwnArtifact: a Contributor can edit its OWN
// artifact but not someone else's; an Editor+ can edit any. This is the one
// ownership-sensitive rule in the matrix.
func TestAuthorize_ContributorOwnArtifact(t *testing.T) {
	// Contributor editing own artifact: allowed.
	if err := CanArtifact(RoleContributor, OpEditArtifact, true); err != nil {
		t.Errorf("Contributor editing own artifact denied: %v", err)
	}
	// Contributor editing another's artifact: denied.
	if err := CanArtifact(RoleContributor, OpEditArtifact, false); err == nil {
		t.Error("Contributor editing another's artifact should be denied")
	}
	// Contributor deleting own artifact: still denied (delete is Editor+).
	if err := CanArtifact(RoleContributor, OpDeleteArtifact, true); err == nil {
		t.Error("Contributor deleting any artifact (even own) should be denied")
	}
	// Editor editing another's artifact: allowed.
	if err := CanArtifact(RoleEditor, OpEditArtifact, false); err != nil {
		t.Errorf("Editor editing another's artifact denied: %v", err)
	}
	// Reader editing own artifact: denied (Reader cannot edit at all).
	if err := CanArtifact(RoleReader, OpEditArtifact, true); err == nil {
		t.Error("Reader editing artifact should be denied")
	}
}

func sameOpSet(a, b []Operation) bool {
	if len(a) != len(b) {
		return false
	}
	as := append([]Operation(nil), a...)
	bs := append([]Operation(nil), b...)
	sort.Slice(as, func(i, j int) bool { return as[i] < as[j] })
	sort.Slice(bs, func(i, j int) bool { return bs[i] < bs[j] })
	for i := range as {
		if as[i] != bs[i] {
			return false
		}
	}
	return true
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
