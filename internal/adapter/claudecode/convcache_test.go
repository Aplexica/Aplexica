package claudecode

import (
	"fmt"
	"testing"

	"github.com/aplexica/aplexica/internal/acf"
)

// convLine builds one Claude Code session.jsonl line (top-level content form).
func convLine(typ, text string) string {
	return fmt.Sprintf(`{"type":%q,"timestamp":"2026-01-01T00:00:00Z","content":%q}`+"\n", typ, text)
}

func eventsText(t *testing.T, events []acf.ConversationEvent) []string {
	t.Helper()
	out := make([]string, 0, len(events))
	for _, e := range events {
		var s string
		for _, b := range e.Content {
			if b.Type == "text" {
				s = b.Text
			}
		}
		out = append(out, string(e.Role)+":"+s)
	}
	return out
}

func eqStrs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// --- encodeCanonicalFrom resume semantics ---------------------------------

func TestEncodeCanonicalFrom_ResumeMatchesFull(t *testing.T) {
	full := convLine("user", "one") + convLine("assistant", "two") +
		convLine("user", "three") + convLine("assistant", "four")

	wantEvents, _ := encodeCanonicalFrom([]byte(full), 0)

	// Parse the first two rows from 0, capture the resume offset, then parse
	// the remaining tail from there — concat must equal a full parse.
	prefix := convLine("user", "one") + convLine("assistant", "two")
	head, resume := encodeCanonicalFrom([]byte(full), 0)
	_ = head
	// Re-derive resume by parsing only the prefix bytes (independent check).
	_, prefixResume := encodeCanonicalFrom([]byte(prefix), 0)
	if prefixResume != int64(len(prefix))-1 && prefixResume != int64(len(prefix)) {
		// resume lands at the end of the last complete object (before the
		// trailing newline) or exactly at end — both acceptable.
		t.Fatalf("prefix resume = %d, want ~%d", prefixResume, len(prefix))
	}

	prefixEvents, r1 := encodeCanonicalFrom([]byte(full), 0)
	_ = prefixEvents
	_ = r1
	// Incremental: parse [0:resume) then [resume:] and concatenate.
	pe, off := encodeCanonicalFrom([]byte(full[:prefixResume]), 0)
	te, _ := encodeCanonicalFrom([]byte(full), off)
	got := append(append([]acf.ConversationEvent{}, pe...), te...)

	if !eqStrs(eventsText(t, got), eventsText(t, wantEvents)) {
		t.Fatalf("incremental resume mismatch:\n got=%v\nwant=%v", eventsText(t, got), eventsText(t, wantEvents))
	}
	if resume != int64(len(full))-1 && resume != int64(len(full)) {
		t.Fatalf("full resume = %d, want end ~%d", resume, len(full))
	}
}

func TestEncodeCanonicalFrom_PartialLastLine(t *testing.T) {
	complete := convLine("user", "alpha") + convLine("assistant", "beta")
	partial := complete + `{"type":"user","timestamp":"2026-01-01T00:00:00Z","content":"gam` // truncated mid-write

	events, resume := encodeCanonicalFrom([]byte(partial), 0)
	if len(events) != 2 {
		t.Fatalf("partial parse emitted %d events, want 2 (the complete rows only)", len(events))
	}
	// Resume must stay at the end of the last COMPLETE row, not consume the
	// partial tail.
	if resume > int64(len(complete)) {
		t.Fatalf("resume = %d advanced into the partial row (len(complete)=%d)", resume, len(complete))
	}

	// Now flush the rest of the partial row + a newline and resume.
	rest := `ma"}` + "\n"
	whole := partial + rest
	tail, _ := encodeCanonicalFrom([]byte(whole), resume)
	if len(tail) != 1 {
		t.Fatalf("tail parse after flush emitted %d events, want 1", len(tail))
	}
	if got := eventsText(t, tail)[0]; got != "user:gamma" {
		t.Fatalf("tail event = %q, want user:gamma", got)
	}
}

// --- convEncodeCache: incremental equals full -----------------------------

