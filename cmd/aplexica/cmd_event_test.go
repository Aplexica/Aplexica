package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/stretchr/testify/require"
)

func runEventCmd(t *testing.T, args ...string) (string, error) {
	t.Helper()
	t.Cleanup(func() {
		eventStoreRoot = ""
		eventTagJSON = false
	})
	return runRoot(t, append([]string{"event"}, args...)...)
}

func TestEventTag_AddListRemove(t *testing.T) {
	tmp := t.TempDir()
	storeRoot := filepath.Join(tmp, "store")
	id := seedMemoryArtifact(t, storeRoot, "claude-code", "# x\n")
	store := &acf.Store{Root: storeRoot}
	events, _ := store.ReadEvents(acf.KindMemory, id)
	ev := events[0]

	out, err := runEventCmd(t, "tag", "add", ev.EventID, "decision-point", "--store", storeRoot)
	require.NoError(t, err, "add:\n%s", out)
	require.Contains(t, out, "added")

	out, err = runEventCmd(t, "tag", "list", ev.EventID, "--store", storeRoot)
	require.NoError(t, err)
	require.Contains(t, out, "decision-point")

	out, err = runEventCmd(t, "tag", "list-all", id, "--store", storeRoot)
	require.NoError(t, err)
	require.Contains(t, out, "decision-point")

	out, err = runEventCmd(t, "tag", "remove", ev.EventID, "decision-point", "--store", storeRoot)
	require.NoError(t, err, "remove:\n%s", out)
	require.Contains(t, out, "removed")

	out, err = runEventCmd(t, "tag", "list", ev.EventID, "--store", storeRoot)
	require.NoError(t, err)
	require.Contains(t, out, "no tags")
}

func TestEventTag_RejectsReservedNamespace(t *testing.T) {
	tmp := t.TempDir()
	storeRoot := filepath.Join(tmp, "store")
	id := seedMemoryArtifact(t, storeRoot, "claude-code", "# x\n")
	store := &acf.Store{Root: storeRoot}
	events, _ := store.ReadEvents(acf.KindMemory, id)
	ev := events[0]

	out, err := runEventCmd(t, "tag", "add", ev.EventID, "aplexica:system-note", "--store", storeRoot)
	require.Error(t, err, "expected error for reserved tag; out:\n%s", out)
	require.True(t,
		strings.Contains(out, "reserved") || strings.Contains(err.Error(), "reserved"),
		"want reserved-namespace rejection; got out=%q err=%v", out, err)

	out, err = runEventCmd(t, "tag", "add", ev.EventID, "auto:archived", "--store", storeRoot)
	require.Error(t, err)
	require.True(t,
		strings.Contains(out, "reserved") || strings.Contains(err.Error(), "reserved"),
		"want reserved-namespace rejection; got out=%q err=%v", out, err)
}

func TestLog_EventTagFilter(t *testing.T) {
	tmp := t.TempDir()
	storeRoot := filepath.Join(tmp, "store")
	id := seedMemoryArtifact(t, storeRoot, "claude-code", "# v1\n")
	store := &acf.Store{Root: storeRoot}
	events, _ := store.ReadEvents(acf.KindMemory, id)
	ev := events[0]

	_, err := runEventCmd(t, "tag", "add", ev.EventID, "highlight", "--store", storeRoot)
	require.NoError(t, err)

	out, err := runLogCmd(t, id, "--event-tag", "highlight", "--store", storeRoot)
	require.NoError(t, err)
	require.Contains(t, out, ev.Hash)

	out, err = runLogCmd(t, id, "--event-tag", "nope", "--store", storeRoot)
	require.NoError(t, err)
	require.NotContains(t, out, ev.Hash, "non-matching tag should yield no events")
}
