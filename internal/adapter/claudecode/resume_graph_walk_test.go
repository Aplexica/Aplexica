package claudecode

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter"
	"github.com/stretchr/testify/require"
)

// claudeRow renders one JSONL row for the graph fixtures below. Every fixture in
// this file is written row by row on purpose: the shapes being pinned are graph
// shapes Claude Code produces and the materializer never writes, so building
// them through the transcoder would test the transcoder instead.
func claudeRow(fields map[string]any) string {
	encoded, err := json.Marshal(fields)
	if err != nil {
		panic(err)
	}
	return string(encoded)
}

func claudeUserRow(uuid, parent, text string) string {
	return claudeRow(map[string]any{
		"type": "user", "uuid": uuid, "parentUuid": parentOrNil(parent), "sessionId": "graph-fixture",
		"message": map[string]any{"role": "user", "content": text},
	})
}

func claudeAssistantRow(uuid, parent, text string) string {
	return claudeRow(map[string]any{
		"type": "assistant", "uuid": uuid, "parentUuid": parentOrNil(parent), "sessionId": "graph-fixture",
		"message": map[string]any{
			"role": "assistant", "type": "message", "model": "claude-opus-4-8",
			"content": []any{map[string]any{"type": "text", "text": text}},
		},
	})
}

// claudeBridgeRowJSON renders the row type at the centre of this whole class: modern
// Claude Code threads its parentUuid chain THROUGH attachment and system rows,
// which carry a uuid but no conversation.
func claudeBridgeRowJSON(rowType, uuid, parent string) string {
	return claudeRow(map[string]any{
		"type": rowType, "uuid": uuid, "parentUuid": parentOrNil(parent), "sessionId": "graph-fixture",
	})
}

func claudeLastPromptRow(leaf, text string) string {
	return claudeRow(map[string]any{
		"type": "last-prompt", "lastPrompt": text, "leafUuid": leaf, "sessionId": "graph-fixture",
	})
}

func claudeTranscript(rows ...string) []byte {
	return []byte(strings.Join(rows, "\n") + "\n")
}

// --- R1: the recorded leaf is routinely a strict ancestor of the real tip ---

// Claude Code writes the last-prompt row when the prompt is SUBMITTED and
// appends the answer after it, so `leafUuid` names the user row and the
// assistant reply hangs below it. Walking up from that leaf truncates the
// projection by every turn written since, and — because the native append
// parents its canonical suffix at projection.leafUUID — the next write would
// hang that suffix off a node that is no longer the tip and manufacture a real
// fork.
//
// This is the non-fork mismatch shape covered by the regression.
func TestParseClaudeVisibleLeaf_DescendsAStaleLastPromptLeaf(t *testing.T) {
	raw := claudeTranscript(
		claudeUserRow("u1", "", "what is the capital of Canada?"),
		claudeLastPromptRow("u1", "what is the capital of Canada?"),
		claudeAssistantRow("a1", "u1", "Ottawa."),
	)

	projection, err := parseClaudeVisibleLeaf(raw)
	require.NoError(t, err)
	require.Equal(t, []acf.TextTurn{
		{Role: "user", Text: "what is the capital of Canada?"},
		{Role: "assistant", Text: "Ottawa."},
	}, projection.turns, "the answer written after the last-prompt row must be reachable")
	require.Equal(t, 2, projection.nodeCount)
	require.True(t, projection.spans())
	require.False(t, projection.forked)
	require.True(t, projection.leafAdvanced)
	require.Equal(t, "a1", projection.leafUUID,
		"the canonical suffix must be parented at the real tip, not at a stale leaf")
}

