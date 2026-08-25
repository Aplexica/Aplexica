package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/stretchr/testify/require"
)

// turnEventsAtDistinctTimes lifts a turn sequence into conversation events one
// second apart. Distinct timestamps keep the replayed-event pass out of the way
// so a fixture exercises the adjacent/trailing rules on their own.
func turnEventsAtDistinctTimes(turns []acf.TextTurn) []acf.ConversationEvent {
	base := time.Date(2026, 7, 20, 9, 0, 0, 0, time.UTC)
	events := make([]acf.ConversationEvent, 0, len(turns))
	for i, turn := range turns {
		events = append(events, acf.ConversationEvent{
			Type:      acf.EventTypeTurn,
			Timestamp: base.Add(time.Duration(i) * time.Second),
			Role:      turn.Role,
			Content:   []acf.ContentBlock{{Type: "text", Text: turn.Text}},
		})
	}
	return events
}

func TestRepairConversationCollapsesAdjacentDuplicateTurns(t *testing.T) {
	polluted := turnEventsAtDistinctTimes([]acf.TextTurn{
		{Role: "user", Text: "sky?"},
		{Role: "user", Text: "sky?"},
		{Role: "assistant", Text: "Blue."},
		{Role: "user", Text: "watter?"},
		{Role: "assistant", Text: "Also blue."},
		{Role: "user", Text: "watter?"},
	})
	got, changed := collapseDuplicateConversationTurns(polluted)
	if !changed {
		t.Fatal("a polluted head must be reported as repairable")
	}
	want := []acf.TextTurn{
		{Role: "user", Text: "sky?"},
		{Role: "assistant", Text: "Blue."},
		{Role: "user", Text: "watter?"},
		{Role: "assistant", Text: "Also blue."},
	}
	if !acf.TextTurnsEqual(got, want) {
		t.Fatalf("collapsed = %+v, want %+v", got, want)
	}
}

func TestRepairConversationKeepsLegitimateRepeats(t *testing.T) {
	// A user genuinely asking the same thing twice, with an answer between, is
	// not an echo and must survive untouched.
	clean := turnEventsAtDistinctTimes([]acf.TextTurn{
		{Role: "user", Text: "again?"},
		{Role: "assistant", Text: "Sure."},
		{Role: "user", Text: "again?"},
		{Role: "assistant", Text: "Sure."},
	})
	if _, changed := collapseDuplicateConversationTurns(clean); changed {
		t.Fatal("separated repeats are legitimate and must not be collapsed")
	}
}

// localCommandTurnText is a Claude Code local-command scaffolding row
// (internal/acf/conversation_turns.go's IsLocalCommandContext). It is an
// EventTypeTurn with Role "user" — isVisibleTurnEvent's first two guard
// conditions both pass — but acf.NormalizeTextTurn rejects it, so it is the
// one event class that makes the turn/event cursor mapping non-trivial: a
// turn event that must NOT advance the visible-turn cursor. These rows are
// routine in real Claude Code session.jsonl history, which is exactly what
// conversation repairs.
const localCommandTurnText = "<command-name>/model</command-name>"

