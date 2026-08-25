package openclaw

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/stretchr/testify/require"
)

func TestImportSkill_RoundTrip(t *testing.T) {
	tmp := t.TempDir()
	storeRoot := filepath.Join(tmp, "store")
	store := &acf.Store{Root: storeRoot}
	require.NoError(t, store.Init())

	skillPath := filepath.Join(tmp, "SKILL.md")
	body := "---\nname: example\n---\n\n# Example skill\n"
	require.NoError(t, os.WriteFile(skillPath, []byte(body), 0o644))

	a := &Adapter{HomeDir: tmp, DeviceID: "dev"}
	ids, err := a.ImportSkill(context.Background(), store, skillPath)
	require.NoError(t, err)
	require.Len(t, ids, 1)

	outPath := filepath.Join(tmp, "out-SKILL.md")
	require.NoError(t, a.ExportSkill(context.Background(), store, ids[0], outPath))

	got, err := os.ReadFile(outPath)
	require.NoError(t, err)
	require.Equal(t, body, string(got))
}
