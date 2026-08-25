package claudecode

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter"
)

// claudeMirrorPreimageForked and claudeMirrorPreimageDiverged name which repair
// took a pre-image. They are file-name suffixes, so an operator recovering from
// ~/.aplexica/quarantine can tell an Aplexica-owned mirror's pre-repair bytes
// from a copy of their OWN transcript without opening either.
const (
	claudeMirrorPreimageForked   = "forked"
	claudeMirrorPreimageDiverged = "diverged"
)

// nativeRepairMaxBytes bounds what this pass will read into memory and parse.
//
// The pass is reachable from live fan-out and pays a whole-file read plus a
// per-row canonical encode before it can discover it will refuse, so an
// unbounded one is an unbounded latency spike inside a held import slot.
// A file past the bound is refused with the ordinary structural reason rather
// than read — a rebuild of a quarter-gigabyte transcript is not something to
// attempt inside a materialization pass whatever the proof says.
//
// It is a var solely so the bound itself is testable without writing a
// 64-megabyte fixture. Nothing in the shipped tree assigns it.
var nativeRepairMaxBytes int64 = 64 << 20

// nativeRepairVerdict memoizes a REFUSAL against the evidence the drain already
// trusts to decide whether anything changed.
//
// Only refusals are cached, and only against the exact (size, mtime, plan)
// triple they were computed from: a successful repair rewrites the file, so its
// verdict is invalidated by construction. This keeps the cost for transcripts
// that can never satisfy the loss proof — such as those containing an
// extended-thinking block, a tool_use, or an image beside its caption — at one full
// parse rather than one per pass, which matters most on the paths that have no
// short circuit at all: a whole-store RefanOutAll, and the first drain sweep
// after every daemon restart.
type nativeRepairVerdict struct {
	size      int64
	modTime   time.Time
	planHash  string
	sessionID string
}

