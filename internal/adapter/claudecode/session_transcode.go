package claudecode

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/json"
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

const (
	materializedClaudeTitleMaxRunes               = 80
	claudeAppendValidationMaxAttempts             = 4
	claudeSessionFileMode             os.FileMode = 0o644
)

type claudeBeforeAppendHook func(path string) error

// MaterializeConversationSession (adapter.ConversationSessionTarget) transcodes
// a foreign conversation into Claude Code's NATIVE session JSONL so it appears
// in `claude` /resume and can be opened/continued. A Claude-origin conversation
// is extended at its original stable pathname only when its visible parentUuid
// chain is an exact canonical prefix and the byte/inode snapshot is unchanged.
// Foreign-origin conversations use one deterministic synthetic pathname. A
// detected race or divergence is left untouched and retried after watcher
// import; materialization never creates a second session for the same thread.
func (a *Adapter) MaterializeConversationSession(art acf.Artifact, head acf.Event, sourceAgent string) (string, bool, error) {
	path, ok, _, err := a.materializeConversationSession(art, head, sourceAgent, nil, nil)
	return path, ok, err
}

// MaterializeConversationSessionReason (adapter.ConversationSessionDeclineReporter)
// is MaterializeConversationSession plus the typed reason for a decline, so the
// orchestrator can tell a live agent racing us apart from a projection that can
// never converge instead of retrying both forever.
func (a *Adapter) MaterializeConversationSessionReason(
	art acf.Artifact, head acf.Event, sourceAgent string,
) (string, bool, adapter.SessionDeclineReason, error) {
	return a.materializeConversationSession(art, head, sourceAgent, nil, nil)
}

func (a *Adapter) materializeConversationSession(
	art acf.Artifact,
	head acf.Event,
	sourceAgent string,
	beforeAppend claudeBeforeAppendHook,
	afterAppend claudeBeforeAppendHook,
) (string, bool, adapter.SessionDeclineReason, error) {
	plan, ok, err := a.conversationSessionPlan(art, head)
	if err != nil {
		return "", false, adapter.SessionDeclineUnspecified, err
	}
	if !ok {
		// No home directory, an unsupported payload format, or no text turns:
		// nothing here is ever materializable, which is not a failure.
		return "", false, adapter.SessionDeclineOptOut, nil
	}
	portableTitle := adapter.ResolveConversationTitle(a.HomeDir, sourceAgent, art.SourcePath, art.Name)
	materializedTitle := claudeConversationDisplayTitle(plan.turns, sourceAgent, portableTitle)
	primaryTitle := adapter.ConversationBranchDisplayTitle(materializedTitle, plan.branchID)
	primaryTitle = truncate(primaryTitle, materializedClaudeTitleMaxRunes)
	if plan.nativeOrigin && (!plan.nativeSource || !plan.nativeWritable) {
		// An unimported or structurally ambiguous native suffix must remain
		// visible at its original path. Decline this pass without guard-marking
		// it or creating a second session.
		//
		// One exception, and it is a reachable repair point for a conversation
		// that started in Claude Code: a genuine DIVERGENCE, where canonical and
		// the file each hold a turn the other lacks, is repairable on the
		// row-level containment proof. nativeRepairableReason states which
		// classes qualify and why the others do not.
		if plan.nativeSource && nativeRepairableReason(plan.declineReason) {
			repaired, repairErr := a.repairNativeConversationSession(plan, art.ArtifactID, primaryTitle)
			if repairErr != nil {
				return plan.dest, false, adapter.SessionDeclineUnspecified, repairErr
			}
			if repaired {
				return plan.dest, true, adapter.SessionDeclineUnspecified, nil
			}
		}
		return plan.dest, false, plan.declineReason, nil
	}
	consumeBeforeAppend := func(path string) error {
		if beforeAppend == nil {
			return nil
		}
		hook := beforeAppend
		beforeAppend = nil
		return hook(path)
	}
	consumeAfterAppend := func(path string) error {
		if afterAppend == nil {
			return nil
		}
		hook := afterAppend
		afterAppend = nil
		return hook(path)
	}
	if plan.nativeSource {
		compatible, exactNativeCWD, nativeReason, nativeErr := a.writeClaudeNativeConversationSession(
			plan.dest,
			plan.turns,
			plan.sessionID,
			art.ArtifactID,
			plan.branchID,
			plan.cwd,
			plan.base,
			consumeBeforeAppend,
			consumeAfterAppend,
		)
		if nativeErr != nil {
			return plan.dest, false, adapter.SessionDeclineUnspecified,
				fmt.Errorf("claudecode: extend native source session: %w", nativeErr)
		}
		if exactNativeCWD != "" {
			plan.cwd = exactNativeCWD
		}
		if compatible {
			a.bestEffortUpsertDesktopSession(plan.sessionID, primaryTitle, plan.cwd, plan.base)
			a.bestEffortQuarantineClaudeThreadDuplicates(plan, art.ArtifactID)
			return plan.dest, true, adapter.SessionDeclineUnspecified, nil
		}
		// The WRITER's own reason routes the repair too, not just the planner's.
		// claudeNativeSourceSessionPlan compares turns in FILE ORDER, so the
		// owner's scenario — Aplexica appends the foreign turns, the still-open
		// Claude Code appends the user's next prompt as a child of its in-memory
		// leaf, canonical then absorbs that prompt — arrives here as a perfectly
		// writable plan whose graph nonetheless forked. Only this call learns
		// that. Gating the repair on plan.declineReason alone left the common
		// population with a repair that existed and could never be reached.
		if plan.nativeOrigin && nativeRepairableReason(nativeReason) {
			repaired, repairErr := a.repairNativeConversationSession(plan, art.ArtifactID, primaryTitle)
			if repairErr != nil {
				return plan.dest, false, adapter.SessionDeclineUnspecified, repairErr
			}
			if repaired {
				return plan.dest, true, adapter.SessionDeclineUnspecified, nil
			}
		}
		// The original Claude session changed or is ahead/divergent. Preserve the
		// single visible conversation and let the watcher import that native
		// change before a later fan-out retry. Creating a second "continuation"
		// mirror here is exactly what made one thread appear three times.
		return plan.dest, false, nativeReason, nil
	}
	if err := os.MkdirAll(filepath.Dir(plan.dest), 0o755); err != nil {
		return "", false, adapter.SessionDeclineUnspecified, fmt.Errorf("claudecode: mkdir project dir: %w", err)
	}

	compatible, mirrorReason, err := writeClaudeConversationSession(
		plan.dest,
		func(uuids []string, bridges []claudeBridgeRow) string {
			return transcodeToClaudeSessionWithUUIDs(
				plan.turns, uuids, bridges, plan.sessionID, art.ArtifactID, plan.branchID,
				plan.cwd, sourceAgent, materializedTitle, plan.base)
		},
		plan.turns, plan.sessionID, art.ArtifactID, plan.branchID, plan.cwd, plan.base,
		consumeBeforeAppend, consumeAfterAppend,
		a.mirrorRepairPolicy(plan, art.ArtifactID),
	)
	if err != nil {
		return "", false, adapter.SessionDeclineUnspecified, fmt.Errorf("claudecode: write session: %w", err)
	}
	if compatible {
		a.bestEffortUpsertDesktopSession(plan.sessionID, primaryTitle, plan.cwd, plan.base)
		a.bestEffortQuarantineClaudeThreadDuplicates(plan, art.ArtifactID)
		return plan.dest, true, adapter.SessionDeclineUnspecified, nil
	}
	// A divergent stable mirror contains an unimported local continuation.
	// Leave that one pathname untouched and retry after its watcher import.
	// Never multiply one canonical thread into recovery sessions.
	return plan.dest, false, mirrorReason, nil
}

// repairNativeConversationSession is the ONE place the native rebuild is
// committed from, so its two entry points — the planner's decline and the
// writer's decline — cannot drift in what they do afterwards. A repaired file
// gets exactly the post-write bookkeeping a successful write gets.
func (a *Adapter) repairNativeConversationSession(
	plan claudeConversationSessionPlan, artifactID, primaryTitle string,
) (bool, error) {
	repaired, err := a.repairDivergedNativeSession(
		plan.dest, plan.turns, plan.sessionID, plan.cwd, plan.base,
		a.nativeRepairPolicy(plan, artifactID))
	if err != nil {
		return false, fmt.Errorf("claudecode: repair diverged native session: %w", err)
	}
	if !repaired {
		return false, nil
	}
	a.bestEffortUpsertDesktopSession(plan.sessionID, primaryTitle, plan.cwd, plan.base)
	a.bestEffortQuarantineClaudeThreadDuplicates(plan, artifactID)
	return true, nil
}

// claudeSessionRenderer produces the whole synthetic session for the planned
// turns. uuids may be nil (deterministic per-index uuids) or one uuid per
// planned turn, and bridges may be nil (nothing carried); see
// transcodeToClaudeSessionWithUUIDs.
type claudeSessionRenderer func(uuids []string, bridges []claudeBridgeRow) string

