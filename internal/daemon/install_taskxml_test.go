package daemon

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// These tests cover the build-tag-free Task Scheduler XML generator used by
// the Windows installer (install_windows.go). Keeping the generator and its
// tests tag-free lets us TDD the logic on any platform; only the schtasks
// exec wiring is windows-tagged.

func TestGenerateTaskXML_ContainsExpectedFields(t *testing.T) {
	xmlBytes, err := generateTaskXML(InstallOptions{
		AplexicaPath: `C:\Users\Aplexica\aplexica.exe`,
		Dir:          `C:\Users\Aplexica\aplexica-test`,
		StoreRoot:    `C:\Users\Aplexica\.aplexica\store`,
		Recursive:    true,
		Quiet:        "500ms",
	}, `TESTHOST\Aplexica`)
	require.NoError(t, err)
	s := string(xmlBytes)

	// Per-user logon task, not an SCM service.
	require.Contains(t, s, `<Task version="1.2"`)
	require.Contains(t, s, "schemas.microsoft.com/windows/2004/02/mit/task")
	require.Contains(t, s, "<LogonTrigger>")
	require.Contains(t, s, "<LogonType>InteractiveToken</LogonType>")
	require.Contains(t, s, "<RunLevel>LeastPrivilege</RunLevel>")
	require.Contains(t, s, `<UserId>TESTHOST\Aplexica</UserId>`)
	// Long-running daemon: no execution time limit.
	require.Contains(t, s, "<ExecutionTimeLimit>PT0S</ExecutionTimeLimit>")
	// Keep-alive: Task Scheduler restarts the monitored serve on crash.
	require.Contains(t, s, "<RestartOnFailure>")
	require.Contains(t, s, "<Interval>PT1M</Interval>")
	// Action runs the long-lived `daemon serve` directly (so Task Scheduler can
	// monitor + restart it) with --windows-detach-console to suppress the window.
	require.Contains(t, s, `<Command>C:\Users\Aplexica\aplexica.exe</Command>`)
	require.Contains(t, s, "daemon serve")
	require.Contains(t, s, "--windows-detach-console")
	require.Contains(t, s, "--dir")
	require.Contains(t, s, "aplexica-test")
	require.Contains(t, s, "--store")
	require.Contains(t, s, "--recursive")
	require.Contains(t, s, "--quiet")
	require.Contains(t, s, "500ms")
}

func TestGenerateTaskXML_RequiresUserID(t *testing.T) {
	_, err := generateTaskXML(InstallOptions{
		AplexicaPath: `C:\a.exe`,
		Dir:          `C:\p`,
	}, "")
	require.Error(t, err)
}

func TestGenerateTaskXML_EscapesXML(t *testing.T) {
	xmlBytes, err := generateTaskXML(InstallOptions{
		AplexicaPath: `C:\bin\a<b>&c.exe`,
		Dir:          `C:\proj`,
	}, `DOM\user`)
	require.NoError(t, err)
	s := string(xmlBytes)
	require.NotContains(t, s, "a<b>")
	require.Contains(t, s, "a&lt;b&gt;")
	require.Contains(t, s, "&amp;c.exe")
}

func TestGenerateTaskXML_IncludesHermesFlags(t *testing.T) {
	hw := true
	xmlBytes, err := generateTaskXML(InstallOptions{
		AplexicaPath:        `C:\a.exe`,
		Dir:                 `C:\p`,
		HermesWatch:         &hw,
		HermesWatchInterval: "10s",
		HermesDB:            `C:\h\state.db`,
	}, `D\u`)
	require.NoError(t, err)
	s := string(xmlBytes)
	// Single-token =form so a bool flag never swallows the next argument when
	// the Arguments string is re-split by the exe at launch.
	require.Contains(t, s, "--hermes-watch=true")
	require.Contains(t, s, "--hermes-watch-interval")
	require.Contains(t, s, "10s")
	require.Contains(t, s, "--hermes-db")
	require.Contains(t, s, "state.db")
}

func TestGenerateTaskXML_OmitsHermesFlagsWhenUnset(t *testing.T) {
	xmlBytes, err := generateTaskXML(InstallOptions{
		AplexicaPath: `C:\a.exe`,
		Dir:          `C:\p`,
	}, `D\u`)
	require.NoError(t, err)
	s := string(xmlBytes)
	require.NotContains(t, s, "--hermes-watch")
	require.NotContains(t, s, "--hermes-db")
}

func TestQuoteWinArg(t *testing.T) {
	// Bare tokens (flags, no whitespace) pass through unquoted.
	require.Equal(t, "--dir", quoteWinArg("--dir"))
	require.Equal(t, `C:\nospace\path`, quoteWinArg(`C:\nospace\path`))
	// Values containing spaces get double-quoted so CommandLineToArgvW
	// re-splits them as a single argv element.
	require.Equal(t, `"C:\Users\Some One\proj"`, quoteWinArg(`C:\Users\Some One\proj`))
}

func TestUTF16LEWithBOM(t *testing.T) {
	out := utf16LEWithBOM([]byte("A"))
	require.Equal(t, []byte{0xFF, 0xFE, 'A', 0x00}, out)
}
