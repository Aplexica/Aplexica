package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/stretchr/testify/require"
)

// seedStoreWithMemory writes a single global-scope memory artifact + one
// create event into a fresh store at storeRoot and returns the artifact
// ID. The payload is plain markdown so it round-trips through the
// claude-code memory exporter (format "markdown").
func seedStoreWithMemory(t *testing.T, storeRoot, content string) string {
	t.Helper()
	store := &acf.Store{Root: storeRoot}
	require.NoError(t, store.Init())

	id := acf.NewID()
	art := acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion,
		ArtifactID:       id,
		Kind:             acf.KindMemory,
		Scope:            acf.ScopeGlobal,
		Name:             "CLAUDE.md",
		CreatedAt:        time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC),
		UpdatedAt:        time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC),
	}
	require.NoError(t, store.WriteArtifact(art))

	payload, err := acf.EncodePayload(acf.MemoryPayload{
		Format:  "markdown",
		Content: content,
	})
	require.NoError(t, err)
	e := acf.Event{
		EventID:    acf.NewID(),
		ArtifactID: id,
		Type:       acf.EventTypeCreate,
		Timestamp:  time.Now().UTC(),
		Provenance: acf.Provenance{
			DeviceID:       "test-device",
			SourceAgent:    "test",
			AdapterVersion: "0.0.0",
		},
		Payload:    payload,
		ParentHash: "",
	}
	require.NoError(t, store.AppendEvent(acf.KindMemory, e))
	return id
}

// bundleStore exports the store at storeRoot into a .tar.gz at bundlePath.
func bundleStore(t *testing.T, storeRoot, bundlePath string) {
	t.Helper()
	store := &acf.Store{Root: storeRoot}
	f, err := os.Create(bundlePath)
	require.NoError(t, err)
	defer f.Close()
	require.NoError(t, store.Bundle(f, acf.BundleOpts{AplexicaVersion: "v0.65.0"}))
}

// runConvertCmd invokes `aplexica convert …` via rootCmd and returns
// combined stdout+stderr. Resets the package globals on Cleanup so
// repeated runs don't leak state.
func runConvertCmd(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&out)
	rootCmd.SetArgs(append([]string{"convert"}, args...))
	t.Cleanup(func() {
		convertTo = ""
		convertOut = ""
		convertVerbose = false
		convertUnsignedOK = false
	})
	err := rootCmd.Execute()
	return out.String(), err
}

func TestConvert_ClaudeMemoryRoundTrip(t *testing.T) {
	tmp := t.TempDir()
	storeRoot := filepath.Join(tmp, "src-store")
	bundlePath := filepath.Join(tmp, "bundle.tar.gz")
	outDir := filepath.Join(tmp, "out")

	id := seedStoreWithMemory(t, storeRoot, "# Hello world\nfrom convert test\n")
	bundleStore(t, storeRoot, bundlePath)

	out, err := runConvertCmd(t,
		bundlePath, "--to", "claude-code", "--out", outDir, "--verbose", "--unsigned-ok")
	require.NoError(t, err, "convert failed: %s", out)

	// Report sanity.
	require.Contains(t, out, "exported: 1")
	require.Contains(t, out, "skipped:  0")

	// The claude-code adapter writes global-scope memory to
	// <HomeDir>/.claude/CLAUDE.md. We pointed HomeDir at <out>/global.
	got := filepath.Join(outDir, "global", ".claude", "CLAUDE.md")
	body, err := os.ReadFile(got)
	require.NoError(t, err,
		"expected exported file at %s; got error: %v\ntree:\n%s",
		got, err, listTree(t, outDir))
	require.Contains(t, string(body), "Hello world")
	require.Contains(t, string(body), "from convert test")
	_ = id
}

