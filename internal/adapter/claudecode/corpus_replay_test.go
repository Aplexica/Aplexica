package claudecode

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/stretchr/testify/require"
)

// The real-corpus replay. It is OPT-IN — set APLEXICA_CLAUDE_CORPUS to a
// directory of Claude project folders, e.g. ~/.claude/projects — because it
// reads a developer's own transcripts and there is nothing to read in CI.
//
// It exists because every claim this change rests on is a claim about real
// files: which shapes the walk cannot span, how many of them are genuinely
// forked, and whether the rewritten walk can ever return FEWER turns than the
// one it replaces. A fixture cannot answer the last one; only the corpus can.
//
// It is strictly read-only. Nothing here writes, renames or repairs anything.
func claudeReplayCorpus(t *testing.T) []string {
	t.Helper()
	root := os.Getenv("APLEXICA_CLAUDE_CORPUS_DIR")
	if root == "" {
		t.Skip("set APLEXICA_CLAUDE_CORPUS_DIR to a ~/.claude/projects tree to run the corpus replay")
	}
	files := claudeCorpusFiles(t, root)
	require.NotEmpty(t, files, "no transcripts under %s", root)
	return files
}

// THE ANTI-REGRESSION PROPERTY, stated as strongly as it can be: over every
// transcript on the device, the rewritten walk must never return a turn
// sequence that is missing something the shipped walk found. A file may gain
// turns — that is the point of descending a stale last-prompt leaf — but it may
// never lose one, and a file the shipped walk parsed must not start erroring.
//
// The comparison runs against a verbatim copy of the shipped walk
// (shippedClaudeVisibleLeaf below) rather than against recorded numbers, so it
// keeps answering the question after both sides change.
func TestCorpusReplay_NewWalkNeverLosesATurn(t *testing.T) {
	files := claudeReplayCorpus(t)
	identical, gained, newErrors := 0, 0, 0
	gainedTurns := 0
	var spans, unspanned, forked int
	for _, path := range files {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		before, beforeErr := shippedClaudeVisibleLeaf(raw)
		after, afterErr := parseClaudeVisibleLeaf(raw)
		if beforeErr != nil {
			if afterErr == nil {
				gained++
			}
			continue
		}
		if afterErr != nil {
			newErrors++
			t.Errorf("%s: the rewritten walk errors on a file the shipped walk parsed: %v",
				filepath.Base(path), afterErr)
			continue
		}
		if after.spans() {
			spans++
		} else {
			unspanned++
		}
		if after.forked {
			forked++
		}
		require.True(t, claudeTextTurnsSubsequence(before.turns, after.turns),
			"%s: the rewritten walk lost a turn (%d -> %d)",
			filepath.Base(path), len(before.turns), len(after.turns))
		switch {
		case len(after.turns) == len(before.turns):
			identical++
		default:
			gained++
			gainedTurns += len(after.turns) - len(before.turns)
		}
	}
	require.Zero(t, newErrors)
	t.Logf("corpus=%d identical=%d gained=%d turnsRecovered=%d spans=%d unspanned=%d forked=%d",
		len(files), identical, gained, gainedTurns, spans, unspanned, forked)
}

// The second corpus property, and the one the data-loss findings are about: for
// every transcript the loss proof would authorize a rebuild of, the rebuild must
// preserve every uuid the file holds and every uuid-bearing row's bytes.
//
// It replays the real rebuild renderer against the file's own physical-order
// turns, which is what canonical holds for a file imported from itself.
func TestCorpusReplay_RebuildPreservesEveryUUIDBearingRow(t *testing.T) {
	files := claudeReplayCorpus(t)
	proven, checked := 0, 0
	for _, path := range files {
		raw, err := os.ReadFile(path)
		if err != nil || len(raw) == 0 {
			continue
		}
		state, resume := encodeCanonicalInto(raw, 0, claudeCanonicalState{})
		if state.hasExplicitThreadStamp || state.sessionID == "" {
			continue
		}
		if len(strings.TrimSpace(string(raw[resume:]))) != 0 {
			continue
		}
		planned := acf.ExtractTextTurns(state.events)
		if len(planned) == 0 {
			continue
		}
		contained, result, _ := claudeMirrorRowsContained(raw, planned)
		if !contained {
			continue
		}
		proven++
		rebuilt := transcodeClaudeNativeSession(
			result.preamble, planned,
			claudeRebuildUUIDs(state.sessionID, len(planned), result), result.bridges,
			state.sessionID, state.lastCWD, time.Time{})
		require.NotEmpty(t, rebuilt, "%s: a contained file must render", filepath.Base(path))
		for _, uuid := range corpusRowUUIDs(raw) {
			require.Contains(t, rebuilt, uuid,
				"%s: the rebuild deleted uuid %s", filepath.Base(path), uuid)
			checked++
		}
		// And the rebuilt file is a single walkable chain holding exactly the
		// planned turns, which is what the post-write verification demands.
		ok, err := claudeNativeSessionMatches([]byte(rebuilt), planned, state.sessionID)
		require.NoError(t, err)
		require.True(t, ok, "%s: the rebuild must verify against its own plan", filepath.Base(path))
	}
	t.Logf("containment-proven files=%d uuids preserved=%d", proven, checked)
}

