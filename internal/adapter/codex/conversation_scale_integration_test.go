package codex

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter/claudecode"
	"github.com/aplexica/aplexica/internal/adapter/hermes"
	"github.com/aplexica/aplexica/internal/hermesdb"
	"github.com/stretchr/testify/require"
)

const (
	scaleConversationVisibleTurns = 10_000
	scaleConversationChunkTurns   = 100
)

type scaleConversationFixture struct {
	chunks      [][]byte
	full        []byte
	visible     []acf.TextTurn
	leakMarkers []string
}

// TestConversationScale_DeltaReplayAndNativeMaterializationMatchFullHistory
// is the daemon-side half of the durable-delta design's 10,000-turn scale
// gate. Here a "visible turn" is one user or assistant TextTurn, so the
// fixture is exactly 5,000 question/answer exchanges and 10,000 visible
// messages. The cloud-side scale test separately proves wire-byte linearity;
// this test proves those deltas reconstruct and materialize the same content
// as the former self-contained/full-history representation.
func TestConversationScale_DeltaReplayAndNativeMaterializationMatchFullHistory(t *testing.T) {
	fixture := newScaleConversationFixture(t)
	require.Len(t, fixture.visible, scaleConversationVisibleTurns)
	for _, marker := range fixture.leakMarkers {
		require.Contains(t, string(fixture.full), marker,
			"the source fixture must actually contain every nonportable probe")
	}

	root := t.TempDir()
	nativePath := filepath.Join(root, "rollout-scale.jsonl")
	deltaRoot := filepath.Join(root, "delta-store")
	deltaStore := &acf.Store{Root: deltaRoot}
	require.NoError(t, deltaStore.Init())

	deltaAdapter := New()
	deltaAdapter.DeviceID = "scale-device"
	deltaAdapter.CanonicalConversations = true

	var (
		artifactID        string
		appendedTailBytes uint64
	)
	for chunkIndex, chunk := range fixture.chunks {
		if chunkIndex == 0 {
			require.NoError(t, os.WriteFile(nativePath, chunk, 0o600))
		} else {
			f, err := os.OpenFile(nativePath, os.O_WRONLY|os.O_APPEND, 0)
			require.NoError(t, err)
			_, err = f.Write(chunk)
			require.NoError(t, err)
			require.NoError(t, f.Close())
			appendedTailBytes += uint64(len(chunk))
		}
		ids, err := deltaAdapter.ImportConversation(t.Context(), deltaStore, nativePath)
		require.NoError(t, err)
		require.Len(t, ids, 1)
		if artifactID == "" {
			artifactID = ids[0]
		} else {
			require.Equal(t, artifactID, ids[0], "append imports must retain one canonical identity")
		}
	}

	require.NotNil(t, deltaAdapter.convCache)
	require.Equal(t, uint64(1), deltaAdapter.convCache.fullParses,
		"the 10,000-turn source must be parsed from byte zero only once")
	require.Equal(t, uint64(len(fixture.chunks)-1), deltaAdapter.convCache.incParses)
	require.Equal(t, appendedTailBytes, deltaAdapter.convCache.incBytes,
		"steady-state imports must read exactly the newly appended source bytes")

	deltaLog, err := deltaStore.ReadEvents(acf.KindConversation, artifactID)
	require.NoError(t, err)
	require.Len(t, deltaLog, len(fixture.chunks),
		"one initial full event plus one live delta per append chunk")
	require.NoError(t, acf.VerifyChain(deltaLog))
	for i, event := range deltaLog {
		payload, decodeErr := acf.DecodeConversationPayload(event)
		require.NoError(t, decodeErr)
		if i == 0 {
			require.Equal(t, acf.ConversationFormatV1, payload.Format)
		} else {
			require.Equal(t, acf.ConversationDeltaFormatV1, payload.Format,
				"append %d must persist only its live delta", i)
		}
		require.Len(t, payload.Events, scaleConversationChunkTurns)
	}

	// Import the identical completed source into a fresh store in one pass. Its
	// single self-contained event is the legacy/full-history behavior against
	// which the production delta replay is compared.
	legacyRoot := filepath.Join(root, "full-history-store")
	legacyStore := &acf.Store{Root: legacyRoot}
	require.NoError(t, legacyStore.Init())
	legacyAdapter := New()
	legacyAdapter.DeviceID = "scale-device"
	legacyAdapter.CanonicalConversations = true
	legacyIDs, err := legacyAdapter.ImportConversation(t.Context(), legacyStore, nativePath)
	require.NoError(t, err)
	require.Len(t, legacyIDs, 1)
	legacyLog, err := legacyStore.ReadEvents(acf.KindConversation, legacyIDs[0])
	require.NoError(t, err)
	require.Len(t, legacyLog, 1)
	legacyHeadPayload, err := acf.DecodeConversationPayload(legacyLog[0])
	require.NoError(t, err)
	require.Equal(t, acf.ConversationFormatV1, legacyHeadPayload.Format)
	require.Len(t, legacyHeadPayload.Events, scaleConversationVisibleTurns)

	// Re-open both roots through fresh Store values so this comparison cannot
	// pass through the importer's in-memory materialization cache.
	deltaReplayStore := freshScaleStore(t, deltaRoot)
	deltaPayload, deltaHead, ok, err := deltaReplayStore.MaterializedConversationHeadFromStore(artifactID)
	require.NoError(t, err)
	require.True(t, ok)
	legacyReplayStore := freshScaleStore(t, legacyRoot)
	legacyPayload, _, ok, err := legacyReplayStore.MaterializedConversationHeadFromStore(legacyIDs[0])
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, legacyPayload, deltaPayload,
		"delta replay must produce the exact full canonical payload emitted by full-history import")
	require.Equal(t, fixture.visible, acf.ExtractTextTurns(deltaPayload.Events))
	require.Equal(t, fixture.visible[0].Role, deltaPayload.Events[0].Role)
	require.Equal(t, fixture.visible[len(fixture.visible)-1].Role,
		deltaPayload.Events[len(deltaPayload.Events)-1].Role)
	require.Equal(t, deltaLog[len(deltaLog)-1].EventID, deltaHead.EventID)
	deltaArtifact, err := deltaReplayStore.ReadArtifact(acf.KindConversation, artifactID)
	require.NoError(t, err)
	require.Equal(t, deltaHead.Hash, deltaArtifact.HeadEventHash,
		"the materialized head must be the exact persisted canonical head")

	canonicalJSON, err := acf.EncodePayload(deltaPayload)
	require.NoError(t, err)
	assertScaleMarkersAbsent(t, canonicalJSON, fixture.leakMarkers)
	for _, event := range deltaPayload.Events {
		require.Equal(t, acf.EventTypeTurn, event.Type,
			"system/commentary/tool source rows must not enter the portable canonical projection")
		require.Contains(t, []string{"user", "assistant"}, event.Role)
	}

	// Exercise each production user-visible materializer against a fresh store,
	// preventing one target's cache from masking another target's replay path.
	codexPath := filepath.Join(root, "materialized", "codex.jsonl")
	require.NoError(t, New().ExportConversation(
		context.Background(), freshScaleStore(t, deltaRoot), artifactID, codexPath,
	))
	assertScaleJSONLProjection(t, codexPath, fixture.visible, fixture.leakMarkers, EncodeCanonical)

	claudePath := filepath.Join(root, "materialized", "claude.jsonl")
	require.NoError(t, claudecode.New().ExportConversation(
		context.Background(), freshScaleStore(t, deltaRoot), artifactID, claudePath,
	))
	assertScaleJSONLProjection(t, claudePath, fixture.visible, fixture.leakMarkers, claudecode.EncodeCanonical)

	hermesPath := filepath.Join(root, "materialized", "hermes.db")
	require.NoError(t, os.MkdirAll(filepath.Dir(hermesPath), 0o700))
	db, err := hermesdb.InitTestDB(hermesPath)
	require.NoError(t, err)
	require.NoError(t, db.Close())
	require.NoError(t, hermes.New().ExportConversationsToDB(
		context.Background(), freshScaleStore(t, deltaRoot), artifactID, hermesPath,
	))
	assertScaleHermesProjection(t, hermesPath, fixture.visible, fixture.leakMarkers)
}