// TestCollapseConversationEvents_MatchesTurnProjectionAndKeepsNonVisible pins
// the event-level collapse to the turn-level rule collapseDuplicateConversationTurns
// implements: extracting text turns from the collapsed events must equal
// exactly what the turn-level function returns for the same input, and every
// non-visible event (tool_call/tool_result, and a turn event NormalizeTextTurn
// rejects) must survive the repair untouched. Without this assertion the
// command could silently drop the wrong ConversationEvent while still
// reporting the right turn-level diff.
func TestCollapseConversationEvents_MatchesTurnProjectionAndKeepsNonVisible(t *testing.T) {
	at := time.Date(2026, 7, 20, 9, 0, 0, 0, time.UTC)
	polluted := []acf.ConversationEvent{
		{Type: acf.EventTypeTurn, Timestamp: at, Role: "user",
			Content: []acf.ContentBlock{{Type: "text", Text: "sky?"}}},
		{Type: acf.EventTypeTurn, Timestamp: at.Add(time.Second), Role: "user",
			Content: []acf.ContentBlock{{Type: "text", Text: "sky?"}}},
		{Type: acf.EventTypeTurn, Timestamp: at.Add(2 * time.Second), Role: "assistant",
			Content: []acf.ContentBlock{{Type: "text", Text: "Blue."}}},
		{Type: acf.EventTypeToolCall, Timestamp: at.Add(3 * time.Second),
			CallID: "call-1", ToolName: "Read"},
		{Type: acf.EventTypeToolResult, Timestamp: at.Add(4 * time.Second), CallID: "call-1",
			Content: []acf.ContentBlock{{Type: "text", Text: "file contents"}}},
		{Type: acf.EventTypeTurn, Timestamp: at.Add(5 * time.Second), Role: "user",
			Content: []acf.ContentBlock{{Type: "text", Text: localCommandTurnText}}},
		{Type: acf.EventTypeTurn, Timestamp: at.Add(6 * time.Second), Role: "user",
			Content: []acf.ContentBlock{{Type: "text", Text: "watter?"}}},
		{Type: acf.EventTypeTurn, Timestamp: at.Add(7 * time.Second), Role: "assistant",
			Content: []acf.ContentBlock{{Type: "text", Text: "Also blue."}}},
		{Type: acf.EventTypeTurn, Timestamp: at.Add(8 * time.Second), Role: "user",
			Content: []acf.ContentBlock{{Type: "text", Text: "watter?"}}},
	}

	collapsedEvents, changed := collapseConversationEvents(polluted)
	if !changed {
		t.Fatal("a polluted event log must be reported as repairable")
	}

	wantTurns, turnsChanged := collapseDuplicateConversationTurns(polluted)
	if !turnsChanged {
		t.Fatal("turn-level projection of the same fixture must also report a change")
	}
	gotTurns := acf.ExtractTextTurns(collapsedEvents)
	if !acf.TextTurnsEqual(gotTurns, wantTurns) {
		t.Fatalf("ExtractTextTurns(collapsed events) = %+v, want %+v (turn-level projection)", gotTurns, wantTurns)
	}

	var sawToolCall, sawToolResult, sawLocalCommandTurn bool
	for _, ev := range collapsedEvents {
		switch {
		case ev.Type == acf.EventTypeToolCall:
			sawToolCall = true
		case ev.Type == acf.EventTypeToolResult:
			sawToolResult = true
		case ev.Type == acf.EventTypeTurn && ev.Role == "user" &&
			len(ev.Content) == 1 && ev.Content[0].Text == localCommandTurnText:
			sawLocalCommandTurn = true
		}
	}
	if !sawToolCall || !sawToolResult {
		t.Fatalf("non-visible tool_call/tool_result events must survive untouched; got %+v", collapsedEvents)
	}
	if !sawLocalCommandTurn {
		t.Fatalf("a turn event NormalizeTextTurn rejects must survive untouched (it must never advance "+
			"the visible-turn cursor); got %+v", collapsedEvents)
	}
	if len(collapsedEvents) != len(polluted)-2 {
		t.Fatalf("expected exactly 2 events dropped (the adjacent + trailing echo), got %d survive out of %d",
			len(collapsedEvents), len(polluted))
	}
}

