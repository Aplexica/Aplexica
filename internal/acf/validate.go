package acf

import (
	"errors"
	"fmt"
	"regexp"

	"github.com/aplexica/aplexica/internal/safepath"
	"github.com/aplexica/aplexica/internal/securityerr"
	"github.com/google/uuid"
)

var (
	canonicalBranchPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)
	retainedWireIDPattern  = regexp.MustCompile(`^([0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12})-r-([0-9a-f]{8})$`)
)

// ValidateKind rejects unknown wire/store kinds before they can select a path.
func ValidateKind(kind Kind) error {
	switch kind {
	case KindMemory, KindSkill, KindTool, KindConversation:
		return nil
	default:
		return fmt.Errorf("acf: kind: %w", securityerr.ErrUnsafeIdentifier)
	}
}

// ValidateStoreComponent exposes the common platform-independent component
// rules to ACF callers.
func ValidateStoreComponent(component string) error {
	return safepath.ValidateStoreComponent(component)
}

// ValidateWireUUIDv7 requires canonical lowercase RFC 9562 UUIDv7 text.
func ValidateWireUUIDv7(value string) error {
	parsed, err := uuid.Parse(value)
	if err != nil || parsed.Version() != 7 || parsed.Variant() != uuid.RFC4122 || parsed.String() != value {
		return fmt.Errorf("acf: uuidv7: %w", securityerr.ErrUnsafeIdentifier)
	}
	return nil
}

func ValidateCanonicalEventID(value string) error {
	return ValidateWireUUIDv7(value)
}

// ValidateWireEventID accepts a canonical event UUIDv7 or the exact v2
// retained companion grammar UUIDv7-r-<8 lowercase hex origin digest>.
func ValidateWireEventID(value string) error {
	if err := ValidateWireUUIDv7(value); err == nil {
		return nil
	}
	match := retainedWireIDPattern.FindStringSubmatch(value)
	if len(match) != 3 {
		return fmt.Errorf("acf: wire event id: %w", securityerr.ErrUnsafeIdentifier)
	}
	if err := ValidateWireUUIDv7(match[1]); err != nil {
		return errors.Join(securityerr.ErrUnsafeIdentifier, err)
	}
	return nil
}

// ValidateBranch validates an already canonical branch. User-entered branch
// names may first pass through NormalizeBranchName, but remote/store input must
// never be normalized into a different identity at the trust boundary.
func ValidateBranch(branch string) error {
	if branch == MainBranch || canonicalBranchPattern.MatchString(branch) {
		return nil
	}
	return fmt.Errorf("acf: branch: %w", securityerr.ErrUnsafeIdentifier)
}
