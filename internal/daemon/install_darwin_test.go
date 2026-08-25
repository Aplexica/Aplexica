//go:build darwin

package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLaunchdInstaller_GeneratedPlistContainsExpectedFields(t *testing.T) {
	inst := &launchdInstaller{opts: InstallOptions{
		AplexicaPath: "/usr/local/bin/aplexica",
		Dir:          "/proj/foo",
		StoreRoot:    "/store",
		Recursive:    true,
		Quiet:        "500ms",
	}}
	plist, err := inst.generatePlist()
	require.NoError(t, err)
	s := string(plist)

	require.Contains(t, s, "<key>Label</key>")
	require.Contains(t, s, "<string>com.aplexica.aplexicad</string>")
	require.Contains(t, s, "<key>ProgramArguments</key>")
	require.Contains(t, s, "<string>/usr/local/bin/aplexica</string>")
	require.Contains(t, s, "<string>daemon</string>")
	require.Contains(t, s, "<string>serve</string>")
	require.Contains(t, s, "<string>--dir</string>")
	require.Contains(t, s, "<string>/proj/foo</string>")
	require.Contains(t, s, "<string>--store</string>")
	require.Contains(t, s, "<string>/store</string>")
	require.Contains(t, s, "<string>--recursive</string>")
	require.Contains(t, s, "<string>--quiet</string>")
	require.Contains(t, s, "<string>500ms</string>")
	require.Contains(t, s, "<key>RunAtLoad</key>")
	require.Contains(t, s, "<key>KeepAlive</key>")
}

func TestLaunchdInstaller_GeneratedPlist_EscapesXMLInPaths(t *testing.T) {
	// Pathological path with characters that need XML escaping.
	inst := &launchdInstaller{opts: InstallOptions{
		AplexicaPath: "/path/with<weird>&chars",
		Dir:          "/proj",
	}}
	plist, err := inst.generatePlist()
	require.NoError(t, err)
	s := string(plist)
	require.NotContains(t, s, "<weird>")
	require.Contains(t, s, "&lt;weird&gt;")
	require.Contains(t, s, "&amp;chars")
}

func TestLaunchdInstaller_PlistIncludesHermesFlags(t *testing.T) {
	hw := true
	inst := &launchdInstaller{opts: InstallOptions{
		AplexicaPath:        "/usr/local/bin/aplexica",
		Dir:                 "/tmp/proj",
		HermesWatch:         &hw,
		HermesWatchInterval: "10s",
		HermesDB:            "/custom/state.db",
	}}
	plist, err := inst.generatePlist()
	require.NoError(t, err)
	s := string(plist)
	require.Contains(t, s, "<string>--hermes-watch</string>")
	require.Contains(t, s, "<string>true</string>")
	require.Contains(t, s, "<string>--hermes-watch-interval</string>")
	require.Contains(t, s, "<string>10s</string>")
	require.Contains(t, s, "<string>--hermes-db</string>")
	require.Contains(t, s, "<string>/custom/state.db</string>")
}

func TestLaunchdInstaller_PlistOmitsHermesFlagsWhenUnset(t *testing.T) {
	inst := &launchdInstaller{opts: InstallOptions{
		AplexicaPath: "/usr/local/bin/aplexica",
		Dir:          "/tmp/proj",
		// HermesWatch nil, HermesWatchInterval/HermesDB empty
	}}
	plist, err := inst.generatePlist()
	require.NoError(t, err)
	s := string(plist)
	require.NotContains(t, s, "--hermes-watch")
	require.NotContains(t, s, "--hermes-db")
}

func TestLaunchdInstaller_WritePlist_CreatesFile(t *testing.T) {
	dir := t.TempDir()
	inst := &launchdInstaller{
		opts: InstallOptions{
			AplexicaPath: "/bin/aplexica",
			Dir:          "/proj",
		},
		plistDirOverride: dir,
	}
	// Bypass launchctl by directly writing — we test the plist path / write
	// half without invoking launchctl on the test runner.
	plistPath := inst.plistPath()
	content, err := inst.generatePlist()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(plistPath, content, 0o644))

	_, err = os.Stat(plistPath)
	require.NoError(t, err)
	require.True(t, strings.HasSuffix(plistPath, "com.aplexica.aplexicad.plist"))
	require.Equal(t, filepath.Join(dir, "com.aplexica.aplexicad.plist"), plistPath)
}
