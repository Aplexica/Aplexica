//go:build tray && windows

package main

import (
	"testing"

	"golang.org/x/sys/windows"
)

func TestWindowsTerminalCommandIsDirectNewConsole(t *testing.T) {
	argv, err := terminalArgv([]string{`C:\Program Files\Aplexica\aplexica.exe`, "conflicts", "show", `id&second`})
	if err != nil {
		t.Fatal(err)
	}
	cmd, err := platformTerminalCommand(argv)
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Path != argv[0] {
		t.Fatalf("Path = %q, want exact executable %q", cmd.Path, argv[0])
	}
	if cmd.SysProcAttr == nil || cmd.SysProcAttr.CreationFlags != windows.CREATE_NEW_CONSOLE {
		t.Fatalf("CreationFlags = %#v, want CREATE_NEW_CONSOLE", cmd.SysProcAttr)
	}
	if len(cmd.Args) != len(argv) {
		t.Fatalf("Args = %#v, want %#v", cmd.Args, argv)
	}
	for i := range argv {
		if cmd.Args[i] != argv[i] {
			t.Fatalf("Args[%d] = %q, want %q", i, cmd.Args[i], argv[i])
		}
	}
}