// The same, with the answer separated from its prompt by the attachment/system
// bridge rows the reference transcript actually uses, so the descent is
// exercised THROUGH the bridge resolution rather than on a direct child.
func TestParseClaudeVisibleLeaf_DescendsThroughBridgeRows(t *testing.T) {
	raw := claudeTranscript(
		claudeBridgeRowJSON("attachment", "att0", ""),
		claudeUserRow("u1", "att0", "what is the closest planet to Neptune?"),
		claudeLastPromptRow("u1", "what is the closest planet to Neptune?"),
		claudeBridgeRowJSON("system", "sys1", "u1"),
		claudeBridgeRowJSON("system", "sys2", "sys1"),
		claudeAssistantRow("a1", "sys2", "Uranus."),
	)

	projection, err := parseClaudeVisibleLeaf(raw)
	require.NoError(t, err)
	require.Equal(t, []acf.TextTurn{
		{Role: "user", Text: "what is the closest planet to Neptune?"},
		{Role: "assistant", Text: "Uranus."},
	}, projection.turns)
	require.True(t, projection.spans())
	require.False(t, projection.forked)
	require.Equal(t, "a1", projection.leafUUID)
}

// A trailing bookkeeping row below the real tip carries no turn, so there is
// nothing to descend to and the leaf must stay put.
func TestParseClaudeVisibleLeaf_DoesNotDescendIntoTurnlessRows(t *testing.T) {
	raw := claudeTranscript(
		claudeUserRow("u1", "", "what is the capital of Canada?"),
		claudeAssistantRow("a1", "u1", "Ottawa."),
		claudeBridgeRowJSON("system", "sys1", "a1"),
		claudeLastPromptRow("a1", "what is the capital of Canada?"),
	)

	projection, err := parseClaudeVisibleLeaf(raw)
	require.NoError(t, err)
	require.True(t, projection.spans())
	require.False(t, projection.leafAdvanced)
	require.Equal(t, "a1", projection.leafUUID)
}

// --- R1 backward compatibility: a spanning file can never move ---

// THE THEOREM the corpus differential measures and this test proves directly:
// if the shipped walk already spanned a file, the descent cannot execute
// (spanning means every turn-bearing row is on the leaf→root chain, so the leaf
// has no turn-bearing descendant) and the file is provably not forked (a fork
// puts turn-bearing rows on two branches, and one chain can hold only one of
// them).
//
// Generated graphs cover the shapes the corpus holds: bridge chains, split
// empty-thinking rows, <synthetic> replies and sidechains.
func TestParseClaudeVisibleLeaf_SpanningProjectionsAreNeverForkedOrMoved(t *testing.T) {
	rng := rand.New(rand.NewSource(20260731))
	spanning := 0
	for iteration := 0; iteration < 1000; iteration++ {
		rows, wantTurns := generateSpanningClaudeGraph(rng, iteration)
		raw := claudeTranscript(rows...)

		projection, err := parseClaudeVisibleLeaf(raw)
		require.NoErrorf(t, err, "iteration %d: %s", iteration, raw)
		if !projection.spans() {
			t.Fatalf("iteration %d: generator produced a non-spanning graph:\n%s", iteration, raw)
		}
		spanning++
		require.Equalf(t, wantTurns, projection.turns, "iteration %d", iteration)
		require.Falsef(t, projection.forked, "iteration %d: a spanning graph is never forked", iteration)
		require.Falsef(t, projection.leafAdvanced,
			"iteration %d: a spanning graph has nothing below its leaf to descend to", iteration)
	}
	require.Equal(t, 1000, spanning)
}

