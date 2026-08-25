package adapter

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/stretchr/testify/require"
)

func testSkillEncode(content []byte) (json.RawMessage, error) {
	return acf.EncodePayload(acf.SkillPayload{Format: "skill.md", Content: string(content)})
}

func testSkillDecode(e acf.Event) (string, error) {
	p, err := acf.DecodeSkillPayload(e)
	if err != nil {
		return "", err
	}
	return p.Content, nil
}

func testParams(agent string) OpaqueParams {
	return OpaqueParams{
		DeviceID:       "dev",
		SourceAgent:    agent,
		AdapterVersion: "test",
		InferScope:     func(string) acf.Scope { return acf.ScopeGlobal },
	}
}

func TestSkillMarker_RoundTrip(t *testing.T) {
	content := []byte("---\nname: x\n---\n\nBody.\n")
	marked := AppendSkillMarker(content, "id-123")
	id, stripped, found := ParseSkillMarker(marked)
	require.True(t, found)
	require.Equal(t, "id-123", id)
	require.Equal(t, string(content), string(stripped))

	_, same, found := ParseSkillMarker(content)
	require.False(t, found)
	require.Equal(t, string(content), string(same))

	// A marker-like string mid-document is NOT a marker (last line only).
	doc := []byte("The format is <!-- aplexica:skill:demo --> as shown.\n\nMore text.\n")
	_, _, found = ParseSkillMarker(doc)
	require.False(t, found)
}

func TestSkillPathVendorHidden(t *testing.T) {
	require.True(t, SkillPathVendorHidden("/h/.codex/skills/.system/skill-creator/SKILL.md"),
		"codex vendor skills under skills/.system must not import")
	require.False(t, SkillPathVendorHidden("/h/.codex/skills/deploy-helper/SKILL.md"))
	require.False(t, SkillPathVendorHidden("/h/.claude/skills/pdf-tools/SKILL.md"),
		"dot-dirs BEFORE the skills segment (the agent root) are exempt")
	require.False(t, SkillPathVendorHidden("/proj/SKILL.md"), "no skills segment at all")
}

// One skill, fan-out copy in another agent's watched tree: importing the
// marked copy must not mint a duplicate artifact (the echo that
// turned one probe skill into 13 artifacts).
func TestImportSkillReconciled_MarkedEchoIsInert(t *testing.T) {
	tmp := t.TempDir()
	store := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, store.Init())

	src := filepath.Join(tmp, ".claude", "skills", "probe", "SKILL.md")
	require.NoError(t, os.MkdirAll(filepath.Dir(src), 0o755))
	require.NoError(t, os.WriteFile(src, []byte("# Probe\n\nReply OK.\n"), 0o644))
	ids, err := ImportSkillReconciled(context.Background(), store, testParams("claude-code"), src, testSkillEncode)
	require.NoError(t, err)
	require.Len(t, ids, 1)
	canonical := ids[0]

	// Cross-agent materialization (codex's copy carries the marker).
	copyPath := filepath.Join(tmp, ".codex", "skills", "probe", "SKILL.md")
	require.NoError(t, os.MkdirAll(filepath.Dir(copyPath), 0o755))
	require.NoError(t, ExportSkillMarked(context.Background(), store, "codex", canonical, copyPath, testSkillDecode))
	b, _ := os.ReadFile(copyPath)
	require.Contains(t, string(b), skillMarkerPrefix, "cross-agent copy must carry the provenance marker")

	// The watcher re-imports the copy -> must be a true no-op. Returning the
	// existing id would let SourcePath-based freshness checks treat the copied
	// path as a fresh commit and fan it out again.
	evBefore, _ := store.ReadEvents(acf.KindSkill, canonical)
	ids2, err := ImportSkillReconciled(context.Background(), store, testParams("codex"), copyPath, testSkillEncode)
	require.NoError(t, err)
	require.Empty(t, ids2)
	evAfter, _ := store.ReadEvents(acf.KindSkill, canonical)
	require.Equal(t, len(evBefore), len(evAfter), "pure echo must append no event")

	// Same-agent export (back to the origin) stays marker-free.
	require.NoError(t, ExportSkillMarked(context.Background(), store, "claude-code", canonical, src, testSkillDecode))
	b, _ = os.ReadFile(src)
	require.NotContains(t, string(b), skillMarkerPrefix, "origin-agent export must stay byte-identical to canonical content")
}

