//go:build !windows

package kilo

import "os/exec"

func hideImportWindow(_ *exec.Cmd) {}