// TestRepairConversationCommand_DryRunThenApply drives the real cobra command
// end to end. The fixture exercises a corrupted
// artifact, so its --apply path mutates canonical data — the two mandated
// unit tests above never touch the store or the CLI wiring, this test does.
func TestRepairConversationCommand_DryRunThenApply(t *testing.T) {
	tmp := t.TempDir()
	storeRoot := filepath.Join(tmp, "store")
	store := &acf.Store{Root: storeRoot}
	require.NoError(t, store.Init())

	const id = "repair-target"
	at := time.Date(2026, 7, 20, 9, 0, 0, 0, time.UTC)
	require.NoError(t, store.WriteArtifact(acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion,
		ArtifactID:       id,
		Kind:             acf.KindConversation,
		Scope:            acf.ScopeGlobal,
		Name:             "repair-target.jsonl",
		CreatedAt:        at,
		UpdatedAt:        at,
	}))

	pollutedEvents := []acf.ConversationEvent{
		{Type: acf.EventTypeTurn, Timestamp: at, Role: "user",
			Content: []acf.ContentBlock{{Type: "text", Text: "sky?"}}},
		{Type: acf.EventTypeTurn, Timestamp: at.Add(time.Second), Role: "user",
			Content: []acf.ContentBlock{{Type: "text", Text: "sky?"}}},
		{Type: acf.EventTypeTurn, Timestamp: at.Add(2 * time.Second), Role: "assistant",
			Content: []acf.ContentBlock{{Type: "text", Text: "Blue."}}},
		{Type: acf.EventTypeToolCall, Timestamp: at.Add(3 * time.Second),
			CallID: "call-1", ToolName: "Read"},
		{Type: acf.EventTypeToolResult, Timestamp: at.Add(4 * time.Second), CallID: "call-1",
			Content: []acf.ContentBlock{{Type: "text", Text: "file contents"}}},
		{Type: acf.EventTypeTurn, Timestamp: at.Add(5 * time.Second), Role: "user",
			Content: []acf.ContentBlock{{Type: "text", Text: localCommandTurnText}}},
		{Type: acf.EventTypeTurn, Timestamp: at.Add(6 * time.Second), Role: "user",
			Content: []acf.ContentBlock{{Type: "text", Text: "watter?"}}},
		{Type: acf.EventTypeTurn, Timestamp: at.Add(7 * time.Second), Role: "assistant",
			Content: []acf.ContentBlock{{Type: "text", Text: "Also blue."}}},
		{Type: acf.EventTypeTurn, Timestamp: at.Add(8 * time.Second), Role: "user",
			Content: []acf.ContentBlock{{Type: "text", Text: "watter?"}}},
	}
	payload, err := acf.EncodePayload(acf.ConversationPayload{
		Format: acf.ConversationFormatV1,
		Events: pollutedEvents,
	})
	require.NoError(t, err)
	require.NoError(t, store.AppendEvent(acf.KindConversation, acf.Event{
		EventID:    acf.NewID(),
		ArtifactID: id,
		Type:       acf.EventTypeCreate,
		Timestamp:  at,
		Provenance: acf.Provenance{SourceAgent: "claude-code"},
		Payload:    payload,
	}))

	eventLogPath := filepath.Join(storeRoot, "events", "conversations", id+".jsonl")
	origLog, err := os.ReadFile(eventLogPath)
	require.NoError(t, err)

	// --apply proves the daemon is stopped by taking its instance lock, so the
	// test must point at its own state dir rather than the real one.
	stateDir := t.TempDir()

	t.Cleanup(func() {
		home, _ := os.UserHomeDir()
		repairStoreRoot = filepath.Join(home, ".aplexica", "store") // the registered flag default, not ""
		repairStateDir = filepath.Join(home, ".aplexica", "state")
		repairApply = false
		repairBackupDir = ""
		repairAll = false
		repairDeviceID = ""
	})

	before := storeFileDigest(t, storeRoot)
	dryOut, err := runRoot(t, "repair", "conversation", id, "--store", storeRoot)
	require.NoError(t, err, "dry run output:\n%s", dryOut)
	require.Contains(t, dryOut, "6 turns before")
	require.Contains(t, dryOut, "4 turns after")
	require.Contains(t, dryOut, "9 events before")
	require.Contains(t, dryOut, "7 events after")
	require.Contains(t, dryOut, "sky?")
	require.Contains(t, dryOut, "watter?")
	require.Contains(t, dryOut, "--apply")
	after := storeFileDigest(t, storeRoot)
	require.Equal(t, before, after, "dry run must not write anything to the store")

	backupDir := filepath.Join(filepath.Dir(storeRoot), "aplexica-repair-backups")
	_, statErr := os.Stat(backupDir)
	require.True(t, os.IsNotExist(statErr), "dry run must not create a backup directory")

	applyOut, err := runRoot(t, "repair", "conversation", id, "--apply",
		"--store", storeRoot, "--state-dir", stateDir)
	require.NoError(t, err, "apply output:\n%s", applyOut)
	require.Contains(t, applyOut, "backup")
	require.Contains(t, applyOut, "repair applied")

	entries, err := os.ReadDir(backupDir)
	require.NoError(t, err)
	require.Len(t, entries, 1, "exactly one backup file expected")
	backupBytes, err := os.ReadFile(filepath.Join(backupDir, entries[0].Name()))
	require.NoError(t, err)
	require.Equal(t, origLog, backupBytes, "backup must be a verbatim pre-repair copy of the event log")

	payloadAfter, _, ok, err := store.MaterializedConversationHeadFromStore(id)
	require.NoError(t, err)
	require.True(t, ok)
	gotTurns := acf.ExtractTextTurns(payloadAfter.Events)
	wantTurns := []acf.TextTurn{
		{Role: "user", Text: "sky?"},
		{Role: "assistant", Text: "Blue."},
		{Role: "user", Text: "watter?"},
		{Role: "assistant", Text: "Also blue."},
	}
	require.True(t, acf.TextTurnsEqual(gotTurns, wantTurns), "got %+v", gotTurns)
	require.Len(t, payloadAfter.Events, 7, "9 seeded events minus the 2 dropped echoes")

	var sawToolCall, sawToolResult, sawLocalCommandTurn bool
	for _, ev := range payloadAfter.Events {
		switch {
		case ev.Type == acf.EventTypeToolCall:
			sawToolCall = true
		case ev.Type == acf.EventTypeToolResult:
			sawToolResult = true
		case ev.Type == acf.EventTypeTurn && ev.Role == "user" &&
			len(ev.Content) == 1 && ev.Content[0].Text == localCommandTurnText:
			sawLocalCommandTurn = true
		}
	}
	require.True(t, sawToolCall && sawToolResult, "non-visible tool events must survive the applied repair")
	require.True(t, sawLocalCommandTurn,
		"a turn event NormalizeTextTurn rejects must survive the applied repair untouched")
}

