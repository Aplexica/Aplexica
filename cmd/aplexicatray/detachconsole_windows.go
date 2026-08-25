//go:build tray && windows

package main

import "syscall"

// detachTrayConsole frees the console window Windows attaches to the
// console-subsystem tray binary, so the background indicator shows no window.
// Best-effort: when no console is attached FreeConsole just fails (ignored).
func detachTrayConsole() {
	_, _, _ = syscall.NewLazyDLL("kernel32.dll").NewProc("FreeConsole").Call()
}
