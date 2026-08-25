package acf

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"
)

// seedSecretsWithSidecars mirrors the on-disk shape that
// internal/secrets v0.72.0 produces:
//
//	<root>/<name>             # value
//	<root>/.meta/<name>.json  # sidecar
//	<root>/<artifact-id>/<key> # legacy per-artifact pair
//
// Returns the absolute secrets root path.
func seedSecretsWithSidecars(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "secrets")
	require.NoError(t, os.MkdirAll(filepath.Join(root, ".meta"), 0o700))

	write := func(name, body string) {
		require.NoError(t, os.WriteFile(filepath.Join(root, name), []byte(body), 0o600))
	}
	writeMeta := func(name string, enabled bool) {
		flag := "false"
		if enabled {
			flag = "true"
		}
		body := `{"name":"` + name + `","syncEnabled":` + flag + `}`
		require.NoError(t, os.WriteFile(
			filepath.Join(root, ".meta", name+".json"),
			[]byte(body), 0o600))
	}

	// Two global secrets: one opt-in, one opt-out.
	write("github-token", "ghp_opted_in")
	writeMeta("github-token", true)

	write("private-key", "RSA-opted-out")
	writeMeta("private-key", false)

	// One per-artifact pair — should pass through unfiltered.
	require.NoError(t, os.MkdirAll(filepath.Join(root, "artifact-1"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(root, "artifact-1", "API_KEY"),
		[]byte("per-artifact-value"), 0o600))

	return root
}

// listTarEntries extracts every entry name from a gzipped tar.
func listTarEntries(t *testing.T, buf *bytes.Buffer) []string {
	t.Helper()
	gz, err := gzip.NewReader(buf)
	require.NoError(t, err)
	defer gz.Close()
	tr := tar.NewReader(gz)
	var names []string
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		names = append(names, hdr.Name)
		_, _ = io.Copy(io.Discard, tr)
	}
	sort.Strings(names)
	return names
}

func TestBundle_RespectSyncFlags_OnlyOptInGlobals(t *testing.T) {
	src := &Store{Root: filepath.Join(t.TempDir(), "src")}
	require.NoError(t, src.Init())
	secretsRoot := seedSecretsWithSidecars(t)

	var buf bytes.Buffer
	require.NoError(t, src.Bundle(&buf, BundleOpts{
		AplexicaVersion:  "0.73.0",
		SecretsRoot:      secretsRoot,
		RespectSyncFlags: true,
	}))

	names := listTarEntries(t, &buf)

	// The opt-in global secret + its sidecar are present.
	require.Contains(t, names, "secrets/github-token")
	require.Contains(t, names, "secrets/.meta/github-token.json")

	// The opt-out global secret + its sidecar are filtered out.
	require.NotContains(t, names, "secrets/private-key")
	require.NotContains(t, names, "secrets/.meta/private-key.json")

	// The per-artifact pair passes through unfiltered.
	require.Contains(t, names, "secrets/artifact-1/API_KEY")
}

func TestBundle_RespectSyncFlags_False_IncludesEverything(t *testing.T) {
	src := &Store{Root: filepath.Join(t.TempDir(), "src")}
	require.NoError(t, src.Init())
	secretsRoot := seedSecretsWithSidecars(t)

	var buf bytes.Buffer
	require.NoError(t, src.Bundle(&buf, BundleOpts{
		AplexicaVersion:  "0.73.0",
		SecretsRoot:      secretsRoot,
		RespectSyncFlags: false,
	}))
	names := listTarEntries(t, &buf)

	require.Contains(t, names, "secrets/github-token")
	require.Contains(t, names, "secrets/private-key",
		"with --respect-sync-flags=false, opt-out secrets are bundled too")
	require.Contains(t, names, "secrets/artifact-1/API_KEY")
}

func TestBundle_NoSecretsRoot_StillBundlesAcf(t *testing.T) {
	// Defensive: RespectSyncFlags=true with no SecretsRoot must NOT
	// crash; the secrets walk is gated on SecretsRoot != "".
	src := &Store{Root: filepath.Join(t.TempDir(), "src")}
	require.NoError(t, src.Init())

	var buf bytes.Buffer
	require.NoError(t, src.Bundle(&buf, BundleOpts{
		AplexicaVersion:  "0.73.0",
		RespectSyncFlags: true,
	}))
	names := listTarEntries(t, &buf)
	for _, n := range names {
		require.False(t,
			len(n) >= len("secrets/") && n[:len("secrets/")] == "secrets/",
			"no secrets/ entries expected when SecretsRoot is empty; got %q", n)
	}
}

func TestLoadSyncEnabledNames_SkipsMalformedSidecar(t *testing.T) {
	tmp := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(tmp, ".meta"), 0o700))
	require.NoError(t, os.WriteFile(
		filepath.Join(tmp, ".meta", "good.json"),
		[]byte(`{"name":"good","syncEnabled":true}`), 0o600))
	require.NoError(t, os.WriteFile(
		filepath.Join(tmp, ".meta", "broken.json"),
		[]byte(`not valid json`), 0o600))

	got, err := loadSyncEnabledNames(tmp)
	require.NoError(t, err)
	require.True(t, got["good"])
	require.False(t, got["broken"], "malformed sidecars must NOT default to opt-in (the value is load-bearing)")
}

func TestLoadSyncEnabledNames_MissingMetaDir(t *testing.T) {
	tmp := t.TempDir()
	got, err := loadSyncEnabledNames(tmp)
	require.NoError(t, err)
	require.Empty(t, got)
}