// generateSpanningClaudeGraph builds one healthy transcript: a single chain of
// user/assistant turns, optionally separated by bridge rows, optionally carrying
// a <synthetic> reply, a split empty-thinking record, or a sidechain sub-agent
// transcript on its own root. The last-prompt row names the real tip, which is
// what makes the graph span.
func generateSpanningClaudeGraph(rng *rand.Rand, seed int) ([]string, []acf.TextTurn) {
	var rows []string
	var turns []acf.TextTurn
	parent := ""
	id := func(kind string, i int) string { return fmt.Sprintf("%s-%d-%d", kind, seed, i) }
	bridges := 0
	turnCount := 1 + rng.Intn(5)
	for i := 0; i < turnCount; i++ {
		for b := rng.Intn(3); b > 0; b-- {
			uuid := id("bridge", bridges)
			bridges++
			kind := "attachment"
			if rng.Intn(2) == 0 {
				kind = "system"
			}
			rows = append(rows, claudeBridgeRowJSON(kind, uuid, parent))
			parent = uuid
		}
		userText := fmt.Sprintf("question %d of %d", i, seed)
		uuid := id("user", i)
		rows = append(rows, claudeUserRow(uuid, parent, userText))
		turns = append(turns, acf.TextTurn{Role: "user", Text: userText})
		parent = uuid

		if rng.Intn(4) == 0 {
			// The split record Claude Code >= 2.1.204 emits before the visible
			// text: a thinking block whose thinking text is empty. It is a parent
			// bridge, never a turn.
			uuid = id("thinking", i)
			rows = append(rows, claudeRow(map[string]any{
				"type": "assistant", "uuid": uuid, "parentUuid": parentOrNil(parent),
				"sessionId": "graph-fixture",
				"message": map[string]any{
					"role": "assistant", "type": "message", "model": "claude-opus-4-8",
					"content": []any{map[string]any{"type": "thinking", "thinking": ""}},
				},
			}))
			parent = uuid
		}
		if rng.Intn(6) == 0 {
			// Claude Desktop's reserved bookkeeping reply.
			uuid = id("synthetic", i)
			rows = append(rows, claudeRow(map[string]any{
				"type": "assistant", "uuid": uuid, "parentUuid": parentOrNil(parent),
				"sessionId": "graph-fixture",
				"message": map[string]any{
					"role": "assistant", "type": "message", "model": "<synthetic>",
					"content": []any{map[string]any{"type": "text", "text": "indexed"}},
				},
			}))
			parent = uuid
		}
		if i == turnCount-1 && rng.Intn(3) == 0 {
			// A prompt still awaiting its answer: the chain ends on the user row.
			break
		}
		answer := fmt.Sprintf("answer %d of %d", i, seed)
		uuid = id("assistant", i)
		rows = append(rows, claudeAssistantRow(uuid, parent, answer))
		turns = append(turns, acf.TextTurn{Role: "assistant", Text: answer})
		parent = uuid
	}
	if rng.Intn(3) == 0 {
		// A Task/sub-agent transcript on its own root. It is conversational in
		// shape but never bears a main-chain turn.
		side := id("side", 0)
		rows = append(rows, claudeRow(map[string]any{
			"type": "user", "uuid": side, "parentUuid": nil, "isSidechain": true,
			"sessionId": "graph-fixture",
			"message":   map[string]any{"role": "user", "content": "sub-agent prompt"},
		}))
	}
	rows = append(rows, claudeLastPromptRow(parent, "last"))
	return rows, turns
}

// --- R2: the fork signal is a direct measurement ---

// A parent with two turn-bearing children is a fork and must still be named one.
func TestParseClaudeVisibleLeaf_GenuineForkIsMeasured(t *testing.T) {
	raw := claudeTranscript(
		claudeUserRow("u1", "", "what is the capital of Canada?"),
		claudeAssistantRow("a1", "u1", "Ottawa."),
		claudeAssistantRow("a2", "u1", "Toronto."),
		claudeLastPromptRow("a1", "what is the capital of Canada?"),
	)

	projection, err := parseClaudeVisibleLeaf(raw)
	require.NoError(t, err)
	require.False(t, projection.spans())
	require.True(t, projection.forked)
	require.Equal(t, adapter.SessionDeclineForkedMirror, projection.declineReason())
}