const repairPeerTestArtifactID = "peer-guard-target"

// resetRepairPeerGlobals restores the repair command's package globals and
// the peer-status test seam after a test drives the cobra command — cobra
// flag values persist across Execute calls, so every test that sets them
// must put them back.
func resetRepairPeerGlobals(t *testing.T) {
	t.Helper()
	origQuery := repairPeerStatusQuery
	t.Cleanup(func() {
		home, _ := os.UserHomeDir()
		repairStoreRoot = filepath.Join(home, ".aplexica", "store") // the registered flag default, not ""
		repairStateDir = filepath.Join(home, ".aplexica", "state")
		repairApply = false
		repairBackupDir = ""
		repairAll = false
		repairDeviceID = ""
		repairCheckPeers = false
		repairForce = false
		repairPeerStatusQuery = origQuery
	})
}

// seedEchoPollutedStore writes one conversation artifact (id defaulting to
// repairPeerTestArtifactID) with a single adjacent echo so an --apply run has
// something to collapse.
func seedEchoPollutedStore(t *testing.T, storeRoot string, ids ...string) *acf.Store {
	t.Helper()
	if len(ids) == 0 {
		ids = []string{repairPeerTestArtifactID}
	}
	store := &acf.Store{Root: storeRoot}
	require.NoError(t, store.Init())
	at := time.Date(2026, 7, 28, 22, 0, 0, 0, time.UTC)
	for _, id := range ids {
		require.NoError(t, store.WriteArtifact(acf.Artifact{
			AcfSchemaVersion: acf.SchemaVersion,
			ArtifactID:       id,
			Kind:             acf.KindConversation,
			Scope:            acf.ScopeGlobal,
			Name:             id + ".jsonl",
			CreatedAt:        at,
			UpdatedAt:        at,
		}))
		payload, err := acf.EncodePayload(acf.ConversationPayload{
			Format: acf.ConversationFormatV1,
			Events: []acf.ConversationEvent{
				{Type: acf.EventTypeTurn, Timestamp: at, Role: "user",
					Content: []acf.ContentBlock{{Type: "text", Text: "sky?"}}},
				{Type: acf.EventTypeTurn, Timestamp: at.Add(time.Second), Role: "user",
					Content: []acf.ContentBlock{{Type: "text", Text: "sky?"}}},
				{Type: acf.EventTypeTurn, Timestamp: at.Add(2 * time.Second), Role: "assistant",
					Content: []acf.ContentBlock{{Type: "text", Text: "Blue."}}},
			},
		})
		require.NoError(t, err)
		require.NoError(t, store.AppendEvent(acf.KindConversation, acf.Event{
			EventID:    acf.NewID(),
			ArtifactID: id,
			Type:       acf.EventTypeCreate,
			Timestamp:  at,
			Provenance: acf.Provenance{SourceAgent: "claude-code"},
			Payload:    payload,
		}))
	}
	return store
}

func TestRepairConversationCheckPeers_FlagCombinations(t *testing.T) {
	resetRepairPeerGlobals(t)

	_, err := runRoot(t, "repair", "conversation", "whatever", "--check-peers")
	require.ErrorContains(t, err, "--check-peers guards --apply")

	repairCheckPeers = false // cobra globals persist across Execute calls
	_, err = runRoot(t, "repair", "conversation", "whatever", "--force")
	require.ErrorContains(t, err, "--force only overrides the --check-peers refusal")
}

func TestRepairConversationCheckPeers_RefusesWhenRelayPaired(t *testing.T) {
	resetRepairPeerGlobals(t)
	storeRoot := filepath.Join(t.TempDir(), "store")
	seedEchoPollutedStore(t, storeRoot)
	repairPeerStatusQuery = func(context.Context) (bool, string, error) {
		return true, "device-a1b2", nil
	}

	before := storeFileDigest(t, storeRoot)
	out, err := runRoot(t, "repair", "conversation", repairPeerTestArtifactID,
		"--apply", "--check-peers", "--store", storeRoot, "--state-dir", t.TempDir())
	require.ErrorContains(t, err, "live relay pairing")
	require.ErrorContains(t, err, "device-a1b2")
	require.ErrorContains(t, err, "flag-day")
	require.ErrorContains(t, err, "--force")
	require.NotContains(t, out, "repair applied")
	require.Equal(t, before, storeFileDigest(t, storeRoot), "a refused apply must not write anything")

	backupDir := filepath.Join(filepath.Dir(storeRoot), "aplexica-repair-backups")
	_, statErr := os.Stat(backupDir)
	require.True(t, os.IsNotExist(statErr), "a refused apply must not create a backup")
}