func newScaleConversationFixture(t *testing.T) scaleConversationFixture {
	t.Helper()
	const chunkCount = scaleConversationVisibleTurns / scaleConversationChunkTurns
	require.Zero(t, scaleConversationVisibleTurns%scaleConversationChunkTurns)

	fixture := scaleConversationFixture{
		chunks:  make([][]byte, 0, chunkCount),
		visible: make([]acf.TextTurn, 0, scaleConversationVisibleTurns),
	}
	base := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	row := 0
	probeAt := map[int]bool{0: true, scaleConversationVisibleTurns / 2: true, scaleConversationVisibleTurns - 1: true}

	for chunkStart := 0; chunkStart < scaleConversationVisibleTurns; chunkStart += scaleConversationChunkTurns {
		var chunk bytes.Buffer
		if chunkStart == 0 {
			fmt.Fprintf(&chunk,
				`{"timestamp":%q,"type":"session_meta","payload":{"id":"scale-thread","aplexica_thread_id":"scale-thread"}}`+"\n",
				base.Format(time.RFC3339Nano))
		}
		for turnIndex := chunkStart; turnIndex < chunkStart+scaleConversationChunkTurns; turnIndex++ {
			if probeAt[turnIndex] {
				probe := fmt.Sprintf("%05d", turnIndex)
				private := []string{
					"PRIVATE_DEVELOPER_" + probe,
					"PRIVATE_SYSTEM_" + probe,
					"PRIVATE_AGENTS_INSTRUCTION_" + probe,
					"PRIVATE_COMMENTARY_" + probe,
					"PRIVATE_TOOL_INPUT_" + probe,
					"PRIVATE_TOOL_OUTPUT_" + probe,
				}
				fixture.leakMarkers = append(fixture.leakMarkers, private...)
				appendScaleCodexMessage(&chunk, base.Add(time.Duration(row)*time.Millisecond),
					"developer", "", "<permissions instructions>"+private[0]+"</permissions instructions>")
				row++
				appendScaleCodexMessage(&chunk, base.Add(time.Duration(row)*time.Millisecond),
					"system", "", private[1])
				row++
				appendScaleCodexMessage(&chunk, base.Add(time.Duration(row)*time.Millisecond),
					"user", "", "# AGENTS.md instructions for /private\n"+private[2])
				row++
				appendScaleCodexMessage(&chunk, base.Add(time.Duration(row)*time.Millisecond),
					"assistant", "commentary", private[3])
				row++
				fmt.Fprintf(&chunk,
					`{"timestamp":%q,"type":"response_item","payload":{"type":"function_call","name":"exec","arguments":%q,"call_id":%q}}`+"\n",
					base.Add(time.Duration(row)*time.Millisecond).Format(time.RFC3339Nano),
					`{"cmd":"`+private[4]+`"}`, "private-call-"+probe)
				row++
				fmt.Fprintf(&chunk,
					`{"timestamp":%q,"type":"response_item","payload":{"type":"function_call_output","call_id":%q,"output":%q}}`+"\n",
					base.Add(time.Duration(row)*time.Millisecond).Format(time.RFC3339Nano),
					"private-call-"+probe, private[5])
				row++
			}

			role := "user"
			text := fmt.Sprintf("scale-question-%05d", turnIndex/2)
			phase := ""
			if turnIndex%2 == 1 {
				role = "assistant"
				text = fmt.Sprintf("scale-answer-%05d", turnIndex/2)
				phase = "final_answer"
			}
			fixture.visible = append(fixture.visible, acf.TextTurn{Role: role, Text: text})
			appendScaleCodexMessage(&chunk, base.Add(time.Duration(row)*time.Millisecond), role, phase, text)
			row++
		}
		fixture.chunks = append(fixture.chunks, append([]byte(nil), chunk.Bytes()...))
	}
	fixture.full = bytes.Join(fixture.chunks, nil)
	return fixture
}

