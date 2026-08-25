package daemon

import (
	"encoding/binary"
	"encoding/xml"
	"fmt"
	"strings"
	"unicode/utf16"
)

// This file holds the Windows Task Scheduler XML generator and helpers. It is
// deliberately build-tag-free: the logic is pure string assembly with no
// windows-only imports, so it compiles and is unit-tested on every platform.
// The windows-only installer (install_windows.go) calls generateTaskXML and
// shells the result to `schtasks /create /xml`.
//
// Why a per-user logon task instead of an SCM service: the macOS (launchd
// LaunchAgent) and Linux (systemd --user) installers both register the daemon
// as the *logged-in user* so os.UserHomeDir() resolves to that user's home and
// the adapters discover the user's agents. A Windows SCM service runs as
// LocalSystem, whose home is ...\config\systemprofile, so it would discover no
// agents. A logon-triggered Scheduled Task with an InteractiveToken principal
// restores the per-user semantics (runs as the user, no admin, no stored
// password) — see install_windows.go.

// scheduledTaskName is the Task Scheduler task registered by the Windows
// installer. Tag-free so both the installer and its tests can reference it.
const scheduledTaskName = `Aplexica Sync Daemon`

// generateTaskXML builds a Task Scheduler v1.2 task definition for a per-user,
// logon-triggered daemon. userID is the principal/trigger account in
// DOMAIN\User (or .\User) form; it must be non-empty.
//
// The returned bytes are UTF-8 with a `<?xml ... encoding="UTF-16"?>`
// declaration: the installer re-encodes them to UTF-16LE+BOM (via
// utf16LEWithBOM) before handing the file to schtasks, which expects UTF-16.
// Tests assert on the UTF-8 logical content.
func generateTaskXML(opts InstallOptions, userID string) ([]byte, error) {
	if strings.TrimSpace(userID) == "" {
		return nil, fmt.Errorf("daemon: scheduled task needs a principal user id")
	}

	// The action runs the long-lived `daemon serve` directly so Task Scheduler
	// monitors it and RestartOnFailure (in Settings) re-launches it within ~1m
	// of a crash — Windows keep-alive parity with launchd KeepAlive / systemd
	// Restart=on-failure. `--windows-detach-console` (appended below) makes
	// serve FreeConsole on startup so the Task-Scheduler-launched console
	// process shows no window. We pass the core flags; serve fills its own
	// defaults for the rest and also reads <state-dir>/config.json (same
	// effective config as the old `daemon start` action).
	args := []string{"daemon", "serve", "--dir", opts.Dir}
	if opts.StoreRoot != "" {
		args = append(args, "--store", opts.StoreRoot)
	}
	if opts.SecretsRoot != "" {
		args = append(args, "--secrets-root", opts.SecretsRoot)
	}
	if opts.StateDir != "" {
		args = append(args, "--state-dir", opts.StateDir)
	}
	if opts.LogDir != "" {
		args = append(args, "--log-dir", opts.LogDir)
	}
	if opts.Quiet != "" {
		args = append(args, "--quiet", opts.Quiet)
	}
	if opts.GuardWindow != "" {
		args = append(args, "--guard-window", opts.GuardWindow)
	}
	if opts.Recursive {
		args = append(args, "--recursive")
	}
	if opts.HermesWatch != nil {
		// =form: a bare bool flag followed by a separate "true"/"false" token
		// would leave that token as a stray positional once the single
		// Arguments string is re-split at launch.
		if *opts.HermesWatch {
			args = append(args, "--hermes-watch=true")
		} else {
			args = append(args, "--hermes-watch=false")
		}
	}
	if opts.HermesWatchInterval != "" {
		args = append(args, "--hermes-watch-interval", opts.HermesWatchInterval)
	}
	if opts.HermesDB != "" {
		args = append(args, "--hermes-db", opts.HermesDB)
	}
	// Windows-only effect: serve calls FreeConsole on startup so the
	// Task-Scheduler-launched serve has no visible console window.
	args = append(args, "--windows-detach-console")

	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = quoteWinArg(a)
	}
	argLine := strings.Join(quoted, " ")

	body := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <RegistrationInfo>
    <Description>Aplexica cross-agent state portability daemon (per-user; auto-starts at logon).</Description>
    <Author>%s</Author>
  </RegistrationInfo>
  <Triggers>
    <LogonTrigger>
      <Enabled>true</Enabled>
      <UserId>%s</UserId>
    </LogonTrigger>
  </Triggers>
  <Principals>
    <Principal id="Author">
      <UserId>%s</UserId>
      <LogonType>InteractiveToken</LogonType>
      <RunLevel>LeastPrivilege</RunLevel>
    </Principal>
  </Principals>
  <Settings>
    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>
    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>
    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>
    <AllowHardTerminate>true</AllowHardTerminate>
    <StartWhenAvailable>true</StartWhenAvailable>
    <RunOnlyIfNetworkAvailable>false</RunOnlyIfNetworkAvailable>
    <AllowStartOnDemand>true</AllowStartOnDemand>
    <Enabled>true</Enabled>
    <Hidden>false</Hidden>
    <RunOnlyIfIdle>false</RunOnlyIfIdle>
    <WakeToRun>false</WakeToRun>
    <ExecutionTimeLimit>PT0S</ExecutionTimeLimit>
    <Priority>7</Priority>
    <RestartOnFailure>
      <Interval>PT1M</Interval>
      <Count>999</Count>
    </RestartOnFailure>
  </Settings>
  <Actions Context="Author">
    <Exec>
      <Command>%s</Command>
      <Arguments>%s</Arguments>
    </Exec>
  </Actions>
</Task>
`,
		xmlEscape(userID),
		xmlEscape(userID),
		xmlEscape(userID),
		xmlEscape(opts.AplexicaPath),
		xmlEscape(argLine),
	)
	return []byte(body), nil
}

// xmlEscape returns the XML-text-escaped form of s (escaping & < > " ').
func xmlEscape(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

// quoteWinArg wraps a command-line argument in double quotes when it contains
// whitespace or a quote, escaping internal quotes per the standard MSVCRT /
// CommandLineToArgvW rules so the value re-splits as one argv element. Tokens
// with no whitespace (flag names, durations) pass through unchanged.
func quoteWinArg(s string) string {
	if s == "" {
		return `""`
	}
	needsQuoting := false
	for _, r := range s {
		if r == ' ' || r == '\t' || r == '"' {
			needsQuoting = true
			break
		}
	}
	if !needsQuoting {
		return s
	}
	var b strings.Builder
	b.WriteByte('"')
	for _, c := range s {
		if c == '"' {
			b.WriteByte('\\')
		}
		b.WriteRune(c)
	}
	b.WriteByte('"')
	return b.String()
}

// utf16LEWithBOM re-encodes UTF-8 bytes to UTF-16LE with a byte-order mark.
// schtasks /create /xml expects a UTF-16 task definition file. The BOM is
// emitted by prepending the U+FEFF rune and letting binary.LittleEndian write
// each code unit, so there are no raw byte-order literals here.
func utf16LEWithBOM(b []byte) []byte {
	units := utf16.Encode([]rune("\uFEFF" + string(b)))
	out := make([]byte, 0, len(units)*2)
	for _, u := range units {
		out = binary.LittleEndian.AppendUint16(out, u)
	}
	return out
}