// repairDivergedNativeSession rebuilds the user's OWN Claude transcript so it
// holds the whole canonical conversation, and only when the replacement
// provably loses nothing.
//
// It exists because a conversation that STARTS in Claude Code, is continued in
// another agent, and is then continued again in Claude Code WITHOUT resuming
// ends with the two sides irreconcilable by appending. That happens in two
// shapes, and BOTH reach here:
//
//   - DIVERGED. Canonical and the file each hold a turn the other lacks. The
//     native writer's suffix is prefix-gated, so it reports
//     SessionDeclineDiverged on every pass forever.
//   - FORKED / CHAIN_UNSPANNED. The file holds a conversational row its own
//     resume walk cannot reach. This is the shape the owner's scenario actually
//     produces once Aplexica's own append lands first: Aplexica appends the
//     foreign turns as a child of the file's tip, the still-open Claude Code
//     appends the user's next prompt as a child of the leaf it holds IN MEMORY,
//     and the graph forks at that node. Aplexica MANUFACTURED that fork, so
//     refusing to flatten it would be refusing to clean up after ourselves.
//
// WHY FLATTENING A FORK IS SOUND, which is the one question this pass has to
// answer that the synthetic rebuild never had to. Nothing is dropped:
// claudeMirrorRowsContained matches every conversational row, in order, against
// the planned turns, and carries every other uuid-bearing row through verbatim.
// Nothing is reordered either — because the match pointer only ever advances,
// containment also proves the file's conversational rows are an in-order
// subsequence of the plan, and the plan is canonical, which was itself built by
// reading this same file top to bottom. So the rebuilt chain is the file's OWN
// PHYSICAL ROW ORDER with the foreign turns interleaved. The only thing lost is
// the branch topology, and the topology is represented nowhere else in the
// system: import is physical-order, so canonical and therefore every other
// agent already hold both branches flattened. Declining here would keep exactly
// one consumer — Claude Code — permanently disagreeing with every other one,
// which is the state the owner reported.
//
// That is a real trade and it is stated rather than hidden: a user who rewound
// a prompt and asked something else will, after this repair, see the abandoned
// pair replayed as part of the thread. It is behind an opt-in flag, the
// pre-image is mandatory, and the alternative is that no turn from any other
// agent ever reaches the file again.
//
// WHY THE ROW-LEVEL PROOF IS SUFFICIENT FOR A FILE THE USER'S OWN AGENT OWNS.
// The only reason this adapter never rewrites a Claude-visible pathname is to
// protect an unimported local continuation. claudeMirrorRowsContained proves
// there is none, and it proves it the three ways a turn-level proof cannot: it
// re-parses the file line by line and requires every non-empty line to decode
// on its own (EncodeCanonical's streaming decoder breaks on the first bad row
// and returns a nil error); it proves containment over ROWS rather than turns
// (acf.ExtractTextTurns drops rows normalizing to empty — injected context,
// local-command context, scheduled-task preambles, image-only rows — which
// would pass a turn-level proof trivially and then be destroyed); and it
// requires each matched row's own text to round-trip, so a row that normalizes
// to something SHORTER than it holds is refused rather than truncated.
//
// Every step below is a hard gate and every one of them fails closed.
//
// Repair-pass budget (design rule 3 — a repair pass must stay under the failure
// budget of the systems it drives). The quarantine breaker is 3 failures / 10
// minutes per adapter and blocks ALL materialization, live sync included:
//
//   - Failures charged to the breaker by this pass: ZERO. The breaker is fed
//     from exactly one call site, the Export loop in fanOut. This code runs
//     inside MaterializeConversationSession, which the orchestrator reaches
//     through writeConversationSession; that path records an adapter error
//     string for status and never calls Quarantine.RecordFailure. 0 < 3.
//   - Commit attempts per materialization pass: at most ONE. Both call sites are
//     single statements on branches that return either way.
//   - Passes per artifact: bounded by the deferral backoff, which doubles to a
//     15-minute ceiling. Even if the breaker did count these, one artifact
//     yields at most one attempt per 10 minutes.
//   - Whole-file parses per REFUSED artifact: one, ever, per (size, mtime,
//     plan). The memo above is what bounds a whole-store fan-out, which has no
//     short circuit in front of it.
//   - A successful repair is terminal: the next pass finds the file writable and
//     exactly equal to the plan, so it writes nothing. A repair that removes its
//     own trigger cannot loop.
func (a *Adapter) repairDivergedNativeSession(
	path string,
	plannedTurns []acf.TextTurn,
	sessionID, cwd string,
	base time.Time,
	policy claudeMirrorRepairPolicy,
) (bool, error) {
	// 1. Authorization, re-checked against the path actually being written so
	// the scope limit is enforced at the commit site rather than inferred from
	// the caller's control flow.
	if !policy.repairDivergedNative || policy.nativeDest == "" ||
		filepath.Clean(policy.nativeDest) != filepath.Clean(path) {
		return false, nil
	}
	if len(plannedTurns) == 0 || sessionID == "" {
		return false, nil
	}
	// 2. The cheap gates first: a stat answers both the size bound and the memo,
	// so a file this pass has already refused costs one syscall rather than a
	// read plus a full parse.
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 ||
		info.Size() > nativeRepairMaxBytes {
		return false, nil
	}
	verdict := nativeRepairVerdict{
		size:      info.Size(),
		modTime:   info.ModTime(),
		planHash:  adapter.ConversationTurnsHash(plannedTurns),
		sessionID: sessionID,
	}
	if a.nativeRepairRefused(path, verdict) {
		return false, nil
	}
	// 3. The bytes every proof below is computed from.
	snapshot, exists, changed, err := readClaudeSessionSnapshot(path)
	if err != nil || !exists || changed || len(snapshot.raw) == 0 {
		return false, err
	}
	// 4. An unconsumed tail is a row being written right now. A writer
	// mid-append is never a permanent state and never grounds for a rewrite.
	// This is also where identity is re-proven from the bytes on disk, using
	// exactly the fields claudeNativeSourceSessionPlan authenticated the plan
	// with: the file must still be its own native session and must NOT carry an
	// Aplexica thread stamp, because a stamp on a pristine native source is the
	// permanent contradiction that planner refuses, so rebuilding a stamped file
	// would be rebuilding something else entirely.
	state, resume := encodeCanonicalInto(snapshot.raw, 0, claudeCanonicalState{})
	if len(bytes.TrimSpace(snapshot.raw[resume:])) != 0 {
		// Transient: a snapshot taken mid-append is not evidence about the file,
		// so it must not be memoized as a refusal.
		return false, nil
	}
	if state.hasExplicitThreadStamp || state.sessionID != sessionID {
		a.rememberNativeRepairRefusal(path, verdict)
		return false, nil
	}
	// 5. The loss proof. It also yields everything the rebuild may not
	// regenerate: the uuid-less preamble, and every uuid-bearing row that does
	// not correspond to a planned turn.
	contained, result, _ := claudeMirrorRowsContained(snapshot.raw, plannedTurns)
	if !contained || result.preambleStamped {
		a.rememberNativeRepairRefusal(path, verdict)
		return false, nil
	}
	// 5b. The resume graph, consulted BEFORE the rewrite and not only after it.
	// Containment proves nothing is LOST; this is what decides whether there is
	// anything to do and whether the file is one Aplexica may reason about at
	// all. It runs after containment on purpose: some transcripts can
	// never satisfy the loss proof, and they must not pay a second whole-file
	// walk to learn it.
	projection, projectionErr := parseClaudeVisibleLeaf(snapshot.raw)
	if projectionErr != nil {
		// A file whose own walk hard-faults is graph_malformed: duplicate uuids,
		// a cycle, a parent the file does not hold. Flattening it would be
		// guessing at what it replaces. No shipped route reaches here with such a
		// file — the writer returns graph_malformed, which nativeRepairableReason
		// excludes — and this is what keeps that true if one ever does.
		a.rememberNativeRepairRefusal(path, verdict)
		return false, nil
	}
	if projection.spans() && acf.TextTurnsEqual(projection.turns, plannedTurns) {
		// Already one chain holding exactly the plan. There is nothing to
		// flatten and nothing to add, so rewriting would churn a file for no
		// reason. Not memoized: this is a statement about a healthy file, and
		// the caller's own comparison will reach it first next pass.
		return false, nil
	}
	rebuilt := transcodeClaudeNativeSession(
		result.preamble, plannedTurns,
		claudeRebuildUUIDs(sessionID, len(plannedTurns), result), result.bridges,
		sessionID, cwd, base,
	)
	if rebuilt == "" {
		a.rememberNativeRepairRefusal(path, verdict)
		return false, nil
	}
	// 6. The pre-image is MANDATORY here, unlike the synthetic path where it is
	// insurance. This rewrite replaces a file the user owns, and the quarantined
	// copy is the only way back to its original graph. It is fsynced, and its
	// directory entry fsynced, BEFORE the destructive truncate.
	if err := writeClaudeMirrorPreimage(
		policy.preimageDir, sessionID, claudeMirrorPreimageDiverged, snapshot.raw); err != nil {
		return false, err
	}
	// 7. Inode-preserving commit. rewriteClaudeSessionIfSnapshotCurrent
	// re-verifies the bytes and the inode through both the descriptor and the
	// pathname before and after, and restores the original bytes on the SAME
	// inode if anything fails after the truncate.
	written, raced, err := rewriteClaudeSessionIfSnapshotCurrent(path, snapshot, []byte(rebuilt))
	if err != nil {
		return false, err
	}
	if raced || !written {
		return false, nil
	}
	// 8. Read-back verification against the NATIVE identity contract.
	post, postExists, postChanged, err := readClaudeSessionSnapshot(path)
	if err != nil {
		return false, err
	}
	if !postExists || postChanged || !os.SameFile(post.info, snapshot.info) {
		// Something replaced the pathname while we held it open. Report the
		// repair as not taken so the caller defers and re-reads.
		return false, nil
	}
	return claudeNativeSessionMatches(post.raw, plannedTurns, sessionID)
}

