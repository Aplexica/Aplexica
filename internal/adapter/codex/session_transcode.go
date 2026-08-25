package codex

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter"
	"github.com/aplexica/aplexica/internal/atomicfile"
)

// AplexicaThreadKey is the session_meta field that stamps a Codex rollout with
// the canonical conversation (thread) id, so a round-trip import recognizes it
// as a materialization of a shared thread rather than a brand-new session.
const AplexicaThreadKey = "aplexica_thread_id"

const AplexicaBranchKey = "aplexica_branch_id"

// codexBaseInstructions is a minimal system prompt for synthesized rollouts;
// Codex re-injects its own on resume, so this only needs to keep session_meta
// well-formed.
const codexBaseInstructions = "You are Codex, a coding agent based on GPT-5. Collaborate with the user until their goal is genuinely handled."

// syntheticCodexCLIVersion stays SemVer-compatible for Codex readers without
// pretending the rollout came from whichever Codex CLI happened to be current
// when this adapter was released.
const syntheticCodexCLIVersion = "0.0.0-aplexica"

const syntheticCodexContextWindow = 258400

var (
	errCodexSessionChanged  = errors.New("codex session changed during materialization")
	errCodexSessionDiverged = errors.New("codex session is not a prefix of the canonical conversation")
)

// MaterializeConversationSession (adapter.ConversationSessionTarget) transcodes
// a foreign conversation into a Codex rollout under ~/.codex/sessions/<Y>/<M>/<D>/
// so it shows up in `codex` resume, then best-effort registers that rollout for
// app surfaces through app-server. The canonical artifact id is stamped into
// session_meta (AplexicaThreadKey) for the loop-safe merge. The deterministic
// primary path is extended in place. A concurrent/divergent native continuation
// is preserved and retried after the watcher imports it; materialization never
// creates a second Codex-visible session for the same canonical thread.
func (a *Adapter) MaterializeConversationSession(art acf.Artifact, head acf.Event, sourceAgent string) (string, bool, error) {
	path, ok, _, err := a.materializeConversationSession(art, head, sourceAgent, nil)
	return path, ok, err
}

// MaterializeConversationSessionReason (adapter.ConversationSessionDeclineReporter)
// is MaterializeConversationSession plus the typed reason for a decline. Codex
// declines foreign heads exactly as claude-code does, so it must classify them
// the same way or the queue cannot tell a lost append race from a rollout that
// will never converge.
func (a *Adapter) MaterializeConversationSessionReason(
	art acf.Artifact, head acf.Event, sourceAgent string,
) (string, bool, adapter.SessionDeclineReason, error) {
	return a.materializeConversationSession(art, head, sourceAgent, nil)
}

