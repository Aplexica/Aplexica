package acf

import (
	"errors"
	"strings"
	"testing"

	"github.com/aplexica/aplexica/internal/securityerr"
	"github.com/stretchr/testify/require"
)

const validUUIDv7 = "0197f000-aaaa-7000-8000-000000000001"

func TestValidateKind(t *testing.T) {
	for _, kind := range []Kind{KindMemory, KindSkill, KindTool, KindConversation} {
		require.NoError(t, ValidateKind(kind))
	}
	for _, kind := range []Kind{"", "memories", "../memory", "MEMORY"} {
		err := ValidateKind(kind)
		require.ErrorIs(t, err, securityerr.ErrUnsafeIdentifier)
		if kind != "" {
			require.NotContains(t, err.Error(), string(kind))
		}
	}
}

func TestValidateWireIDs(t *testing.T) {
	require.NoError(t, ValidateWireUUIDv7(validUUIDv7))
	require.NoError(t, ValidateCanonicalEventID(validUUIDv7))
	require.NoError(t, ValidateWireEventID(validUUIDv7))
	require.NoError(t, ValidateWireEventID(validUUIDv7+"-r-deadbeef"))

	invalid := []string{
		"", "art-1", "0197F000-AAAA-7000-8000-000000000001",
		"0197f000-aaaa-6000-8000-000000000001",
		validUUIDv7 + "-r", validUUIDv7 + "-r-DEADBEEF", validUUIDv7 + "-r-deadbee",
		validUUIDv7 + "-r-deadbeef-extra", "../" + validUUIDv7,
	}
	for _, value := range invalid {
		err := ValidateWireEventID(value)
		require.True(t, errors.Is(err, securityerr.ErrUnsafeIdentifier), "%q: %v", value, err)
		if value != "" {
			require.NotContains(t, err.Error(), value)
		}
	}
}

func TestValidateBranch(t *testing.T) {
	for _, branch := range []string{"main", "feature-1", "a", strings.Repeat("a", 64)} {
		require.NoError(t, ValidateBranch(branch), branch)
	}
	for _, branch := range []string{"", "Feature", "../main", "a/b", "a b", strings.Repeat("a", 65)} {
		require.ErrorIs(t, ValidateBranch(branch), securityerr.ErrUnsafeIdentifier, branch)
	}
}

func FuzzValidateWireEventID(f *testing.F) {
	f.Add(validUUIDv7)
	f.Add(validUUIDv7 + "-r-deadbeef")
	f.Add("../outside")
	f.Fuzz(func(t *testing.T, value string) {
		if ValidateWireEventID(value) == nil {
			require.True(t, value == validUUIDv7 || retainedWireIDPattern.MatchString(value) || ValidateWireUUIDv7(value) == nil)
		}
	})
}