// writeClaudeConversationSession publishes a new synthetic session exactly
// once, reuses an exact snapshot, or incrementally extends a stable
// Aplexica-owned mirror. It never replaces a Claude-visible pathname. Every
// append is authorized by an exact whole-file/inode comparison and checked
// again after fsync; compatible=false tells the caller to defer until the
// watcher imports the native change, and reason names why that pass declined.
// policy carries the caller's forked-mirror repair authorization; its zero
// value authorizes nothing and reproduces the pre-repair behaviour exactly.
func writeClaudeConversationSession(
	path string,
	render claudeSessionRenderer,
	plannedTurns []acf.TextTurn,
	sessionID, threadID, branchID, cwd string,
	base time.Time,
	beforeAppend claudeBeforeAppendHook,
	afterAppend claudeBeforeAppendHook,
	policy claudeMirrorRepairPolicy,
) (bool, adapter.SessionDeclineReason, error) {
	fullSession := render(nil, nil)
	// verified turns a post-write verification result into the caller's
	// (compatible, reason) pair: a mirror that fails its own read-back is
	// incompatible for a structural reason, not because we lost a race.
	verified := func(raw []byte) (bool, adapter.SessionDeclineReason, error) {
		matches, err := claudeSessionMatches(raw, plannedTurns, sessionID, threadID, branchID)
		if err != nil || matches {
			return matches, adapter.SessionDeclineUnspecified, err
		}
		return false, classifyClaudeMirrorDecline(raw, plannedTurns, sessionID, threadID, branchID), nil
	}
	for attempt := 0; attempt < claudeAppendValidationMaxAttempts; attempt++ {
		snapshot, exists, changed, err := readClaudeSessionSnapshot(path)
		if err != nil {
			return false, adapter.SessionDeclineUnspecified, err
		}
		if changed {
			continue
		}
		if !exists {
			if beforeAppend != nil {
				if err := beforeAppend(path); err != nil {
					return false, adapter.SessionDeclineUnspecified, err
				}
				beforeAppend = nil
			}
			created, createErr := writeClaudeSessionExclusive(path, []byte(fullSession), claudeSessionFileMode)
			if createErr != nil {
				return false, adapter.SessionDeclineUnspecified, createErr
			}
			if created {
				if afterAppend != nil {
					if err := afterAppend(path); err != nil {
						return false, adapter.SessionDeclineUnspecified, err
					}
					afterAppend = nil
				}
				post, postExists, postChanged, readErr := readClaudeSessionSnapshot(path)
				if readErr != nil {
					return false, adapter.SessionDeclineUnspecified, readErr
				}
				if !postExists || postChanged {
					return false, adapter.SessionDeclineRace, nil
				}
				return verified(post.raw)
			}
			continue
		}
		if len(snapshot.raw) == 0 {
			// The pathname exists and holds NOTHING. The create branch above is
			// gated on !exists, every identity proof below fails on empty bytes,
			// and classifyClaudeMirrorDecline used to call this a race — so an
			// empty mirror was terminal in the one direction that cannot be
			// tolerated: the thread vanished from /resume and no code path could
			// recreate it. An empty file holds no unimported turn by definition,
			// so writing the session into it loses nothing. Inode-preserving, so
			// a co-owning writer keeps the file it opened.
			if beforeAppend != nil {
				if err := beforeAppend(path); err != nil {
					return false, adapter.SessionDeclineUnspecified, err
				}
				beforeAppend = nil
			}
			written, raced, writeErr := rewriteClaudeSessionIfSnapshotCurrent(
				path, snapshot, []byte(fullSession))
			if writeErr != nil {
				return false, adapter.SessionDeclineUnspecified, writeErr
			}
			if raced || !written {
				continue
			}
			if afterAppend != nil {
				if err := afterAppend(path); err != nil {
					return false, adapter.SessionDeclineUnspecified, err
				}
			}
			post, postExists, postChanged, readErr := readClaudeSessionSnapshot(path)
			if readErr != nil {
				return false, adapter.SessionDeclineUnspecified, readErr
			}
			if !postExists || postChanged {
				return false, adapter.SessionDeclineRace, nil
			}
			return verified(post.raw)
		}

		matches, matchErr := claudeSessionMatches(snapshot.raw, plannedTurns, sessionID, threadID, branchID)
		if matchErr != nil {
			return false, adapter.SessionDeclineUnspecified, matchErr
		}
		if matches {
			return true, adapter.SessionDeclineUnspecified, nil
		}
		appendix, appendable, appendErr := claudeSessionAppendix(
			snapshot.raw, plannedTurns, sessionID, threadID, branchID, cwd, base,
		)
		if appendErr != nil {
			return false, adapter.SessionDeclineUnspecified, appendErr
		}
		if !appendable || appendix == "" {
			// Appending is prefix-gated, and a mirror can diverge from the plan
			// in a way that no future append will ever repair: the user
			// continues inside a mirror that is behind, the continuation is
			// imported and linearized AFTER the remote turns it missed, and
			// from that moment the mirror is a SUBSEQUENCE of canonical rather
			// than a prefix. Declining then re-queues forever — the artifact
			// re-enters the deferral queue on every inbound event and the user
			// watches a transcript frozen at whatever it held when it diverged.
			//
			// Rebuild instead, but only on proof that nothing is lost: every
			// visible turn in the mirror must already appear, in order, in the
			// plan. That is what makes the rewrite safe — the reason this
			// function never replaces a Claude-visible pathname is to protect an
			// unimported local continuation, and a mirror whose turns are all
			// canonical has none. A mirror holding even one turn the plan lacks
			// still declines and waits for its watcher import.
			if rebuilt, rebuildErr := rebuildDivergedClaudeMirror(
				path, snapshot, render, plannedTurns, sessionID, threadID, branchID, policy,
			); rebuildErr != nil || rebuilt {
				return rebuilt, adapter.SessionDeclineUnspecified, rebuildErr
			}
			return false, classifyClaudeMirrorDecline(
				snapshot.raw, plannedTurns, sessionID, threadID, branchID), nil
		}
		if beforeAppend != nil {
			if err := beforeAppend(path); err != nil {
				return false, adapter.SessionDeclineUnspecified, err
			}
			beforeAppend = nil
		}
		written, raced, appendErr := appendClaudeSessionIfSnapshotCurrent(path, snapshot, appendix)
		if appendErr != nil {
			return false, adapter.SessionDeclineUnspecified, appendErr
		}
		if raced || !written {
			return false, adapter.SessionDeclineRace, nil
		}
		if afterAppend != nil {
			if err := afterAppend(path); err != nil {
				return false, adapter.SessionDeclineUnspecified, err
			}
			afterAppend = nil
		}
		post, exists, changed, readErr := readClaudeSessionSnapshot(path)
		if readErr != nil {
			return false, adapter.SessionDeclineUnspecified, readErr
		}
		if !exists || changed {
			return false, adapter.SessionDeclineRace, nil
		}
		return verified(post.raw)
	}
	// Every attempt saw the file change under it. That is a live writer, not a
	// structural fault.
	return false, adapter.SessionDeclineRace, nil
}

// classifyClaudeMirrorDecline names why a stable Aplexica-owned mirror could
// neither be reused nor appended. It re-reads nothing and re-decides nothing:
// it walks the same predicates writeClaudeConversationSession just evaluated,
// in the same order, over the exact snapshot bytes those decisions were made
// from, and reports which one refused.
func classifyClaudeMirrorDecline(
	raw []byte,
	plannedTurns []acf.TextTurn,
	sessionID, threadID, branchID string,
) adapter.SessionDeclineReason {
	if len(raw) == 0 {
		// An EMPTY file is not a writer mid-append: no writer publishes a
		// zero-length transcript and then stops. It is a structural state — a
		// truncated commit, a failed restore — and reporting it as a race is how
		// a permanently destroyed thread got retried forever under the
		// explanation "the destination was being written on every attempt".
		return adapter.SessionDeclineGraphMalformed
	}
	if raw[len(raw)-1] != '\n' {
		// A torn trailing row is a writer mid-append, never a permanent state.
		return adapter.SessionDeclineRace
	}
	ref, ok := claudeSessionThreadRef(raw)
	if !ok || ref.ArtifactID != threadID ||
		normalizeClaudeBranchID(ref.BranchID) != normalizeClaudeBranchID(branchID) ||
		claudeSessionThreadID(raw) != sessionID {
		// The pathname is occupied by a session that cannot be authenticated as
		// this thread. Retrying re-reads the same bytes.
		return adapter.SessionDeclineGraphMalformed
	}
	projection, err := parseClaudeVisibleLeaf(raw)
	if err != nil {
		return adapter.SessionDeclineGraphMalformed
	}
	if !projection.spans() {
		// Conversational rows exist on disk that the resume walk cannot reach.
		// This is the state that fails closed at every repair door, so it must
		// not read as an ordinary divergence — but naming it is a separate
		// question from detecting it. The trigger stays "the walk did not span
		// the file", which is what the writer actually evaluated; the REASON
		// comes from a direct fork measurement, so a non-forked shape is no
		// longer reported as, and offered the remedy of, a fork.
		return projection.declineReason()
	}
	if !claudeSyntheticBaseMatches(ref, projection.turns) {
		// This is an Aplexica-owned MIRROR, so the side holding something the
		// other lacks is the mirror, not the user's own transcript. Naming the
		// two apart is what keeps the canonical-dedupe remedy off declines it
		// cannot resolve.
		return adapter.SessionDeclineMirrorDiverged
	}
	if acf.TextTurnsEqual(projection.turns, plannedTurns) {
		// Content agrees, so whatever refused was a snapshot-level race.
		return adapter.SessionDeclineRace
	}
	if claudeTextTurnsPrefix(plannedTurns, projection.turns) {
		return adapter.SessionDeclineNativeAhead
	}
	if !claudeTextTurnsPrefix(projection.turns, plannedTurns) {
		return adapter.SessionDeclineMirrorDiverged
	}
	return adapter.SessionDeclineRace
}