// The same fork with each branch hidden behind bridge rows, so the measurement
// is exercised through the bridge resolution rather than on direct children.
func TestParseClaudeVisibleLeaf_ForkBehindBridgesIsMeasured(t *testing.T) {
	raw := claudeTranscript(
		claudeUserRow("u1", "", "what is the capital of Canada?"),
		claudeBridgeRowJSON("attachment", "attA", "u1"),
		claudeAssistantRow("a1", "attA", "Ottawa."),
		claudeBridgeRowJSON("system", "sysB", "u1"),
		claudeAssistantRow("a2", "sysB", "Toronto."),
		claudeLastPromptRow("a1", "what is the capital of Canada?"),
	)

	projection, err := parseClaudeVisibleLeaf(raw)
	require.NoError(t, err)
	require.False(t, projection.spans())
	require.True(t, projection.forked, "a fork resolved through bridge rows is still a fork")
	require.Equal(t, adapter.SessionDeclineForkedMirror, projection.declineReason())
}

// The descent must STOP at a fork rather than pick a branch: choosing between
// two turn-bearing children is exactly what a fork means.
func TestParseClaudeVisibleLeaf_DescentStopsAtAFork(t *testing.T) {
	raw := claudeTranscript(
		claudeUserRow("u1", "", "what is the capital of Canada?"),
		claudeLastPromptRow("u1", "what is the capital of Canada?"),
		claudeAssistantRow("a1", "u1", "Ottawa."),
		claudeAssistantRow("a2", "u1", "Toronto."),
	)

	projection, err := parseClaudeVisibleLeaf(raw)
	require.NoError(t, err)
	require.Equal(t, []acf.TextTurn{{Role: "user", Text: "what is the capital of Canada?"}}, projection.turns)
	require.True(t, projection.forked)
	require.Equal(t, "u1", projection.leafUUID)
	require.False(t, projection.leafAdvanced)
}

// --- R3: a chain that still cannot be spanned gets its own honest reason ---

// Two disjoint turn-bearing roots: no node has two conversational branches, so
// this is provably NOT a fork, yet no single chain can reach both. It must be
// named for what it is instead of borrowing the fork's name and the fork's
// remedy.
func TestParseClaudeVisibleLeaf_DisjointRootsAreUnspannedNotForked(t *testing.T) {
	raw := claudeTranscript(
		claudeUserRow("u1", "", "first thread question"),
		claudeAssistantRow("a1", "u1", "first thread answer"),
		claudeUserRow("u2", "", "second thread question"),
		claudeAssistantRow("a2", "u2", "second thread answer"),
		claudeLastPromptRow("a2", "second thread question"),
	)

	projection, err := parseClaudeVisibleLeaf(raw)
	require.NoError(t, err)
	require.False(t, projection.spans())
	require.False(t, projection.forked, "two disjoint roots are not a parent with two children")
	require.Equal(t, adapter.SessionDeclineChainUnspanned, projection.declineReason())
}

// --- R3: the hard-fault classes stay separate and become identifiable ---

func TestParseClaudeVisibleLeaf_HardFaultsAreTyped(t *testing.T) {
	for _, tc := range []struct {
		name  string
		raw   []byte
		fault claudeGraphFault
		msg   string
	}{
		{
			name: "duplicate uuid",
			raw: claudeTranscript(
				claudeUserRow("u1", "", "q"),
				claudeAssistantRow("u1", "u1", "a"),
			),
			fault: claudeGraphFaultDuplicateUUID,
			msg:   `duplicate Claude graph uuid "u1"`,
		},
		{
			name: "missing parent",
			raw: claudeTranscript(
				claudeUserRow("u1", "gone", "q"),
				claudeLastPromptRow("u1", "q"),
			),
			fault: claudeGraphFaultMissingParent,
			msg:   `missing Claude parentUuid node "gone"`,
		},
		{
			name:  "conversational row without uuid",
			raw:   claudeTranscript(claudeUserRow("", "", "q")),
			fault: claudeGraphFaultMissingUUID,
			msg:   "Claude conversational row has no uuid",
		},
		{
			name:  "undecodable row",
			raw:   []byte("{not json}\n"),
			fault: claudeGraphFaultRowDecode,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseClaudeVisibleLeaf(tc.raw)
			require.Error(t, err)
			var graphErr *ClaudeGraphError
			require.ErrorAs(t, err, &graphErr)
			require.Equal(t, tc.fault, graphErr.Fault)
			if tc.msg != "" {
				require.Equal(t, tc.msg, err.Error(), "error text must not drift")
			}
		})
	}
}