// materializeConversationSession contains the production materializer plus a
// narrow test seam immediately before an optimistic append. Codex itself does
// not cooperate with advisory locks, so correctness depends on re-reading and
// validating the exact open inode after this point rather than on a lock that
// the native writer would ignore.
func (a *Adapter) materializeConversationSession(
	art acf.Artifact,
	head acf.Event,
	sourceAgent string,
	beforeAppend func(string) error,
) (string, bool, adapter.SessionDeclineReason, error) {
	plan, ok, err := a.conversationSessionPlan(art, head)
	if err != nil {
		return "", false, adapter.SessionDeclineUnspecified, err
	}
	if !ok {
		// No home directory, an unsupported payload format, or no text turns.
		return "", false, adapter.SessionDeclineOptOut, nil
	}
	cwd := a.conversationCWD(art, sourceAgent)
	rollout := transcodeToCodexRollout(plan.turns, plan.sessionID, art.ArtifactID, plan.branchID, cwd, sourceAgent, plan.sessionTime)
	if err := os.MkdirAll(filepath.Dir(plan.dest), 0o755); err != nil {
		return "", false, adapter.SessionDeclineUnspecified, fmt.Errorf("codex: mkdir sessions: %w", err)
	}
	if err := writeCodexConversationSession(
		plan.dest, rollout, plan.turns, plan.sessionID, art.ArtifactID, plan.branchID, plan.nativeOrigin, beforeAppend,
	); err != nil {
		switch {
		case errors.Is(err, errCodexSessionChanged):
			// Codex won the optimistic append race. Keep the one stable
			// pathname and let its watcher import the native change before a
			// later fan-out retry.
			return plan.dest, false, adapter.SessionDeclineRace, nil
		case errors.Is(err, errCodexSessionDiverged):
			// The rollout holds a turn the canonical plan lacks while the plan
			// holds one the rollout lacks. No append repairs that.
			//
			// WHICH side diverged decides the remedy. A native-origin rollout is
			// the user's own transcript, so canonical is the repairable side; an
			// Aplexica-owned mirror is blocked until its foreign turn is
			// imported, and offering the canonical-dedupe command for that costs
			// the operator the attempt to discover it does not apply.
			if plan.nativeOrigin {
				return plan.dest, false, adapter.SessionDeclineDiverged, nil
			}
			return plan.dest, false, adapter.SessionDeclineMirrorDiverged, nil
		default:
			return "", false, adapter.SessionDeclineUnspecified, fmt.Errorf("codex: write rollout: %w", err)
		}
	}
	title := adapter.ResolveConversationTitle(a.HomeDir, sourceAgent, art.SourcePath, art.Name)
	if title == "" {
		title = codexConversationTitleFromTurns(plan.turns)
	}
	title = adapter.ConversationBranchDisplayTitle(title, plan.branchID)
	a.bestEffortRegisterAppThread(plan.sessionID, cwd, title, plan.branchID != acf.MainBranch)
	a.bestEffortQuarantineCodexThreadDuplicates(plan.dest, art.ArtifactID, plan.branchID, plan.turns)
	return plan.dest, true, adapter.SessionDeclineUnspecified, nil
}

// writeCodexConversationSession creates a new materialized rollout atomically,
// but extends an existing one in place. Codex keeps the rollout descriptor open
// for the lifetime of a resumed session. Replacing the pathname atomically
// would leave Codex appending to the now-unlinked old inode, making every later
// answer invisible to Aplexica. An append preserves that descriptor and is safe
// whenever the existing visible turns are a prefix of the canonical plan.
func writeCodexConversationSession(
	path, fullRollout string,
	plannedTurns []acf.TextTurn,
	sessionID, threadID, branchID string,
	nativeOrigin bool,
	beforeAppend func(string) error,
) error {
	raw, snapshotInfo, err := readCodexSessionSnapshot(path)
	if os.IsNotExist(err) {
		return atomicfile.WriteFile(path, []byte(fullRollout), 0o644)
	}
	if err != nil {
		return err
	}
	ref, ok := codexThreadRef(raw)
	switch {
	case ok && ref.ArtifactID == threadID &&
		normalizeCodexBranchID(ref.BranchID) == normalizeCodexBranchID(branchID):
		// Our own generated rollout — unchanged behavior.
	case nativeOrigin && codexNativeSessionID(raw) != "" &&
		strings.HasSuffix(strings.TrimSuffix(filepath.Base(path), ".jsonl"), "-"+codexNativeSessionID(raw)):
		// The originating native rollout. It carries no Aplexica marker and never
		// will, so identity is its own session_meta id appearing in the pathname
		// the artifact recorded. Suffix match (not Contains) so a "-COPY" sibling
		// whose name merely contains the id as a substring cannot pass. The prefix
		// check below still guards the content.
		ref = adapter.ThreadRef{ArtifactID: threadID, BranchID: normalizeCodexBranchID(branchID)}
	default:
		return fmt.Errorf("refusing to overwrite unrelated existing session %s", path)
	}
	events, err := EncodeCanonical(raw)
	if err != nil {
		return err
	}
	events, _ = sanitizeGeneratedMaterializedEchoes(ref, events)
	existingTurns := acf.ExtractTextTurns(events)
	if acf.TextTurnsEqual(existingTurns, plannedTurns) || codexTextTurnsPrefix(plannedTurns, existingTurns) {
		// Equal means an idempotent re-materialization. planned<existing means
		// the native agent is already ahead of the canonical event currently
		// being delivered; preserve it for its pending import.
		return nil
	}
	if !codexTextTurnsPrefix(existingTurns, plannedTurns) {
		return errCodexSessionDiverged
	}

	appendix := transcodeCodexTurnAppend(plannedTurns[len(existingTurns):], sessionID, len(existingTurns), time.Now().UTC())
	if appendix == "" {
		return nil
	}
	if beforeAppend != nil {
		if err := beforeAppend(path); err != nil {
			return err
		}
	}
	return appendCodexRolloutIfUnchanged(path, raw, snapshotInfo, appendix)
}

