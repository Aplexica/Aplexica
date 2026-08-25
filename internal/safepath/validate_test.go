package safepath

import (
	"errors"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/aplexica/aplexica/internal/securityerr"
	"github.com/stretchr/testify/require"
)

func TestValidateStoreComponent(t *testing.T) {
	valid := []string{"0197f000-aaaa-7000-8000-000000000001", "config.json", "é", "safe_name-1"}
	for _, component := range valid {
		require.NoError(t, ValidateStoreComponent(component), component)
	}

	invalid := []string{
		"", ".", "..", "a/b", `a\b`, "C:", `C:\temp`, `\\server\share`,
		"file:stream", "name.", "name ", "NUL", "con.txt", "LPT9.log",
		"a\x00b", "a\nb", string([]byte{0xff}), "e\u0301", strings.Repeat("a", MaxStoreComponentBytes+1),
	}
	for _, component := range invalid {
		err := ValidateStoreComponent(component)
		require.Error(t, err, "%q", component)
		require.True(t, errors.Is(err, securityerr.ErrUnsafeIdentifier) || errors.Is(err, securityerr.ErrPathEscape), "%q: %v", component, err)
		if component != "" {
			require.NotContains(t, err.Error(), component)
		}
	}
}

func TestValidateNativeComponentUsesHostFilenameRules(t *testing.T) {
	component := "rollout-2026-06-30T18:16:48.3NZ.jsonl"
	err := ValidateNativeComponent(component)
	if runtime.GOOS == "windows" {
		require.Error(t, err)
		return
	}
	require.NoError(t, err)
	require.NoError(t, ValidateNativeArchiveName("codex/sessions/"+component))
	require.Error(t, ValidateStoreComponent(component), "portable store identifiers remain platform-independent")
	require.Error(t, ValidateArchiveName("codex/sessions/"+component), "portable archives remain platform-independent")
}

func TestValidateArchiveName(t *testing.T) {
	valid := []string{
		"meta.json",
		"acf/memories/0197f000-aaaa-7000-8000-000000000001.json",
		"events/.compacted/conversations/0197f000-aaaa-7000-8000-000000000001.jsonl.gz",
		"secrets/nested/config.json",
	}
	for _, name := range valid {
		require.NoError(t, ValidateArchiveName(name), name)
	}
	invalid := []string{
		"", "/etc/passwd", "../outside", "a/../outside", "a//b", `a\b`,
		"C:/outside", "secrets/NUL", "secrets/name.", "secrets/e\u0301",
		strings.Repeat("a", MaxArchiveNameBytes+1),
	}
	for _, name := range invalid {
		require.Error(t, ValidateArchiveName(name), "%q", name)
	}
}

func TestWithin(t *testing.T) {
	root := t.TempDir()
	require.True(t, Within(root, root))
	require.True(t, Within(root, filepath.Join(root, "nested", "file")))
	require.False(t, Within(root, filepath.Join(root, "..", "outside")))
	require.False(t, Within(root, root+"-sibling"))
}

func FuzzValidateArchiveName(f *testing.F) {
	for _, seed := range []string{"meta.json", "../outside", `C:\outside`, "a//b", "secrets/config.json"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, name string) {
		err := ValidateArchiveName(name)
		if err == nil {
			require.NotContains(t, name, "\\")
			require.False(t, strings.HasPrefix(name, "/"))
		}
	})
}
