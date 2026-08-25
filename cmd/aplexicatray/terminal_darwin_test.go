//go:build tray && darwin

package main

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDarwinTerminalCommandKeepsDataOutOfAppleScriptSource(t *testing.T) {
	marker := `id\" & do shell script "touch /tmp/pwned" & "`
	argv, err := terminalArgv([]string{"/path/aplexica", "conflicts", "show", marker})
	require.NoError(t, err)
	cmd, err := platformTerminalCommand(argv)
	require.NoError(t, err)
	require.Equal(t, "osascript", filepath.Base(cmd.Path))
	require.Equal(t, "-e", cmd.Args[1])
	require.Equal(t, macTerminalScript, cmd.Args[2])
	require.NotContains(t, cmd.Args[2], marker)
	require.Equal(t, marker, cmd.Args[len(cmd.Args)-1])
}
