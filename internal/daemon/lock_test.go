package daemon

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLock_AcquireOnFreshPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.lock")

	l, err := Acquire(path)
	require.NoError(t, err)
	defer l.Release()

	// Lock file exists with our PID.
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	pid, err := strconv.Atoi(string(data))
	require.NoError(t, err)
	require.Equal(t, os.Getpid(), pid)
}

func TestLock_DoubleAcquire_SecondFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.lock")

	l1, err := Acquire(path)
	require.NoError(t, err)
	defer l1.Release()

	_, err = Acquire(path)
	require.Error(t, err, "second Acquire on same path while first is held must fail")
}

func TestLock_Release_AllowsReAcquire(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.lock")

	l1, err := Acquire(path)
	require.NoError(t, err)
	require.NoError(t, l1.Release())

	l2, err := Acquire(path)
	require.NoError(t, err)
	require.NoError(t, l2.Release())
}

func TestLock_StaleLock_TakesOver(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.lock")

	// Write a lock file with a PID that's certainly not running.
	// PID 0 is the kernel; using a very high PID that won't conflict.
	stalePID := 999999
	require.NoError(t, os.WriteFile(path, []byte(strconv.Itoa(stalePID)), 0o644))

	l, err := Acquire(path)
	require.NoError(t, err, "stale lock (dead PID) must be takeable")
	defer l.Release()

	data, _ := os.ReadFile(path)
	pid, _ := strconv.Atoi(string(data))
	require.Equal(t, os.Getpid(), pid, "lock file should now hold our PID")
}

func TestLock_Release_RemovesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.lock")

	l, err := Acquire(path)
	require.NoError(t, err)
	require.NoError(t, l.Release())

	_, err = os.Stat(path)
	require.True(t, os.IsNotExist(err), "Release should remove the lock file")
}