// A cycle must still be rejected, and the descent must not spin on it.
func TestParseClaudeVisibleLeaf_CycleIsRejected(t *testing.T) {
	raw := claudeTranscript(
		claudeUserRow("u1", "a1", "q"),
		claudeAssistantRow("a1", "u1", "a"),
		claudeLastPromptRow("a1", "q"),
	)
	_, err := parseClaudeVisibleLeaf(raw)
	require.Error(t, err)
	var graphErr *ClaudeGraphError
	require.ErrorAs(t, err, &graphErr)
	require.Equal(t, claudeGraphFaultCycle, graphErr.Fault)
}

// ResumableTextTurns is the acceptance probe's only entry point into this walk.
// It keeps its message, and gains the typed fault so the probe can tell an
// unreachable row apart from a file it could not parse at all.
func TestResumableTextTurns_TypedUnspannedFault(t *testing.T) {
	raw := claudeTranscript(
		claudeUserRow("u1", "", "first thread question"),
		claudeUserRow("u2", "", "second thread question"),
		claudeLastPromptRow("u2", "second thread question"),
	)
	_, err := ResumableTextTurns(raw)
	require.Error(t, err)
	require.Equal(t, "claude resume graph contains non-visible conversational nodes", err.Error())
	var graphErr *ClaudeGraphError
	require.ErrorAs(t, err, &graphErr)
	require.Equal(t, claudeGraphFaultChainUnspanned, graphErr.Fault)

	forked := claudeTranscript(
		claudeUserRow("u1", "", "q"),
		claudeAssistantRow("a1", "u1", "Ottawa."),
		claudeAssistantRow("a2", "u1", "Toronto."),
		claudeLastPromptRow("a1", "q"),
	)
	_, err = ResumableTextTurns(forked)
	require.Error(t, err)
	require.ErrorAs(t, err, &graphErr)
	require.Equal(t, claudeGraphFaultForked, graphErr.Fault)
}

// --- R2 exemption: the quarantine sweep keeps the containment predicate ---

// bestEffortQuarantineClaudeThreadDuplicates guards a DESTRUCTIVE rename, and
// its question is containment — "is every conversational row in this sibling
// accounted for" — not "is it forked". A sibling whose walk does not span it
// holds rows nobody has proven canonical, so moving it out of ~/.claude would
// hide them. This test exists so a later reader cannot "finish the job" by
// relaxing that site to the fork test with the others.
func TestQuarantineClaudeThreadDuplicates_KeepsUnspannedSibling(t *testing.T) {
	home := t.TempDir()
	a := &Adapter{HomeDir: home}
	turns := []acf.TextTurn{
		{Role: "user", Text: "what is the capital of Canada?"},
		{Role: "assistant", Text: "Ottawa."},
	}
	art := acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion,
		ArtifactID:       acf.NewID(),
		Kind:             acf.KindConversation,
		Scope:            acf.ScopeGlobal,
		SourcePath:       filepath.Join(home, ".codex", "sessions", "rollout-quarantine.jsonl"),
	}
	dest, ok, err := a.MaterializeConversationSession(
		art, canonicalConversationHead(t, art.ArtifactID, turns...), "codex")
	require.NoError(t, err)
	require.True(t, ok)

	// A stale Aplexica-generated sibling of the SAME thread whose graph holds a
	// second, disjoint root: not forked, but not spanned either.
	sibling := filepath.Join(filepath.Dir(dest), "sibling-session.jsonl")
	require.NoError(t, os.WriteFile(sibling, claudeTranscript(
		claudeRow(map[string]any{
			"type": "user", "uuid": "s-u1", "parentUuid": nil, "sessionId": "sibling-session",
			"aplexicaThreadId": art.ArtifactID, "aplexicaBranchId": acf.MainBranch,
			"message": map[string]any{"role": "user", "content": "what is the capital of Canada?"},
		}),
		claudeRow(map[string]any{
			"type": "user", "uuid": "s-u2", "parentUuid": nil, "sessionId": "sibling-session",
			"aplexicaThreadId": art.ArtifactID, "aplexicaBranchId": acf.MainBranch,
			"message": map[string]any{"role": "user", "content": "a row canonical never saw"},
		}),
		claudeRow(map[string]any{
			"type": "last-prompt", "leafUuid": "s-u1", "sessionId": "sibling-session",
			"aplexicaThreadId": art.ArtifactID, "aplexicaBranchId": acf.MainBranch,
		}),
	), 0o644))

	plan, planOK, err := a.conversationSessionPlan(
		art, canonicalConversationHead(t, art.ArtifactID, turns...))
	require.NoError(t, err)
	require.True(t, planOK)
	a.bestEffortQuarantineClaudeThreadDuplicates(plan, art.ArtifactID)

	_, statErr := os.Stat(sibling)
	require.NoError(t, statErr,
		"a sibling holding an unreachable conversational row must never be quarantined")
}