func TestRepairConversationCheckPeers_AllSweepRefusesOnceAndAborts(t *testing.T) {
	// The real flag-day command is `repair conversation --all --apply`: the
	// guard must stop the whole sweep with ONE refusal before the first
	// write, not step over it per artifact.
	resetRepairPeerGlobals(t)
	storeRoot := filepath.Join(t.TempDir(), "store")
	seedEchoPollutedStore(t, storeRoot, "peer-guard-a", "peer-guard-b")
	queries := 0
	repairPeerStatusQuery = func(context.Context) (bool, string, error) {
		queries++
		return true, "device-a1b2", nil
	}

	before := storeFileDigest(t, storeRoot)
	out, err := runRoot(t, "repair", "conversation", "--all",
		"--apply", "--check-peers", "--store", storeRoot, "--state-dir", t.TempDir())
	require.ErrorContains(t, err, "live relay pairing")
	require.Equal(t, 1, queries, "the fleet must be asked exactly once, not per artifact")
	require.NotContains(t, out, "SKIPPED", "a guard refusal aborts the sweep; it is not a per-artifact skip")
	require.NotContains(t, out, "repair applied")
	require.Equal(t, before, storeFileDigest(t, storeRoot), "a refused sweep must not write anything")
}

func TestRepairConversationCheckPeers_FailsClosedOnQueryError(t *testing.T) {
	resetRepairPeerGlobals(t)
	storeRoot := filepath.Join(t.TempDir(), "store")
	seedEchoPollutedStore(t, storeRoot)
	repairPeerStatusQuery = func(context.Context) (bool, string, error) {
		return false, "", errors.New("plugin exploded")
	}

	before := storeFileDigest(t, storeRoot)
	_, err := runRoot(t, "repair", "conversation", repairPeerTestArtifactID,
		"--apply", "--check-peers", "--store", storeRoot, "--state-dir", t.TempDir())
	require.ErrorContains(t, err, "plugin exploded")
	require.ErrorContains(t, err, "--force")
	require.Equal(t, before, storeFileDigest(t, storeRoot), "an unanswerable peer check must not write anything")
}

func TestRepairConversationCheckPeers_ForceOverrides(t *testing.T) {
	resetRepairPeerGlobals(t)
	storeRoot := filepath.Join(t.TempDir(), "store")
	store := seedEchoPollutedStore(t, storeRoot)
	queried := false
	repairPeerStatusQuery = func(context.Context) (bool, string, error) {
		queried = true
		return true, "device-a1b2", nil
	}

	out, err := runRoot(t, "repair", "conversation", repairPeerTestArtifactID,
		"--apply", "--check-peers", "--force", "--store", storeRoot, "--state-dir", t.TempDir())
	require.NoError(t, err, "output:\n%s", out)
	require.Contains(t, out, "skipping the --check-peers live-relay guard")
	require.Contains(t, out, "repair applied")
	require.False(t, queried, "--force must not spawn a plugin process at all")

	payload, _, ok, err := store.MaterializedConversationHeadFromStore(repairPeerTestArtifactID)
	require.NoError(t, err)
	require.True(t, ok)
	got := acf.ExtractTextTurns(payload.Events)
	want := []acf.TextTurn{
		{Role: "user", Text: "sky?"},
		{Role: "assistant", Text: "Blue."},
	}
	require.True(t, acf.TextTurnsEqual(got, want), "got %+v", got)
}

func TestRepairConversationCheckPeers_AllowsWhenUnpaired(t *testing.T) {
	resetRepairPeerGlobals(t)
	storeRoot := filepath.Join(t.TempDir(), "store")
	store := seedEchoPollutedStore(t, storeRoot)
	repairPeerStatusQuery = func(context.Context) (bool, string, error) {
		return false, "", nil
	}

	out, err := runRoot(t, "repair", "conversation", repairPeerTestArtifactID,
		"--apply", "--check-peers", "--store", storeRoot, "--state-dir", t.TempDir())
	require.NoError(t, err, "output:\n%s", out)
	require.Contains(t, out, "repair applied")

	payload, _, ok, err := store.MaterializedConversationHeadFromStore(repairPeerTestArtifactID)
	require.NoError(t, err)
	require.True(t, ok)
	require.Len(t, acf.ExtractTextTurns(payload.Events), 2)
}