// writeClaudeSessionExclusive writes and fsyncs a private sibling temporary,
// then publishes it with an atomic hard link. Link is create-exclusive on both
// APFS and NTFS: a racing producer can win the destination name, but can never
// be overwritten. The caller then re-reads that winner and either accepts its
// exact canonical state or defers until the competing writer is imported.
func writeClaudeSessionExclusive(path string, content []byte, mode os.FileMode) (created bool, err error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".aplexica-claude-session-*.tmp")
	if err != nil {
		return false, err
	}
	tmpPath := tmp.Name()
	defer func() {
		_ = os.Remove(tmpPath)
	}()
	if err = tmp.Chmod(mode); err == nil {
		var n int
		n, err = tmp.Write(content)
		if err == nil && n != len(content) {
			err = io.ErrShortWrite
		}
	}
	if err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return false, err
	}
	if err = os.Link(tmpPath, path); err != nil {
		if os.IsExist(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// writeClaudeNativeConversationSession extends the original Claude transcript
// in place when its visible parentUuid chain is an exact prefix of the
// canonical conversation. This preserves one session id and one /resume entry.
// Every append is tied to an unchanged inode+byte snapshot; a concurrent native
// write loses no data and simply defers materialization until the watcher has
// imported it.
func (a *Adapter) writeClaudeNativeConversationSession(
	path string,
	plannedTurns []acf.TextTurn,
	sessionID, threadID, branchID, cwd string,
	base time.Time,
	beforeAppend claudeBeforeAppendHook,
	afterAppend claudeBeforeAppendHook,
) (bool, string, adapter.SessionDeclineReason, error) {
	validatedPath, valid := a.localConversationSourcePath(path)
	if !valid || filepath.Clean(validatedPath) != filepath.Clean(path) {
		// The caller already proved this is the artifact's native source, so a
		// path that no longer validates is a structural contradiction.
		return false, "", adapter.SessionDeclineGraphMalformed, nil
	}
	exactCWD := cwd
	for attempt := 0; attempt < claudeAppendValidationMaxAttempts; attempt++ {
		snapshot, exists, changed, err := readClaudeSessionSnapshot(path)
		if err != nil {
			return false, exactCWD, adapter.SessionDeclineUnspecified, err
		}
		if !exists {
			// The native transcript vanished between planning and writing.
			return false, exactCWD, adapter.SessionDeclineRace, nil
		}
		if changed {
			continue
		}
		state, resume := encodeCanonicalInto(snapshot.raw, 0, claudeCanonicalState{})
		if state.lastCWD != "" {
			exactCWD = state.lastCWD
		}
		if len(bytes.TrimSpace(snapshot.raw[resume:])) != 0 {
			// An unconsumed tail is a row being written right now.
			return false, exactCWD, adapter.SessionDeclineRace, nil
		}
		if state.sessionID != sessionID {
			return false, exactCWD, adapter.SessionDeclineGraphMalformed, nil
		}
		projection, projectionErr := parseClaudeVisibleLeaf(snapshot.raw)
		if projectionErr != nil {
			return false, exactCWD, adapter.SessionDeclineGraphMalformed, nil
		}
		if !projection.spans() {
			// Spanning, not forkedness, is the right guard here: the append below
			// slices plannedTurns at len(projection.turns) and parents the suffix
			// at projection.leafUUID, so a turn-bearing row off the walked chain
			// makes both wrong. Only the NAME changes — a non-spanning NATIVE
			// transcript is the user's own file, never a "forked mirror".
			return false, exactCWD, projection.declineReason(), nil
		}
		if acf.TextTurnsEqual(projection.turns, plannedTurns) {
			return true, exactCWD, adapter.SessionDeclineUnspecified, nil
		}
		if !claudeTextTurnsPrefix(projection.turns, plannedTurns) {
			// Native is ahead or divergent. Its watcher import is the only
			// authority allowed to change the canonical plan.
			if claudeTextTurnsPrefix(plannedTurns, projection.turns) {
				return false, exactCWD, adapter.SessionDeclineNativeAhead, nil
			}
			return false, exactCWD, adapter.SessionDeclineDiverged, nil
		}
		appendix := transcodeClaudeTurnAppend(
			plannedTurns[len(projection.turns):],
			sessionID,
			threadID,
			branchID,
			false,
			nil,
			nil,
			len(projection.turns),
			exactCWD,
			base,
			projection.leafUUID,
			lastClaudeUserText(plannedTurns),
		)
		if appendix == "" {
			return false, exactCWD, adapter.SessionDeclineRace, nil
		}
		if beforeAppend != nil {
			if err := beforeAppend(path); err != nil {
				return false, exactCWD, adapter.SessionDeclineUnspecified, err
			}
			beforeAppend = nil
		}
		written, raced, appendErr := appendClaudeSessionIfSnapshotCurrent(path, snapshot, appendix)
		if appendErr != nil {
			return false, exactCWD, adapter.SessionDeclineUnspecified, appendErr
		}
		if raced || !written {
			continue
		}
		if afterAppend != nil {
			if err := afterAppend(path); err != nil {
				return false, exactCWD, adapter.SessionDeclineUnspecified, err
			}
			afterAppend = nil
		}
	}
	// Either every attempt lost the snapshot race, or the loop appended and
	// re-entered to re-verify. Both are live-writer states.
	return false, exactCWD, adapter.SessionDeclineRace, nil
}

// bestEffortQuarantineClaudeThreadDuplicates removes only Aplexica-generated
// sibling sessions that carry the same authenticated thread marker. They are
// moved outside ~/.claude rather than deleted, so old v1.0.41 continuation and
// recovery files stop appearing in /resume without losing recovery evidence.
func (a *Adapter) bestEffortQuarantineClaudeThreadDuplicates(plan claudeConversationSessionPlan, artifactID string) {
	dir := filepath.Dir(plan.syntheticDest)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	keep, err := filepath.Abs(plan.dest)
	if err != nil {
		return
	}
	quarantine := filepath.Join(
		a.HomeDir,
		".aplexica",
		"quarantine",
		"claude-conversations",
		shortHash(artifactID),
	)
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".jsonl" {
			continue
		}
		candidate := filepath.Join(dir, entry.Name())
		candidateAbs, absErr := filepath.Abs(candidate)
		if absErr != nil || filepath.Clean(candidateAbs) == filepath.Clean(keep) {
			continue
		}
		if !claudeGeneratedSessionBelongsToThread(candidate, artifactID, plan.branchID) {
			continue
		}
		raw, readErr := os.ReadFile(candidate)
		if readErr != nil {
			continue
		}
		projection, projectionErr := parseClaudeVisibleLeaf(raw)
		if projectionErr != nil || !projection.spans() ||
			!claudeTextTurnsPrefix(projection.turns, plan.turns) {
			// A sibling with an unimported or divergent continuation remains in
			// place until that content is merged into the canonical plan.
			//
			// This site is EXEMPT from the fork/spanning split applied elsewhere,
			// deliberately. Its question is containment — "is every conversational
			// row in this sibling accounted for by the canonical plan, so renaming
			// it out of ~/.claude loses nothing" — and it guards a DESTRUCTIVE
			// rename. A file whose walk does not span it holds rows nobody has
			// proven canonical, forked or not; quarantining it would hide them.
			continue
		}
		if err := os.MkdirAll(quarantine, 0o700); err != nil {
			return
		}
		dest := filepath.Join(quarantine, entry.Name())
		if _, err := os.Stat(dest); err == nil {
			dest = filepath.Join(
				quarantine,
				fmt.Sprintf("%d-%s", time.Now().UTC().UnixNano(), entry.Name()),
			)
		}
		_ = os.Rename(candidate, dest)
	}
}

func claudeGeneratedSessionBelongsToThread(path, artifactID, branchID string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, scanBufInitial), scanBufMax)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var row struct {
			SessionID        string `json:"sessionId"`
			AplexicaThreadID string `json:"aplexicaThreadId"`
			AplexicaBranchID string `json:"aplexicaBranchId"`
		}
		if json.Unmarshal(line, &row) != nil ||
			row.AplexicaThreadID != artifactID ||
			normalizeClaudeBranchID(row.AplexicaBranchID) != normalizeClaudeBranchID(branchID) ||
			row.SessionID == "" ||
			strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)) != row.SessionID {
			return false
		}
		return true
	}
	return false
}

type claudeSessionSnapshot struct {
	raw  []byte
	info os.FileInfo
}

func readClaudeSessionSnapshot(path string) (snapshot claudeSessionSnapshot, exists, changed bool, err error) {
	linkInfo, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return claudeSessionSnapshot{}, false, false, nil
	}
	if err != nil {
		return claudeSessionSnapshot{}, false, false, err
	}
	if linkInfo.Mode()&os.ModeSymlink != 0 || !linkInfo.Mode().IsRegular() {
		return claudeSessionSnapshot{}, false, false, fmt.Errorf("claudecode: refusing non-regular session path %q", path)
	}
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return claudeSessionSnapshot{}, false, true, nil
	}
	if err != nil {
		return claudeSessionSnapshot{}, false, false, err
	}
	defer f.Close()

	before, err := f.Stat()
	if err != nil {
		return claudeSessionSnapshot{}, false, false, err
	}
	raw, err := io.ReadAll(f)
	if err != nil {
		return claudeSessionSnapshot{}, false, false, err
	}
	after, err := f.Stat()
	if err != nil {
		return claudeSessionSnapshot{}, false, false, err
	}
	pathInfo, err := os.Stat(path)
	if os.IsNotExist(err) {
		return claudeSessionSnapshot{}, false, true, nil
	}
	if err != nil {
		return claudeSessionSnapshot{}, false, false, err
	}
	if !os.SameFile(linkInfo, before) || !os.SameFile(before, after) || !os.SameFile(after, pathInfo) ||
		before.Size() != after.Size() || after.Size() != pathInfo.Size() || int64(len(raw)) != after.Size() {
		return claudeSessionSnapshot{}, false, true, nil
	}
	return claudeSessionSnapshot{raw: raw, info: after}, true, false, nil
}

func claudeSessionAppendix(
	raw []byte,
	plannedTurns []acf.TextTurn,
	sessionID, threadID, branchID, cwd string,
	base time.Time,
) (string, bool, error) {
	if len(raw) == 0 || raw[len(raw)-1] != '\n' {
		return "", false, nil
	}
	ref, ok := claudeSessionThreadRef(raw)
	if !ok || ref.ArtifactID != threadID ||
		normalizeClaudeBranchID(ref.BranchID) != normalizeClaudeBranchID(branchID) ||
		claudeSessionThreadID(raw) != sessionID {
		return "", false, nil
	}
	projection, err := parseClaudeVisibleLeaf(raw)
	if err != nil || !projection.spans() {
		// Same append arithmetic as the native writer: the suffix is sliced at
		// len(existingTurns) and parented at projection.leafUUID, so an append is
		// sound only while the walked chain accounts for every conversational
		// row. No reason is plumbed here on purpose — the caller re-derives it
		// through classifyClaudeMirrorDecline, and two places that can disagree
		// about why a write refused is how the reason stopped meaning anything.
		return "", false, nil
	}
	existingTurns := projection.turns
	if !claudeSyntheticBaseMatches(ref, existingTurns) {
		return "", false, nil
	}
	if acf.TextTurnsEqual(existingTurns, plannedTurns) {
		return "", true, nil
	}
	if !claudeTextTurnsPrefix(existingTurns, plannedTurns) {
		return "", false, nil
	}
	return transcodeClaudeTurnAppend(
		plannedTurns[len(existingTurns):], sessionID, threadID, branchID,
		true, plannedTurns, nil, len(existingTurns), cwd, base, projection.leafUUID, lastClaudeUserText(plannedTurns),
	), true, nil
}

// appendClaudeSessionIfSnapshotCurrent performs the only mutation Aplexica is
// allowed to make to a Claude-visible transcript: one O_APPEND write whose
// complete bytes, inode, and length still match the snapshot used to construct
// the parentUuid suffix. Both native-origin and generated sessions pass their
// stricter identity/prefix proofs before reaching this commit primitive.
func appendClaudeSessionIfSnapshotCurrent(path string, snapshot claudeSessionSnapshot, appendix string) (written, changed bool, err error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_APPEND, 0)
	if os.IsNotExist(err) {
		return false, true, nil
	}
	if err != nil {
		return false, false, err
	}
	defer func() {
		if closeErr := f.Close(); err == nil && closeErr != nil {
			err = closeErr
		}
	}()

	openInfo, err := f.Stat()
	if err != nil {
		return false, false, err
	}
	pathInfo, err := os.Stat(path)
	if os.IsNotExist(err) {
		return false, true, nil
	}
	if err != nil {
		return false, false, err
	}
	if !os.SameFile(snapshot.info, openInfo) || !os.SameFile(openInfo, pathInfo) ||
		openInfo.Size() != pathInfo.Size() || openInfo.Size() != int64(len(snapshot.raw)) {
		return false, true, nil
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return false, false, err
	}
	current, err := io.ReadAll(f)
	if err != nil {
		return false, false, err
	}
	verifiedInfo, err := f.Stat()
	if err != nil {
		return false, false, err
	}
	pathInfo, err = os.Stat(path)
	if os.IsNotExist(err) {
		return false, true, nil
	}
	if err != nil {
		return false, false, err
	}
	if !os.SameFile(openInfo, verifiedInfo) || !os.SameFile(verifiedInfo, pathInfo) ||
		verifiedInfo.Size() != int64(len(current)) || !bytes.Equal(current, snapshot.raw) {
		return false, true, nil
	}
	finalInfo, err := f.Stat()
	if err != nil {
		return false, false, err
	}
	pathInfo, err = os.Stat(path)
	if os.IsNotExist(err) {
		return false, true, nil
	}
	if err != nil {
		return false, false, err
	}
	if !os.SameFile(verifiedInfo, finalInfo) || !os.SameFile(finalInfo, pathInfo) ||
		finalInfo.Size() != pathInfo.Size() || finalInfo.Size() != int64(len(current)) {
		return false, true, nil
	}
	n, err := f.WriteString(appendix)
	if err != nil {
		return false, false, err
	}
	if n != len(appendix) {
		return false, false, io.ErrShortWrite
	}
	if err = f.Sync(); err != nil {
		return false, false, err
	}
	return true, false, nil
}