func appendScaleCodexMessage(out *bytes.Buffer, at time.Time, role, phase, content string) {
	blockType := "input_text"
	if role == "assistant" {
		blockType = "output_text"
	}
	fmt.Fprintf(out,
		`{"timestamp":%q,"type":"response_item","payload":{"type":"message","role":%q`,
		at.Format(time.RFC3339Nano), role)
	if phase != "" {
		fmt.Fprintf(out, `,"phase":%q`, phase)
	}
	fmt.Fprintf(out, `,"content":[{"type":%q,"text":%q}]}}`+"\n", blockType, content)
}

func freshScaleStore(t *testing.T, root string) *acf.Store {
	t.Helper()
	store := &acf.Store{Root: root}
	require.NoError(t, store.Init())
	return store
}

func assertScaleJSONLProjection(
	t *testing.T,
	path string,
	want []acf.TextTurn,
	leakMarkers []string,
	parse func([]byte) ([]acf.ConversationEvent, error),
) {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	assertScaleMarkersAbsent(t, raw, leakMarkers)
	events, err := parse(raw)
	require.NoError(t, err)
	require.Len(t, events, scaleConversationVisibleTurns)
	require.Equal(t, want, acf.ExtractTextTurns(events))
	for _, event := range events {
		require.Equal(t, acf.EventTypeTurn, event.Type)
		require.Contains(t, []string{"user", "assistant"}, event.Role)
	}
}

func assertScaleHermesProjection(t *testing.T, path string, want []acf.TextTurn, leakMarkers []string) {
	t.Helper()
	sessions, err := hermesdb.ListSessions(path, 0)
	require.NoError(t, err)
	require.Len(t, sessions, 1)
	require.Nil(t, sessions[0].Session.SystemPrompt)
	require.Len(t, sessions[0].Messages, scaleConversationVisibleTurns)
	got := make([]acf.TextTurn, 0, len(sessions[0].Messages))
	var visible bytes.Buffer
	for _, message := range sessions[0].Messages {
		require.Contains(t, []string{"user", "assistant"}, message.Role)
		require.Nil(t, message.ToolCalls)
		require.Nil(t, message.ToolCallID)
		require.Nil(t, message.ToolName)
		require.NotNil(t, message.Content)
		got = append(got, acf.TextTurn{Role: message.Role, Text: *message.Content})
		visible.WriteString(*message.Content)
		visible.WriteByte('\n')
	}
	require.Equal(t, want, got)
	assertScaleMarkersAbsent(t, visible.Bytes(), leakMarkers)
}

func assertScaleMarkersAbsent(t *testing.T, content []byte, markers []string) {
	t.Helper()
	for _, marker := range markers {
		require.NotContains(t, string(content), marker)
	}
}