func TestExportSkillMarked_SameAgentNativeMirrorKeepsMarker(t *testing.T) {
	tmp := t.TempDir()
	store := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, store.Init())

	src := filepath.Join(tmp, ".claude", "skills", "probe", "SKILL.md")
	require.NoError(t, os.MkdirAll(filepath.Dir(src), 0o755))
	require.NoError(t, os.WriteFile(src, []byte("# Probe\n"), 0o644))
	ids, err := ImportSkillReconciled(context.Background(), store, testParams("claude-code"), src, testSkillEncode)
	require.NoError(t, err)
	require.Len(t, ids, 1)

	mirror := filepath.Join(tmp, "worktree", ".claude", "skills", "probe", "SKILL.md")
	ctx := WithNativeMirror(context.Background())
	require.NoError(t, ExportSkillMarked(ctx, store, "claude-code", ids[0], mirror, testSkillDecode))
	got, err := os.ReadFile(mirror)
	require.NoError(t, err)
	require.Contains(t, string(got), "<!-- aplexica:skill:"+ids[0]+" -->",
		"same-agent surface copies need the identity marker for later edit reconciliation")
}

// Editing the COPY updates the canonical artifact (bidirectional sync from
// any agent's materialization), preserving the original artifact identity.
func TestImportSkillReconciled_EditedCopyUpdatesCanonical(t *testing.T) {
	tmp := t.TempDir()
	store := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, store.Init())

	src := filepath.Join(tmp, ".claude", "skills", "probe", "SKILL.md")
	require.NoError(t, os.MkdirAll(filepath.Dir(src), 0o755))
	require.NoError(t, os.WriteFile(src, []byte("# Probe v1\n"), 0o644))
	ids, err := ImportSkillReconciled(context.Background(), store, testParams("claude-code"), src, testSkillEncode)
	require.NoError(t, err)
	canonical := ids[0]

	copyPath := filepath.Join(tmp, ".kilo", "skills", "probe", "SKILL.md")
	require.NoError(t, os.MkdirAll(filepath.Dir(copyPath), 0o755))
	require.NoError(t, ExportSkillMarked(context.Background(), store, "kilo", canonical, copyPath, testSkillDecode))

	// User edits the kilo copy (marker still present at EOF).
	b, _ := os.ReadFile(copyPath)
	edited := []byte("# Probe v2 — edited in kilo\n" + string(b[len("# Probe v1\n"):]))
	require.NoError(t, os.WriteFile(copyPath, edited, 0o644))

	ids2, err := ImportSkillReconciled(context.Background(), store, testParams("kilo"), copyPath, testSkillEncode)
	require.NoError(t, err)
	require.Equal(t, []string{canonical}, ids2, "copy edit must update the canonical artifact, not mint a new one")

	content, tomb, err := ReplayOpaqueContent(store, acf.KindSkill, canonical, testSkillDecode)
	require.NoError(t, err)
	require.False(t, tomb)
	require.Contains(t, content, "Probe v2", "canonical content carries the copy's edit")
	require.NotContains(t, content, skillMarkerPrefix, "marker is stripped before storage")

	events, _ := store.ReadEvents(acf.KindSkill, canonical)
	require.Equal(t, "kilo", events[len(events)-1].Provenance.SourceAgent, "update provenance names the editing agent")

	art, _ := store.ReadArtifact(acf.KindSkill, canonical)
	require.Equal(t, src, art.SourcePath, "SourcePath stays the origin agent's file")
}

func TestImportSkillReconciled_StaleMarkerImportsFresh(t *testing.T) {
	tmp := t.TempDir()
	store := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, store.Init())

	p := filepath.Join(tmp, ".codex", "skills", "orphan", "SKILL.md")
	require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
	require.NoError(t, os.WriteFile(p, AppendSkillMarker([]byte("# Orphan\n"), "no-such-artifact"), 0o644))

	ids, err := ImportSkillReconciled(context.Background(), store, testParams("codex"), p, testSkillEncode)
	require.NoError(t, err)
	require.Len(t, ids, 1)
	content, _, err := ReplayOpaqueContent(store, acf.KindSkill, ids[0], testSkillDecode)
	require.NoError(t, err)
	require.NotContains(t, content, skillMarkerPrefix, "stale marker must not be stored")
}

func TestImportSkillReconciled_VendorHiddenSkipped(t *testing.T) {
	tmp := t.TempDir()
	store := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, store.Init())

	p := filepath.Join(tmp, ".codex", "skills", ".system", "skill-creator", "SKILL.md")
	require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
	require.NoError(t, os.WriteFile(p, []byte("# Vendor\n"), 0o644))

	ids, err := ImportSkillReconciled(context.Background(), store, testParams("codex"), p, testSkillEncode)
	require.NoError(t, err)
	require.Empty(t, ids, "vendor-internal skills must not enter the store")
}
