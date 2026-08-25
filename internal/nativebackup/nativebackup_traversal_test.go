//go:build !windows

// Restore reconstructs absolute native write targets from manifest entry paths.
// A malicious or corrupt manifest must not be able to steer those writes
// outside the agent's mirrored root via "../" segments or an absolute-looking
// path component. These tests pin that a traversal entry is REFUSED as a
// per-file error rather than written, while legitimate entries still restore.
package nativebackup

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// writeManifestForTest serializes a hand-built manifest into backupDir so a
// Restore can be driven against a deliberately malicious entry.
func writeManifestForTest(t *testing.T, backupDir string, man Manifest) {
	t.Helper()
	require.NoError(t, os.MkdirAll(backupDir, 0o700))
	require.NoError(t, writeManifest(backupDir, man))
}

// TestRestore_TraversalManifestPathRefused: a manifest entry whose Path uses
// "../" to climb out of the agent's mirrored root must be REFUSED (per-file Err
// set, OK=false) and must NOT result in any write outside the agent root, while
// a legitimate sibling entry still restores OK so one poisoned entry doesn't
// sink the batch.
//
// Threat model: the backups directory is plain user-writable files, so a
// corrupt or maliciously-edited manifest.json is untrusted input on the restore
// path. nativeTargetFor used to strip "claude/" and re-root the remainder at
// "/" with no containment check; an entry like "claude/../../../<abs>" then
// reconstructed to an arbitrary absolute write target.
func TestRestore_TraversalManifestPathRefused(t *testing.T) {
	workspace := t.TempDir()
	backupsRoot := filepath.Join(t.TempDir(), "backups")

	// A real agent root + one good file, snapshotted normally.
	root := nativeRoot(t, workspace, "agentroot", map[string]string{
		"good.txt": "GOOD-ORIGINAL",
	})
	preSync := filepath.Join(backupsRoot, "pre-sync-2026-05-29T00-00-00Z")
	man, err := Snapshot([]AgentRoots{{Name: "claude", Roots: []string{root}}}, preSync)
	require.NoError(t, err)
	require.Len(t, man.Agents[0].Roots, 1)

	// A canary OUTSIDE the agent root. We assert Restore neither creates nor
	// overwrites it. Seed it so an "overwrite" would be detectable too.
	const canary = "CANARY-UNTOUCHED"
	escapeTarget := filepath.Join(workspace, "ESCAPED.txt")
	writeFile(t, escapeTarget, canary)

	// The forged entry climbs out of the "claude/" mirror. The legacy
	// reconstruction (strip "claude/", re-root at "/") would resolve this to an
	// absolute write target chosen by the manifest — the exact escape the fix
	// must refuse. We use the fix-sketch's canonical "agent/../../../..." shape.
	evilManifestPath := "claude/../../../../../../tmp/aplexica-restore-escape/evil"
	evilBytes := "PWNED"
	man.Agents[0].Roots = append(man.Agents[0].Roots, FileEntry{
		Path:   evilManifestPath,
		Bytes:  int64(len(evilBytes)),
		SHA256: sha256Hex(evilBytes),
	})
	writeManifestForTest(t, preSync, man)

	res, err := Restore(preSync, "")
	require.NoError(t, err, "Restore setup must succeed; the bad entry is a per-file failure, not fatal")

	// Locate the two per-file results by their basenames.
	var evil, good *FileResult
	for i := range res.Files {
		switch filepath.Base(res.Files[i].Path) {
		case "evil":
			evil = &res.Files[i]
		case "good.txt":
			good = &res.Files[i]
		}
	}

	// The malicious entry is refused (not silently restored, not a generic
	// "missing backup copy"): OK=false with an Err naming the refusal. This is
	// the assertion that flips between the vulnerable and hardened code.
	require.NotNil(t, evil, "the traversal entry must appear as a per-file result")
	require.False(t, evil.OK, "the traversal entry must not restore")
	require.Contains(t, evil.Err, "refused",
		"the traversal entry must be reported as a containment refusal")

	// No write escaped the agent root: the canary is untouched, and nothing was
	// created under /tmp/aplexica-restore-escape (the reconstructed target).
	got, rerr := os.ReadFile(escapeTarget)
	require.NoError(t, rerr)
	require.Equal(t, canary, string(got), "Restore must not overwrite a path outside the agent root")
	require.NoFileExists(t, "/tmp/aplexica-restore-escape/evil",
		"Restore must not write the reconstructed escape target")

	// The legitimate sibling still restored.
	require.NotNil(t, good, "the good entry must appear as a per-file result")
	require.True(t, good.OK, "a legitimate entry must still restore despite a poisoned sibling: %s", good.Err)
}

// TestNativeTargetForSafe_RejectsTraversal is a focused unit test on the
// reconstruction+validation helper: an entry that escapes the agent root is
// rejected, while a normal nested entry resolves to its absolute native path.
func TestNativeTargetForSafe_RejectsTraversal(t *testing.T) {
	cases := []struct {
		name      string
		agent     string
		path      string
		wantOK    bool
		wantClean string // expected target when wantOK (cleaned), else ignored
	}{
		{
			name:      "normal nested path",
			agent:     "claude",
			path:      "claude/Users/x/.claude/config.json",
			wantOK:    true,
			wantClean: filepath.FromSlash("/Users/x/.claude/config.json"),
		},
		{
			name:   "dotdot escape",
			agent:  "claude",
			path:   "claude/../../etc/passwd",
			wantOK: false,
		},
		{
			name:   "embedded dotdot climbs out",
			agent:  "hermes",
			path:   "hermes/Users/x/../../../../etc/cron.d/evil",
			wantOK: false,
		},
		{
			name:   "agent-prefix mismatch is not silently re-rooted",
			agent:  "claude",
			path:   "../../etc/passwd",
			wantOK: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := nativeTargetForSafe(tc.agent, tc.path)
			require.Equal(t, tc.wantOK, ok, "ok for %q/%q", tc.agent, tc.path)
			if tc.wantOK {
				require.Equal(t, tc.wantClean, got)
			}
		})
	}
}