// claudeSessionMatches accepts only an authenticated Aplexica synthetic
// session whose Claude-visible leaf is exactly the requested canonical head.
// Claude-authored continuations are allowed only while the immutable generated
// prefix still satisfies claudeSyntheticBaseMatches.
// Physical JSONL order alone is insufficient: Claude reconstructs /resume by
// walking parentUuid from last-prompt.leafUuid, so a sibling branch can be
// present on disk yet invisible in the resumed conversation.
func claudeSessionMatches(raw []byte, plannedTurns []acf.TextTurn, sessionID, threadID, branchID string) (bool, error) {
	ref, ok := claudeSessionThreadRef(raw)
	if !ok || ref.ArtifactID != threadID ||
		normalizeClaudeBranchID(ref.BranchID) != normalizeClaudeBranchID(branchID) ||
		claudeSessionThreadID(raw) != sessionID {
		return false, nil
	}
	projection, err := parseClaudeVisibleLeaf(raw)
	if err != nil {
		// A malformed or branched parent graph is an incompatible existing
		// pathname, not permission to mutate it. The caller defers while its
		// watcher imports the native state.
		return false, nil
	}
	// EXACTNESS, not forkedness: this is both the accept-as-current test and the
	// post-write read-back verification, so turn equality alone is insufficient —
	// the spanning clause is the only thing excluding an off-chain sibling
	// branch. Accepting a file that holds an unreachable conversational row as an
	// exact match would then let the caller append onto it.
	if !projection.spans() ||
		!acf.TextTurnsEqual(projection.turns, plannedTurns) {
		return false, nil
	}
	if !claudeSyntheticBaseMatches(ref, plannedTurns) {
		return false, nil
	}
	return true, nil
}

// claudeSyntheticBaseMatches authenticates the immutable Aplexica-generated
// prefix of a synthetic session before the adapter accepts or extends rows
// Claude appended to that session. Current snapshots carry both an exact turn
// count and hash. Pre-hash legacy snapshots remain reusable only while every
// row is still Aplexica-stamped; once Claude has continued one, the missing
// base hash fails closed and materialization defers.
//
// This proof is intentionally used only by writeClaudeConversationSession,
// whose caller has already excluded original native sources and selected a
// deterministic synthetic path.
func claudeSyntheticBaseMatches(ref adapter.ThreadRef, visibleTurns []acf.TextTurn) bool {
	count := ref.MaterializedTurnCount
	if count <= 0 || count > len(visibleTurns) {
		return false
	}
	if ref.MaterializedTurnsHash == "" {
		return ref.GeneratedSnapshot && count == len(visibleTurns)
	}
	return ref.MaterializedTurnsHash == adapter.ConversationTurnsHash(visibleTurns[:count])
}

// claudeVisibleLeafTextTurns mirrors Claude Code's resume reconstruction: it
// chooses the leaf recorded by the last last-prompt row (falling back to the
// last conversational row for legacy sessions), then walks parentUuid to the
// root. A malformed graph fails closed.
func claudeVisibleLeafTextTurns(raw []byte) ([]acf.TextTurn, error) {
	projection, err := parseClaudeVisibleLeaf(raw)
	return projection.turns, err
}

// ResumableTextTurns returns the user-visible turn chain Claude Code will
// reconstruct through /resume. It follows parentUuid from the selected leaf
// instead of trusting physical JSONL order, so diagnostics reject transcripts
// whose expected rows exist on disk but are unreachable in Claude Code.
func ResumableTextTurns(raw []byte) ([]acf.TextTurn, error) {
	projection, err := parseClaudeVisibleLeaf(raw)
	if err != nil {
		return nil, err
	}
	if !projection.spans() {
		// The message already describes a spanning failure and never claimed a
		// fork, so it stays byte-identical. The typed fault is what lets the
		// acceptance probe tell an unreachable row apart from a file it could not
		// parse at all.
		fault := claudeGraphFaultChainUnspanned
		if projection.forked {
			fault = claudeGraphFaultForked
		}
		return nil, claudeGraphErrorf(fault, "claude resume graph contains non-visible conversational nodes")
	}
	return append([]acf.TextTurn(nil), projection.turns...), nil
}

// claudeGraphFault names WHICH structural fault rejected a session graph. The
// four hard-parse faults and the two "the walk did not span the file" outcomes
// are separate populations with separate remedies, and the shipped code told
// them apart only by comparing error prose.
type claudeGraphFault string

const (
	claudeGraphFaultRowDecode      claudeGraphFault = "row_decode"
	claudeGraphFaultMissingUUID    claudeGraphFault = "conversational_row_without_uuid"
	claudeGraphFaultDuplicateUUID  claudeGraphFault = "duplicate_uuid"
	claudeGraphFaultMissingParent  claudeGraphFault = "missing_parent"
	claudeGraphFaultCycle          claudeGraphFault = "cycle"
	claudeGraphFaultMultiTurnRow   claudeGraphFault = "row_encodes_multiple_turns"
	claudeGraphFaultForked         claudeGraphFault = "forked"
	claudeGraphFaultChainUnspanned claudeGraphFault = "chain_unspanned"
)

// ClaudeGraphError carries a claudeGraphFault beside the message the walk has
// always produced. The message text is deliberately unchanged so nothing that
// reads or logs it can regress; the fault is additive, for callers that need to
// route on the class rather than match prose.
//
// msg carries uuids only. Nothing a user wrote can reach it.
type ClaudeGraphError struct {
	Fault claudeGraphFault
	msg   string
	cause error
}

func (e *ClaudeGraphError) Error() string { return e.msg }

// Unwrap keeps the decoder's own error matchable, which the %w-wrapped
// predecessor of this type allowed. It is nil for the faults this package
// diagnoses itself.
func (e *ClaudeGraphError) Unwrap() error { return e.cause }

func claudeGraphErrorf(fault claudeGraphFault, format string, args ...any) *ClaudeGraphError {
	return &ClaudeGraphError{Fault: fault, msg: fmt.Sprintf(format, args...)}
}

func claudeGraphRowDecodeError(cause error, format string, args ...any) *ClaudeGraphError {
	err := claudeGraphErrorf(claudeGraphFaultRowDecode, format, args...)
	err.cause = cause
	return err
}

// The original diagnosis blamed attachment or system parents for stopping the
// walk, but the shipped walk already registered every uuid-bearing row as a
// bridge. Import is also independent: it runs EncodeCanonical over every row in
// physical order and never calls this function.
//
// What was actually broken, and what the descent below fixes, is a STALE
// LAST-PROMPT LEAF: Claude Code writes that row when a prompt is submitted and
// appends the answer after it, so the recorded leaf is routinely a strict
// ancestor of the real tip and the up-walk drops every turn written since. The
// corrected walk descends from that stale marker to the actual tip.
//
// claudeLeafProjection is Claude Code's own resume reconstruction of one session
// file: the visible turn chain, the tip those turns hang from, and the two
// whole-file measurements every write-path gate reads.
type claudeLeafProjection struct {
	turns     []acf.TextTurn
	leafUUID  string
	nodeCount int

	// forked is a DIRECT measurement, not an inference from nodeCount: some node
	// — or the virtual root — has more than one child subtree containing a
	// turn-bearing row, resolved THROUGH the attachment/system bridge rows
	// Claude Code threads its parentUuid chain with. It is the only thing
	// SessionDeclineForkedMirror may be reported from. A spanning projection is
	// provably never forked.
	forked bool

	// leafAdvanced records that the last-prompt leafUuid named a strict ancestor
	// of the deepest turn-bearing row and the walk descended to it. Diagnostics
	// only; no predicate reads it.
	leafAdvanced bool
}

// spans reports that the walk reached every turn-bearing row in the file. It is
// the APPEND-SAFETY question, and it is what the write path has always
// evaluated: a turn-bearing row off the walked chain makes both the suffix slice
// and the suffix's parent uuid wrong.
func (p claudeLeafProjection) spans() bool { return p.nodeCount == len(p.turns) }

// declineReason separates a measured fork from every other non-spanning shape.
// Meaningful only when !spans().
func (p claudeLeafProjection) declineReason() adapter.SessionDeclineReason {
	if p.forked {
		return adapter.SessionDeclineForkedMirror
	}
	return adapter.SessionDeclineChainUnspanned
}

// claudeGraphNode is one uuid-bearing row's contribution to the resume graph:
// its edge, and the turn it carries if it carries one.
type claudeGraphNode struct {
	parentUUID string
	synthetic  bool
	turn       *acf.TextTurn
}

