//go:build tray

package main

import (
	_ "embed"
	"runtime"
)

//go:embed assets/idle.png
var iconIdlePNG []byte

//go:embed assets/active.png
var iconActivePNG []byte

//go:embed assets/paused.png
var iconPausedPNG []byte

//go:embed assets/conflict.png
var iconConflictPNG []byte

//go:embed assets/error.png
var iconErrorPNG []byte

//go:embed assets/idle.ico
var iconIdleICO []byte

//go:embed assets/active.ico
var iconActiveICO []byte

//go:embed assets/paused.ico
var iconPausedICO []byte

//go:embed assets/conflict.ico
var iconConflictICO []byte

//go:embed assets/error.ico
var iconErrorICO []byte

// regularIconFor returns the bytes that systray.SetIcon (the non-
// template path) would use on the current platform: .ico on Windows,
// .png elsewhere.
func regularIconFor(s TrayState) []byte {
	if runtime.GOOS == "windows" {
		switch s {
		case StateIdle:
			return iconIdleICO
		case StateActive:
			return iconActiveICO
		case StatePaused:
			return iconPausedICO
		case StateConflict:
			return iconConflictICO
		default:
			return iconErrorICO
		}
	}
	switch s {
	case StateIdle:
		return iconIdlePNG
	case StateActive:
		return iconActivePNG
	case StatePaused:
		return iconPausedPNG
	case StateConflict:
		return iconConflictPNG
	default:
		return iconErrorPNG
	}
}
