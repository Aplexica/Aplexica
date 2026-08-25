//go:build tray && linux

package main

import (
	"fmt"
	"os/exec"
)

func platformTerminalCommand(argv []string) (*exec.Cmd, error) {
	type candidate struct {
		name   string
		prefix []string
	}
	for _, terminal := range []candidate{
		{name: "x-terminal-emulator", prefix: []string{"-e"}},
		{name: "gnome-terminal", prefix: []string{"--"}},
		{name: "xterm", prefix: []string{"-e"}},
	} {
		path, err := exec.LookPath(terminal.name)
		if err != nil {
			continue
		}
		args := append(append([]string{}, terminal.prefix...), argv...)
		return exec.Command(path, args...), nil
	}
	return nil, fmt.Errorf("tray: no supported terminal emulator available")
}