// readCodexSessionSnapshot returns bytes tied to a stable inode and length. A
// native append while the read is in flight is a recoverable conflict, not a
// partially parsed rollout that may be treated as an authoritative prefix.
func readCodexSessionSnapshot(path string) ([]byte, os.FileInfo, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	before, err := f.Stat()
	if err != nil {
		return nil, nil, err
	}
	raw, err := io.ReadAll(f)
	if err != nil {
		return nil, nil, err
	}
	after, err := f.Stat()
	if err != nil {
		return nil, nil, err
	}
	pathInfo, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, errCodexSessionChanged
		}
		return nil, nil, err
	}
	if !os.SameFile(before, after) || !os.SameFile(after, pathInfo) ||
		before.Size() != after.Size() || after.Size() != int64(len(raw)) || pathInfo.Size() != int64(len(raw)) {
		return nil, nil, errCodexSessionChanged
	}
	return raw, after, nil
}

// appendCodexRolloutIfUnchanged performs the optimistic commit. It reopens and
// rereads the same inode, checks both the descriptor and current pathname, and
// verifies the expected length immediately before one O_APPEND write. If Codex
// won the race, the caller defers until the watcher imports that native append.
func appendCodexRolloutIfUnchanged(path string, snapshot []byte, snapshotInfo os.FileInfo, appendix string) error {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_APPEND, 0)
	if err != nil {
		if os.IsNotExist(err) {
			return errCodexSessionChanged
		}
		return err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(snapshotInfo, opened) || opened.Size() != int64(len(snapshot)) {
		return errCodexSessionChanged
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	current, err := io.ReadAll(f)
	if err != nil {
		return err
	}
	validated, err := f.Stat()
	if err != nil {
		return err
	}
	pathInfo, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return errCodexSessionChanged
		}
		return err
	}
	if !bytes.Equal(current, snapshot) || validated.Size() != int64(len(snapshot)) ||
		!os.SameFile(snapshotInfo, validated) || !os.SameFile(validated, pathInfo) ||
		pathInfo.Size() != int64(len(snapshot)) {
		return errCodexSessionChanged
	}
	end, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return err
	}
	if end != int64(len(snapshot)) {
		return errCodexSessionChanged
	}
	commitInfo, err := f.Stat()
	if err != nil {
		return err
	}
	commitPathInfo, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return errCodexSessionChanged
		}
		return err
	}
	if !os.SameFile(snapshotInfo, commitInfo) || !os.SameFile(commitInfo, commitPathInfo) ||
		commitInfo.Size() != int64(len(snapshot)) || commitPathInfo.Size() != int64(len(snapshot)) {
		return errCodexSessionChanged
	}
	if _, err := f.WriteString(appendix); err != nil {
		return err
	}
	return f.Sync()
}

func codexTextTurnsPrefix(prefix, full []acf.TextTurn) bool {
	return len(prefix) <= len(full) && acf.TextTurnsEqual(prefix, full[:len(prefix)])
}