func parseClaudeVisibleLeaf(raw []byte) (claudeLeafProjection, error) {
	nodes := make(map[string]claudeGraphNode)
	// order is registration order. The graph passes below iterate it rather than
	// the map so their results are deterministic run to run — a structural
	// decline that flipped between passes would make the retry class a lie.
	var order []string
	portableNodeCount := 0
	leafUUID := ""
	fallbackLeaf := ""
	for _, line := range bytes.Split(raw, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
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
		if err := json.Unmarshal(line, &row); err != nil {
			return claudeLeafProjection{}, claudeGraphRowDecodeError(err, "decode Claude session row: %s", err)
		}
		if row.Type == "last-prompt" && row.LeafUUID != "" {
			leafUUID = row.LeafUUID
			continue
		}
		if row.UUID == "" {
			// A row with no uuid cannot be anyone's parent, so it contributes no
			// edge. The supported uuid-bearing row types are user, assistant,
			// attachment and system.
			if row.Type == "user" || row.Type == "assistant" {
				return claudeLeafProjection{}, claudeGraphErrorf(
					claudeGraphFaultMissingUUID, "Claude conversational row has no uuid")
			}
			continue
		}
		if _, duplicate := nodes[row.UUID]; duplicate {
			return claudeLeafProjection{}, claudeGraphErrorf(
				claudeGraphFaultDuplicateUUID, "duplicate Claude graph uuid %q", row.UUID)
		}
		parentUUID := ""
		if row.ParentUUID != nil {
			parentUUID = *row.ParentUUID
		}
		if row.Type != "user" && row.Type != "assistant" || row.IsSidechain {
			// Claude Code inserts attachment and bookkeeping rows directly in
			// the parentUuid chain. Preserve their edges as non-visible bridges;
			// their bodies must never become conversational text.
			//
			// A sidechain row is a Task/sub-agent transcript. It is conversational
			// in shape but lives on its own root, so the main resume walk can
			// never reach it — counting it would make `nodeCount != len(turns)`
			// true for a perfectly healthy file and classify it as a forked
			// mirror. That mis-classification is not cosmetic: with the Stage-B
			// repair enabled it routes a healthy mirror into a whole-file rewrite
			// that would flatten the sub-agent's prompts into the main chain.
			// Carrying no turn also keeps a sidechain out of the fork
			// measurement below, which counts turn-BEARING branches only.
			nodes[row.UUID] = claudeGraphNode{parentUUID: parentUUID}
			order = append(order, row.UUID)
			continue
		}
		synthetic := row.Type == "assistant" && row.Message != nil && row.Message.Model == "<synthetic>"
		var visibleTurn *acf.TextTurn
		if !synthetic {
			events, err := EncodeCanonical(append(append([]byte(nil), line...), '\n'))
			if err != nil {
				// Unreachable today (EncodeCanonical's error is unconditionally
				// nil), but typed anyway so no walk fault can escape untyped.
				// "%s" plus the retained cause keeps both the message and the
				// unwrap chain byte-identical to the bare return.
				return claudeLeafProjection{}, claudeGraphRowDecodeError(err, "%s", err)
			}
			turns := acf.ExtractTextTurns(events)
			switch len(turns) {
			case 0:
				// Current Claude Code can split one assistant response into an
				// empty-thinking row followed by its visible text row. The first
				// row remains part of parentUuid traversal, but is not a visible
				// conversation turn and must never surface as answer text.
			case 1:
				turn := turns[0]
				visibleTurn = &turn
				portableNodeCount++
			default:
				return claudeLeafProjection{}, claudeGraphErrorf(
					claudeGraphFaultMultiTurnRow, "Claude node %q encodes multiple text turns", row.UUID)
			}
		}
		nodes[row.UUID] = claudeGraphNode{
			parentUUID: parentUUID, synthetic: synthetic, turn: visibleTurn,
		}
		order = append(order, row.UUID)
		fallbackLeaf = row.UUID
	}
	if leafUUID == "" {
		leafUUID = fallbackLeaf
	}
	if leafUUID == "" {
		return claudeLeafProjection{}, nil
	}

	children := claudeGraphChildren(nodes, order)
	bearing := claudeBearingBelow(nodes, order)
	forked := claudeGraphForked(children, bearing)
	leafAdvanced := false
	if _, known := nodes[leafUUID]; known {
		// Claude Code writes the last-prompt row when the prompt is SUBMITTED and
		// appends the answer after it, so the recorded leaf is routinely a strict
		// ancestor of the deepest turn-bearing row. Walking up from there drops
		// every turn written since — and, because the native append parents its
		// canonical suffix at this leaf, the next write would hang that suffix
		// off a node that is no longer the tip and manufacture a real fork.
		//
		// Descend while exactly one child subtree bears a turn; stop at the first
		// node with two, because choosing between them is what a fork means. An
		// unknown leaf is left alone so the up-walk still reports it missing.
		tip, descendForked := claudeDescendToTip(leafUUID, children, bearing)
		forked = forked || descendForked
		if tip != leafUUID {
			leafUUID = tip
			leafAdvanced = true
		}
	}

	selectedPortableLeaf := ""
	reversed := make([]acf.TextTurn, 0, len(nodes))
	seen := make(map[string]bool, len(nodes))
	for leafUUID != "" {
		if seen[leafUUID] {
			return claudeLeafProjection{}, claudeGraphErrorf(
				claudeGraphFaultCycle, "cycle in Claude parentUuid graph at %q", leafUUID)
		}
		seen[leafUUID] = true
		current, ok := nodes[leafUUID]
		if !ok {
			return claudeLeafProjection{}, claudeGraphErrorf(
				claudeGraphFaultMissingParent, "missing Claude parentUuid node %q", leafUUID)
		}
		if current.synthetic || current.turn == nil {
			// Claude Desktop appends this reserved local bookkeeping reply when
			// it indexes a prompt-only imported session. EncodeCanonical already
			// excludes it. Current Claude Code also emits non-text assistant rows
			// for split thinking. Treat either UUID as a parent bridge, so a
			// last-prompt row still resolves to the visible user/assistant chain.
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
		forked: forked, leafAdvanced: leafAdvanced,
	}, nil
}

// claudeGraphChildren inverts the parent edges. A node whose parentUuid names a
// uuid the file does not contain is deliberately bucketed under that missing
// uuid rather than under the virtual root: it is an orphaned subtree, and
// counting it as a second root would report a fork where there is only a broken
// reference.
func claudeGraphChildren(nodes map[string]claudeGraphNode, order []string) map[string][]string {
	children := make(map[string][]string, len(nodes))
	for _, uuid := range order {
		parent := nodes[uuid].parentUUID
		children[parent] = append(children[parent], uuid)
	}
	return children
}

// claudeBearingBelow marks every node whose subtree — itself included — holds a
// turn-bearing row.
//
// Each turn-bearing row is propagated upward ONCE and the climb short-circuits
// on an already-marked ancestor, so the whole pass is O(n) even on the
// large transcripts; the naive recursive form is O(n²) and
// this runs on the deferral drain's hot path. Marking before stepping is also
// what terminates it on a cycle: the second visit to any node in the cycle finds
// it marked and stops. The up-walk still rejects that graph.
func claudeBearingBelow(nodes map[string]claudeGraphNode, order []string) map[string]bool {
	bearing := make(map[string]bool, len(nodes))
	for _, uuid := range order {
		if nodes[uuid].turn == nil {
			continue
		}
		for current := uuid; current != ""; {
			if bearing[current] {
				break
			}
			bearing[current] = true
			node, ok := nodes[current]
			if !ok {
				break
			}
			current = node.parentUUID
		}
	}
	return bearing
}

// claudeGraphForked is the whole-file fork measurement: some NODE has more than
// one child subtree holding a turn-bearing row. It is independent of which leaf
// the walk started from, so a stale last-prompt row can neither create nor hide
// one.
//
// The virtual root is deliberately excluded. A root-parented row has
// `parentUuid: null`, which means "no parent", not "child of a shared node", so
// two turn-bearing roots are two threads in one file rather than one thread that
// branched. That shape cannot be spanned either, but it is not a fork and must
// not be offered the fork's rebuild remedy; it reports chain_unspanned.
func claudeGraphForked(children map[string][]string, bearing map[string]bool) bool {
	for parent, kids := range children {
		if parent == "" {
			continue
		}
		bearingKids := 0
		for _, child := range kids {
			if !bearing[child] {
				continue
			}
			bearingKids++
			if bearingKids > 1 {
				return true
			}
		}
	}
	return false
}

// claudeDescendToTip walks DOWN from the recorded leaf while exactly one child
// subtree bears a turn, and stops — reporting forked — at the first node with
// two.
//
// It cannot change a file whose walk already spans: spanning means every
// turn-bearing row is on the leaf→root chain, so no descendant of the leaf can
// bear one and the loop exits on its first test. A downward cycle stops the
// descent where it started re-treading; the up-walk rejects that graph.
func claudeDescendToTip(leaf string, children map[string][]string, bearing map[string]bool) (string, bool) {
	seen := make(map[string]bool, len(children))
	for !seen[leaf] {
		seen[leaf] = true
		next := ""
		bearingKids := 0
		for _, child := range children[leaf] {
			if !bearing[child] {
				continue
			}
			bearingKids++
			next = child
		}
		switch {
		case bearingKids == 0:
			return leaf, false
		case bearingKids > 1:
			return leaf, true
		}
		leaf = next
	}
	return leaf, false
}

// baseTurns is the COMPLETE materialized base the appended rows belong to, not
// just the appended suffix. A generated session's base marker has to describe
// every materialized turn in the file: ingest reads the stamp to size the
// trustworthy stale base, and MergeConversationByThreadRef's turns-hash loop
// break only fires when the stamp matches what the file actually contains.
// Stamping just the thread/branch here left the count frozen at whatever the
// first full transcode wrote, so an extended session reported a short base and
// its next continuation was unioned by timestamp into duplicate turns.
// It is ignored unless stampCanonicalThread is set, because the native-origin
// append writes a canonical suffix into the user's own transcript and must not
// mark that file as Aplexica-generated.
//
// uuids may be nil (the deterministic per-index uuid every append has always
// used) or exactly len(turns) long, which is how the native rebuild keeps the
// uuid a contained row already carried: Claude Code appends a child of its
// IN-MEMORY leaf, so regenerating uuids would strand its next append on a
// parent the file no longer holds.
func transcodeClaudeTurnAppend(
	turns []acf.TextTurn,
	sessionID, threadID, branchID string,
	stampCanonicalThread bool,
	baseTurns []acf.TextTurn,
	uuids []string,
	startIndex int,
	cwd string,
	base time.Time,
	parentUUID, lastPromptText string,
) string {
	if len(turns) == 0 {
		return ""
	}
	if len(uuids) != len(turns) {
		uuids = nil
	}
	branchID = normalizeClaudeBranchID(branchID)
	type obj = map[string]any
	baseHash := adapter.ConversationTurnsHash(baseTurns)
	stamp := func(row obj) obj {
		if stampCanonicalThread {
			row["aplexicaThreadId"] = threadID
			row["aplexicaBranchId"] = branchID
			if len(baseTurns) > 0 {
				row["aplexicaTurnsHash"] = baseHash
				row["aplexicaTurnCount"] = len(baseTurns)
			}
		}
		return row
	}
	lines := make([]string, 0, len(turns)+1)
	for offset, turn := range turns {
		index := startIndex + offset
		uuid := deterministicUUID(sessionID, index)
		if uuids != nil && uuids[offset] != "" {
			uuid = uuids[offset]
		}
		row := claudeTurnRow(turn, uuid, parentUUID, sessionID, cwd, base, index)
		encoded, _ := json.Marshal(stamp(row))
		lines = append(lines, string(encoded))
		parentUUID = uuid
	}
	lastPrompt, _ := json.Marshal(stamp(obj{
		"type": "last-prompt", "lastPrompt": oneLine(lastPromptText), "leafUuid": parentUUID, "sessionId": sessionID,
	}))
	lines = append(lines, string(lastPrompt))
	return strings.Join(lines, "\n") + "\n"
}

func lastClaudeUserText(turns []acf.TextTurn) string {
	for i := len(turns) - 1; i >= 0; i-- {
		if turns[i].Role == "user" {
			return turns[i].Text
		}
	}
	return ""
}

// ConversationSessionPath lets the orchestrator recursion-guard Claude's
// deterministic session path before MaterializeConversationSession writes it.
func (a *Adapter) ConversationSessionPath(art acf.Artifact, head acf.Event, sourceAgent string) (string, bool, error) {
	path, ok, _, err := a.ConversationSessionPathReason(art, head, sourceAgent)
	return path, ok, err
}

// ConversationSessionPathReason (adapter.ConversationSessionPathDeclineReporter)
// is ConversationSessionPath plus the typed reason. The planner declines
// before any write is attempted, so without this the orchestrator would learn
// nothing about a native-origin session that diverged.
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
	if plan.nativeOrigin && (!plan.nativeSource || !plan.nativeWritable) {
		if plan.nativeSource && nativeRepairableReason(plan.declineReason) &&
			a.nativeRepairPolicy(plan, art.ArtifactID).repairDivergedNative {
			// The orchestrator STOPS at an unsupported plan — it never calls the
			// materializer — so a planner that closes this door makes the write
			// path's repair unreachable however correct that repair is. This is
			// the same structural blocker the native-origin exclusion in
			// mirrorRepairPolicy has: a gate that is technically true and
			// practically the end of the road.
			//
			// The planner's question is "may a write to this path be attempted
			// this pass", not "will it succeed". With the repair authorized the
			// honest answer is yes, and the writer still owns the final word: if
			// the containment proof refuses, the materializer declines with this
			// very reason and the orchestrator withdraws its unwritten guard.
			return plan.dest, true, adapter.SessionDeclineUnspecified, nil
		}
		return plan.dest, false, plan.declineReason, nil
	}
	return plan.dest, true, adapter.SessionDeclineUnspecified, nil
}