func TestConvEncodeCache_AppendEqualsFullParse(t *testing.T) {
	c := newConvEncodeCache(defaultConvCacheMaxEntries, defaultConvCacheMaxBytes)
	const path = "/p/session.jsonl"

	v1 := convLine("user", "one") + convLine("assistant", "two")
	v2 := v1 + convLine("user", "three")
	v3 := v2 + convLine("assistant", "four")

	c.encode(path, []byte(v1)) // cold → full parse
	c.encode(path, []byte(v2)) // append → incremental
	got := c.encode(path, []byte(v3))

	want, _ := encodeCanonicalFrom([]byte(v3), 0)
	if !eqStrs(eventsText(t, got), eventsText(t, want)) {
		t.Fatalf("cached events != full parse:\n got=%v\nwant=%v", eventsText(t, got), eventsText(t, want))
	}
	if c.fullParses != 1 {
		t.Errorf("fullParses = %d, want 1 (only the cold call)", c.fullParses)
	}
	if c.incParses != 2 {
		t.Errorf("incParses = %d, want 2 (the two appends)", c.incParses)
	}
}

func TestConvEncodeCache_HeadChangeForcesFullReparse(t *testing.T) {
	c := newConvEncodeCache(defaultConvCacheMaxEntries, defaultConvCacheMaxBytes)
	const path = "/p/session.jsonl"

	v1 := convLine("user", "one") + convLine("assistant", "two")
	c.encode(path, []byte(v1))

	// A compaction-style rewrite: same path, longer, but the HEAD changed.
	v2 := convLine("system", "compacted") + convLine("user", "fresh") + convLine("assistant", "reply")
	got := c.encode(path, []byte(v2))

	want, _ := encodeCanonicalFrom([]byte(v2), 0)
	if !eqStrs(eventsText(t, got), eventsText(t, want)) {
		t.Fatalf("head-change reparse wrong:\n got=%v\nwant=%v", eventsText(t, got), eventsText(t, want))
	}
	if c.fullParses != 2 {
		t.Errorf("fullParses = %d, want 2 (cold + head-change)", c.fullParses)
	}
	if c.incParses != 0 {
		t.Errorf("incParses = %d, want 0 (no valid append)", c.incParses)
	}
}

func TestConvEncodeCache_ShrinkForcesFullReparse(t *testing.T) {
	c := newConvEncodeCache(defaultConvCacheMaxEntries, defaultConvCacheMaxBytes)
	const path = "/p/session.jsonl"

	v1 := convLine("user", "one") + convLine("assistant", "two") + convLine("user", "three")
	c.encode(path, []byte(v1))

	// File truncated to a shorter prefix → cannot be an append.
	v2 := convLine("user", "one")
	got := c.encode(path, []byte(v2))
	want, _ := encodeCanonicalFrom([]byte(v2), 0)
	if !eqStrs(eventsText(t, got), eventsText(t, want)) {
		t.Fatalf("shrink reparse wrong: got=%v want=%v", eventsText(t, got), eventsText(t, want))
	}
	if c.incParses != 0 {
		t.Errorf("incParses = %d, want 0 after shrink", c.incParses)
	}
}

func TestConvEncodeCache_LRUEvictsByEntryCount(t *testing.T) {
	c := newConvEncodeCache(2, defaultConvCacheMaxBytes) // cap = 2 entries
	a := convLine("user", "a")
	b := convLine("user", "b")
	d := convLine("user", "d")

	c.encode("/a", []byte(a)) // {a}
	c.encode("/b", []byte(b)) // {a,b}
	c.encode("/d", []byte(d)) // {b,d} — /a evicted (LRU)

	if _, ok := c.peek("/a"); ok {
		t.Errorf("/a should have been evicted at entry-count cap")
	}
	if _, ok := c.peek("/b"); !ok {
		t.Errorf("/b should still be cached")
	}
	if _, ok := c.peek("/d"); !ok {
		t.Errorf("/d should still be cached")
	}

	// Re-encoding the evicted /a must full-parse (cold) again.
	before := c.fullParses
	c.encode("/a", []byte(a))
	if c.fullParses != before+1 {
		t.Errorf("re-encoding evicted path should full-parse; fullParses %d→%d", before, c.fullParses)
	}
}