func TestConvert_CrossAdapter_ClaudeBundle_ToCodex(t *testing.T) {
	tmp := t.TempDir()
	storeRoot := filepath.Join(tmp, "src-store")
	bundlePath := filepath.Join(tmp, "bundle.tar.gz")
	outDir := filepath.Join(tmp, "out")

	seedStoreWithMemory(t, storeRoot, "# Cross-adapter test\n")
	bundleStore(t, storeRoot, bundlePath)

	out, err := runConvertCmd(t,
		bundlePath, "--to", "codex", "--out", outDir, "--unsigned-ok")
	require.NoError(t, err, "convert failed: %s", out)
	require.Contains(t, out, "exported: 1")

	// codex writes global memory to <HomeDir>/.codex/AGENTS.md.
	got := filepath.Join(outDir, "global", ".codex", "AGENTS.md")
	body, err := os.ReadFile(got)
	require.NoError(t, err,
		"expected exported file at %s (tree:\n%s)",
		got, listTree(t, outDir))
	require.Contains(t, string(body), "Cross-adapter test")
}

func TestConvert_DefaultOutDir(t *testing.T) {
	tmp := t.TempDir()
	storeRoot := filepath.Join(tmp, "src-store")
	bundlePath := filepath.Join(tmp, "mybundle.tar.gz")

	seedStoreWithMemory(t, storeRoot, "# Default-out test\n")
	bundleStore(t, storeRoot, bundlePath)

	// Run from inside tmp so the default --out lands there.
	t.Chdir(tmp)

	out, err := runConvertCmd(t, bundlePath, "--to", "claude-code", "--unsigned-ok")
	require.NoError(t, err, "convert failed: %s", out)

	// Default --out: <basename>-<adapter>/ which for "mybundle.tar.gz"
	// strips .gz then .tar and appends "-claude-code".
	expected := filepath.Join(tmp, "mybundle-claude-code", "global", ".claude", "CLAUDE.md")
	_, err = os.Stat(expected)
	require.NoError(t, err, "expected default out path %s", expected)
}

func TestConvert_RequiresTo(t *testing.T) {
	tmp := t.TempDir()
	bundlePath := filepath.Join(tmp, "empty.tar.gz")
	// Make a valid-but-empty bundle so we exercise just the flag check.
	storeRoot := filepath.Join(tmp, "empty-store")
	require.NoError(t, (&acf.Store{Root: storeRoot}).Init())
	bundleStore(t, storeRoot, bundlePath)

	_, err := runConvertCmd(t, bundlePath)
	require.Error(t, err, "convert without --to must fail")
}

func TestConvert_UnknownAdapter(t *testing.T) {
	tmp := t.TempDir()
	storeRoot := filepath.Join(tmp, "src-store")
	bundlePath := filepath.Join(tmp, "bundle.tar.gz")
	seedStoreWithMemory(t, storeRoot, "# x\n")
	bundleStore(t, storeRoot, bundlePath)

	_, err := runConvertCmd(t, bundlePath, "--to", "no-such-agent",
		"--out", filepath.Join(tmp, "out"), "--unsigned-ok")
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown adapter")
}

func TestSanitizePathSegment(t *testing.T) {
	cases := []struct{ in, want string }{
		{"github.com/foo/bar", "github.com_foo_bar"},
		{"https://github.com/foo/bar.git", "https_github.com_foo_bar.git"},
		{"with:colon", "with_colon"},
		{"", "_"},
		{".", "_"},
		{"..", "_"},
		{"plain-name", "plain-name"},
	}
	for _, c := range cases {
		require.Equal(t, c.want, sanitizePathSegment(c.in),
			"sanitizePathSegment(%q)", c.in)
	}
}

// listTree returns a newline-separated tree listing of root, for use in
// require.NoError failure messages so we can see what convert actually
// produced when the expected path is missing.
func listTree(t *testing.T, root string) string {
	t.Helper()
	var b bytes.Buffer
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		b.WriteString("  ")
		b.WriteString(rel)
		if info.IsDir() {
			b.WriteString("/")
		}
		b.WriteString("\n")
		return nil
	})
	return b.String()
}