// storeFileDigest fingerprints every file under root by content hash so a
// caller can assert a directory tree is byte-identical before and after an
// operation (used to prove the dry-run path never writes).
func storeFileDigest(t *testing.T, root string) map[string]string {
	t.Helper()
	digest := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		sum := sha256.Sum256(b)
		digest[rel] = hex.EncodeToString(sum[:])
		return nil
	})
	require.NoError(t, err)
	return digest
}

// syntheticReplayFieldShape models sixteen turns in which two question-answer
// blocks are exact replays sitting well away from their originals. The replays
// carry the originals' synthetic timestamps verbatim, as re-imported rows do.
func syntheticReplayFieldShape() []acf.ConversationEvent {
	base := time.Date(2026, 1, 2, 3, 4, 0, 0, time.UTC)
	turn := func(role, text string, offset time.Duration) acf.ConversationEvent {
		return acf.ConversationEvent{
			Type: acf.EventTypeTurn, Role: role, Timestamp: base.Add(offset),
			Content: []acf.ContentBlock{{Type: "text", Text: text}},
		}
	}
	u1 := turn("user", "Question one?", 10*time.Second)
	a1 := turn("assistant", "Answer one.", 11*time.Second+100*time.Millisecond)
	u2 := turn("user", "Question two?", 20*time.Second+200*time.Millisecond)
	a2 := turn("assistant", "Answer two.", 21*time.Second+300*time.Millisecond)
	u3 := turn("user", "Question three?", 30*time.Second+400*time.Millisecond)
	a3 := turn("assistant", "Answer three.", 31*time.Second+500*time.Millisecond)
	u4 := turn("user", "Question four?", 40*time.Second+600*time.Millisecond)
	a4 := turn("assistant", "Answer four.", 41*time.Second+700*time.Millisecond)
	u5 := turn("user", "Question five?", 50*time.Second+800*time.Millisecond)
	a5 := turn("assistant", "Answer five.", 51*time.Second+900*time.Millisecond)
	u6 := turn("user", "Question six?", 60*time.Second+100*time.Millisecond)
	a6 := turn("assistant", "Answer six.", 61*time.Second+200*time.Millisecond)
	return []acf.ConversationEvent{u1, a1, u2, a2, u3, a3, u2, a2, u4, a4, u5, a5, u3, a3, u6, a6}
}

// TestRepairConversationCollapsesReplayedTurnBlocks is the regression the
// shipped command could not express: the duplicates are neither adjacent nor
// trailing, so the pre-existing adjacent/trailing rules were a verified no-op
// on this artifact.
func TestRepairConversationCollapsesReplayedTurnBlocks(t *testing.T) {
	polluted := syntheticReplayFieldShape()
	got, changed := collapseDuplicateConversationTurns(polluted)
	require.True(t, changed, "the field shape must be reported as repairable")

	want := []acf.TextTurn{
		{Role: "user", Text: "Question one?"},
		{Role: "assistant", Text: "Answer one."},
		{Role: "user", Text: "Question two?"},
		{Role: "assistant", Text: "Answer two."},
		{Role: "user", Text: "Question three?"},
		{Role: "assistant", Text: "Answer three."},
		{Role: "user", Text: "Question four?"},
		{Role: "assistant", Text: "Answer four."},
		{Role: "user", Text: "Question five?"},
		{Role: "assistant", Text: "Answer five."},
		{Role: "user", Text: "Question six?"},
		{Role: "assistant", Text: "Answer six."},
	}
	require.Equal(t, want, got)

	collapsedEvents, eventsChanged := collapseConversationEvents(polluted)
	require.True(t, eventsChanged)
	require.Len(t, collapsedEvents, 12, "16 seeded events minus the two replayed 2-turn blocks")
	require.Equal(t, want, acf.ExtractTextTurns(collapsedEvents),
		"the event-level collapse must reproduce the turn-level diff exactly")
}

// TestRepairConversationCollapseIsIdempotentAndOrderPreserving pins the two
// properties the fleet-wide repair depends on: every device must compute the
// same result from the same log, and re-running must be a no-op. Re-sorting
// would rewrite the rendered order of legitimately out-of-order history, so the
// collapse keeps log order.
func TestRepairConversationCollapseIsIdempotentAndOrderPreserving(t *testing.T) {
	once, changed := collapseConversationEvents(syntheticReplayFieldShape())
	require.True(t, changed)

	twice, changedAgain := collapseConversationEvents(once)
	require.False(t, changedAgain, "a repaired head must not be repairable a second time")
	require.Equal(t, once, twice)

	// Log order, not timestamp order: the surviving events must appear in the
	// order the log holds them.
	for i := 1; i < len(once); i++ {
		require.False(t, once[i].Timestamp.Before(once[i-1].Timestamp),
			"this fixture's survivors happen to be chronological; the collapse must not have reordered them")
	}
}

