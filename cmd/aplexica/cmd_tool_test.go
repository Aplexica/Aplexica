package main

import (
	"bytes"
	"path/filepath"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/syncstate"
	"github.com/stretchr/testify/require"
)

// seedToolArtifact writes a global-scope tool artifact with a redacted
// MCP-server config that references two named secrets. Returns the
// artifact ID. Mirrors what claudecode.ImportTool would produce after
// the secret-externalization pass in internal/mcp/secrets.go.
func seedToolArtifact(t *testing.T, storeRoot string) string {
	t.Helper()
	store := &acf.Store{Root: storeRoot}
	require.NoError(t, store.Init())

	id := acf.NewID()
	art := acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion,
		ArtifactID:       id,
		Kind:             acf.KindTool,
		Scope:            acf.ScopeGlobal,
		Name:             ".mcp.json",
		CreatedAt:        time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC),
		UpdatedAt:        time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC),
	}
	require.NoError(t, store.WriteArtifact(art))

	// Realistic redacted-MCP content carrying two ${secret:…} refs.
	content := `{
  "mcpServers": {
    "github": {
      "command": "github-mcp",
      "env": {
        "GITHUB_TOKEN": "${secret:github.GITHUB_TOKEN}",
        "GITHUB_USER":  "${secret:github.GITHUB_USER}"
      }
    }
  }
}`
	payload, err := acf.EncodePayload(acf.ToolPayload{
		Format:  "acf.mcp.v1",
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
			SourceAgent:    "claude-code",
			AdapterVersion: "0.0.0",
		},
		Payload:    payload,
		ParentHash: "",
	}
	require.NoError(t, store.AppendEvent(acf.KindTool, e))
	return id
}

// runToolCmd invokes `aplexica tool …` via rootCmd. Pins --store and
// --state-dir to test fixtures so the user's real config is never
// touched.
func runToolCmd(t *testing.T, storeRoot, stateDir string, args ...string) (string, error) {
	t.Helper()
	// Reset package globals BEFORE Execute so values from a prior
	// call within the same test (e.g. --enable then --disable) don't
	// poison MarkFlagsMutuallyExclusive's parse-time check.
	toolStoreRoot = ""
	toolStateDir = ""
	toolSyncEnable = false
	toolSyncDisable = false

	var out bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&out)
	full := append([]string{"tool",
		"--store", storeRoot,
		"--state-dir", stateDir,
	}, args...)
	rootCmd.SetArgs(full)
	t.Cleanup(func() {
		toolStoreRoot = ""
		toolStateDir = ""
		toolSyncEnable = false
		toolSyncDisable = false
	})
	err := rootCmd.Execute()
	return out.String(), err
}

func TestToolList_Empty(t *testing.T) {
	tmp := t.TempDir()
	storeRoot := filepath.Join(tmp, "store")
	stateDir := filepath.Join(tmp, "state")
	require.NoError(t, (&acf.Store{Root: storeRoot}).Init())

	out, err := runToolCmd(t, storeRoot, stateDir, "list")
	require.NoError(t, err)
	require.Contains(t, out, "no tool artifacts")
}

func TestToolList_ShowsSecretCountAndSyncOff(t *testing.T) {
	tmp := t.TempDir()
	storeRoot := filepath.Join(tmp, "store")
	stateDir := filepath.Join(tmp, "state")

	id := seedToolArtifact(t, storeRoot)

	out, err := runToolCmd(t, storeRoot, stateDir, "list")
	require.NoError(t, err)
	require.Contains(t, out, id)
	require.Contains(t, out, ".mcp.json")
	require.Contains(t, out, "off", "default sync flag must render off")
	require.Contains(t, out, "claude-code")
	// Two ${secret:…} refs → "2" appears in the SECRETS column.
	// Tabwriter renders columns padded with spaces, so match the
	// surrounding shape rather than literal tabs.
	require.Regexp(t, `\.mcp\.json\s+global\s+2\b`, out)
}

