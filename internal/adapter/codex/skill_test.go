package codex

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/stretchr/testify/require"
)

func TestImportSkill_WritesArtifactAndEvent(t *testing.T) {
	tmp := t.TempDir()
	s := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, s.Init())

	src := filepath.Join(tmp, "skills", "my-skill", "SKILL.md")
	content := "---\nname: codex-skill\ndescription: Test\n---\n\n# Body\n"
	writeFile(t, src, content)

	a := New()
	ids, err := a.ImportSkill(context.Background(), s, src)
	require.NoError(t, err)
	require.Len(t, ids, 1)

	got, err := s.ReadArtifact(acf.KindSkill, ids[0])
	require.NoError(t, err)
	require.Equal(t, acf.KindSkill, got.Kind)
	require.Equal(t, "SKILL.md", got.Name)

	events, err := s.ReadEvents(acf.KindSkill, ids[0])
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.Equal(t, "codex", events[0].Provenance.SourceAgent)
	require.NoError(t, acf.VerifyChain(events))

	payload, err := acf.DecodeSkillPayload(events[0])
	require.NoError(t, err)
	require.Equal(t, "skill.md", payload.Format)
	require.Equal(t, content, payload.Content)
}

func TestExportSkill_WritesBytesIdenticalToImport(t *testing.T) {
	tmp := t.TempDir()
	s := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, s.Init())

	src := filepath.Join(tmp, "in", "SKILL.md")
	original := "---\nname: round\n---\n# Body\nLine 2\n"
	writeFile(t, src, original)

	a := New()
	ids, err := a.ImportSkill(context.Background(), s, src)
	require.NoError(t, err)

	dest := filepath.Join(tmp, "out", "SKILL.md")
	require.NoError(t, a.ExportSkill(context.Background(), s, ids[0], dest))

	got, err := os.ReadFile(dest)
	require.NoError(t, err)
	require.Equal(t, original, string(got))
}