// nativeRepairRefused and rememberNativeRepairRefusal are the memo's two halves.
// The map holds one entry per pathname, replaced rather than accumulated, so a
// long-running daemon cannot grow it past the number of native transcripts it
// has actually attempted.
func (a *Adapter) nativeRepairRefused(path string, verdict nativeRepairVerdict) bool {
	a.nativeRepairMu.Lock()
	defer a.nativeRepairMu.Unlock()
	previous, ok := a.nativeRepairRefusals[path]
	return ok && previous == verdict
}

func (a *Adapter) rememberNativeRepairRefusal(path string, verdict nativeRepairVerdict) {
	a.nativeRepairMu.Lock()
	defer a.nativeRepairMu.Unlock()
	if a.nativeRepairRefusals == nil {
		a.nativeRepairRefusals = map[string]nativeRepairVerdict{}
	}
	a.nativeRepairRefusals[path] = verdict
}

// nativeRepairableReason names the decline classes the native rebuild can
// actually address. All three mean "the two sides cannot be reconciled by
// appending", which is the only thing a whole-file rewrite is for.
//
// The set is deliberately wider than the reason the PLANNER can produce.
// claudeNativeSourceSessionPlan compares turns in FILE ORDER, so the owner's
// scenario — where canonical has already absorbed the native continuation and
// the two turn lists are equal — reaches the planner as a perfectly writable
// plan and only the WRITER discovers the graph cannot be walked. Gating the
// repair on the planner's reason alone left that population, which is the
// common one, with a repair that existed and could never be reached.
//
// The classes deliberately NOT here: native_ahead is transient and its pending
// import is the authority, race must never be terminaled, and graph_malformed
// means the file could not be authenticated as this session at all.
func nativeRepairableReason(reason adapter.SessionDeclineReason) bool {
	switch reason {
	case adapter.SessionDeclineDiverged,
		adapter.SessionDeclineForkedMirror,
		adapter.SessionDeclineChainUnspanned:
		return true
	default:
		return false
	}
}

