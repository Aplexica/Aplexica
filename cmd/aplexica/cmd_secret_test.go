package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aplexica/aplexica/internal/secrets"
	"github.com/stretchr/testify/require"
)

// runSecretCmd invokes `aplexica secret …` with the given args, pointing
// it at the supplied secrets-root. Returns combined stdout+stderr. Uses
// the real cobra wiring (via rootCmd.Execute) so persistent-flag
// resolution and subcommand dispatch are exercised end-to-end.
func runSecretCmd(t *testing.T, root string, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&out)
	full := append([]string{"secret", "--secrets-root", root}, args...)
	rootCmd.SetArgs(full)
	// Reset the package globals between tests so flag values from one
	// invocation don't leak into the next when cobra reuses parse state.
	t.Cleanup(func() {
		secretValue = ""
		secretFromFile = ""
		secretReveal = false
		secretJSON = false
		secretFilter = ""
		secretRoot = ""
	})
	err := rootCmd.Execute()
	return out.String(), err
}

func TestSecretCmd_SetWriteThenList(t *testing.T) {
	root := filepath.Join(t.TempDir(), "secrets")

	// set via --value (avoids stdin path)
	out, err := runSecretCmd(t, root,
		"set", "artifact-aaa", "API_KEY", "--value", "supersecret")
	require.NoError(t, err)
	require.Contains(t, out, "wrote artifact-aaa/API_KEY")

	// list shows the new pair
	out, err = runSecretCmd(t, root, "list")
	require.NoError(t, err)
	require.Contains(t, out, "artifact-aaa")
	require.Contains(t, out, "API_KEY")
	require.NotContains(t, out, "supersecret", "list must never print values")
}

func TestSecretCmd_Get_DefaultRedacts(t *testing.T) {
	root := filepath.Join(t.TempDir(), "secrets")
	ss := &secrets.Store{Root: root}
	require.NoError(t, ss.Init())
	require.NoError(t, ss.Put("a", "K", "sensitive"))

	// Default: no --reveal → must NOT print the value
	out, err := runSecretCmd(t, root, "get", "a", "K")
	require.NoError(t, err)
	require.NotContains(t, out, "sensitive",
		"get without --reveal must not print the value")
	require.Contains(t, out, "(present")
}

func TestSecretCmd_Get_RevealPrintsValue(t *testing.T) {
	root := filepath.Join(t.TempDir(), "secrets")
	ss := &secrets.Store{Root: root}
	require.NoError(t, ss.Init())
	require.NoError(t, ss.Put("a", "K", "the-value"))

	out, err := runSecretCmd(t, root, "get", "a", "K", "--reveal")
	require.NoError(t, err)
	require.Contains(t, out, "the-value")
}

func TestSecretCmd_Delete(t *testing.T) {
	root := filepath.Join(t.TempDir(), "secrets")
	ss := &secrets.Store{Root: root}
	require.NoError(t, ss.Init())
	require.NoError(t, ss.Put("a", "K", "v"))

	out, err := runSecretCmd(t, root, "delete", "a", "K")
	require.NoError(t, err)
	require.Contains(t, out, "deleted a/K")

	// Confirm gone via the underlying store.
	_, err = ss.Get("a", "K")
	require.Error(t, err)
}

func TestSecretCmd_Rotate_RefusesIfMissing(t *testing.T) {
	root := filepath.Join(t.TempDir(), "secrets")

	_, err := runSecretCmd(t, root,
		"rotate", "no-such-artifact", "no-such-key", "--value", "new")
	require.Error(t, err, "rotate must refuse to create a new secret")
	require.Contains(t, err.Error(), "does not exist")
}

func TestSecretCmd_Rotate_ReplacesExisting(t *testing.T) {
	root := filepath.Join(t.TempDir(), "secrets")
	ss := &secrets.Store{Root: root}
	require.NoError(t, ss.Init())
	require.NoError(t, ss.Put("a", "K", "old-value"))

	out, err := runSecretCmd(t, root,
		"rotate", "a", "K", "--value", "new-value")
	require.NoError(t, err)
	require.Contains(t, out, "rotated a/K")

	v, err := ss.Get("a", "K")
	require.NoError(t, err)
	require.Equal(t, "new-value", v)
}

func TestSecretCmd_List_ArtifactFilter(t *testing.T) {
	root := filepath.Join(t.TempDir(), "secrets")
	ss := &secrets.Store{Root: root}
	require.NoError(t, ss.Init())
	require.NoError(t, ss.Put("a", "K1", "v1"))
	require.NoError(t, ss.Put("b", "K2", "v2"))

	out, err := runSecretCmd(t, root, "list", "--artifact", "a")
	require.NoError(t, err)
	require.Contains(t, out, "K1")
	require.NotContains(t, out, "K2", "filter must exclude artifact b")
}

func TestSecretCmd_List_JSON(t *testing.T) {
	root := filepath.Join(t.TempDir(), "secrets")
	ss := &secrets.Store{Root: root}
	require.NoError(t, ss.Init())
	require.NoError(t, ss.Put("a", "K1", "v1"))

	out, err := runSecretCmd(t, root, "list", "--json")
	require.NoError(t, err)
	// Trivial shape check — full structural unmarshal would be over-spec'd.
	require.True(t, strings.Contains(out, `"ArtifactID": "a"`),
		"JSON output missing ArtifactID key; got %q", out)
	require.True(t, strings.Contains(out, `"Key": "K1"`),
		"JSON output missing Key field; got %q", out)
}

