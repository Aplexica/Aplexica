//go:build tray && !windows

package main

// detachTrayConsole is a no-op on non-windows (no console-window concept).
func detachTrayConsole() {}