// InspectConversationSessionSource (adapter.ConversationSessionSourceInspector)
// reports, read-only, whether the artifact's own native transcript currently
// presents the canonical head on Claude Code's resume walk.
//
// It is deliberately NOT ConversationSessionPathReason. The planner compares
// turns in FILE ORDER, so the origin-session fork — Aplexica appends the
// foreign turns, the still-open Claude Code appends the user's next prompt as
// a child of its stale in-memory leaf, canonical then absorbs that prompt —
// reaches the planner as a perfectly writable EXACT plan even though the
// resume walk shows only the fork's own branch. And with RepairForkedMirrors
// on, the planner intentionally reports a repairable divergence as
// supports=true so the write path can reach the rebuild — which folds "needs a
// repair" and "already healthy" into one answer. This inspector keeps the two
// apart: an exact, spanning file is reusable (the trigger's loop-termination
// proof), a forked or diverged one is not, whatever the repair flag says.
//
// Every gate mirrors the write path's own vocabulary so the trigger, the
// drain's decline classification, and the escalation rows can never disagree
// about what a state means. It writes nothing and memoizes nothing: it runs
// once per changed origin file, on bytes the import has just paid to read.
func (a *Adapter) InspectConversationSessionSource(
	art acf.Artifact, head acf.Event,
) (bool, bool, adapter.SessionDeclineReason, error) {
	plan, ok, err := a.conversationSessionPlan(art, head)
	if err != nil {
		return false, false, adapter.SessionDeclineUnspecified, err
	}
	if !ok || !plan.nativeOrigin {
		return false, false, adapter.SessionDeclineUnspecified, nil
	}
	if !plan.nativeSource || !plan.nativeWritable {
		// The plan itself already classified this: race, graph_malformed,
		// native_ahead, or a file-order divergence.
		return false, true, plan.declineReason, nil
	}
	// File-order agreement is not reuse. Re-prove it from the bytes on disk
	// with the same identity gates the native writer applies, then consult the
	// resume walk the planner never runs.
	//
	// Bounded first: the walk below is a whole-file read on the live import
	// path, and the rebuild this inspection feeds refuses a file past
	// nativeRepairMaxBytes anyway (a quarter-gigabyte rewrite is not a
	// materialization-pass action, whatever the proof says). Above the bound
	// the honest answer is "cannot be judged", not a verdict either way — a
	// verdict could only drive an unrepairable decline loop that re-reads the
	// same oversized bytes on every paced pass.
	if info, statErr := os.Stat(plan.dest); statErr != nil {
		return false, true, adapter.SessionDeclineRace, nil
	} else if info.Size() > nativeRepairMaxBytes {
		return false, false, adapter.SessionDeclineUnspecified, nil
	}
	snapshot, exists, changed, err := readClaudeSessionSnapshot(plan.dest)
	if err != nil {
		return false, true, adapter.SessionDeclineUnspecified, err
	}
	if !exists || changed || len(snapshot.raw) == 0 {
		return false, true, adapter.SessionDeclineRace, nil
	}
	state, resume := encodeCanonicalInto(snapshot.raw, 0, claudeCanonicalState{})
	if len(bytes.TrimSpace(snapshot.raw[resume:])) != 0 {
		// An unconsumed tail is a row being written right now.
		return false, true, adapter.SessionDeclineRace, nil
	}
	if state.hasExplicitThreadStamp || state.sessionID != plan.sessionID {
		return false, true, adapter.SessionDeclineGraphMalformed, nil
	}
	projection, projectionErr := parseClaudeVisibleLeaf(snapshot.raw)
	if projectionErr != nil {
		return false, true, adapter.SessionDeclineGraphMalformed, nil
	}
	if !projection.spans() {
		// Conversational rows exist that the walk cannot reach: the fork (or
		// its multi-root sibling). This is the state the trigger exists for.
		return false, true, projection.declineReason(), nil
	}
	switch {
	case acf.TextTurnsEqual(projection.turns, plan.turns),
		claudeTextTurnsPrefix(projection.turns, plan.turns):
		// Exact — or a spanning prefix, which the ordinary prefix-gated append
		// converges without any repair. Both are honest reuse.
		return true, true, adapter.SessionDeclineUnspecified, nil
	case claudeTextTurnsPrefix(plan.turns, projection.turns):
		// Ahead: the file's own pending import is the only authority allowed
		// to move canonical, so there is nothing to queue toward the file.
		return false, true, adapter.SessionDeclineNativeAhead, nil
	default:
		return false, true, adapter.SessionDeclineDiverged, nil
	}
}

type claudeConversationSessionPlan struct {
	turns []acf.TextTurn
	base  time.Time
	cwd   string
	dest  string
	// sessionID is what Claude Code sees inside each row. It is the canonical
	// artifact id for main, or a deterministic branch session id for forks.
	sessionID string
	branchID  string

	// nativeOrigin means the artifact points at this device's original Claude
	// transcript. nativeSource additionally proves its session identity, and
	// nativeWritable proves its visible turns are equal to or a prefix of the
	// canonical plan.
	//
	// A canonical suffix is appended to that same parentUuid chain so Claude's
	// resume picker continues to expose exactly one conversation.
	nativeOrigin       bool
	nativeSource       bool
	nativeWritable     bool
	syntheticDest      string
	syntheticSessionID string

	// declineReason explains a nativeOrigin decline — that is, the state
	// (!nativeSource || !nativeWritable) — so the path planner and the writer
	// report the same cause without re-deriving it at each call site. Empty
	// whenever the plan is writable.
	declineReason adapter.SessionDeclineReason
}

type claudeNativeSessionRelation uint8

const (
	claudeNativeSessionInvalid claudeNativeSessionRelation = iota
	claudeNativeSessionExact
	claudeNativeSessionAppendable
	// claudeNativeSessionAheadOrDiverged now means ONLY "ahead": the native
	// session already contains the whole canonical plan and continues past it,
	// which is transient and must never be terminaled. Genuine divergence moved
	// to claudeNativeSessionDiverged below so the two can be routed to
	// different retry classes; collapsing them is what made a permanently
	// diverged conversation retry forever with a false explanation.
	claudeNativeSessionAheadOrDiverged
	// claudeNativeSessionDiverged: neither the native visible turns nor the
	// canonical plan is a prefix of the other, so each holds at least one turn
	// the other lacks and no append can converge them.
	claudeNativeSessionDiverged
)

func (a *Adapter) conversationSessionPlan(art acf.Artifact, head acf.Event) (claudeConversationSessionPlan, bool, error) {
	if a.HomeDir == "" {
		return claudeConversationSessionPlan{}, false, nil
	}
	p, err := acf.DecodeConversationPayload(head)
	if err != nil {
		return claudeConversationSessionPlan{}, false, err
	}
	// Hermes-native conversations carry a SessionBundle payload rather
	// than canonical events; without this branch they silently never
	// appeared in /resume (codex and kilo threads showed, hermes didn't).
	var turns []acf.TextTurn
	switch p.Format {
	case acf.ConversationFormatV1:
		turns = acf.ExtractTextTurns(p.Events)
	case acf.ConversationFormatHermesBundle:
		turns = acf.TurnsFromHermesBundleJSON(p.Content)
	default:
		return claudeConversationSessionPlan{}, false, nil
	}
	if len(turns) == 0 {
		return claudeConversationSessionPlan{}, false, nil
	}
	// Stamp with the conversation's LAST-ACTIVITY time, not its creation
	// time: /resume orders by recency, and a creation-time stamp buried an
	// actively-continuing conversation among re-materialized test sessions.
	// UpdatedAt bumps on every imported turn, so a
	// live conversation keeps floating to the top. Mirrors the openclaw
	// session materializer.
	base := art.UpdatedAt.UTC()
	if base.IsZero() {
		base = art.CreatedAt.UTC()
	}
	if base.IsZero() {
		base = head.Timestamp.UTC()
	}
	cwd := a.HomeDir
	projDir := filepath.Join(a.HomeDir, ".claude", "projects", encodeProjectDir(cwd))
	branchID := normalizeClaudeBranchID(head.Branch)
	sessionID := claudeSyncedSessionID(art.ArtifactID, branchID)
	dest := filepath.Join(projDir, sessionID+".jsonl")
	plan := claudeConversationSessionPlan{
		turns: turns, base: base, cwd: cwd, dest: dest, sessionID: sessionID, branchID: branchID,
		syntheticDest: dest, syntheticSessionID: sessionID,
	}
	// SourcePath is device-local identity. Remote artifacts may coincidentally
	// carry the same absolute path (for example two Macs with the same account
	// name), so only a native-origin artifact may claim it. Forks always remain
	// separate sessions. Finally, require the existing visible turns to equal the
	// canonical turns before returning the original file as an already-complete
	// destination. Any delta goes to the deterministic synthetic session.
	if branchID == acf.MainBranch && art.RemoteOriginDeviceID == "" {
		if source, ok := a.localConversationSourcePath(art.SourcePath); ok {
			plan.nativeOrigin = true
			plan.dest = source
			nativeID, nativeCWD, relation, decline := a.claudeNativeSourceSessionPlan(source, turns)
			if nativeCWD != "" {
				plan.cwd = nativeCWD
			}
			plan.declineReason = decline
			if nativeID != "" {
				plan.sessionID = nativeID
				plan.nativeSource = true
				plan.nativeWritable = relation == claudeNativeSessionExact ||
					relation == claudeNativeSessionAppendable
			}
		}
	}
	return plan, true, nil
}

