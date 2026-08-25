package main

import (
	"bytes"
	"regexp"
	"testing"

	"github.com/spf13/cobra"
)

func TestCLIHelpDoesNotExposeInternalSpecReferences(t *testing.T) {
	forbidden := []*regexp.Regexp{
		regexp.MustCompile(`\bBRD-\d`),
		regexp.MustCompile(`\bFR-\d`),
		regexp.MustCompile(`\bNFR-\d`),
		regexp.MustCompile(`\bADR-\d`),
		regexp.MustCompile(`§`),
	}

	var walk func(*cobra.Command)
	walk = func(cmd *cobra.Command) {
		if cmd.Hidden {
			return
		}
		var buf bytes.Buffer
		cmd.SetOut(&buf)
		cmd.SetErr(&buf)
		defer cmd.SetOut(nil)
		defer cmd.SetErr(nil)

		if err := cmd.Help(); err != nil {
			t.Fatalf("%s help failed: %v", cmd.CommandPath(), err)
		}
		help := buf.String()
		for _, re := range forbidden {
			if loc := re.FindStringIndex(help); loc != nil {
				start := loc[0] - 80
				if start < 0 {
					start = 0
				}
				end := loc[1] + 80
				if end > len(help) {
					end = len(help)
				}
				t.Fatalf("%s help contains internal spec reference %q near:\n%s",
					cmd.CommandPath(), re.String(), help[start:end])
			}
		}

		for _, child := range cmd.Commands() {
			walk(child)
		}
	}

	walk(rootCmd)
}
