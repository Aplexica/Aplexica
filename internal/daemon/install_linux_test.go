//go:build linux

package daemon

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSystemdInstaller_GeneratedUnitContainsExpectedSections(t *testing.T) {
	inst := &systemdInstaller{opts: InstallOptions{
		AplexicaPath: "/usr/local/bin/aplexica",
		Dir:          "/proj",
		StoreRoot:    "/var/aplexica/store",
		Recursive:    true,
		Quiet:        "500ms",
	}}
	unit := inst.generateUnit()

	require.Contains(t, unit, "[Unit]")
	require.Contains(t, unit, "Description=Aplexica sync daemon")
	require.Contains(t, unit, "[Service]")
	require.Contains(t, unit, "Type=simple")
	require.Contains(t, unit, "Restart=on-failure")
	require.Contains(t, unit, "[Install]")
	require.Contains(t, unit, "WantedBy=default.target")

	// ExecStart contains the binary path + the expected args.
	lines := strings.Split(unit, "\n")
	var execStart string
	for _, l := range lines {
		if strings.HasPrefix(l, "ExecStart=") {
			execStart = l
			break
		}
	}
	require.NotEmpty(t, execStart)
	require.Contains(t, execStart, "/usr/local/bin/aplexica")
	require.Contains(t, execStart, "daemon serve")
	require.Contains(t, execStart, "--dir /proj")
	require.Contains(t, execStart, "--store /var/aplexica/store")
	require.Contains(t, execStart, "--recursive")
	require.Contains(t, execStart, "--quiet 500ms")
}

func TestSystemdInstaller_UnitIncludesHermesFlags(t *testing.T) {
	hw := false
	inst := &systemdInstaller{opts: InstallOptions{
		AplexicaPath:        "/usr/local/bin/aplexica",
		Dir:                 "/tmp/proj",
		HermesWatch:         &hw,
		HermesWatchInterval: "30s",
		HermesDB:            "/var/lib/state.db",
	}}
	unit := inst.generateUnit()
	require.Contains(t, unit, "--hermes-watch=false")
	require.Contains(t, unit, "--hermes-watch-interval 30s")
	require.Contains(t, unit, "--hermes-db /var/lib/state.db")
}

func TestSystemdInstaller_UnitOmitsHermesFlagsWhenUnset(t *testing.T) {
	inst := &systemdInstaller{opts: InstallOptions{
		AplexicaPath: "/usr/local/bin/aplexica",
		Dir:          "/tmp/proj",
	}}
	unit := inst.generateUnit()
	require.NotContains(t, unit, "--hermes-watch")
	require.NotContains(t, unit, "--hermes-db")
}
