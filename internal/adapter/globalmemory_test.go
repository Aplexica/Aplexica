package adapter

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

const baseBody = "# Project memory\n\n## Conventions\n- Language: Go 1.23.\n\n## Preferences\n- user name is Example User\n"
const cometExtra = "# Personal memory\n\n- Example User's dog's name is Comet.\n"

func TestComposeAppendedMemory_AppendsNewEntries(t *testing.T) {
	got := ComposeAppendedMemory(baseBody, []string{cometExtra})
	require.Contains(t, got, baseBody)
	require.Contains(t, got, "- Example User's dog's name is Comet.")
	require.True(t, len(got) > len(baseBody))
}

func TestComposeAppendedMemory_NoExtras_ByteIdentical(t *testing.T) {
	require.Equal(t, baseBody, ComposeAppendedMemory(baseBody, nil))
	require.Equal(t, baseBody, ComposeAppendedMemory(baseBody, []string{"", "   \n"}))
}

func TestComposeAppendedMemory_NoDuplicateOfEntryInBody(t *testing.T) {
	body := "# Project memory\n\n- Example User's dog's name is Comet.\n"
	got := ComposeAppendedMemory(body, []string{cometExtra})
	require.Equal(t, 1, countOccurrences(got, "- Example User's dog's name is Comet."))
}

func TestStripIsInverseOfCompose(t *testing.T) {
	composed := ComposeAppendedMemory(baseBody, []string{cometExtra})
	require.Equal(t, baseBody, StripAppendedMemory(composed, []string{cometExtra}))
}

func TestStrip_CollapsesSeparatorLeftByMiddleBlock(t *testing.T) {
	incoming := "# Shared instructions\n\n# Personal memory\n- Example User's dog's name is Comet.\n\nExample User lives in Example City.\n"
	got := StripAppendedMemory(incoming, []string{cometExtra})
	require.Equal(t, "# Shared instructions\n\nExample User lives in Example City.\n", got)
}

func TestStrip_KeepsGenuinelyNewMemory(t *testing.T) {
	incoming := baseBody + "\n# Personal memory\n- Example User's dog's name is Comet.\n- Favorite editor is vim.\n"
	got := StripAppendedMemory(incoming, []string{cometExtra})
	require.NotContains(t, got, "Comet")
	require.Contains(t, got, "- Favorite editor is vim.")
}

func TestReadMarkdownDir_SortedAndSkips(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "MEMORY.md"), []byte("# index\n- [Dogs](dogs.md)\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "dogs.md"), []byte("# Dogs\n- Comet and Nova\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "zoo.md"), []byte("# Zoo\n- lions\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("ignored"), 0o644))

	got := ReadMarkdownDir(dir, "MEMORY.md")
	require.Len(t, got, 2, "only .md files, index skipped")
	require.Contains(t, got[0], "Comet") // dogs.md sorts before zoo.md
	require.Contains(t, got[1], "lions")
	for _, c := range got {
		require.NotContains(t, c, "[Dogs](dogs.md)", "MEMORY.md index must be skipped")
	}
}

func TestReadMarkdownDir_MissingDir(t *testing.T) {
	require.Nil(t, ReadMarkdownDir(filepath.Join(t.TempDir(), "nope")))
}

func countOccurrences(haystack, needle string) int {
	n := 0
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			n++
		}
	}
	return n
}