// bestEffortQuarantineCodexThreadDuplicates removes only Aplexica-generated
// sibling rollouts that carry the same authenticated thread and branch markers.
// Older releases created remote/recovery rollouts during append conflicts;
// moving those files out of the sessions tree restores the one-thread/one-entry
// invariant while keeping them recoverable for diagnosis.
func (a *Adapter) bestEffortQuarantineCodexThreadDuplicates(
	primaryPath, threadID, branchID string,
	canonicalTurns []acf.TextTurn,
) {
	entries, err := os.ReadDir(filepath.Dir(primaryPath))
	if err != nil {
		return
	}
	sum := sha256.Sum256([]byte(threadID + ":" + normalizeCodexBranchID(branchID)))
	quarantineDir := filepath.Join(a.HomeDir, ".aplexica", "quarantine", "codex-conversations", fmt.Sprintf("%x", sum[:8]))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".jsonl" {
			continue
		}
		path := filepath.Join(filepath.Dir(primaryPath), entry.Name())
		if path == primaryPath {
			continue
		}
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			continue
		}
		ref, ok := codexThreadRef(raw)
		if !ok || ref.ArtifactID != threadID ||
			normalizeCodexBranchID(ref.BranchID) != normalizeCodexBranchID(branchID) {
			continue
		}
		events, encodeErr := EncodeCanonical(raw)
		if encodeErr != nil {
			continue
		}
		events, _ = sanitizeGeneratedMaterializedEchoes(ref, events)
		if !codexTextTurnsPrefix(acf.ExtractTextTurns(events), canonicalTurns) {
			// Preserve a sibling with an unimported/divergent continuation.
			continue
		}
		sessionID, ok := codexSessionMetaID(raw)
		if !ok || !strings.HasSuffix(
			strings.TrimSuffix(filepath.Base(path), ".jsonl"),
			"-"+sessionID,
		) {
			continue
		}
		if mkdirErr := os.MkdirAll(quarantineDir, 0o700); mkdirErr != nil {
			return
		}
		dest := filepath.Join(quarantineDir, entry.Name())
		if _, statErr := os.Stat(dest); statErr == nil {
			continue
		}
		_ = os.Rename(path, dest)
	}
}

func codexSessionMetaID(raw []byte) (string, bool) {
	for _, line := range bytes.Split(raw, []byte{'\n'}) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var row struct {
			Type    string `json:"type"`
			Payload struct {
				ID string `json:"id"`
			} `json:"payload"`
		}
		if json.Unmarshal(line, &row) != nil {
			return "", false
		}
		if row.Type == "session_meta" {
			return row.Payload.ID, row.Payload.ID != ""
		}
	}
	return "", false
}

func codexConversationTitleFromTurns(turns []acf.TextTurn) string {
	for _, turn := range turns {
		if turn.Role != "user" {
			continue
		}
		if title, ok := acf.NormalizeTextTurn("user", turn.Text); ok {
			return title
		}
	}
	return ""
}

func (a *Adapter) conversationCWD(art acf.Artifact, sourceAgent string) string {
	cwd := a.HomeDir
	if art.Project != nil {
		if a.Registry != nil {
			if entry, ok := a.Registry.Get(art.Project.ID); ok && existingDirectory(entry.Path) {
				cwd = entry.Path
			}
		} else if art.RemoteOriginDeviceID == "" && existingDirectory(art.Project.Path) {
			cwd = art.Project.Path
		}
	}
	// Only local-source artifacts may resolve SourcePath. Remote SourcePath is
	// provenance from another device and must not steer a local Codex session.
	if art.RemoteOriginDeviceID == "" {
		if sourceCWD := adapter.ResolveConversationCWD(a.HomeDir, sourceAgent, art.SourcePath); sourceCWD != "" {
			cwd = sourceCWD
		}
	}
	return cwd
}

