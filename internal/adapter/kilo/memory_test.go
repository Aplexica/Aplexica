package kilo

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/stretchr/testify/require"
)

func TestRoundTrip_Memory_VariousInputs(t *testing.T) {
	cases := []struct {
		name    string
		content string
	}{
		{"plain", "Hello world.\n"},
		{"empty", ""},
		{"no-trailing-newline", "no newline"},
		{"multiline-markdown", "# Title\n\nPara 1.\n\n- item a\n- item b\n"},
		{"unicode", "héllo — wörld ✓ 🎉\n"},
		{"crlf", "line1\r\nline2\r\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tmp := t.TempDir()
			s := &acf.Store{Root: filepath.Join(tmp, "store")}
			require.NoError(t, s.Init())

			in := filepath.Join(tmp, "in", "AGENTS.md")
			require.NoError(t, os.MkdirAll(filepath.Dir(in), 0o755))
			require.NoError(t, os.WriteFile(in, []byte(tc.content), 0o644))

			a := New()
			ids, err := a.ImportMemory(context.Background(), s, in)
			require.NoError(t, err)
			require.Len(t, ids, 1)

			out := filepath.Join(tmp, "out", "AGENTS.md")
			require.NoError(t, a.ExportMemory(context.Background(), s, ids[0], out))

			got, err := os.ReadFile(out)
			require.NoError(t, err)
			require.Equal(t, tc.content, string(got),
				"kilo memory round-trip MUST be byte-identical")
		})
	}
}