// The shipped walk's diagnoses, as sentinel errors. Only their PRESENCE matters
// to the differential — it compares turn sequences, not messages — so they are
// named rather than formatted.
var (
	errShippedWalkNoUUID        = errors.New("conversational row has no uuid")
	errShippedWalkDuplicate     = errors.New("duplicate graph uuid")
	errShippedWalkMultiTurn     = errors.New("row encodes multiple text turns")
	errShippedWalkCycle         = errors.New("cycle in parentUuid graph")
	errShippedWalkMissingParent = errors.New("missing parentUuid node")
)

func corpusRowUUIDs(raw []byte) []string {
	var out []string
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var row struct {
			UUID string `json:"uuid"`
		}
		if json.Unmarshal([]byte(line), &row) != nil || row.UUID == "" {
			continue
		}
		out = append(out, row.UUID)
	}
	return out
}

// shippedClaudeVisibleLeaf is the walk as it stood before this change, copied
// verbatim so the differential above compares behaviour rather than recorded
// numbers. It is test-only and must not be "refactored to share code" with the
// live walk: sharing is precisely what would make the comparison vacuous.
func shippedClaudeVisibleLeaf(raw []byte) (claudeLeafProjection, error) {
	type node struct {
		parentUUID string
		synthetic  bool
		turn       *acf.TextTurn
	}
	nodes := make(map[string]node)
	portableNodeCount := 0
	leafUUID := ""
	fallbackLeaf := ""
	for _, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		var row struct {
			Type        string  `json:"type"`
			UUID        string  `json:"uuid"`
			ParentUUID  *string `json:"parentUuid"`
			LeafUUID    string  `json:"leafUuid"`
			IsSidechain bool    `json:"isSidechain"`
			Message     *struct {
				Model string `json:"model"`
			} `json:"message"`
		}
		if err := json.Unmarshal([]byte(trimmed), &row); err != nil {
			return claudeLeafProjection{}, err
		}
		if row.Type == "last-prompt" && row.LeafUUID != "" {
			leafUUID = row.LeafUUID
			continue
		}
		if row.UUID == "" {
			if row.Type == "user" || row.Type == "assistant" {
				return claudeLeafProjection{}, errShippedWalkNoUUID
			}
			continue
		}
		if _, duplicate := nodes[row.UUID]; duplicate {
			return claudeLeafProjection{}, errShippedWalkDuplicate
		}
		parentUUID := ""
		if row.ParentUUID != nil {
			parentUUID = *row.ParentUUID
		}
		if row.Type != "user" && row.Type != "assistant" || row.IsSidechain {
			nodes[row.UUID] = node{parentUUID: parentUUID}
			continue
		}
		synthetic := row.Type == "assistant" && row.Message != nil && row.Message.Model == "<synthetic>"
		var visibleTurn *acf.TextTurn
		if !synthetic {
			events, err := EncodeCanonical(append([]byte(trimmed), '\n'))
			if err != nil {
				return claudeLeafProjection{}, err
			}
			turns := acf.ExtractTextTurns(events)
			switch len(turns) {
			case 0:
			case 1:
				turn := turns[0]
				visibleTurn = &turn
				portableNodeCount++
			default:
				return claudeLeafProjection{}, errShippedWalkMultiTurn
			}
		}
		nodes[row.UUID] = node{parentUUID: parentUUID, synthetic: synthetic, turn: visibleTurn}
		fallbackLeaf = row.UUID
	}
	if leafUUID == "" {
		leafUUID = fallbackLeaf
	}
	if leafUUID == "" {
		return claudeLeafProjection{}, nil
	}
	selectedPortableLeaf := ""
	reversed := make([]acf.TextTurn, 0, len(nodes))
	seen := make(map[string]bool, len(nodes))
	for leafUUID != "" {
		if seen[leafUUID] {
			return claudeLeafProjection{}, errShippedWalkCycle
		}
		seen[leafUUID] = true
		current, ok := nodes[leafUUID]
		if !ok {
			return claudeLeafProjection{}, errShippedWalkMissingParent
		}
		if current.synthetic || current.turn == nil {
			leafUUID = current.parentUUID
			continue
		}
		if selectedPortableLeaf == "" {
			selectedPortableLeaf = leafUUID
		}
		reversed = append(reversed, *current.turn)
		leafUUID = current.parentUUID
	}
	visible := make([]acf.TextTurn, len(reversed))
	for i := range reversed {
		visible[len(reversed)-1-i] = reversed[i]
	}
	return claudeLeafProjection{
		turns: visible, leafUUID: selectedPortableLeaf, nodeCount: portableNodeCount,
	}, nil
}
