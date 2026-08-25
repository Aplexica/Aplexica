package web

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// TestWritePortInfoCreatesFile confirms the file exists at the requested
// path with the expected JSON shape after WritePortInfo returns nil.
func TestWritePortInfoCreatesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "portinfo.json")

	info := PortInfo{
		Port:    51234,
		Bind:    "127.0.0.1",
		Version: "v1.0.0",
	}
	if err := WritePortInfo(path, info); err != nil {
		t.Fatalf("WritePortInfo: %v", err)
	}

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var got PortInfo
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Port != 51234 {
		t.Errorf("Port = %d, want 51234", got.Port)
	}
	if got.Bind != "127.0.0.1" {
		t.Errorf("Bind = %q, want 127.0.0.1", got.Bind)
	}
	if got.Version != "v1.0.0" {
		t.Errorf("Version = %q, want v1.0.0", got.Version)
	}
}

// TestWritePortInfoSetsMode0600 confirms the file is created with owner-
// only permissions on POSIX. Windows is skipped because its file ACL
// model doesn't map to POSIX permission bits — daemon relies on default
// %USERPROFILE% ACLs there. Documented in the package comment.
func TestWritePortInfoSetsMode0600(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX file mode bits not applicable on Windows")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "portinfo.json")
	if err := WritePortInfo(path, PortInfo{Port: 1, Bind: "127.0.0.1", Version: "x"}); err != nil {
		t.Fatalf("WritePortInfo: %v", err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if mode := st.Mode().Perm(); mode != fs.FileMode(0o600) {
		t.Errorf("mode = %o, want 0o600", mode)
	}
}

// TestWritePortInfoOverwrites confirms a second WritePortInfo to the same
// path replaces the contents atomically (important for daemon restart
// scenarios where the previous port assignment is now stale).
func TestWritePortInfoOverwrites(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "portinfo.json")
	if err := WritePortInfo(path, PortInfo{Port: 1111, Bind: "127.0.0.1", Version: "x"}); err != nil {
		t.Fatal(err)
	}
	if err := WritePortInfo(path, PortInfo{Port: 2222, Bind: "::1", Version: "y"}); err != nil {
		t.Fatal(err)
	}
	got, err := ReadPortInfo(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Port != 2222 || got.Bind != "::1" || got.Version != "y" {
		t.Errorf("overwrite incomplete: %+v", got)
	}
}

// TestWritePortInfoStampsStartedAtIfZero confirms the writer fills
// StartedAt with the current time when the caller leaves it zero. The
// caller can override by providing a non-zero StartedAt (used by tests
// that pin clock behavior).
func TestWritePortInfoStampsStartedAtIfZero(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "portinfo.json")

	before := time.Now().UTC()
	if err := WritePortInfo(path, PortInfo{Port: 7600, Bind: "127.0.0.1", Version: "v1"}); err != nil {
		t.Fatalf("WritePortInfo: %v", err)
	}
	after := time.Now().UTC()

	got, err := ReadPortInfo(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.StartedAt.IsZero() {
		t.Fatal("StartedAt should be populated")
	}
	if got.StartedAt.Before(before) || got.StartedAt.After(after) {
		t.Errorf("StartedAt %v not in [%v, %v]", got.StartedAt, before, after)
	}
}

// TestWritePortInfoHonorsCallerStartedAt verifies that a non-zero
// StartedAt passed by the caller is preserved (i.e., the writer only
// stamps when the value is zero).
func TestWritePortInfoHonorsCallerStartedAt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "portinfo.json")
	want := time.Date(2026, time.January, 15, 10, 0, 0, 0, time.UTC)
	if err := WritePortInfo(path, PortInfo{
		Port: 1, Bind: "127.0.0.1", Version: "x", StartedAt: want,
	}); err != nil {
		t.Fatal(err)
	}
	got, err := ReadPortInfo(path)
	if err != nil {
		t.Fatal(err)
	}
	if !got.StartedAt.Equal(want) {
		t.Errorf("StartedAt = %v, want %v", got.StartedAt, want)
	}
}

// TestReadPortInfoMissingFile returns a clear error when the file
// doesn't exist (callers — `aplexica web port`, the tray — distinguish
// "daemon not started yet" from "config corrupt").
func TestReadPortInfoMissingFile(t *testing.T) {
	dir := t.TempDir()
	_, err := ReadPortInfo(filepath.Join(dir, "does-not-exist.json"))
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

// TestReadPortInfoMalformedJSON returns a clear error when the file
// contents are not valid JSON.
func TestReadPortInfoMalformedJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "portinfo.json")
	if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := ReadPortInfo(path)
	if err == nil {
		t.Fatal("expected error for malformed JSON")
	}
}
