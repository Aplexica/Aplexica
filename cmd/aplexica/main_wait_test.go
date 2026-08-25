package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestWaitForEnterWaitsAndPrintsPrompt(t *testing.T) {
	var out bytes.Buffer
	waitForEnter(strings.NewReader("\n"), &out)
	require.Equal(t, "Press Enter to close…", out.String())
}

func TestWaitBeforeExitFlagIsHidden(t *testing.T) {
	flag := rootCmd.PersistentFlags().Lookup("wait-before-exit")
	require.NotNil(t, flag)
	require.True(t, flag.Hidden)
}
