package generationactivation

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/google/uuid"
)

// PendingGate is the read-only runtime barrier between a locally prepared
// generation activation and artifact traffic. A state file is not itself a
// barrier: completed state intentionally remains durable. Only a strictly
// decoded, scope-bound pending attestation blocks traffic.
//
// Corrupt, ambiguous, or path-unsafe state fails closed. Check never creates
// or removes identity state.
type PendingGate struct {
	IdentityRoot string
}

// Check returns nil when scope has no state file or has a completed state. It
// returns ErrPendingActivation for an exact unresolved pending attestation and
// ErrInvalidState (possibly wrapped) for any state that cannot be trusted.
func (g PendingGate) Check(scope string) error {
	root, err := filepath.Abs(g.IdentityRoot)
	if err != nil || root != filepath.Clean(g.IdentityRoot) {
		return fmt.Errorf("%w: invalid identity root", ErrInvalidState)
	}

	expectedNamespace := ""
	dir := filepath.Join(root, "account")
	if scope != "account" {
		parsed, parseErr := uuid.Parse(scope)
		if parseErr != nil || parsed.Version() != 7 || parsed.Variant() != uuid.RFC4122 || parsed.String() != scope {
			return fmt.Errorf("%w: invalid activation scope", ErrInvalidState)
		}
		expectedNamespace = scope
		dir = filepath.Join(root, "namespaces", scope)
	}

	path := filepath.Join(dir, "generation-activation.json")
	info, statErr := os.Lstat(path)
	if errors.Is(statErr, os.ErrNotExist) {
		return nil
	}
	if statErr != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("%w: activation state is not a regular file", ErrInvalidState)
	}
	state, err := (FileStateStore{Path: path}).Load()
	if err != nil {
		return fmt.Errorf("%w: load activation state", errors.Join(ErrInvalidState, err))
	}
	if state.Pending == nil {
		return nil
	}

	signed, err := DecodeCanonical(state.Pending.AttestationBlob)
	if err != nil || signed.Attestation.NamespaceID != expectedNamespace ||
		state.ActivatedBindingDigest == state.Pending.BindingDigest {
		return fmt.Errorf("%w: pending activation is not uniquely scope-bound", ErrInvalidState)
	}
	return ErrPendingActivation
}
