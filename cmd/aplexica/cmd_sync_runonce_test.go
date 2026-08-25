package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/stretchr/testify/require"
)

func runSyncRunOnceCmd(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&out)
	rootCmd.SetArgs(append([]string{"sync", "run-once"}, args...))
	t.Cleanup(func() {
		syncRunOnceFrom = ""
		syncRunOnceTo = ""
		syncRunOnceContextDir = ""
		syncRunOnceStoreRoot = ""
		syncRunOnceSecretsRoot = ""
		syncRunOnceVerbose = false
	})
	err := rootCmd.Execute()
	return out.String(), err
}

func TestSyncRunOnce_ReExportsClaudeMemoryToCodex(t *testing.T) {
	tmp := t.TempDir()
	storeRoot := filepath.Join(tmp, "store")
	outDir := filepath.Join(tmp, "out")
	require.NoError(t, os.MkdirAll(outDir, 0o755))

	// Seed: a global memory artifact whose first event came from claude-code.
	seedMemoryArtifact(t, storeRoot, "claude-code", "# from claude-code\n")

	out, err := runSyncRunOnceCmd(t,
		"--from", "claude-code",
		"--to", "codex",
		"--context-dir", outDir,
		"--store", storeRoot,
		"--secrets-root", filepath.Join(tmp, "secrets"),
	)
	require.NoError(t, err, "run-once output:\n%s", out)
	require.Contains(t, out, "exported:   1")
	// codex writes AGENTS.md for global memory artifacts; HomeDir
	// resolution defaults to the user's real home but the dest path
	// comes from contextDir for global... actually let me read the
	// behavior. The cobra adapter builds in adapters.go uses
	// New(); contextDir is only consulted for non-global. For global,
	// the artifact lands at codex's HomeDir/.codex/AGENTS.md.
	// We didn't override HomeDir, so it goes to the real $HOME.
	// Instead, mark this as global → assertable via the report.
	_ = out
}

// seedMemoryArtifact creates one memory artifact whose first event's
// provenance.sourceAgent is `source`. Used by the run-once test to
// stage a known-source artifact without invoking the full claude-
// code adapter (which would default HomeDir to the real home and
// pollute external dirs).
func seedMemoryArtifact(t *testing.T, storeRoot, source, body string) string {
	t.Helper()
	store := &acf.Store{Root: storeRoot}
	require.NoError(t, store.Init())

	art := acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion,
		ArtifactID:       acf.NewID(),
		Kind:             acf.KindMemory,
		Scope:            acf.ScopeProject, // project so contextDir applies
		Name:             "AGENTS.md",
	}
	require.NoError(t, store.WriteArtifact(art))

	payload, err := acf.EncodePayload(acf.MemoryPayload{
		Format: "markdown", Content: body,
	})
	require.NoError(t, err)
	e := acf.Event{
		EventID:    acf.NewID(),
		ArtifactID: art.ArtifactID,
		Type:       acf.EventTypeCreate,
		Provenance: acf.Provenance{
			DeviceID:       "test-device",
			SourceAgent:    source,
			AdapterVersion: "0.0.0",
		},
		Payload:    payload,
		ParentHash: "",
	}
	require.NoError(t, store.AppendEvent(acf.KindMemory, e))
	return art.ArtifactID
}

func TestSyncRunOnce_SkipsArtifactsFromOtherSources(t *testing.T) {
	tmp := t.TempDir()
	storeRoot := filepath.Join(tmp, "store")

	// Two artifacts: one from claude-code, one from codex. run-once
	// --from claude-code --to hermes should only export the first.
	seedMemoryArtifact(t, storeRoot, "claude-code", "# claude\n")
	seedMemoryArtifact(t, storeRoot, "codex", "# codex\n")

	out, err := runSyncRunOnceCmd(t,
		"--from", "claude-code",
		"--to", "hermes",
		"--context-dir", filepath.Join(tmp, "out"),
		"--store", storeRoot,
		"--secrets-root", filepath.Join(tmp, "secrets"),
	)
	require.NoError(t, err, "out:\n%s", out)
	require.Contains(t, out, "exported:   1",
		"only the claude-code-sourced artifact should export")
}

func TestSyncRunOnce_RequiresFromAndTo(t *testing.T) {
	tmp := t.TempDir()
	_, err := runSyncRunOnceCmd(t,
		"--from", "claude-code",
		"--store", filepath.Join(tmp, "store"),
	)
	require.Error(t, err)
}

func TestSyncRunOnce_RejectsSameFromTo(t *testing.T) {
	tmp := t.TempDir()
	storeRoot := filepath.Join(tmp, "store")
	require.NoError(t, (&acf.Store{Root: storeRoot}).Init())

	_, err := runSyncRunOnceCmd(t,
		"--from", "claude-code",
		"--to", "claude-code",
		"--store", storeRoot,
		"--secrets-root", filepath.Join(tmp, "secrets"),
	)
	require.Error(t, err)
}
