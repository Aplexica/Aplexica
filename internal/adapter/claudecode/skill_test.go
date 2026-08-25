package claudecode

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

	src := filepath.Join(tmp, "myskill", "SKILL.md")
	content := "---\nname: my-skill\ndescription: A test skill\n---\n\n# My Skill\n\nBody.\n"
	writeFile(t, src, content)

	a := New()
	ids, err := a.ImportSkill(context.Background(), s, src)
	require.NoError(t, err)
	require.Len(t, ids, 1)

	got, err := s.ReadArtifact(acf.KindSkill, ids[0])
	require.NoError(t, err)
	require.Equal(t, acf.KindSkill, got.Kind)
	require.Equal(t, "SKILL.md", got.Name)
	require.NotEmpty(t, got.HeadEventHash)

	events, err := s.ReadEvents(acf.KindSkill, ids[0])
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.Equal(t, acf.EventTypeCreate, events[0].Type)
	require.Equal(t, "claude-code", events[0].Provenance.SourceAgent)
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
	original := "---\nname: round-trip\ndescription: Test\n---\n\n# Body\n\nLine 1.\nLine 2.\n"
	writeFile(t, src, original)

	a := New()
	ids, err := a.ImportSkill(context.Background(), s, src)
	require.NoError(t, err)

	dest := filepath.Join(tmp, "out", "SKILL.md")
	require.NoError(t, a.ExportSkill(context.Background(), s, ids[0], dest))

	got, err := os.ReadFile(dest)
	require.NoError(t, err)
	require.Equal(t, original, string(got),
		"skill round-trip MUST preserve bytes exactly; same fidelity claim as memory round-trip")
}
