package main

import (
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

// argValue returns the token following the first occurrence of flag in
// args, or "" if flag isn't present (or has no following token).
func argValue(args []string, flag string) (string, bool) {
	for i, a := range args {
		if a == flag {
			if i+1 < len(args) {
				return args[i+1], true
			}
			return "", true
		}
	}
	return "", false
}

func argContains(args []string, tok string) bool {
	for _, a := range args {
		if a == tok {
			return true
		}
	}
	return false
}

// newServeArgsTestCmd builds a throwaway command carrying just the
// optional self-exec flags buildDaemonServeArgs inspects via Changed(),
// so tests don't mutate the shared daemonStartCmd's flag state.
func newServeArgsTestCmd() *cobra.Command {
	c := &cobra.Command{Use: "start"}
	c.Flags().DurationVar(&daemonProjectScanInterval, "project-scan-interval", 60*time.Minute, "")
	c.Flags().StringSliceVar(&daemonProjectScanRoots, "project-scan-roots", nil, "")
	c.Flags().IntVar(&daemonProjectScanMaxDepth, "project-scan-max-depth", 6, "")
	c.Flags().IntVar(&daemonBranchAutoArchiveAfterDays, "branch-auto-archive-after-days", 90, "")
	c.Flags().DurationVar(&daemonBranchAutoArchiveInterval, "branch-auto-archive-interval", 24*time.Hour, "")
	c.Flags().StringSliceVarP(&daemonCLISets, "config-set", "c", nil, "")
	return c
}

// TestBuildDaemonServeArgs_ForwardsChangedScanAndArchiveFlags asserts the
// self-exec child arg list forwards the project-scan / branch-auto-archive
// / --config-set flags when they were explicitly set on `daemon start`.
// These used to be silently dropped: parsed on start but never passed to
// the serving child, so the override was lost.
func TestBuildDaemonServeArgs_ForwardsChangedScanAndArchiveFlags(t *testing.T) {
	t.Cleanup(func() {
		daemonProjectScanInterval = 0
		daemonProjectScanRoots = nil
		daemonProjectScanMaxDepth = 0
		daemonBranchAutoArchiveAfterDays = 0
		daemonBranchAutoArchiveInterval = 0
		daemonCLISets = nil
	})
	cmd := newServeArgsTestCmd()
	require.NoError(t, cmd.Flags().Set("project-scan-interval", "30m"))
	require.NoError(t, cmd.Flags().Set("project-scan-roots", "/a,/b"))
	require.NoError(t, cmd.Flags().Set("project-scan-max-depth", "9"))
	require.NoError(t, cmd.Flags().Set("branch-auto-archive-after-days", "30"))
	require.NoError(t, cmd.Flags().Set("branch-auto-archive-interval", "12h"))
	require.NoError(t, cmd.Flags().Set("config-set", "retention.grace_period_days=14"))

	args := buildDaemonServeArgs(cmd)

	v, ok := argValue(args, "--project-scan-interval")
	require.True(t, ok, "args: %v", args)
	require.Equal(t, "30m0s", v)

	v, ok = argValue(args, "--project-scan-roots")
	require.True(t, ok)
	require.Equal(t, "/a,/b", v)

	v, ok = argValue(args, "--project-scan-max-depth")
	require.True(t, ok)
	require.Equal(t, "9", v)

	v, ok = argValue(args, "--branch-auto-archive-after-days")
	require.True(t, ok)
	require.Equal(t, "30", v)

	v, ok = argValue(args, "--branch-auto-archive-interval")
	require.True(t, ok)
	require.Equal(t, "12h0m0s", v)

	require.True(t, argContains(args, "--config-set=retention.grace_period_days=14"),
		"expected --config-set forwarded; args: %v", args)
}

// TestBuildDaemonServeArgs_OmitsUnsetFlags asserts that when the optional
// flags are NOT set, they are not forwarded (the child keeps its own
// defaults / TOML config).
func TestBuildDaemonServeArgs_OmitsUnsetFlags(t *testing.T) {
	t.Cleanup(func() {
		daemonProjectScanRoots = nil
		daemonBranchAutoArchiveAfterDays = 0
	})
	cmd := newServeArgsTestCmd()
	args := buildDaemonServeArgs(cmd)
	_, ok := argValue(args, "--project-scan-roots")
	require.False(t, ok, "unset --project-scan-roots must not be forwarded; args: %v", args)
	_, ok = argValue(args, "--branch-auto-archive-after-days")
	require.False(t, ok, "unset --branch-auto-archive-after-days must not be forwarded; args: %v", args)
}
