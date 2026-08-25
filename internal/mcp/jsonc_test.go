package mcp

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestStripComments_NoComments_Identical(t *testing.T) {
	in := `{"a": 1, "b": "hello"}`
	out := StripComments([]byte(in))
	require.Equal(t, in, string(out))
}

func TestStripComments_LineComments(t *testing.T) {
	in := `{
		"a": 1, // first key
		"b": 2  // second key
	}`
	out := StripComments([]byte(in))

	var parsed map[string]int
	require.NoError(t, json.Unmarshal(out, &parsed))
	require.Equal(t, 1, parsed["a"])
	require.Equal(t, 2, parsed["b"])
}

func TestStripComments_BlockComments(t *testing.T) {
	in := `{
		/* leading block */
		"a": 1,
		/*
		   multi-line
		   block
		*/
		"b": 2
	}`
	out := StripComments([]byte(in))

	var parsed map[string]int
	require.NoError(t, json.Unmarshal(out, &parsed))
	require.Equal(t, 1, parsed["a"])
	require.Equal(t, 2, parsed["b"])
}

func TestStripComments_StringContainingDoubleSlash_NotStripped(t *testing.T) {
	in := `{"url": "https://example.com/path"}`
	out := StripComments([]byte(in))

	var parsed map[string]string
	require.NoError(t, json.Unmarshal(out, &parsed))
	require.Equal(t, "https://example.com/path", parsed["url"],
		"// inside a string MUST NOT be treated as a comment start")
}

func TestStripComments_StringContainingBlockCommentTokens_NotStripped(t *testing.T) {
	in := `{"hint": "use /* block */ syntax for multi-line"}`
	out := StripComments([]byte(in))

	var parsed map[string]string
	require.NoError(t, json.Unmarshal(out, &parsed))
	require.Equal(t, "use /* block */ syntax for multi-line", parsed["hint"])
}

func TestStripComments_EscapedQuoteInString(t *testing.T) {
	in := `{"msg": "she said \"hi\" // greeting"}`
	out := StripComments([]byte(in))

	var parsed map[string]string
	require.NoError(t, json.Unmarshal(out, &parsed))
	require.Equal(t, `she said "hi" // greeting`, parsed["msg"])
}

func TestStripComments_CommentAtEOF(t *testing.T) {
	in := `{"a": 1} // trailing comment with no newline`
	out := StripComments([]byte(in))

	var parsed map[string]int
	require.NoError(t, json.Unmarshal(out, &parsed))
	require.Equal(t, 1, parsed["a"])
}

func TestStripComments_BlockCommentAtEOF(t *testing.T) {
	in := `{"a": 1} /* trailing unclosed?`
	out := StripComments([]byte(in))
	var parsed map[string]int
	require.NoError(t, json.Unmarshal(out, &parsed))
	require.Equal(t, 1, parsed["a"])
}

func TestStripComments_OnlyComments(t *testing.T) {
	in := `// just a line comment`
	out := StripComments([]byte(in))
	require.Empty(t, string(out))
}

func TestStripComments_PreservesWhitespaceShape(t *testing.T) {
	in := "{\n\t\"a\": 1 // here\n}"
	out := StripComments([]byte(in))
	var parsed map[string]int
	require.NoError(t, json.Unmarshal(out, &parsed))
	require.Equal(t, 1, parsed["a"])
}
