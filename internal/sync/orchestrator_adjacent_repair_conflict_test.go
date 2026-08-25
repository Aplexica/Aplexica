package syncd

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter"
	codexadapter "github.com/aplexica/aplexica/internal/adapter/codex"
	"github.com/aplexica/aplexica/internal/conflicts"
	"github.com/stretchr/testify/require"
)

type adjacentRepairConflictFixture struct {
	orch      *Orchestrator
	primary   adapter.Adapter
	conflicts *conflicts.Store
	artifact  string
	dirty     acf.Event
	delta     acf.Event
	corrected acf.Event
}

func newAdjacentRepairConflictFixture(
	t *testing.T,
	tagCorrection bool,
	correctionSource string,
	changeCorrectionAttachments bool,
) adjacentRepairConflictFixture {
	t.Helper()
	root := t.TempDir()
	store := &acf.Store{Root: filepath.Join(root, "store")}
	require.NoError(t, store.Init())
	conflictStore := &conflicts.Store{Root: filepath.Join(root, "conflicts")}
	require.NoError(t, conflictStore.Init())

	id := acf.NewID()
	now := time.Date(2026, 7, 18, 21, 47, 30, 0, time.UTC)
	require.NoError(t, store.WriteArtifact(acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion,
		ArtifactID:       id,
		Kind:             acf.KindConversation,
		Scope:            acf.ScopeGlobal,
		Name:             "france.jsonl",
		CreatedAt:        now,
		UpdatedAt:        now,
	}))
	turn := func(role, text string, at time.Time) acf.ConversationEvent {
		return acf.ConversationEvent{
			Type: acf.EventTypeTurn, Role: role, Timestamp: at,
			Content: []acf.ContentBlock{{Type: "text", Text: text}},
		}
	}
	cleanEvents := []acf.ConversationEvent{
		turn("user", "what is capital of France?", now),
		turn("assistant", "Paris.", now.Add(2*time.Second)),
		turn("user", "how many people live in Paris?", now.Add(3*time.Second)),
		turn("assistant", "About 2.1 million.", now.Add(4*time.Second)),
	}
	dirtyEvents := []acf.ConversationEvent{
		cleanEvents[0], turn("assistant", "Paris.", now.Add(time.Second)),
		cleanEvents[1], cleanEvents[2], cleanEvents[3],
	}
	deltaEvents := []acf.ConversationEvent{
		turn("assistant", "About 2.1 million.", now.Add(5*time.Second)),
		turn("user", "how many people live in Paris?", now.Add(6*time.Second)),
	}
	dirtyAttachments := []acf.Attachment{{
		Kind: "image", MimeType: "image/png", ContentHash: "image-proof", Bytes: 42, Filename: "proof.png",
	}}
	deltaAttachments := []acf.Attachment{{
		Kind: "file", MimeType: "text/plain", ContentHash: "delta-proof", Bytes: 7, Filename: "delta.txt",
	}}
	encode := func(format string, events []acf.ConversationEvent, attached []acf.Attachment) []byte {
		payload, err := acf.EncodePayload(acf.ConversationPayload{
			Format: format, Events: events, Attachments: attached,
		})
		require.NoError(t, err)
		return payload
	}
	appendHead := func(source, deviceID string, at time.Time, payload []byte, tags []string) acf.Event {
		art, err := store.ReadArtifact(acf.KindConversation, id)
		require.NoError(t, err)
		event := acf.Event{
			EventID: acf.NewID(), ArtifactID: id, Type: acf.EventTypeUpdate,
			Branch: acf.MainBranch, Timestamp: at, ParentHash: art.HeadEventHash,
			Provenance: acf.Provenance{DeviceID: deviceID, SourceAgent: source},
			Payload:    payload, EventTags: tags,
		}
		require.NoError(t, store.AppendEvent(acf.KindConversation, event))
		head, ok, err := store.LastEvent(acf.KindConversation, id)
		require.NoError(t, err)
		require.True(t, ok)
		return head
	}
	dirty := appendHead(
		"codex", "remote-device", now,
		encode(acf.ConversationFormatV1, dirtyEvents, dirtyAttachments), nil,
	)
	delta := appendHead(
		"claude-code", "remote-device", now.Add(7*time.Second),
		encode(acf.ConversationDeltaFormatV1, deltaEvents, deltaAttachments), nil,
	)
	correctedAttachments := append(append([]acf.Attachment(nil), dirtyAttachments...), deltaAttachments...)
	if changeCorrectionAttachments {
		correctedAttachments = correctedAttachments[:len(correctedAttachments)-1]
	}
	var tags []string
	if tagCorrection {
		tags = []string{acf.LegacyAdjacentAssistantEchoRepairEventTag}
	}
	corrected := appendHead(
		correctionSource, "local-device", now.Add(8*time.Second),
		encode(acf.ConversationFormatV1, cleanEvents, correctedAttachments), tags,
	)

	return adjacentRepairConflictFixture{
		orch: &Orchestrator{cfg: Config{
			Store: store, ConflictStore: conflictStore, ConflictWindow: time.Minute,
		}},
		primary: codexadapter.New(), conflicts: conflictStore, artifact: id,
		dirty: dirty, delta: delta, corrected: corrected,
	}
}

