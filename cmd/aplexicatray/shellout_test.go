//go:build tray

package main

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTerminalArgvPreservesExactArgumentsAndAddsFixedWaitFlag(t *testing.T) {
	input := []string{"/Applications/Aplexica/a plexica", "conflicts", "show", `id&\"$%💾`}
	got, err := terminalArgv(input)
	require.NoError(t, err)
	require.Equal(t, []string{
		"/Applications/Aplexica/a plexica", "--wait-before-exit",
		"conflicts", "show", `id&\"$%💾`,
	}, got)
	require.Equal(t, `id&\"$%💾`, input[3], "input argv must not be mutated")
}

func TestTerminalArgvRejectsControlCharactersAndInvalidUTF8(t *testing.T) {
	for _, bad := range []string{"line\nnext", "carriage\rreturn", "nul\x00byte", string([]byte{0xff})} {
		_, err := terminalArgv([]string{"aplexica", bad})
		require.Error(t, err)
		require.NotContains(t, err.Error(), bad)
	}
}

func TestSafeProjectID(t *testing.T) {
	require.True(t, safeProjectID("github.com/owner/repo"))
	require.True(t, safeProjectID("local:7448b9:watched"))
	require.True(t, safeProjectID("git@github.com:owner/repo"))
	require.False(t, safeProjectID("github.com/o/repo$(rm -rf ~)"))
	require.False(t, safeProjectID("a&b"))
	require.False(t, safeProjectID("a b"))
	require.False(t, safeProjectID(""))
}