// TestRepairConversationKeepsGenuineRepeatsWithDistinctTimestamps is the
// false-positive guard. Identity includes the timestamp, so a user who really
// does re-send the same prompt keeps both copies.
func TestRepairConversationKeepsGenuineRepeatsWithDistinctTimestamps(t *testing.T) {
	base := time.Date(2026, 7, 27, 2, 40, 0, 0, time.UTC)
	turn := func(role, text string, offset time.Duration) acf.ConversationEvent {
		return acf.ConversationEvent{
			Type: acf.EventTypeTurn, Role: role, Timestamp: base.Add(offset),
			Content: []acf.ContentBlock{{Type: "text", Text: text}},
		}
	}
	clean := []acf.ConversationEvent{
		turn("user", "continue", time.Second),
		turn("assistant", "Done.", 2*time.Second),
		turn("user", "continue", 3*time.Second),
		turn("assistant", "Done.", 4*time.Second),
	}
	_, changed := collapseConversationEvents(clean)
	require.False(t, changed,
		"identical text at DIFFERENT timestamps is a genuine repeat and must survive")
}

// TestRepairConversationNeverDropsDuplicateToolEvents proves the collapse stops
// at visible turns. syncd's conversationEventKey ignores CallID/ToolName/Input,
// so parallel tool calls sharing an assistant message's timestamp look
// identical under it; the repair uses the full body AND refuses to drop
// non-turn events at all, reporting them instead.
func TestRepairConversationNeverDropsDuplicateToolEvents(t *testing.T) {
	at := time.Date(2026, 7, 27, 2, 40, 0, 0, time.UTC)
	dupCall := acf.ConversationEvent{
		Type: acf.EventTypeToolCall, Timestamp: at,
		CallID: "call-1", ToolName: "Read", Input: []byte(`{"p":"a"}`),
	}
	events := []acf.ConversationEvent{
		{Type: acf.EventTypeTurn, Role: "user", Timestamp: at.Add(-time.Second),
			Content: []acf.ContentBlock{{Type: "text", Text: "read it"}}},
		dupCall,
		dupCall,
		{Type: acf.EventTypeTurn, Role: "assistant", Timestamp: at.Add(time.Second),
			Content: []acf.ContentBlock{{Type: "text", Text: "done"}}},
	}
	collapsed, changed := collapseConversationEvents(events)
	require.False(t, changed, "duplicate tool events must never be dropped")
	require.Len(t, collapsed, len(events))

	notes := duplicateNonTurnEventNotes(events)
	require.Len(t, notes, 1, "the duplicate tool call must still be reported")
	require.Contains(t, notes[0], "reported only, never dropped")
}

// TestRepairConversationKeepsRepeatedFlattenedToolRows is the false positive
// found by A/B-ing this rule against the whole live store before shipping it.
// Flattened external-agent transcripts (openclaw/hermes imports) stamp an
// entire imported batch with ONE timestamp and render tool calls as assistant
// TEXT rows, so an agent that genuinely reads the same file twice produces two
// byte-identical turn events. Those runs carry no user turn, which is what
// keeps them out of the collapse — a continuation always begins with a prompt.
func TestRepairConversationKeepsRepeatedFlattenedToolRows(t *testing.T) {
	at := time.Date(2026, 5, 6, 12, 57, 52, 22*int(time.Millisecond), time.UTC)
	assistant := func(text string) acf.ConversationEvent {
		return acf.ConversationEvent{
			Type: acf.EventTypeTurn, Role: "assistant", Timestamp: at,
			Content: []acf.ContentBlock{{Type: "text", Text: text}},
		}
	}
	call := assistant("[external_agent_tool_call: Read]\nfile: /x/y.go")
	result := assistant("[external_agent_tool_result]\nfile contents")
	events := []acf.ConversationEvent{
		{Type: acf.EventTypeTurn, Role: "user", Timestamp: at,
			Content: []acf.ContentBlock{{Type: "text", Text: "fix it"}}},
		call, result,
		assistant("Now let me check the caller."),
		call, result, // the same file read again, legitimately
	}
	_, changed := collapseConversationEvents(events)
	require.False(t, changed,
		"an assistant-only repeated run in a batch-stamped transcript must survive")
}