func (f adjacentRepairConflictFixture) exactConflict() conflicts.Conflict {
	return conflicts.Conflict{
		ArtifactID: f.artifact,
		Kind:       acf.KindConversation,
		Heads:      []conflicts.Head{conflictHeadFromEvent(f.dirty), conflictHeadFromEvent(f.delta)},
	}
}

func TestMaybeRecordConflict_FranceShapedCorrectionClearsExactSidecar(t *testing.T) {
	f := newAdjacentRepairConflictFixture(t, true, "codex", false)
	require.NotEqual(t, f.dirty.Provenance.DeviceID, f.corrected.Provenance.DeviceID,
		"the receiving device writes the correction for a remote dirty/delta pair")
	require.NoError(t, f.conflicts.Record(f.exactConflict()))

	require.False(t, f.orch.MaybeRecordConflictForTest(f.primary, f.artifact))
	_, err := f.conflicts.Get(f.artifact)
	require.ErrorIs(t, err, conflicts.ErrNotRecorded)
}

func TestMaybeRecordConflict_FranceShapedCorrectionWithoutSidecarDoesNotCreateOne(t *testing.T) {
	f := newAdjacentRepairConflictFixture(t, true, "codex", false)

	require.False(t, f.orch.MaybeRecordConflictForTest(f.primary, f.artifact))
	_, err := f.conflicts.Get(f.artifact)
	require.ErrorIs(t, err, conflicts.ErrNotRecorded)
}

func TestMaybeRecordConflict_FranceShapedCorrectionPreservesMismatchedSidecar(t *testing.T) {
	f := newAdjacentRepairConflictFixture(t, true, "codex", false)
	mismatch := f.exactConflict()
	mismatch.Heads[0].EventID = acf.NewID()
	require.NoError(t, f.conflicts.Record(mismatch))

	require.False(t, f.orch.MaybeRecordConflictForTest(f.primary, f.artifact))
	got, err := f.conflicts.Get(f.artifact)
	require.NoError(t, err)
	require.True(t, sameConflictHeadIdentities(mismatch.Heads, got.Heads))
}

func TestMaybeRecordConflict_FranceShapedCorrectionPreservesCorruptSidecar(t *testing.T) {
	f := newAdjacentRepairConflictFixture(t, true, "codex", false)
	path := filepath.Join(f.conflicts.Root, f.artifact+".json")
	require.NoError(t, os.WriteFile(path, []byte("{corrupt"), 0o600))

	require.False(t, f.orch.MaybeRecordConflictForTest(f.primary, f.artifact))
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "{corrupt", string(raw))
	_, err = f.conflicts.Get(f.artifact)
	require.Error(t, err)
	require.False(t, errors.Is(err, conflicts.ErrNotRecorded))
}

func TestMaybeRecordConflict_FranceShapeRequiresCodexAuthorityAndExactAttachments(t *testing.T) {
	for _, tc := range []struct {
		name              string
		tag               bool
		correctionSource  string
		changeAttachments bool
		nilPrimary        bool
	}{
		{name: "untagged", correctionSource: "codex"},
		{name: "missing attachment", tag: true, correctionSource: "codex", changeAttachments: true},
		{name: "nil primary", tag: true, correctionSource: "codex", nilPrimary: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAdjacentRepairConflictFixture(t, tc.tag, tc.correctionSource, tc.changeAttachments)
			primary := f.primary
			if tc.nilPrimary {
				primary = nil
			}
			require.True(t, f.orch.MaybeRecordConflictForTest(primary, f.artifact))
			got, err := f.conflicts.Get(f.artifact)
			require.NoError(t, err)
			require.Equal(t,
				[]string{f.delta.EventID, f.corrected.EventID},
				[]string{got.Heads[0].EventID, got.Heads[1].EventID},
			)
		})
	}
}

func TestMaybeRecordConflict_TaggedClaudeCorrectionCannotClearFranceSidecar(t *testing.T) {
	f := newAdjacentRepairConflictFixture(t, true, "claude-code", false)
	want := f.exactConflict()
	require.NoError(t, f.conflicts.Record(want))

	require.False(t, f.orch.MaybeRecordConflictForTest(f.primary, f.artifact))
	got, err := f.conflicts.Get(f.artifact)
	require.NoError(t, err)
	require.True(t, sameConflictHeadIdentities(want.Heads, got.Heads))
}
