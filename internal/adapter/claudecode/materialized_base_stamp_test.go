package claudecode

import (
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter"
)

// A generated session that was first materialized with one turn and then
// extended by the incremental appender must still describe its COMPLETE
// materialized base. The append path previously stamped only the thread and
// branch, so the base marker stayed frozen at the first full transcode's count
// while the file grew. Ingest then measured a 1-turn base against a 2-turn
// file, the turns-hash loop break in MergeConversationByThreadRef could not
// fire, and the continuation was unioned by timestamp into duplicate turns.
func TestClaudeGeneratedSessionBaseStampCoversIncrementalAppends(t *testing.T) {
	const threadID = "019e0000-0000-7000-8000-000000000102"
	base := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	first := []acf.TextTurn{{Role: "user", Text: "What is the sample color?"}}
	appended := []acf.TextTurn{{Role: "assistant", Text: "The sample color is blue."}}
	complete := append(append([]acf.TextTurn(nil), first...), appended...)

	raw := transcodeToClaudeSessionWithThread(first, threadID, threadID, acf.MainBranch, "/Users/exampleuser", "codex", "", base)
	raw += transcodeClaudeTurnAppend(appended, threadID, threadID, acf.MainBranch, true, complete, nil, len(first), "/Users/exampleuser", base, "", "")

	ref, ok := claudeSessionThreadRef([]byte(raw))
	if !ok {
		t.Fatal("generated session did not yield a thread ref")
	}
	if ref.MaterializedTurnCount != len(complete) {
		t.Fatalf("MaterializedTurnCount = %d, want %d (the complete materialized base)", ref.MaterializedTurnCount, len(complete))
	}
	if want := adapter.ConversationTurnsHash(complete); ref.MaterializedTurnsHash != want {
		t.Fatalf("MaterializedTurnsHash = %q, want %q (hash of the complete base)", ref.MaterializedTurnsHash, want)
	}
}

// Generated output can never carry two stamps with an equal turn count but a
// differing hash — each generation's count strictly grows. That shape can
// only come from a corrupted or hand-edited file. The reader must fail
// closed (GeneratedSnapshot=false) rather than silently trust whichever
// equal-count stamp it happened to read first.
func TestClaudeGeneratedSessionEqualCountDifferingHashFailsClosed(t *testing.T) {
	const threadID = "019e0000-0000-7000-8000-000000000102"
	raw := `{"type":"user","aplexicaThreadId":"` + threadID + `","aplexicaBranchId":"main","aplexicaTurnCount":2,"aplexicaTurnsHash":"hash-a"}` + "\n" +
		`{"type":"assistant","aplexicaThreadId":"` + threadID + `","aplexicaBranchId":"main","aplexicaTurnCount":2,"aplexicaTurnsHash":"hash-b"}` + "\n"

	ref, ok := claudeSessionThreadRef([]byte(raw))
	if !ok {
		t.Fatal("stamped session did not yield a thread ref")
	}
	if ref.GeneratedSnapshot {
		t.Fatal("two stamps with an equal turn count but a differing hash must fail closed " +
			"(GeneratedSnapshot=false), not silently trust whichever stamp was read first")
	}
}