// claudeNativeSourceSessionPlan proves that path still names the original
// native Claude session and that selecting it cannot reorder canonical turns.
// It deliberately fails closed to the synthetic path on any read/parse error.
// decline is the typed reason for every non-writable outcome and is empty for
// the two writable relations.
func (a *Adapter) claudeNativeSourceSessionPlan(
	path string,
	plannedTurns []acf.TextTurn,
) (sessionID, cwd string, relation claudeNativeSessionRelation, decline adapter.SessionDeclineReason) {
	snapshot, err := a.conversationCache().inspectFile(path)
	if err != nil {
		// An unreadable transcript is indistinguishable from one a native
		// writer is holding open right now, so pace it as a race rather than
		// declaring a structural fault we cannot actually observe.
		return "", "", claudeNativeSessionInvalid, adapter.SessionDeclineRace
	}
	if !snapshot.RowsComplete {
		// A trailing row that would not parse is an append in flight.
		return "", "", claudeNativeSessionInvalid, adapter.SessionDeclineRace
	}
	if snapshot.HasExplicitThreadStamp {
		// The artifact records this path as its pristine native source, yet the
		// file carries an Aplexica thread stamp. The two identities contradict
		// and no retry reconciles them.
		return "", "", claudeNativeSessionInvalid, adapter.SessionDeclineGraphMalformed
	}
	sessionID = snapshot.SessionID
	if sessionID == "" || strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)) != sessionID {
		return "", "", claudeNativeSessionInvalid, adapter.SessionDeclineGraphMalformed
	}
	existingTurns := acf.ExtractTextTurns(snapshot.Events)
	switch {
	case acf.TextTurnsEqual(existingTurns, plannedTurns):
		relation = claudeNativeSessionExact
	case claudeTextTurnsPrefix(existingTurns, plannedTurns):
		relation = claudeNativeSessionAppendable
	case claudeTextTurnsPrefix(plannedTurns, existingTurns):
		// The native session holds every planned turn and continues past it:
		// genuinely ahead, and its pending import is what moves canonical.
		relation = claudeNativeSessionAheadOrDiverged
		decline = adapter.SessionDeclineNativeAhead
	default:
		// Neither is a prefix of the other, so each side holds a turn the other
		// lacks and appending can never repair that.
		relation = claudeNativeSessionDiverged
		decline = adapter.SessionDeclineDiverged
	}
	return sessionID, snapshot.LastCWD, relation, decline
}

func claudeTextTurnsPrefix(prefix, full []acf.TextTurn) bool {
	if len(prefix) > len(full) {
		return false
	}
	return acf.TextTurnsEqual(prefix, full[:len(prefix)])
}

// claudeTextTurnsSubsequence reports whether every turn of sub appears in full,
// in order. It is the "nothing would be lost" proof for rebuilding a diverged
// mirror: canonical may have INSERTED turns the mirror missed, but it must not
// have dropped any the mirror still holds.
func claudeTextTurnsSubsequence(sub, full []acf.TextTurn) bool {
	if len(sub) > len(full) {
		return false
	}
	next := 0
	for _, turn := range full {
		if next < len(sub) && sub[next].Role == turn.Role && sub[next].Text == turn.Text {
			next++
		}
	}
	return next == len(sub)
}

// rebuildDivergedClaudeMirror replaces an Aplexica-owned mirror that can never
// be repaired by appending, and only when the replacement provably loses
// nothing. It re-verifies the file is unchanged since snapshot, so a
// continuation written between the read and the write is not clobbered — that
// attempt simply reports rebuilt=false and the caller defers as before.
//
// Two disjoint populations reach it. A mirror whose walk SPANS it is merely out
// of order, and its shipped subsequence proof and rename-based write are
// unchanged. A mirror whose walk does NOT span it — Claude Code appended its own
// child of a node Aplexica had already extended, or any other shape that strands
// a conversational row off the resume chain — used to bail here one statement
// before that proof, which is the permanent-on-disk state that made the artifact
// re-queue forever. That branch is repairable only under policy (default off),
// only behind the stricter row-level loss proof, and only through an
// inode-preserving commit.
//
// The router deliberately stays on SPANNING rather than narrowing to a
// classified fork: both populations need the identical row-level containment proof, and
// that proof — not the fork classification — is the safety argument. Narrowing
// it would strand the non-forked population at the exact bail this exists to end.
func rebuildDivergedClaudeMirror(
	path string,
	snapshot claudeSessionSnapshot,
	render claudeSessionRenderer,
	plannedTurns []acf.TextTurn,
	sessionID, threadID, branchID string,
	policy claudeMirrorRepairPolicy,
) (bool, error) {
	ref, ok := claudeSessionThreadRef(snapshot.raw)
	if !ok || ref.ArtifactID != threadID ||
		normalizeClaudeBranchID(ref.BranchID) != normalizeClaudeBranchID(branchID) ||
		claudeSessionThreadID(snapshot.raw) != sessionID {
		// Not our mirror of this thread. Never rewrite it.
		return false, nil
	}
	projection, err := parseClaudeVisibleLeaf(snapshot.raw)
	if err != nil {
		return false, nil
	}
	if !projection.spans() {
		if !policy.repairForkedMirror {
			// Unauthorized: the exact bail this function has always taken.
			return false, nil
		}
		if len(projection.turns) == 0 {
			return false, nil
		}
		// The finer-grained containment reason is deliberately dropped: a
		// repair that is enabled but refuses must be indistinguishable from a
		// repair that was never enabled, so the reason the caller reports stays
		// classifyClaudeMirrorDecline's. The tests assert the reason directly.
		contained, result, _ := claudeMirrorRowsContained(snapshot.raw, plannedTurns)
		if !contained {
			return false, nil
		}
		// Reuse the uuid every contained row already carries. Claude Code
		// appends a child of its IN-MEMORY leaf, so regenerating uuids would
		// strand its next append on a parent this file no longer holds.
		return repairForkedClaudeMirror(
			path, snapshot,
			[]byte(render(claudeRebuildUUIDs(sessionID, len(plannedTurns), result), result.bridges)),
			plannedTurns, sessionID, threadID, branchID, policy)
	}
	if len(projection.turns) == 0 || !claudeTextTurnsSubsequence(projection.turns, plannedTurns) {
		return false, nil
	}
	current, exists, changed, err := readClaudeSessionSnapshot(path)
	if err != nil || !exists || changed ||
		!os.SameFile(current.info, snapshot.info) ||
		!bytes.Equal(current.raw, snapshot.raw) {
		return false, err
	}
	if err := atomicfile.WriteFile(path, []byte(render(nil, nil)), claudeSessionFileMode); err != nil {
		return false, err
	}
	post, postExists, postChanged, err := readClaudeSessionSnapshot(path)
	if err != nil {
		return false, err
	}
	if !postExists || postChanged {
		return false, nil
	}
	return claudeSessionMatches(post.raw, plannedTurns, sessionID, threadID, branchID)
}

func (a *Adapter) localConversationSourcePath(sourcePath string) (string, bool) {
	if a.HomeDir == "" || sourcePath == "" || filepath.Ext(sourcePath) != ".jsonl" {
		return "", false
	}
	source, err := filepath.Abs(sourcePath)
	if err != nil {
		return "", false
	}
	home, err := filepath.Abs(a.HomeDir)
	if err != nil {
		return "", false
	}
	projectsRoot := filepath.Join(home, ".claude", "projects")
	rel, err := filepath.Rel(projectsRoot, source)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	// Reject every symlink component below the projects root, even when it
	// currently resolves back inside the root. This keeps subsequent
	// revalidation from depending on a mutable directory indirection.
	candidate := projectsRoot
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		candidate = filepath.Join(candidate, part)
		componentInfo, componentErr := os.Lstat(candidate)
		if componentErr != nil || componentInfo.Mode()&os.ModeSymlink != 0 {
			return "", false
		}
	}
	// Never follow a transcript symlink. A final-component symlink could point
	// at an arbitrary user file, and a directory-component symlink could escape
	// ~/.claude/projects even though the lexical path above appears contained.
	info, err := os.Lstat(source)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return "", false
	}
	resolvedRoot, err := filepath.EvalSymlinks(projectsRoot)
	if err != nil {
		return "", false
	}
	resolvedSource, err := filepath.EvalSymlinks(source)
	if err != nil {
		return "", false
	}
	resolvedRel, err := filepath.Rel(resolvedRoot, resolvedSource)
	if err != nil || resolvedRel == "." || resolvedRel == ".." || strings.HasPrefix(resolvedRel, ".."+string(filepath.Separator)) {
		return "", false
	}
	resolvedInfo, err := os.Stat(resolvedSource)
	if err != nil || !resolvedInfo.Mode().IsRegular() {
		return "", false
	}
	// Preserve the caller-visible lexical pathname (macOS commonly aliases
	// /var to /private/var); the resolved path above is validation only.
	return source, true
}

// claudeSessionThreadID returns the sessionId stamped in a Claude session file.
// For an Aplexica-materialized session that equals the canonical thread id; for
// a native Claude session it's a random uuid that won't match any artifact (so
// the merge falls through to a normal native import).
func claudeSessionThreadRef(raw []byte) (adapter.ThreadRef, bool) {
	var ref adapter.ThreadRef
	allStamped := true
	found := false
	hasExplicitTurnCount := false
	bestExplicitCount := 0
	bestExplicitHash := ""
	stampedVisible := 0
	for _, line := range bytes.Split(raw, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var m struct {
			AplexicaThreadID  string `json:"aplexicaThreadId"`
			AplexicaBranchID  string `json:"aplexicaBranchId"`
			AplexicaTurnsHash string `json:"aplexicaTurnsHash"`
			AplexicaTurnCount int    `json:"aplexicaTurnCount"`
			Type              string `json:"type"`
		}
		if json.Unmarshal(line, &m) != nil {
			allStamped = false
			continue
		}
		if m.AplexicaThreadID == "" {
			// Claude Desktop may append display/mode metadata while merely
			// importing an Aplexica CLI transcript. These rows are not a user
			// continuation and are safe to regenerate. An unstamped custom-title
			// is deliberately excluded because it may be a manual user rename.
			if m.Type == "ai-title" || m.Type == "mode" {
				continue
			}
			allStamped = false
			continue
		}
		branchID := normalizeClaudeBranchID(m.AplexicaBranchID)
		if !found {
			ref = adapter.ThreadRef{ArtifactID: m.AplexicaThreadID, BranchID: branchID}
			found = true
		}
		if ref.ArtifactID != m.AplexicaThreadID || ref.BranchID != branchID {
			allStamped = false
		}
		// An incrementally extended session carries several generations of
		// (count, hash): each append re-stamps the base it extended. The largest
		// count is the most recent and complete description, and the smaller
		// earlier stamps are superseded rather than a conflict — treating them as
		// a conflict would drop GeneratedSnapshot for every extended session.
		if m.AplexicaTurnCount > bestExplicitCount {
			bestExplicitCount = m.AplexicaTurnCount
			bestExplicitHash = m.AplexicaTurnsHash
			hasExplicitTurnCount = true
		} else if m.AplexicaTurnCount == bestExplicitCount && m.AplexicaTurnsHash != "" &&
			bestExplicitHash != "" && m.AplexicaTurnsHash != bestExplicitHash {
			// Generated output can never produce two stamps with the same count
			// but different hashes — each generation's count strictly grows. This
			// shape can only come from a corrupted or hand-edited file. Fail
			// closed the way any hash disagreement used to be treated, instead of
			// silently trusting whichever equal-count stamp was read first.
			allStamped = false
		}
		if m.Type == "user" || m.Type == "assistant" {
			// Every generated visible turn is exactly one stamped user/assistant
			// row; native continuation rows are unstamped and never land here.
			stampedVisible++
		}
	}
	if found {
		// Releases before the append path re-stamped the base left the count
		// frozen at whatever the first full transcode wrote while the file kept
		// growing, so a 1-turn stamp could describe a 2-turn base. Ingest then
		// inferred a short base, the turns-hash loop break in
		// MergeConversationByThreadRef could not fire, and the continuation was
		// unioned by timestamp into duplicate turns. Counting the stamped visible
		// rows repairs those already-written sessions in place. The stored hash
		// only describes bestExplicitCount turns, so drop it when the real base is
		// larger: hash-gated shortcuts must fail closed, never match a short base.
		switch {
		case stampedVisible > bestExplicitCount:
			ref.MaterializedTurnCount = stampedVisible
			ref.MaterializedTurnsHash = ""
		case hasExplicitTurnCount:
			ref.MaterializedTurnCount = bestExplicitCount
			ref.MaterializedTurnsHash = bestExplicitHash
		default:
			ref.MaterializedTurnCount = stampedVisible
		}
	}
	if found {
		// Pre-hash Aplexica releases stamped the thread id onto EVERY generated
		// Claude row. A user continuation is appended by Claude itself without
		// that custom field, so an all-stamped legacy file is still provably an
		// unchanged generated snapshot and can take the same no-replay shortcut.
		ref.GeneratedSnapshot = allStamped
		return ref, true
	}
	if tid := claudeSessionThreadID(raw); tid != "" {
		return adapter.ThreadRef{ArtifactID: tid, BranchID: acf.MainBranch}, true
	}
	return adapter.ThreadRef{}, false
}