// TestRepairConversationRequiresARunOfTwo pins the other half of the guard: a
// lone repeated turn is not the bug's signature and is left alone.
func TestRepairConversationRequiresARunOfTwo(t *testing.T) {
	at := time.Date(2026, 7, 27, 2, 40, 0, 0, time.UTC)
	turn := func(role, text string) acf.ConversationEvent {
		return acf.ConversationEvent{
			Type: acf.EventTypeTurn, Role: role, Timestamp: at,
			Content: []acf.ContentBlock{{Type: "text", Text: text}},
		}
	}
	events := []acf.ConversationEvent{
		turn("user", "go"), turn("assistant", "ok"),
		turn("user", "next"), turn("assistant", "sure"),
		turn("user", "go"), // an isolated identity repeat, not a block
		turn("assistant", "done"),
	}
	_, changed := collapseConversationEvents(events)
	require.False(t, changed, "a single repeated turn is not a replayed block")
}

// The repair derives its ParentHash from the event-log tail, but AppendEvent
// validates against the persisted branch index. A stale index head one event
// behind its own log makes repair fail closed with "ParentHash does not match
// branch head". The apply path must rebuild the index before appending.
func TestRepairConversationApply_HealsStaleHeadBookkeeping(t *testing.T) {
	tmp := t.TempDir()
	storeRoot := filepath.Join(tmp, "store")
	store := &acf.Store{Root: storeRoot}
	require.NoError(t, store.Init())

	const id = "stale-index-target"
	at := time.Date(2026, 1, 2, 3, 4, 0, 0, time.UTC)
	require.NoError(t, store.WriteArtifact(acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion,
		ArtifactID:       id,
		Kind:             acf.KindConversation,
		Scope:            acf.ScopeGlobal,
		Name:             "stale-index-target.jsonl",
		CreatedAt:        at,
		UpdatedAt:        at,
	}))
	payload, err := acf.EncodePayload(acf.ConversationPayload{
		Format: acf.ConversationFormatV1,
		Events: []acf.ConversationEvent{
			{Type: acf.EventTypeTurn, Timestamp: at, Role: "user",
				Content: []acf.ContentBlock{{Type: "text", Text: "moon?"}}},
			{Type: acf.EventTypeTurn, Timestamp: at.Add(time.Second), Role: "user",
				Content: []acf.ContentBlock{{Type: "text", Text: "moon?"}}},
			{Type: acf.EventTypeTurn, Timestamp: at.Add(2 * time.Second), Role: "assistant",
				Content: []acf.ContentBlock{{Type: "text", Text: "Far."}}},
		},
	})
	require.NoError(t, err)
	require.NoError(t, store.AppendEvent(acf.KindConversation, acf.Event{
		EventID:    acf.NewID(),
		ArtifactID: id,
		Type:       acf.EventTypeCreate,
		Timestamp:  at,
		Provenance: acf.Provenance{SourceAgent: "claude-code"},
		Payload:    payload,
	}))

	// Reproduce the wedge: artifact head bookkeeping (the chain authority
	// appends validate against) pointing at a hash that is not the log tail —
	// a stale-HeadEventHash shape. Both bookkeeping
	// fields must be corrupted: headHashForAppend prefers BranchHeads[main]
	// over HeadEventHash whenever it is non-empty.
	art, err := store.ReadArtifact(acf.KindConversation, id)
	require.NoError(t, err)
	require.NotEmpty(t, art.HeadEventHash, "append must have stamped head bookkeeping")
	const staleHead = "0000000000000000000000000000000000000000000000000000000000000000"
	art.HeadEventHash = staleHead
	if _, ok := art.BranchHeads[acf.MainBranch]; ok {
		art.BranchHeads[acf.MainBranch] = staleHead
	}
	require.NoError(t, store.WriteArtifact(art))

	t.Cleanup(func() {
		home, _ := os.UserHomeDir()
		repairStoreRoot = filepath.Join(home, ".aplexica", "store")
		repairStateDir = filepath.Join(home, ".aplexica", "state")
		repairApply = false
		repairBackupDir = ""
		repairAll = false
		repairDeviceID = ""
	})

	tailBefore, err := store.ReadEvents(acf.KindConversation, id)
	require.NoError(t, err)
	parentWanted := tailBefore[len(tailBefore)-1].Hash

	out, err := runRoot(t, "repair", "conversation", id, "--apply",
		"--store", storeRoot, "--state-dir", t.TempDir())
	require.NoError(t, err, "apply must heal the stale index, output:\n%s", out)
	require.Contains(t, out, "repair applied")

	// The repaired event landed and chains onto the real log tail, not the
	// corrupted index head.
	events, err := store.ReadEvents(acf.KindConversation, id)
	require.NoError(t, err)
	repaired := events[len(events)-1]
	require.Contains(t, repaired.EventTags, acf.ConversationRepairCommandEventTag)
	require.Equal(t, parentWanted, repaired.ParentHash)
}
