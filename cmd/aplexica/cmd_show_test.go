package main

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestSummarizeDoesNotSplitMultibyteRune verifies that summarize truncates on a
// rune boundary rather than a byte offset. The input is padded with ASCII so the
// naive byte slice content[:maxLen] would land inside a 3-byte rune ("世"),
// producing a trailing U+FFFD replacement character / invalid UTF-8.
func TestSummarizeDoesNotSplitMultibyteRune(t *testing.T) {
	const maxLen = 200

	// maxLen-1 ASCII bytes, then a 3-byte rune. A byte cut at maxLen would take
	// the padding plus the FIRST byte of "世", splitting the rune.
	padding := strings.Repeat("a", maxLen-1)
	content := padding + "世" + strings.Repeat("b", 50)

	got := summarize(content, maxLen)

	// The preview portion is everything before our truncation suffix marker.
	prefix := got
	if idx := strings.Index(got, "...("); idx >= 0 {
		prefix = got[:idx]
	}

	if !utf8.ValidString(prefix) {
		t.Errorf("summarize produced invalid UTF-8 in preview prefix: %q", prefix)
	}
	if strings.ContainsRune(prefix, '�') {
		t.Errorf("summarize preview prefix contains U+FFFD replacement char (split rune): %q", prefix)
	}
}