func claudeSessionThreadID(raw []byte) string {
	for _, line := range bytes.Split(raw, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var m struct {
			SessionID string `json:"sessionId"`
		}
		if json.Unmarshal(line, &m) == nil && m.SessionID != "" {
			return m.SessionID
		}
	}
	return ""
}

func encodeProjectDir(cwd string) string {
	// Windows separators and the drive colon must flatten too: a raw
	// `C:\Users\x` would otherwise contribute path separators and an illegal
	// `C:` segment to the ~/.claude/projects/<encoded> directory name.
	return strings.NewReplacer("/", "-", "\\", "-", ":", "-", ".", "-", "_", "-", " ", "-").Replace(cwd)
}

// materializedModelID is the model id stamped on materialized assistant rows.
// Claude Code's /resume requires message.model to be a present, recognized
// string (it calls .includes on it), so it can be neither omitted nor a fake
// like "aplexica-synced". A cross-agent thread has no real Claude model, so we
// use the current default. Bump this when it ages out — an aged id only triggers
// Claude Code's "could not be restored" warning + fallback, never a resume crash.
const materializedModelID = "claude-opus-4-8"

// transcodeToClaudeSession renders the thread's text turns into Claude session
// JSONL: custom-title + ai-title + chained user/assistant lines + last-prompt. Deterministic
// (per-line UUIDs derived from sessionID+index; synthetic timestamps from base).
func transcodeToClaudeSession(turns []acf.TextTurn, sessionID, cwd, sourceAgent, fallbackTitle string, base time.Time) string {
	return transcodeToClaudeSessionWithThread(turns, sessionID, sessionID, acf.MainBranch, cwd, sourceAgent, fallbackTitle, base)
}

func transcodeToClaudeSessionWithThread(turns []acf.TextTurn, sessionID, threadID, branchID, cwd, sourceAgent, fallbackTitle string, base time.Time) string {
	return transcodeToClaudeSessionWithUUIDs(
		turns, nil, nil, sessionID, threadID, branchID, cwd, sourceAgent, fallbackTitle, base)
}

// transcodeToClaudeSessionWithUUIDs is transcodeToClaudeSessionWithThread with
// an explicit per-turn uuid assignment and the carried rows the rebuild must
// not regenerate. uuids may be nil (every row gets the deterministic
// sessionID+index uuid, exactly as before) or exactly len(turns) long; bridges
// may be nil, which is every caller except the rebuild.
//
// Both overrides exist for ONE caller: the forked-mirror rebuild. Claude Code
// appends a child of the leaf it holds IN MEMORY, so a rebuild that regenerated
// every uuid would strand the very next native append on a parent that no
// longer exists — turning a repairable forked_mirror into a permanently
// unrepairable graph_malformed. Reusing the uuid each contained row already
// carries keeps a stale in-memory leaf resolvable, and carrying the mirror's
// attachment/system rows through keeps both their bodies and their uuids, which
// a live Claude Code is just as capable of naming as a parent.
func transcodeToClaudeSessionWithUUIDs(
	turns []acf.TextTurn,
	uuids []string,
	bridges []claudeBridgeRow,
	sessionID, threadID, branchID, cwd, sourceAgent, fallbackTitle string,
	base time.Time,
) string {
	if len(turns) == 0 {
		return ""
	}
	if len(uuids) != len(turns) {
		uuids = nil
	}
	branchID = normalizeClaudeBranchID(branchID)
	turnsHash := adapter.ConversationTurnsHash(turns)
	type obj = map[string]any
	stamp := func(line obj) obj {
		line["aplexicaThreadId"] = threadID
		line["aplexicaBranchId"] = branchID
		line["aplexicaTurnsHash"] = turnsHash
		line["aplexicaTurnCount"] = len(turns)
		return line
	}

	rowUUIDs := make([]string, len(turns))
	for i := range turns {
		rowUUIDs[i] = deterministicUUID(sessionID, i)
		if uuids != nil && uuids[i] != "" {
			rowUUIDs[i] = uuids[i]
		}
	}
	lines, leafUUID, ok := claudeChainRows(rowUUIDs, bridges, func(i int, parentUUID string) obj {
		return stamp(claudeTurnRow(turns[i], rowUUIDs[i], parentUUID, sessionID, cwd, base, i))
	})
	if !ok {
		return ""
	}

	title := adapter.ConversationBranchDisplayTitle(
		claudeConversationDisplayTitle(turns, sourceAgent, fallbackTitle),
		branchID,
	)
	title = truncate(title, materializedClaudeTitleMaxRunes)
	customTitleLine, _ := json.Marshal(stamp(obj{"type": "custom-title", "customTitle": title, "sessionId": sessionID}))
	titleLine, _ := json.Marshal(stamp(obj{"type": "ai-title", "aiTitle": title, "sessionId": sessionID}))
	lastPromptLine, _ := json.Marshal(stamp(obj{
		"type": "last-prompt", "lastPrompt": oneLine(lastClaudeUserText(turns)),
		"leafUuid": leafUUID, "sessionId": sessionID,
	}))

	out := make([]string, 0, len(lines)+3)
	out = append(out, string(customTitleLine))
	out = append(out, string(titleLine))
	out = append(out, lines...)
	out = append(out, string(lastPromptLine))
	return strings.Join(out, "\n") + "\n"
}

// claudeTurnRow renders ONE planned turn as the Claude session row Aplexica
// writes for it. It is shared by every renderer so the append path, the
// synthetic rebuild and the native rebuild can never drift in what a
// materialized row looks like; only the chaining around it differs.
func claudeTurnRow(
	turn acf.TextTurn,
	uuid, parentUUID, sessionID, cwd string,
	base time.Time,
	index int,
) map[string]any {
	type obj = map[string]any
	ts := base.Add(time.Duration(index) * time.Second).UTC().Format(time.RFC3339Nano)
	if turn.Role == "assistant" {
		return obj{
			"parentUuid": parentOrNil(parentUUID), "isSidechain": false, "type": "assistant",
			// message.model MUST be a present string: Claude Code's /resume
			// calls .includes() on it, so OMITTING it crashes resume with
			// "undefined is not an object (evaluating 'e.includes')". The
			// canonical encoding drops the original model and a cross-agent
			// thread has no Claude model anyway, so we write a currently-valid
			// default id — this avoids BOTH the crash AND the "Session model …
			// could not be restored" warning the old "aplexica-synced"
			// placeholder caused. If the id ever ages out, Claude Code falls
			// back with that warning (never a crash).
			"message": obj{
				"role": "assistant", "type": "message",
				"content":     []any{obj{"type": "text", "text": turn.Text}},
				"model":       materializedModelID,
				"id":          "msg_" + shortHash(uuid),
				"stop_reason": "end_turn",
			},
			"uuid": uuid, "timestamp": ts, "cwd": cwd, "sessionId": sessionID,
			"version": "aplexica-sync", "gitBranch": "",
		}
	}
	return obj{
		"parentUuid": parentOrNil(parentUUID), "isSidechain": false, "type": "user",
		"message": obj{"role": "user", "content": turn.Text},
		"uuid":    uuid, "timestamp": ts, "cwd": cwd, "sessionId": sessionID,
		"version": "aplexica-sync", "gitBranch": "", "userType": "external",
	}
}

func claudeConversationDisplayTitle(turns []acf.TextTurn, sourceAgent, portableTitle string) string {
	// Callers outside MaterializeConversationSession may still pass the legacy
	// Artifact.Name value (the native .jsonl filename). Never expose that as a
	// conversation title; fall back to the first meaningful user turn instead.
	portableTitle = adapter.ResolveConversationTitle("", sourceAgent, "", portableTitle)
	if title := strings.TrimSpace(oneLine(portableTitle)); title != "" {
		return truncate(title, materializedClaudeTitleMaxRunes)
	}
	var firstUser string
	for _, turn := range turns {
		if turn.Role == "user" && strings.TrimSpace(turn.Text) != "" {
			firstUser = turn.Text
			break
		}
	}
	if firstUser == "" {
		firstUser = "Synced conversation"
	}
	return "↪ " + titleWord(sourceAgent) + ": " + truncate(oneLine(firstUser), 56)
}

func claudeSyncedSessionID(artifactID, branchID string) string {
	branchID = normalizeClaudeBranchID(branchID)
	if branchID == acf.MainBranch {
		return artifactID
	}
	return deterministicUUID("aplexica:"+artifactID+":"+branchID, 0)
}

func normalizeClaudeBranchID(branchID string) string {
	if branchID == "" {
		return acf.MainBranch
	}
	norm, err := acf.NormalizeBranchName(branchID)
	if err != nil {
		return acf.MainBranch
	}
	return norm
}

func parentOrNil(u string) any {
	if u == "" {
		return nil
	}
	return u
}

func deterministicUUID(seed string, i int) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("%s#%d", seed, i)))
	b := h[:16]
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func shortHash(s string) string {
	return shortHashBytes([]byte(s))
}

// shortHashBytes is shortHash without forcing a copy of the input into a
// string — the mirror pre-image names hash whole session files.
func shortHashBytes(b []byte) string {
	h := sha256.Sum256(b)
	return fmt.Sprintf("%x", h[:8])
}

func titleWord(s string) string {
	if s == "" {
		return "Agent"
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func oneLine(s string) string {
	return strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