func TestSecretCmd_Set_FromFile(t *testing.T) {
	root := filepath.Join(t.TempDir(), "secrets")
	valueFile := filepath.Join(t.TempDir(), "value.txt")
	require.NoError(t, writeTestFile(valueFile, "from-file-value"))

	out, err := runSecretCmd(t, root,
		"set", "a", "K", "--from-file", valueFile)
	require.NoError(t, err)
	require.Contains(t, out, "wrote a/K")

	ss := &secrets.Store{Root: root}
	v, err := ss.Get("a", "K")
	require.NoError(t, err)
	require.Equal(t, "from-file-value", v)
}

func writeTestFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o600)
}

// ─────────────────────────────────────────────────────────────────────
// v0.72.0 — global-name secrets + sidecar + sync-enable/sync-disable
// ─────────────────────────────────────────────────────────────────────

func TestSecretCmd_SetGlobal_WritesValueAndSidecar(t *testing.T) {
	root := filepath.Join(t.TempDir(), "secrets")

	out, err := runSecretCmd(t, root,
		"set", "github-token", "--value", "ghp_xyz")
	require.NoError(t, err)
	require.Contains(t, out, "wrote global github-token")

	// Verify the on-disk shape via the underlying store.
	ss := &secrets.Store{Root: root}
	v, err := ss.GetGlobal("github-token")
	require.NoError(t, err)
	require.Equal(t, "ghp_xyz", v)
	meta, err := ss.ReadMeta("github-token")
	require.NoError(t, err)
	require.False(t, meta.SyncEnabled)
}

func TestSecretCmd_GetGlobal_Default_AndReveal(t *testing.T) {
	root := filepath.Join(t.TempDir(), "secrets")
	ss := &secrets.Store{Root: root}
	require.NoError(t, ss.PutGlobal("k", "hidden"))

	out, err := runSecretCmd(t, root, "get", "k")
	require.NoError(t, err)
	require.NotContains(t, out, "hidden")
	require.Contains(t, out, "(present")

	out, err = runSecretCmd(t, root, "get", "k", "--reveal")
	require.NoError(t, err)
	require.Contains(t, out, "hidden")
}

func TestSecretCmd_DeleteGlobal(t *testing.T) {
	root := filepath.Join(t.TempDir(), "secrets")
	ss := &secrets.Store{Root: root}
	require.NoError(t, ss.PutGlobal("k", "v"))

	out, err := runSecretCmd(t, root, "delete", "k")
	require.NoError(t, err)
	require.Contains(t, out, "deleted global k")

	_, err = ss.GetGlobal("k")
	require.Error(t, err)
}

func TestSecretCmd_RotateGlobal(t *testing.T) {
	root := filepath.Join(t.TempDir(), "secrets")
	ss := &secrets.Store{Root: root}
	require.NoError(t, ss.PutGlobal("k", "old"))

	out, err := runSecretCmd(t, root,
		"rotate", "k", "--value", "new")
	require.NoError(t, err)
	require.Contains(t, out, "rotated global k")

	v, _ := ss.GetGlobal("k")
	require.Equal(t, "new", v)
}

func TestSecretCmd_RotateGlobal_RefusesMissing(t *testing.T) {
	root := filepath.Join(t.TempDir(), "secrets")
	_, err := runSecretCmd(t, root,
		"rotate", "never-set", "--value", "x")
	require.Error(t, err)
}

func TestSecretCmd_SyncEnableThenDisable(t *testing.T) {
	root := filepath.Join(t.TempDir(), "secrets")
	ss := &secrets.Store{Root: root}
	require.NoError(t, ss.PutGlobal("k", "v"))

	out, err := runSecretCmd(t, root, "sync-enable", "k")
	require.NoError(t, err)
	require.Contains(t, out, "syncEnabled=true")
	meta, _ := ss.ReadMeta("k")
	require.True(t, meta.SyncEnabled)

	out, err = runSecretCmd(t, root, "sync-disable", "k")
	require.NoError(t, err)
	require.Contains(t, out, "syncEnabled=false")
	meta, _ = ss.ReadMeta("k")
	require.False(t, meta.SyncEnabled)
}

func TestSecretCmd_List_MixedGlobalAndPerArtifact(t *testing.T) {
	root := filepath.Join(t.TempDir(), "secrets")
	ss := &secrets.Store{Root: root}
	require.NoError(t, ss.PutGlobal("global-key", "v"))
	require.NoError(t, ss.Put("artifact-1", "API_KEY", "v"))

	out, err := runSecretCmd(t, root, "list")
	require.NoError(t, err)
	require.Contains(t, out, "Global-name secrets")
	require.Contains(t, out, "global-key")
	require.Contains(t, out, "Per-artifact secrets")
	require.Contains(t, out, "artifact-1")
	require.Contains(t, out, "API_KEY")
}
