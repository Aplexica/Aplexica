package adapter

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Several agents expose memory through TWO surfaces: a single hand-authored
// instruction file (Codex ~/.codex/AGENTS.md, Claude Code ~/.claude/CLAUDE.md)
// AND an auto-managed multi-file memory layer in a sibling directory (Codex
// ~/.codex/memories/*.md, Claude Code ~/.claude/projects/<cwd>/memory/*.md).
//
// Whole-file memory replication assumes ONE canonical memory file per agent, so
// the two surfaces can't both be peer artifacts — a second global-memory
// artifact would map to the same single NativePath and clobber the first. The
// helpers here fold the managed layer INTO the single instruction-file-keyed
// artifact (ComposeAppendedMemory) so a memory captured from the managed layer
// fans out to other agents, while the export side strips those same entries
// back out before writing the instruction file (StripAppendedMemory) so the
// hand-authored file is never polluted with a memory it already holds. The two
// transforms are inverses, which keeps the cross-agent round-trip byte-stable.

// ComposeAppendedMemory returns base with the extras entries that are NOT
// already present appended after it. An "entry" is a non-blank line, matched on
// its trimmed text. When extras contribute nothing new, base is returned
// VERBATIM — this guarantees the no-extras path is byte-identical (so a pristine
// instruction file never churns and the round-trip stays exact). Inverse of
// StripAppendedMemory.
func ComposeAppendedMemory(base string, extras []string) string {
	present := map[string]bool{}
	for _, ln := range strings.Split(base, "\n") {
		if t := strings.TrimSpace(ln); t != "" {
			present[t] = true
		}
	}
	var extra []string
	seen := map[string]bool{}
	for _, mc := range extras {
		for _, ln := range strings.Split(mc, "\n") {
			t := strings.TrimSpace(ln)
			if t == "" || present[t] || seen[t] {
				continue
			}
			seen[t] = true
			extra = append(extra, strings.TrimRight(ln, " \t\r"))
		}
	}
	if len(extra) == 0 {
		return base
	}
	block := strings.Join(extra, "\n") + "\n"
	if strings.TrimSpace(base) == "" {
		return block
	}
	body := base
	if !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	// Blank-line separator between the body and the appended block.
	return body + "\n" + block
}

// StripAppendedMemory removes from content every line whose trimmed text matches
// an extras entry, then trims the trailing blank lines the removal leaves behind
// down to a single newline. This is the inverse of ComposeAppendedMemory for the
// appended-block case:
//
//	StripAppendedMemory(ComposeAppendedMemory(body, extras), extras) == body
//
// so writing a composed memory back to its instruction file reproduces the
// pristine body byte-for-byte. With no extras entries it returns content
// unchanged.
func StripAppendedMemory(content string, extras []string) string {
	strip := map[string]bool{}
	for _, mc := range extras {
		for _, ln := range strings.Split(mc, "\n") {
			if t := strings.TrimSpace(ln); t != "" {
				strip[t] = true
			}
		}
	}
	if len(strip) == 0 {
		return content
	}
	in := strings.Split(content, "\n")
	out := make([]string, 0, len(in))
	removedSinceKept := false
	for _, ln := range in {
		if t := strings.TrimSpace(ln); t != "" && strip[t] {
			removedSinceKept = true
			continue
		}
		if strings.TrimSpace(ln) == "" && removedSinceKept {
			if len(out) == 0 || strings.TrimSpace(out[len(out)-1]) == "" {
				continue
			}
		}
		out = append(out, ln)
		removedSinceKept = false
	}
	res := strings.TrimRight(strings.Join(out, "\n"), "\n \t")
	if res != "" {
		res += "\n"
	}
	return res
}

// ReadMarkdownDir reads dir/*.md in sorted filename order, skipping any basename
// in skip (e.g. an index file that only references its siblings). A missing
// directory or unreadable file yields no entries (best-effort).
func ReadMarkdownDir(dir string, skip ...string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	skipSet := map[string]bool{}
	for _, s := range skip {
		skipSet[s] = true
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".md" || skipSet[e.Name()] {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	out := make([]string, 0, len(names))
	for _, n := range names {
		b, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil {
			continue
		}
		out = append(out, string(b))
	}
	return out
}
