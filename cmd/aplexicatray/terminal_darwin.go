//go:build tray && darwin

package main

import "os/exec"

// macTerminalScript is immutable program text. Every dynamic value is passed
// through osascript argv and quoted by AppleScript itself; Go never inserts a
// path or argument into the source language.
const macTerminalScript = `on run argv
set commandText to ""
repeat with argValue in argv
if commandText is not "" then set commandText to commandText & " "
set commandText to commandText & quoted form of (contents of argValue)
end repeat
tell application "Terminal"
do script commandText
activate
end tell
end run`

func platformTerminalCommand(argv []string) (*exec.Cmd, error) {
	args := make([]string, 0, len(argv)+3)
	args = append(args, "-e", macTerminalScript, "--")
	args = append(args, argv...)
	return exec.Command("osascript", args...), nil
}
