package hermes

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/stretchr/testify/require"
)

func TestRoundTrip_Skill_VariousInputs(t *testing.T) {
	cases := []struct {
		name    string
		content string
	}{
		{"minimal", "---\nname: x\n---\n# Hi\n"},
		{"full-frontmatter", "---\nname: full\ndescription: A skill\nversion: 1.0.0\n---\n# Body\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tmp := t.TempDir()
			s := &acf.Store{Root: filepath.Join(tmp, "store")}
			require.NoError(t, s.Init())

			in := filepath.Join(tmp, "in", "SKILL.md")
			require.NoError(t, os.MkdirAll(filepath.Dir(in), 0o755))
			require.NoError(t, os.WriteFile(in, []byte(tc.content), 0o644))

			a := New()
			ids, err := a.ImportSkill(context.Background(), s, in)
			require.NoError(t, err)

			out := filepath.Join(tmp, "out", "SKILL.md")
			require.NoError(t, a.ExportSkill(context.Background(), s, ids[0], out))

			got, err := os.ReadFile(out)
			require.NoError(t, err)
			require.Equal(t, tc.content, string(got))
		})
	}
}
