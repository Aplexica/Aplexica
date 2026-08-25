// SPDX-License-Identifier: AGPL-3.0-or-later
package manager

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// writeManifest creates <root>/<sub>/plugin.json with the given JSON body
// and returns the subdirectory path.
func writeManifest(t *testing.T, root, sub, body string) string {
	t.Helper()
	dir := filepath.Join(root, sub)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, manifestFilename), []byte(body), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	return dir
}

func TestDiscover_MissingDirIsNotAnError(t *testing.T) {
	m := New(filepath.Join(t.TempDir(), "does-not-exist"), nil, "", "0.0.0", testLogger())
	got, err := m.Discover()
	if err != nil {
		t.Fatalf("Discover() error = %v, want nil for absent dir", err)
	}
	if len(got) != 0 {
		t.Fatalf("Discover() = %d entries, want 0 for absent dir", len(got))
	}
}

func TestDiscover_TableDriven(t *testing.T) {
	root := t.TempDir()

	// good adapter manifest
	goodDir := writeManifest(t, root, "good", `{
		"manifest_version": 1,
		"name": "good-plugin",
		"version": "0.1.0",
		"abi_version": "1",
		"executable": "good-plugin",
		"kind": "adapter",
		"kinds": ["memory"]
	}`)

	// missing executable field -> Validate fails -> skipped
	writeManifest(t, root, "no-exe", `{
		"manifest_version": 1,
		"name": "no-exe",
		"version": "0.1.0",
		"abi_version": "1",
		"kinds": ["memory"]
	}`)

	// wrong ABI -> Validate fails -> skipped
	writeManifest(t, root, "bad-abi", `{
		"manifest_version": 1,
		"name": "bad-abi",
		"version": "0.1.0",
		"abi_version": "999",
		"executable": "bad-abi",
		"kinds": ["memory"]
	}`)

	// remote kind -> not an adapter -> skipped
	writeManifest(t, root, "remote", `{
		"manifest_version": 1,
		"name": "remote-plugin",
		"version": "0.1.0",
		"abi_version": "1",
		"executable": "remote-plugin",
		"kind": "remote"
	}`)

	// malformed JSON -> skipped
	writeManifest(t, root, "garbage", `{ this is not json `)

	// subdirectory with no manifest at all -> skipped silently
	if err := os.MkdirAll(filepath.Join(root, "empty-subdir"), 0o755); err != nil {
		t.Fatal(err)
	}

	// a stray file at the root (not a dir) -> ignored
	if err := os.WriteFile(filepath.Join(root, "loose.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	// absolute-path executable should be preserved as-is. JSON-encode the
	// path: a raw Windows path (C:\Users\…) embedded verbatim is invalid
	// JSON (backslash escapes) and the manifest would be dropped as garbage.
	absExe := filepath.Join(t.TempDir(), "abs-exe-bin")
	absExeJSON, err := json.Marshal(absExe)
	if err != nil {
		t.Fatal(err)
	}
	absDir := writeManifest(t, root, "abs", `{
		"manifest_version": 1,
		"name": "abs-plugin",
		"version": "0.1.0",
		"abi_version": "1",
		"executable": `+string(absExeJSON)+`,
		"kinds": ["memory"]
	}`)

	m := New(root, nil, "", "0.0.0", testLogger())
	got, err := m.Discover()
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}

	// Only the two well-formed adapter manifests survive.
	byName := map[string]Discovered{}
	for _, d := range got {
		byName[d.Manifest.Name] = d
	}
	if len(byName) != 2 {
		t.Fatalf("Discover() returned %d adapters %v, want 2 (good-plugin, abs-plugin)", len(byName), keysOf(byName))
	}

	good, ok := byName["good-plugin"]
	if !ok {
		t.Fatal("good-plugin not discovered")
	}
	if good.Dir != goodDir {
		t.Errorf("good.Dir = %q, want %q", good.Dir, goodDir)
	}
	// relative executable resolved against the plugin subdir
	wantExe := filepath.Join(goodDir, "good-plugin")
	if good.Executable != wantExe {
		t.Errorf("good.Executable = %q, want %q (relative resolved against dir)", good.Executable, wantExe)
	}

	abs, ok := byName["abs-plugin"]
	if !ok {
		t.Fatal("abs-plugin not discovered")
	}
	if abs.Dir != absDir {
		t.Errorf("abs.Dir = %q, want %q", abs.Dir, absDir)
	}
	if abs.Executable != absExe {
		t.Errorf("abs.Executable = %q, want %q (absolute preserved)", abs.Executable, absExe)
	}

	// Explicitly confirm the rejected ones are absent.
	for _, bad := range []string{"no-exe", "bad-abi", "remote-plugin", "garbage"} {
		if _, present := byName[bad]; present {
			t.Errorf("Discover() unexpectedly returned rejected plugin %q", bad)
		}
	}
}

func keysOf(m map[string]Discovered) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
