//go:build linux

package daemon

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// execStartLine extracts the single ExecStart= line from a generated unit.
func execStartLine(t *testing.T, unit string) string {
	t.Helper()
	for _, l := range strings.Split(unit, "\n") {
		if strings.HasPrefix(l, "ExecStart=") {
			return l
		}
	}
	t.Fatalf("no ExecStart= line in unit:\n%s", unit)
	return ""
}

// TestSystemdInstaller_QuotesArgsWithSpaces verifies that a watched --dir
// containing whitespace is emitted as a single quoted systemd token rather
// than being split into multiple argv words. systemd applies its own
// word-splitting + quoting rules to ExecStart, so an unquoted
// `--dir /home/u/My Projects` would reach the daemon as two positional
// tokens and watch the wrong directory.
func TestSystemdInstaller_QuotesArgsWithSpaces(t *testing.T) {
	inst := &systemdInstaller{opts: InstallOptions{
		AplexicaPath: "/usr/local/bin/aplexica",
		Dir:          "/home/u/My Projects",
		StoreRoot:    "/var/My Store",
	}}
	unit := inst.generateUnit()
	execStart := execStartLine(t, unit)

	// The space-bearing values must appear in their double-quoted form, not
	// bare. systemd unquotes "..." back to a single argument.
	require.Contains(t, execStart, `--dir "/home/u/My Projects"`,
		"space-bearing --dir must be quoted as one systemd token")
	require.Contains(t, execStart, `--store "/var/My Store"`,
		"space-bearing --store must be quoted as one systemd token")
	// The bare, unsplit form must NOT be present.
	require.NotContains(t, execStart, `--dir /home/u/My Projects`,
		"unquoted space-bearing --dir would split into two argv tokens")
}

// TestSystemdInstaller_QuotesBinaryPathWithSpaces verifies the binary path
// itself is quoted when it contains whitespace.
func TestSystemdInstaller_QuotesBinaryPathWithSpaces(t *testing.T) {
	inst := &systemdInstaller{opts: InstallOptions{
		AplexicaPath: "/opt/My Tools/aplexica",
		Dir:          "/proj",
	}}
	unit := inst.generateUnit()
	execStart := execStartLine(t, unit)

	require.Contains(t, execStart, `ExecStart="/opt/My Tools/aplexica"`,
		"space-bearing binary path must be quoted")
}

// TestSystemdInstaller_DoesNotQuotePlainTokens verifies that tokens without
// whitespace are passed through unchanged (no gratuitous quoting), keeping
// the unit readable and matching the existing assertions.
func TestSystemdInstaller_DoesNotQuotePlainTokens(t *testing.T) {
	inst := &systemdInstaller{opts: InstallOptions{
		AplexicaPath: "/usr/local/bin/aplexica",
		Dir:          "/proj",
		StoreRoot:    "/var/aplexica/store",
		Recursive:    true,
		Quiet:        "500ms",
	}}
	unit := inst.generateUnit()
	execStart := execStartLine(t, unit)

	require.Contains(t, execStart, "/usr/local/bin/aplexica")
	require.Contains(t, execStart, "daemon serve")
	require.Contains(t, execStart, "--dir /proj")
	require.Contains(t, execStart, "--store /var/aplexica/store")
	require.Contains(t, execStart, "--recursive")
	require.Contains(t, execStart, "--quiet 500ms")
	require.NotContains(t, execStart, `"/proj"`, "plain token must not be quoted")
}
