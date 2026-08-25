//go:build !windows

package main

import "os/exec"

func hideRemotePluginWindow(_ *exec.Cmd) {}

func canLaunchTrayFromCurrentSession() (bool, string) {
	return true, ""
}