func existingDirectory(path string) bool {
	if !filepath.IsAbs(path) {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// ConversationSessionPath lets the orchestrator recursion-guard Codex's
// deterministic rollout path before MaterializeConversationSession writes it.
func (a *Adapter) ConversationSessionPath(art acf.Artifact, head acf.Event, sourceAgent string) (string, bool, error) {
	path, ok, _, err := a.ConversationSessionPathReason(art, head, sourceAgent)
	return path, ok, err
}

// ConversationSessionPathReason (adapter.ConversationSessionPathDeclineReporter)
// is ConversationSessionPath plus the typed reason. Codex's planner only ever
// declines by opting out — divergence is detected by the writer — so the reason
// distinguishes "never materializable here" from "not yet".
func (a *Adapter) ConversationSessionPathReason(
	art acf.Artifact, head acf.Event, _ string,
) (string, bool, adapter.SessionDeclineReason, error) {
	plan, ok, err := a.conversationSessionPlan(art, head)
	if err != nil {
		return "", false, adapter.SessionDeclineUnspecified, err
	}
	if !ok {
		return "", false, adapter.SessionDeclineOptOut, nil
	}
	return plan.dest, true, adapter.SessionDeclineUnspecified, nil
}

type codexConversationSessionPlan struct {
	turns        []acf.TextTurn
	sessionTime  time.Time
	sessionID    string
	branchID     string
	dest         string
	nativeOrigin bool
}

func (a *Adapter) conversationSessionPlan(art acf.Artifact, head acf.Event) (codexConversationSessionPlan, bool, error) {
	if a.HomeDir == "" {
		return codexConversationSessionPlan{}, false, nil
	}
	p, err := acf.DecodeConversationPayload(head)
	if err != nil {
		return codexConversationSessionPlan{}, false, err
	}
	// Hermes-native conversations carry a SessionBundle payload rather
	// than canonical events; without this branch they silently never
	// appeared in codex resume (mirrors the claude-code materializer).
	var turns []acf.TextTurn
	switch p.Format {
	case acf.ConversationFormatV1:
		turns = acf.ExtractTextTurns(p.Events)
	case acf.ConversationFormatHermesBundle:
		turns = acf.TurnsFromHermesBundleJSON(p.Content)
	default:
		return codexConversationSessionPlan{}, false, nil
	}
	if len(turns) == 0 {
		return codexConversationSessionPlan{}, false, nil
	}
	sessionTime := art.CreatedAt.UTC()
	if sessionTime.IsZero() {
		sessionTime = head.Timestamp.UTC()
	}
	branchID := normalizeCodexBranchID(head.Branch)
	sessionID := codexSessionID(art.ArtifactID, branchID)
	dir := filepath.Join(a.sessionsDir(), sessionTime.Format("2006"), sessionTime.Format("01"), sessionTime.Format("02"))
	dest := filepath.Join(dir, "rollout-"+sessionTime.Format("2006-01-02T15-04-05")+"-"+sessionID+".jsonl")

	// Prefer extending the artifact's own originating native rollout over a
	// generated twin, but only when it is a clean absolute path inside THIS
	// device's sessions dir. Containment alone is insufficient: a remote
	// artifact carries a peer device's own absolute path, which on two
	// machines with the same username is lexically identical to a local
	// path and would pass containment. RemoteOriginDeviceID is the actual
	// device-local-identity check — SourcePath is only trustworthy when the
	// artifact originated on this device.
	nativeOrigin := false
	if branchID == acf.MainBranch && art.RemoteOriginDeviceID == "" {
		if source := strings.TrimSpace(art.SourcePath); source != "" &&
			filepath.IsAbs(source) && filepath.Clean(source) == source &&
			filepath.Ext(source) == ".jsonl" && strings.HasPrefix(filepath.Base(source), "rollout-") {
			if rel, relErr := filepath.Rel(a.sessionsDir(), source); relErr == nil &&
				rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				dest = source
				nativeOrigin = true
			}
		}
	}
	return codexConversationSessionPlan{
		turns: turns, sessionTime: sessionTime, sessionID: sessionID, branchID: branchID,
		dest: dest, nativeOrigin: nativeOrigin,
	}, true, nil
}

// transcodeToCodexRollout renders the thread's text turns into a Codex
// rollout JSONL (session_meta + per-exchange task_started/user/assistant/
// task_complete). Pairs each user turn with the following assistant turn(s) to
// mirror real rollouts.
func transcodeToCodexRollout(turns []acf.TextTurn, sessionID, threadID, branchID, cwd, sourceAgent string, sessionTime time.Time) string {
	if len(turns) == 0 {
		return ""
	}
	branchID = normalizeCodexBranchID(branchID)
	type obj = map[string]any
	var lines []string
	emit := func(o obj) { b, _ := json.Marshal(o); lines = append(lines, string(b)) }
	ts := sessionTime.UTC().Format("2006-01-02T15:04:05.000Z")

	emit(obj{"timestamp": ts, "type": "session_meta", "payload": obj{
		"id": sessionID, "timestamp": ts, "cwd": cwd, "originator": "codex-tui",
		"cli_version": syntheticCodexCLIVersion, "source": "cli", "thread_source": "user",
		"model_provider": "openai", "base_instructions": obj{"text": codexBaseInstructions},
		AplexicaThreadKey: threadID, AplexicaBranchKey: branchID,
		"aplexica_turns_hash": adapter.ConversationTurnsHash(turns), "aplexica_turn_count": len(turns),
		"aplexica_source_agent": sourceAgent,
	}})

	assistantMsg := func(text string) {
		emit(obj{"timestamp": ts, "type": "response_item", "payload": obj{
			"type": "message", "role": "assistant", "phase": "final_answer",
			"content": []any{obj{"type": "output_text", "text": text}},
		}})
		emit(obj{"timestamp": ts, "type": "event_msg", "payload": obj{
			"type": "agent_message", "message": text, "phase": "final_answer", "memory_citation": nil,
		}})
	}

	i := 0
	for i < len(turns) {
		t := turns[i]
		if t.Role != "user" {
			assistantMsg(t.Text) // rare leading assistant turn
			i++
			continue
		}
		turnID := deterministicCodexID(sessionID, i)
		emit(obj{"timestamp": ts, "type": "event_msg", "payload": obj{
			"type": "task_started", "turn_id": turnID, "model_context_window": syntheticCodexContextWindow, "collaboration_mode_kind": "default",
		}})
		emit(obj{"timestamp": ts, "type": "event_msg", "payload": obj{
			"type": "user_message", "message": t.Text, "images": []any{}, "local_images": []any{}, "text_elements": []any{},
		}})
		emit(obj{"timestamp": ts, "type": "response_item", "payload": obj{
			"type": "message", "role": "user", "content": []any{obj{"type": "input_text", "text": t.Text}},
		}})
		i++
		for i < len(turns) && turns[i].Role == "assistant" {
			assistantMsg(turns[i].Text)
			i++
		}
		emit(obj{"timestamp": ts, "type": "event_msg", "payload": obj{"type": "task_complete", "turn_id": turnID}})
	}
	return strings.Join(lines, "\n") + "\n"
}

// transcodeCodexTurnAppend renders only a canonical suffix for an existing
// rollout. It intentionally omits session_meta: the original marker remains the
// stable stale-base proof, while these later-timestamp rows look exactly like a
// native continuation and therefore re-import through the normal thread merge.
func transcodeCodexTurnAppend(turns []acf.TextTurn, sessionID string, startIndex int, at time.Time) string {
	if len(turns) == 0 {
		return ""
	}
	type obj = map[string]any
	var lines []string
	emit := func(o obj) { b, _ := json.Marshal(o); lines = append(lines, string(b)) }
	ts := at.UTC().Format(time.RFC3339Nano)
	assistantMsg := func(text string) {
		emit(obj{"timestamp": ts, "type": "response_item", "payload": obj{
			"type": "message", "role": "assistant", "phase": "final_answer",
			"content": []any{obj{"type": "output_text", "text": text}},
		}})
		emit(obj{"timestamp": ts, "type": "event_msg", "payload": obj{
			"type": "agent_message", "message": text, "phase": "final_answer", "memory_citation": nil,
		}})
	}

	for i := 0; i < len(turns); {
		t := turns[i]
		if t.Role != "user" {
			assistantMsg(t.Text)
			i++
			continue
		}
		turnID := deterministicCodexID(sessionID, startIndex+i)
		emit(obj{"timestamp": ts, "type": "event_msg", "payload": obj{
			"type": "task_started", "turn_id": turnID, "model_context_window": syntheticCodexContextWindow, "collaboration_mode_kind": "default",
		}})
		emit(obj{"timestamp": ts, "type": "event_msg", "payload": obj{
			"type": "user_message", "message": t.Text, "images": []any{}, "local_images": []any{}, "text_elements": []any{},
		}})
		emit(obj{"timestamp": ts, "type": "response_item", "payload": obj{
			"type": "message", "role": "user", "content": []any{obj{"type": "input_text", "text": t.Text}},
		}})
		i++
		for i < len(turns) && turns[i].Role == "assistant" {
			assistantMsg(turns[i].Text)
			i++
		}
		emit(obj{"timestamp": ts, "type": "event_msg", "payload": obj{"type": "task_complete", "turn_id": turnID}})
	}
	return strings.Join(lines, "\n") + "\n"
}

func codexThreadRef(raw []byte) (adapter.ThreadRef, bool) {
	var ref adapter.ThreadRef
	var generatedTimestamp string
	generatedSnapshot := false
	generatedSession := false
	hasExplicitTurnCount := false
	sanitizedSyntheticTurn := false
	found := false
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var m struct {
			Timestamp string `json:"timestamp"`
			Type      string `json:"type"`
			Payload   struct {
				AplexicaThreadID  string          `json:"aplexica_thread_id"`
				AplexicaBranchID  string          `json:"aplexica_branch_id"`
				AplexicaTurnsHash string          `json:"aplexica_turns_hash"`
				AplexicaTurnCount int             `json:"aplexica_turn_count"`
				CLIVersion        string          `json:"cli_version"`
				Type              string          `json:"type"`
				Role              string          `json:"role"`
				Content           []rawCodexBlock `json:"content"`
			} `json:"payload"`
		}
		if json.Unmarshal([]byte(line), &m) != nil {
			generatedSnapshot = false
			continue
		}
		if m.Type == "session_meta" && !found {
			if m.Payload.AplexicaThreadID == "" {
				return adapter.ThreadRef{}, false
			}
			branchID := normalizeCodexBranchID(m.Payload.AplexicaBranchID)
			ref = adapter.ThreadRef{
				ArtifactID:            m.Payload.AplexicaThreadID,
				BranchID:              branchID,
				MaterializedTurnsHash: m.Payload.AplexicaTurnsHash,
				MaterializedTurnCount: m.Payload.AplexicaTurnCount,
			}
			hasExplicitTurnCount = m.Payload.AplexicaTurnCount > 0
			found = true
			generatedTimestamp = m.Timestamp
			// Older Aplexica releases stamped the durable thread marker but
			// copied the installed Codex CLI version into generated rollouts.
			// The thread marker is the authoritative provenance signal; accepting
			// it keeps those older materializations repairable after upgrade.
			generatedSession = m.Payload.CLIVersion == syntheticCodexCLIVersion ||
				m.Payload.AplexicaThreadID != ""
			generatedSnapshot = generatedSession && generatedTimestamp != ""
			continue
		}
		if found && generatedSession && m.Type == "response_item" &&
			m.Payload.Type == "message" && normalizeCodexRole(m.Payload.Role) == "assistant" &&
			syntheticNoResponse(codexContent(m.Payload.Content)) {
			sanitizedSyntheticTurn = true
		}
		if found && !hasExplicitTurnCount && m.Timestamp == generatedTimestamp &&
			m.Type == "response_item" && m.Payload.Type == "message" {
			role := normalizeCodexRole(m.Payload.Role)
			content := codexContent(m.Payload.Content)
			if (role == "user" || role == "assistant") && len(content) > 0 &&
				!(role == "assistant" && syntheticNoResponse(content)) {
				ref.MaterializedTurnCount++
			}
		}
		if found && m.Timestamp != generatedTimestamp {
			generatedSnapshot = false
		}
	}
	if !found {
		return adapter.ThreadRef{}, false
	}
	// Legacy generated Codex rollouts used one synthetic timestamp on every
	// row. A resumed/continued rollout necessarily appends later timestamps,
	// so the all-equal legacy shape is safe to classify as an unchanged mirror.
	ref.GeneratedSnapshot = generatedSnapshot
	ref.SanitizedSyntheticTurn = sanitizedSyntheticTurn
	return ref, true
}

func codexThreadID(raw []byte) string {
	ref, ok := codexThreadRef(raw)
	if !ok {
		return ""
	}
	return ref.ArtifactID
}

func codexSessionID(artifactID, branchID string) string {
	branchID = normalizeCodexBranchID(branchID)
	if branchID == acf.MainBranch {
		return artifactID
	}
	return deterministicCodexID("aplexica:"+artifactID+":"+branchID, 0)
}

func normalizeCodexBranchID(branchID string) string {
	if branchID == "" {
		return acf.MainBranch
	}
	norm, err := acf.NormalizeBranchName(branchID)
	if err != nil {
		return acf.MainBranch
	}
	return norm
}

func deterministicCodexID(seed string, i int) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("codex:%s#%d", seed, i)))
	b := h[:16]
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
