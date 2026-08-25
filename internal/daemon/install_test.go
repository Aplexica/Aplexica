package daemon

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestInstallOptions_ValidateRequiresBinaryAndDir(t *testing.T) {
	require.Error(t, InstallOptions{}.Validate())
	require.Error(t, InstallOptions{AplexicaPath: "/usr/local/bin/aplexica"}.Validate())
	require.Error(t, InstallOptions{Dir: "/proj"}.Validate())
	require.NoError(t, InstallOptions{
		AplexicaPath: "/usr/local/bin/aplexica",
		Dir:          "/proj",
	}.Validate())
}

func TestNew_ReturnsInstallerForCurrentPlatform(t *testing.T) {
	inst, err := New(InstallOptions{
		AplexicaPath: "/usr/local/bin/aplexica",
		Dir:          "/proj",
	})
	require.NoError(t, err)
	require.NotNil(t, inst)
	require.NotEmpty(t, inst.PlatformLabel())
}

func TestErrNotSupported_IsRecognizable(t *testing.T) {
	// Direct verification that callers can match against ErrNotSupported
	// via errors.Is when a stub returns it.
	wrapped := errors.Join(ErrNotSupported, errors.New("extra context"))
	require.True(t, errors.Is(wrapped, ErrNotSupported))
}