// --- corpus invariants (opt-in; the corpus is the user's own transcripts) ---

// The real-data gate. It runs only when APLEXICA_CLAUDE_CORPUS_DIR points at a
// ~/.claude/projects-shaped tree, because the corpus is private prose that can
// never be committed.
//
// The old/new differential itself lives outside the repo because it needs a
// private transcript corpus and a copy of the shipped walk. What is pinned here
// is the set of invariants that makes the comparison safe.
func TestParseClaudeVisibleLeaf_CorpusInvariants(t *testing.T) {
	root := os.Getenv("APLEXICA_CLAUDE_CORPUS_DIR")
	if root == "" {
		t.Skip("set APLEXICA_CLAUDE_CORPUS_DIR to a ~/.claude/projects tree to run the corpus gate")
	}
	files := claudeCorpusFiles(t, root)
	require.NotEmpty(t, files)

	var parsed, faulted, unspanned, forked int
	for _, path := range files {
		raw, err := os.ReadFile(path)
		require.NoError(t, err)
		projection, err := parseClaudeVisibleLeaf(raw)
		if err != nil {
			faulted++
			var graphErr *ClaudeGraphError
			require.ErrorAsf(t, err, &graphErr, "%s: every walk fault must be typed", path)
			continue
		}
		parsed++
		require.GreaterOrEqualf(t, projection.nodeCount, len(projection.turns),
			"%s: the walk can never report more turns than the file holds turn-bearing rows", path)
		if projection.spans() {
			require.Falsef(t, projection.forked, "%s: a spanning projection is never forked", path)
			continue
		}
		unspanned++
		if projection.forked {
			forked++
			require.Equal(t, adapter.SessionDeclineForkedMirror, projection.declineReason())
		} else {
			require.Equal(t, adapter.SessionDeclineChainUnspanned, projection.declineReason())
		}
		// Determinism: the same bytes must reach the same conclusion, or the
		// structural retry class would be a lie.
		again, againErr := parseClaudeVisibleLeaf(raw)
		require.NoError(t, againErr)
		require.Equal(t, projection.turns, again.turns)
		require.Equal(t, projection.leafUUID, again.leafUUID)
		require.Equal(t, projection.forked, again.forked)
	}
	t.Logf("corpus: %d files, %d parsed, %d hard faults, %d unspanned (%d forked)",
		len(files), parsed, faulted, unspanned, forked)
}

func claudeCorpusFiles(t *testing.T, root string) []string {
	t.Helper()
	dirs, err := os.ReadDir(root)
	require.NoError(t, err)
	var out []string
	for _, dir := range dirs {
		if !dir.IsDir() {
			continue
		}
		entries, readErr := os.ReadDir(filepath.Join(root, dir.Name()))
		if readErr != nil {
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() || filepath.Ext(entry.Name()) != ".jsonl" {
				continue
			}
			out = append(out, filepath.Join(root, dir.Name(), entry.Name()))
		}
	}
	sort.Strings(out)
	return out
}