func TestToolShow_ListsExtractedSecrets(t *testing.T) {
	tmp := t.TempDir()
	storeRoot := filepath.Join(tmp, "store")
	stateDir := filepath.Join(tmp, "state")

	id := seedToolArtifact(t, storeRoot)

	out, err := runToolCmd(t, storeRoot, stateDir, "show", id)
	require.NoError(t, err)
	require.Contains(t, out, "secretsRefs: 2")
	require.Contains(t, out, "github.GITHUB_TOKEN")
	require.Contains(t, out, "github.GITHUB_USER")
	require.Contains(t, out, "claude-code")
	require.Contains(t, out, "acf.mcp.v1")
	require.Contains(t, out, "syncSecrets: false")
}

func TestToolShow_NotFound(t *testing.T) {
	tmp := t.TempDir()
	storeRoot := filepath.Join(tmp, "store")
	stateDir := filepath.Join(tmp, "state")
	require.NoError(t, (&acf.Store{Root: storeRoot}).Init())

	_, err := runToolCmd(t, storeRoot, stateDir, "show", "no-such-id")
	require.Error(t, err)
}

func TestToolSyncSecrets_EnableThenDisable(t *testing.T) {
	tmp := t.TempDir()
	storeRoot := filepath.Join(tmp, "store")
	stateDir := filepath.Join(tmp, "state")
	id := seedToolArtifact(t, storeRoot)

	// Enable
	out, err := runToolCmd(t, storeRoot, stateDir,
		"sync-secrets", id, "--enable")
	require.NoError(t, err)
	require.Contains(t, out, "syncSecrets enabled")

	ss := &syncstate.Store{Path: syncstate.DefaultPath(stateDir)}
	v, err := ss.Get(id)
	require.NoError(t, err)
	require.True(t, v)

	// Disable
	out, err = runToolCmd(t, storeRoot, stateDir,
		"sync-secrets", id, "--disable")
	require.NoError(t, err)
	require.Contains(t, out, "syncSecrets disabled")

	v, err = ss.Get(id)
	require.NoError(t, err)
	require.False(t, v)
}

func TestToolSyncSecrets_RequiresFlag(t *testing.T) {
	tmp := t.TempDir()
	storeRoot := filepath.Join(tmp, "store")
	stateDir := filepath.Join(tmp, "state")
	id := seedToolArtifact(t, storeRoot)

	// No --enable / --disable supplied.
	_, err := runToolCmd(t, storeRoot, stateDir, "sync-secrets", id)
	require.Error(t, err, "sync-secrets must require one of --enable / --disable")
}

func TestToolSyncSecrets_UnknownArtifact(t *testing.T) {
	tmp := t.TempDir()
	storeRoot := filepath.Join(tmp, "store")
	stateDir := filepath.Join(tmp, "state")
	require.NoError(t, (&acf.Store{Root: storeRoot}).Init())

	_, err := runToolCmd(t, storeRoot, stateDir,
		"sync-secrets", "no-such-id", "--enable")
	require.Error(t, err)
}

func TestToolCapabilities_ReportsAdapterAndFormat(t *testing.T) {
	tmp := t.TempDir()
	storeRoot := filepath.Join(tmp, "store")
	stateDir := filepath.Join(tmp, "state")
	id := seedToolArtifact(t, storeRoot)

	out, err := runToolCmd(t, storeRoot, stateDir, "capabilities", id)
	require.NoError(t, err)
	require.Contains(t, out, "source adapter: claude-code")
	require.Contains(t, out, "payload format: acf.mcp.v1")
	require.Contains(t, out, "cross-adapter support:")
	require.Contains(t, out, "claude-code")
	require.Contains(t, out, "codex")
	require.Contains(t, out, "kilo")
	require.Contains(t, out, "hermes")
	require.Contains(t, out, "openclaw")
	require.Contains(t, out, "native (MCP)")
}

func TestExtractSecretNames_DedupesAndSorts(t *testing.T) {
	tmp := t.TempDir()
	storeRoot := filepath.Join(tmp, "store")
	id := seedToolArtifact(t, storeRoot)

	store := &acf.Store{Root: storeRoot}
	art, err := store.ReadArtifact(acf.KindTool, id)
	require.NoError(t, err)

	got := extractSecretNames(store, art)
	require.Equal(t, []string{"github.GITHUB_TOKEN", "github.GITHUB_USER"}, got,
		"secrets must be sorted alphabetically and de-duped")
}
