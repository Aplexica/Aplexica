//go:build tray

package main

import (
	"fmt"
	"unicode/utf8"
)

const waitBeforeExitFlag = "--wait-before-exit"

// openTerminalRun launches Aplexica with an exact argv vector. Platform code
// may open a terminal host, but it never receives an interpolated command
// string. The fixed wait flag keeps the terminal visible without cmd /k or an
// interactive shell.
func openTerminalRun(argv ...string) error {
	launchArgv, err := terminalArgv(argv)
	if err != nil {
		return err
	}
	cmd, err := platformTerminalCommand(launchArgv)
	if err != nil {
		return err
	}
	return cmd.Start()
}

func terminalArgv(argv []string) ([]string, error) {
	if err := validateLaunchArgv(argv); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(argv)+1)
	out = append(out, argv[0], waitBeforeExitFlag)
	out = append(out, argv[1:]...)
	return out, nil
}

func validateLaunchArgv(argv []string) error {
	if len(argv) == 0 || argv[0] == "" {
		return fmt.Errorf("tray: launch executable is empty")
	}
	for _, value := range argv {
		if !utf8.ValidString(value) {
			return fmt.Errorf("tray: launch argument is not valid UTF-8")
		}
		for _, r := range value {
			if r == 0 || r < 0x20 || (r >= 0x7f && r <= 0x9f) {
				return fmt.Errorf("tray: launch argument contains a control character")
			}
		}
	}
	return nil
}