// transcodeClaudeNativeSession renders a complete, UNSTAMPED Claude session for
// the native rebuild: the carried uuid-less preamble, then the planned turns and
// the carried uuid-bearing rows threaded into one chain, then one last-prompt
// row naming the chain's tip.
//
// It is deliberately NOT transcodeToClaudeSessionWithUUIDs. That renderer
// stamps aplexicaThreadId on every row it emits, and
// claudeNativeSourceSessionPlan treats a thread stamp on a pristine native
// source as a permanent graph_malformed contradiction — so rendering with it
// would convert a repaired transcript into an unrepairable one on the very next
// pass. It also emits custom-title/ai-title rows, which are Aplexica's naming of
// a mirror and have no business overwriting what the user's own agent chose.
//
// The uuid-less rows are carried because dropping one is a certain, irreversible
// loss of a user-visible feature — a file-history-snapshot row IS the user's
// file-undo history for that session — while carrying it risks at worst a
// dangling reference in a best-effort convenience surface that already tolerates
// missing snapshots. last-prompt is the one exception: it names a leafUuid, so a
// stale copy would point the resume walk at a row the rebuild no longer
// contains, and it is regenerated.
//
// uuids may be nil (deterministic per-index uuids) or exactly len(turns) long.
func transcodeClaudeNativeSession(
	preamble []string,
	turns []acf.TextTurn,
	uuids []string,
	bridges []claudeBridgeRow,
	sessionID, cwd string,
	base time.Time,
) string {
	if len(turns) == 0 {
		return ""
	}
	rowUUIDs := make([]string, len(turns))
	for i := range turns {
		rowUUIDs[i] = deterministicUUID(sessionID, i)
		if len(uuids) == len(turns) && uuids[i] != "" {
			rowUUIDs[i] = uuids[i]
		}
	}
	lines, leafUUID, ok := claudeChainRows(rowUUIDs, bridges, func(i int, parentUUID string) map[string]any {
		return claudeTurnRow(turns[i], rowUUIDs[i], parentUUID, sessionID, cwd, base, i)
	})
	if !ok {
		return ""
	}
	lastPrompt, err := json.Marshal(map[string]any{
		"type": "last-prompt", "lastPrompt": oneLine(lastClaudeUserText(turns)),
		"leafUuid": leafUUID, "sessionId": sessionID,
	})
	if err != nil {
		return ""
	}
	out := make([]string, 0, len(preamble)+len(lines)+1)
	out = append(out, preamble...)
	out = append(out, lines...)
	out = append(out, string(lastPrompt))
	return strings.Join(out, "\n") + "\n"
}

// claudeNativeSessionMatches is claudeSessionMatches for a file that must NOT
// carry an Aplexica thread stamp: the same spanning + exact-turns proof, but
// identity is the native sessionId and the ABSENCE of a stamp rather than an
// authenticated thread marker.
//
// The spanning clause is load-bearing for the same reason it is there: turn
// equality alone would accept a file holding an unreachable conversational row,
// and this is the post-write verification the repair reports success from. It is
// also what proves the flattening actually happened — a rebuild that somehow
// left the graph forked would be reported as not taken rather than as success.
func claudeNativeSessionMatches(raw []byte, plannedTurns []acf.TextTurn, sessionID string) (bool, error) {
	if sessionID == "" {
		return false, nil
	}
	state, resume := encodeCanonicalInto(raw, 0, claudeCanonicalState{})
	if len(bytes.TrimSpace(raw[resume:])) != 0 {
		return false, nil
	}
	if state.hasExplicitThreadStamp || state.sessionID != sessionID {
		return false, nil
	}
	projection, err := parseClaudeVisibleLeaf(raw)
	if err != nil {
		return false, nil
	}
	if !projection.spans() || projection.forked ||
		!acf.TextTurnsEqual(projection.turns, plannedTurns) {
		return false, nil
	}
	return true, nil
}
