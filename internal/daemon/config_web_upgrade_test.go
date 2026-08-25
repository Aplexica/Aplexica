package daemon

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestConfigHasWebSectionMissingFile returns false (and nil error) when
// the config file doesn't exist at path. A brand-new install hasn't
// "upgraded" because there's nothing to upgrade from — no notice
// should fire.
func TestConfigHasWebSectionMissingFile(t *testing.T) {
	dir := t.TempDir()
	has, err := ConfigHasWebSection(filepath.Join(dir, "missing.json"))
	if err != nil {
		t.Fatalf("ConfigHasWebSection: %v", err)
	}
	if has {
		t.Error("missing file should return false")
	}
}

// TestConfigHasWebSectionWithoutKey returns false for a config that
// predates the web UI configuration. The upgrade-notice path uses this
// to detect "user is upgrading from a pre-W2 daemon."
func TestConfigHasWebSectionWithoutKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"dir":"/tmp","tray":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	has, err := ConfigHasWebSection(path)
	if err != nil {
		t.Fatalf("ConfigHasWebSection: %v", err)
	}
	if has {
		t.Error("config without 'web' key should return false")
	}
}

// TestConfigHasWebSectionWithEmptyObject returns true when the config
// has "web":{} — that's how Go's encoding/json serializes a default
// WebConfig (omitempty doesn't omit nested zero structs). Either the
// user wrote it explicitly, or a prior daemon run already emitted the
// first-run notice and persisted defaults. Either way: don't re-notice.
func TestConfigHasWebSectionWithEmptyObject(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"dir":"/tmp","web":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	has, err := ConfigHasWebSection(path)
	if err != nil {
		t.Fatalf("ConfigHasWebSection: %v", err)
	}
	if !has {
		t.Error("config with empty 'web':{} should return true")
	}
}

// TestConfigHasWebSectionWithPopulatedKey returns true when the user
// has explicitly customized any web setting.
func TestConfigHasWebSectionWithPopulatedKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"web":{"enabled":false,"port":7600}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	has, err := ConfigHasWebSection(path)
	if err != nil {
		t.Fatalf("ConfigHasWebSection: %v", err)
	}
	if !has {
		t.Error("config with populated 'web' should return true")
	}
}

// TestConfigHasWebSectionMalformedJSON returns the parse error so the
// caller can decide how to handle it (typically: skip the notice and
// let the existing LoadConfig path surface the same error).
func TestConfigHasWebSectionMalformedJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`not json`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := ConfigHasWebSection(path)
	if err == nil {
		t.Error("malformed JSON should return error")
	}
}

// TestEmitFirstRunWebNoticeFires confirms the notice prints to the
// writer + persists defaults to disk when triggered. The persistence
// part is what makes the notice one-time: the next start will see a
// "web" key and skip.
func TestEmitFirstRunWebNoticeFires(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"dir":"/tmp","tray":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	fired, err := EmitFirstRunWebNotice(path, &buf)
	if err != nil {
		t.Fatalf("EmitFirstRunWebNotice: %v", err)
	}
	if !fired {
		t.Error("notice should have fired for pre-W2 config")
	}
	out := buf.String()
	if !strings.Contains(out, "local web UI") {
		t.Errorf("notice output missing 'local web UI': %q", out)
	}
	if !strings.Contains(out, "aplexica web") {
		t.Errorf("notice output missing 'aplexica web' command hint: %q", out)
	}

	// After firing, the on-disk config must have a "web" key so the
	// next start doesn't re-notice.
	has, err := ConfigHasWebSection(path)
	if err != nil {
		t.Fatal(err)
	}
	if !has {
		t.Error("after notice fires, 'web' key must be persisted to disk")
	}
}

// TestEmitFirstRunWebNoticeIdempotent confirms a second call after the
// notice has already fired is a silent no-op (no output, fired=false).
// Mirrors the daemon-restart scenario: start once → notice fires →
// daemon stops → start again → notice should NOT re-fire.
func TestEmitFirstRunWebNoticeIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"dir":"/tmp","tray":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	// First call: fires.
	var buf1 bytes.Buffer
	fired, err := EmitFirstRunWebNotice(path, &buf1)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	if !fired {
		t.Fatal("first call should fire")
	}

	// Second call: silent no-op.
	var buf2 bytes.Buffer
	fired, err = EmitFirstRunWebNotice(path, &buf2)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if fired {
		t.Error("second call should not fire (already persisted)")
	}
	if buf2.Len() != 0 {
		t.Errorf("second call wrote %d bytes; want 0: %q", buf2.Len(), buf2.String())
	}
}

// TestEmitFirstRunWebNoticeBrandNewInstall confirms the notice does
// NOT fire on a brand-new install (config file doesn't exist yet —
// daemon hasn't written one). The notice is specifically an UPGRADE
// signal; a fresh user discovers the web UI through `aplexica setup`
// (W10) or the tray menu (W9), not via stderr.
func TestEmitFirstRunWebNoticeBrandNewInstall(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	var buf bytes.Buffer
	fired, err := EmitFirstRunWebNotice(path, &buf)
	if err != nil {
		t.Fatalf("EmitFirstRunWebNotice: %v", err)
	}
	if fired {
		t.Error("brand-new install should not fire notice")
	}
	if buf.Len() != 0 {
		t.Errorf("brand-new install wrote %d bytes; want 0: %q", buf.Len(), buf.String())
	}
	// And: the path should still not exist (we don't pre-create it).
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("brand-new install should not pre-create config: %v", err)
	}
}

// TestEmitFirstRunWebNoticeNilWriter accepts a nil writer (callers may
// disable the print but still want the persistence). The persistence
// must still happen.
func TestEmitFirstRunWebNoticeNilWriter(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"dir":"/tmp"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	fired, err := EmitFirstRunWebNotice(path, io.Discard)
	if err != nil {
		t.Fatalf("EmitFirstRunWebNotice with io.Discard: %v", err)
	}
	if !fired {
		t.Error("notice should fire even with discard writer")
	}
	has, err := ConfigHasWebSection(path)
	if err != nil {
		t.Fatal(err)
	}
	if !has {
		t.Error("persistence must still happen with discard writer")
	}
}
