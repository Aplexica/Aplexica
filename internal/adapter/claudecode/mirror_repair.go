package claudecode

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter"
	"github.com/aplexica/aplexica/internal/atomicfile"
)

// claudeMirrorRepairPolicy carries the per-materialization authorization for
// the forked-mirror repair down the write path. The ZERO VALUE authorizes
// nothing, so every caller that does not explicitly opt in — and the whole
// process when Adapter.RepairForkedMirrors is false — keeps the shipped
// behaviour unchanged, byte for byte and inode for inode.
type claudeMirrorRepairPolicy struct {
	// repairForkedMirror is the single switch. Everything below is only read
	// when it is true.
	repairForkedMirror bool

	// syntheticDest is the one pathname this policy authorizes rewriting. It
	// is compared against the path actually being written, so the scope limit
	// ("deterministic synthetic mirrors only") is enforced at the commit site
	// rather than inferred from the caller's control flow.
	syntheticDest string

	// repairDivergedNative authorizes rebuilding the user's OWN transcript,
	// under the same switch and the same claudeMirrorRowsContained proof, scoped
	// to nativeDest. It is a SEPARATE axis from repairForkedMirror because the
	// two are resolved for different plans and reach different writers; a policy
	// value never carries both.
	repairDivergedNative bool

	// nativeDest is repairDivergedNative's equivalent of syntheticDest: the one
	// pathname that authorization covers, re-checked at the commit site.
	nativeDest string

	// preimageDir receives a copy of the pre-repair bytes before the in-place
	// rewrite. It lives under ~/.aplexica/quarantine, deliberately OUTSIDE
	// ~/.claude, so a preserved copy can never appear in /resume — the same
	// placement bestEffortQuarantineClaudeThreadDuplicates uses. Empty skips
	// the copy.
	preimageDir string
}

// mirrorRepairPolicy resolves the Stage-B authorization for ONE materialization
// of one artifact. It fails closed on every axis: the feature switch, the
// native-origin exclusion, and the synthetic-pathname restriction are each
// checked here and each independently reduce the policy to its inert zero
// value.
//
// The native-origin exclusion is redundant today — writeClaudeConversationSession
// is structurally unreachable for a native-origin plan, because such a plan
// either returns at the !nativeSource/!nativeWritable decline or is handled by
// writeClaudeNativeConversationSession. It is stated anyway: the invariant this
// repair must never violate is "never rewrite the user's own transcript", and
// an invariant that depends on the shape of two enclosing branches is one
// refactor away from being silently lost.
func (a *Adapter) mirrorRepairPolicy(plan claudeConversationSessionPlan, artifactID string) claudeMirrorRepairPolicy {
	if !a.RepairForkedMirrors || a.HomeDir == "" {
		return claudeMirrorRepairPolicy{}
	}
	if plan.nativeOrigin || plan.nativeSource || plan.dest == "" || plan.syntheticDest == "" {
		return claudeMirrorRepairPolicy{}
	}
	if filepath.Clean(plan.dest) != filepath.Clean(plan.syntheticDest) {
		return claudeMirrorRepairPolicy{}
	}
	return claudeMirrorRepairPolicy{
		repairForkedMirror: true,
		syntheticDest:      plan.syntheticDest,
		preimageDir:        a.mirrorPreimageDir(artifactID),
	}
}

// nativeRepairPolicy resolves the authorization for rebuilding the user's OWN
// Claude transcript. It is a separate resolver rather than a relaxation of
// mirrorRepairPolicy's native-origin exclusion, because relaxing that line
// alone would change nothing: mirrorRepairPolicy is consumed only by
// writeClaudeConversationSession, which materializeConversationSession never
// reaches for a native-origin plan — such a plan is either handled by the
// native writer or declined before any writer runs. The invariant that
// exclusion states ("never rewrite the user's own transcript via the SYNTHETIC
// path") is still true and stays stated there.
//
// It fails closed on every axis: the same feature switch, a home directory, a
// proven native session identity, and a non-empty destination. The zero value
// authorizes nothing.
func (a *Adapter) nativeRepairPolicy(plan claudeConversationSessionPlan, artifactID string) claudeMirrorRepairPolicy {
	if !a.RepairForkedMirrors || a.HomeDir == "" {
		return claudeMirrorRepairPolicy{}
	}
	if !plan.nativeOrigin || !plan.nativeSource || plan.dest == "" {
		return claudeMirrorRepairPolicy{}
	}
	// Re-prove the pathname independently rather than inheriting it from the
	// enclosing branch. localConversationSourcePath is what rejects a symlinked
	// component, a final-component symlink, and anything outside
	// ~/.claude/projects; a policy that authorizes a destructive rewrite must
	// state that proof itself, not depend on where it happens to be resolved.
	validated, ok := a.localConversationSourcePath(plan.dest)
	if !ok || filepath.Clean(validated) != filepath.Clean(plan.dest) {
		return claudeMirrorRepairPolicy{}
	}
	return claudeMirrorRepairPolicy{
		repairDivergedNative: true,
		nativeDest:           plan.dest,
		preimageDir:          a.mirrorPreimageDir(artifactID),
	}
}

func (a *Adapter) mirrorPreimageDir(artifactID string) string {
	return filepath.Join(
		a.HomeDir, ".aplexica", "quarantine", "claude-conversations", shortHash(artifactID),
	)
}

// ForkedMirrorRepairEnabled (adapter.ConversationMirrorRepairReporter) reports
// whether this adapter is currently authorized to rebuild a forked synthetic
// mirror, so the orchestrator's needs_attention rows can say whether the fix
// exists-and-is-off or does not exist at all.
func (a *Adapter) ForkedMirrorRepairEnabled() bool {
	return a != nil && a.RepairForkedMirrors
}

// claudeMirrorRow is the strict per-row projection the containment proof works
// from. It carries only the fields that decide whether a row is conversational
// and what it encodes — never the prompt or answer text itself, which stays
// inside the canonical encoder.
type claudeMirrorRow struct {
	Type             string          `json:"type"`
	UUID             string          `json:"uuid,omitempty"`
	IsSidechain      bool            `json:"isSidechain,omitempty"`
	AplexicaThreadID string          `json:"aplexicaThreadId,omitempty"`
	Content          json.RawMessage `json:"content,omitempty"`
	Message          *struct {
		Role    string          `json:"role,omitempty"`
		Content json.RawMessage `json:"content,omitempty"`
		Model   string          `json:"model,omitempty"`
	} `json:"message,omitempty"`
}

// contentBlocks normalizes the two shapes Claude Code writes: real rows wrap
// content under .message, the older/simpler shape puts it at the top level.
func (r claudeMirrorRow) contentBlocks() json.RawMessage {
	if len(r.Content) > 0 {
		return r.Content
	}
	if r.Message != nil {
		return r.Message.Content
	}
	return nil
}

// claudeMirrorBlock is the per-content-block projection. Type decides
// reproducibility, Thinking distinguishes the signature-only split record from
// real extended-thinking content, and Text is read ONLY to re-join the row the
// way the canonical encoder does so the match can be proven both ways. None of
// it is logged or published.
type claudeMirrorBlock struct {
	Type     string `json:"type"`
	Thinking string `json:"thinking"`
	Text     string `json:"text"`
}

// claudeMirrorRowIsTextOnly reports whether the rebuild can reproduce this row
// in full.
//
// The rebuild regenerates every row from a planned TEXT turn
// (transcodeToClaudeSessionWithUUIDs emits exactly one text block), so a row
// carrying anything else — a pasted image, an extended-thinking block, a
// tool_use, a tool_result — cannot be reproduced even when its text projection
// matches a planned turn perfectly. A captioned screenshot is the ordinary case
// and yields exactly one text turn, so a turn-level comparison sails straight
// past it and the rebuild deletes the image.
//
// A bare-string content field is the simple all-text shape and is reproducible.
func claudeMirrorRowIsTextOnly(row claudeMirrorRow) bool {
	_, textOnly := claudeMirrorRowText(row)
	return textOnly
}

// claudeMirrorRowText returns the row's own text exactly as the canonical
// encoder would join it — non-empty text blocks separated by a blank line,
// matching acf.joinTextBlocks — and reports whether the row is reproducible
// from a planned text turn at all.
//
// The text is what makes the containment proof a ROUND TRIP rather than a
// one-way match. acf.NormalizeTextTurn is LOSSY in a second way beyond dropping
// rows to empty: StripScheduledTaskPreamble and StripCodexAttachmentPreamble
// delete a leading section of a user row and keep the rest, so a row can match a
// planned turn while carrying strictly more text than that turn holds. The
// rebuild emits the planned turn, so committing on the match alone silently
// truncates the user's own row. Containment-passing files can contain rows
// shortened this way, including attachment inventories naming the user's files.
func claudeMirrorRowText(row claudeMirrorRow) (string, bool) {
	content := row.contentBlocks()
	if len(content) == 0 {
		return "", false
	}
	var asString string
	if json.Unmarshal(content, &asString) == nil {
		return asString, true
	}
	var blocks []claudeMirrorBlock
	if json.Unmarshal(content, &blocks) != nil || len(blocks) == 0 {
		return "", false
	}
	var parts []string
	for _, block := range blocks {
		if block.Type != "text" {
			return "", false
		}
		if block.Text == "" {
			// joinTextBlocks skips an empty text block outright, so an empty one
			// contributes nothing to either side of the comparison.
			continue
		}
		parts = append(parts, block.Text)
	}
	return strings.Join(parts, "\n\n"), true
}

// claudeMirrorRowsContained is the loss proof that authorizes rewriting a
// forked mirror. It answers one question: can the canonical plan reproduce
// every conversational row this file holds?
//
// It is deliberately NOT the turn-based claudeTextTurnsSubsequence proof, for
// two independent reasons, both of which would make that proof pass on files a
// rebuild would damage:
//
//   - EncodeCanonical drives a streaming json.Decoder that BREAKS on the first
//     row it cannot decode and returns a nil error. A containment set built on
//     it silently measures only the rows before the damage and then reports
//     success. So this proof re-parses the file line by line and requires EVERY
//     non-empty line to decode on its own.
//   - acf.ExtractTextTurns DROPS turns whose normalized text is empty:
//     IsInjectedContext ("# Project memory", "<INSTRUCTIONS>",
//     "<user_instructions>", "<environment_context>", …), IsLocalCommandContext
//     ("<command-name>", "<local-command-stdout>", …), a message that is
//     nothing but StripScheduledTaskPreamble boilerplate, and image-only rows.
//     Those rows contribute nothing to a turn-based set, so it passes
//     trivially — and the rebuild regenerates from the planned TURNS and
//     destroys them. So containment is proven over ROWS.
//
// Every user/assistant row must therefore be either (a) matched, in order, to a
// planned turn AND carry nothing but text blocks whose joined text round-trips
// to that turn, or (b) one of exactly two enumerated shapes that provably carry
// no conversation on either side (claudeNonContentConversationalRow). Anything
// else declines; there is no third outcome and no heuristic.
//
// EVERY OTHER UUID-BEARING ROW IS CARRIED, NOT SKIPPED. The proof used to
// `continue` past attachment and system rows on the theory that they "are not
// conversation", and the rebuild then dropped them because it regenerates from
// planned turns. Both halves were wrong for the native population R4 opened:
//
//   - They carry content. Containment-passing files can hold uuid-bearing
//     non-conversational rows, including `system/local_command` rows whose
//     bodies are the
//     `<command-name>` / `<local-command-stdout>` text the binding constraint
//     names by name, plus queued_command, hook-context and skill-listing
//     attachments.
//   - They are graph participants. A conversational row can name one of them as
//     its parentUuid, so deleting one strands a live
//     Claude Code's next append on a uuid the file no longer holds — the exact
//     hazard transcodeClaudeTurnAppend already preserves TURN uuids to avoid,
//     and its outcome is a permanently unparseable transcript.
//
// So the answer is neither "enumerate which of them are safe to delete" nor
// "decline whenever one exists": they are returned as bridges and threaded back
// into the rebuilt chain verbatim, with only their parentUuid rewritten. The
// invariant is then flat — the rebuild never drops a uuid — instead of resting
// on a subtype allowlist that Claude Code can extend at any release.
//
// result is meaningful only when contained is true; reason only when it is
// false.
func claudeMirrorRowsContained(
	raw []byte, plannedTurns []acf.TextTurn,
) (contained bool, result claudeContainment, reason adapter.SessionDeclineReason) {
	if len(raw) == 0 {
		// An empty file is a structural state, not a writer mid-append.
		return false, claudeContainment{}, adapter.SessionDeclineGraphMalformed
	}
	if raw[len(raw)-1] != '\n' {
		// A torn trailing row is a writer mid-append, never a permanent state,
		// and never grounds for a rewrite.
		return false, claudeContainment{}, adapter.SessionDeclineRace
	}
	result = claudeContainment{preserved: map[int]string{}}
	next := 0
	// matchedTurn is the index of the last planned turn a row matched, so a
	// carried row can be re-anchored where the file itself held it. -1 means
	// "before every turn".
	matchedTurn := -1
	carry := func(row claudeMirrorRow, line []byte) {
		result.bridges = append(result.bridges, claudeBridgeRow{
			line: string(line), uuid: row.UUID, afterTurn: matchedTurn,
		})
	}
	for _, line := range bytes.Split(raw, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var row claudeMirrorRow
		if err := json.Unmarshal(line, &row); err != nil {
			// A safety proof may never be derived from bytes we cannot read.
			return false, claudeContainment{}, adapter.SessionDeclineGraphMalformed
		}
		if row.Type != "user" && row.Type != "assistant" {
			if row.UUID == "" {
				// Not a graph participant. last-prompt is regenerated (it names a
				// leafUuid, and a stale one points the resume walk at a row the
				// rebuild no longer contains); everything else is preamble the
				// native rebuild carries through verbatim.
				if row.Type != "last-prompt" {
					result.preamble = append(result.preamble, string(line))
					result.preambleStamped = result.preambleStamped || row.AplexicaThreadID != ""
				}
				continue
			}
			carry(row, line)
			continue
		}
		if row.IsSidechain {
			// A Task/sub-agent transcript. It lives on its own root, the rebuild
			// emits only the main chain, and flattening a sub-agent's internal
			// prompts into the resumable thread would both lose their structure
			// and replay them to the user. Never rewrite a file holding one.
			return false, claudeContainment{}, adapter.SessionDeclineMirrorDiverged
		}
		// The line is now proven to be a complete, self-contained JSON object,
		// so the canonical encoder cannot truncate it. Reusing that encoder is
		// what makes this proof measure the SAME projection every comparator in
		// the adapter measures.
		events, encoded := claudeRowCanonicalEvents(line)
		if !encoded {
			return false, claudeContainment{}, adapter.SessionDeclineGraphMalformed
		}
		turns := acf.ExtractTextTurns(events)
		switch len(turns) {
		case 1:
			rowText, textOnly := claudeMirrorRowText(row)
			if !textOnly {
				// The text projection matches something the plan holds, but the
				// row carries more than text — an image beside its caption, an
				// extended-thinking block beside the visible answer, a tool call.
				// The rebuild emits a bare text turn, so committing here would
				// delete content the proof never looked at.
				return false, claudeContainment{}, adapter.SessionDeclineMirrorDiverged
			}
			if strings.TrimSpace(rowText) != turns[0].Text {
				// The row normalizes to something SHORTER than it holds — a
				// scheduled-task preamble or a Codex attachment inventory was
				// stripped. The match is real and the truncation would be too.
				return false, claudeContainment{}, adapter.SessionDeclineMirrorDiverged
			}
			matched := false
			for next < len(plannedTurns) {
				planned := plannedTurns[next]
				index := next
				next++
				if planned.Role == turns[0].Role && planned.Text == turns[0].Text {
					matched = true
					matchedTurn = index
					if row.UUID != "" {
						result.preserved[index] = row.UUID
					}
					break
				}
			}
			if !matched {
				// Either canonical never saw this row, or the file holds it in
				// an order the plan would reverse. Both mean a rewrite loses or
				// reorders the user's words.
				return false, claudeContainment{}, adapter.SessionDeclineMirrorDiverged
			}
		case 0:
			if !claudeNonContentConversationalRow(row) {
				return false, claudeContainment{}, adapter.SessionDeclineMirrorDiverged
			}
			if row.UUID != "" {
				// Enumerated as carrying no conversation, which is why it may be
				// left out of the turn chain — but it is still a uuid a live
				// agent can name as a parent, so it is carried rather than
				// deleted.
				carry(row, line)
			}
		default:
			// parseClaudeVisibleLeaf treats this as malformed too; a single
			// Claude row never encodes more than one text turn.
			return false, claudeContainment{}, adapter.SessionDeclineGraphMalformed
		}
	}
	return true, result, adapter.SessionDeclineUnspecified
}

// claudeContainment is what the loss proof learned about a file it is willing
// to see rebuilt: which planned turn each surviving conversational row matched,
// which uuid-bearing rows must be carried through verbatim, and the uuid-less
// preamble a native rebuild has no other way to regenerate.
type claudeContainment struct {
	// preserved maps a planned-turn index to the uuid of the row that matched
	// it, so the rebuild keeps the identity Claude Code may still hold in
	// memory.
	preserved map[int]string

	// bridges are the uuid-bearing rows that carry no matched turn, in file
	// order, each anchored to the planned turn it followed.
	bridges []claudeBridgeRow

	// preamble is the uuid-less non-last-prompt rows, in file order. Only the
	// native rebuild uses it; the synthetic renderer emits its own.
	preamble []string

	// preambleStamped records an Aplexica thread stamp on a uuid-less row. It is
	// a contradiction on a pristine native source — claudeNativeSourceSessionPlan
	// reports graph_malformed for a stamped native file — so the native rebuild
	// fails closed on it rather than producing a file that reads as
	// Aplexica-generated. It is normal on a synthetic mirror, whose title rows
	// are stamped by construction.
	preambleStamped bool
}

// claudeBridgeRow is one uuid-bearing row the rebuild carries through instead
// of regenerating: the row's own bytes, its uuid, and the planned-turn index it
// followed in the file it came from.
//
// afterTurn is -1 when the row preceded every matched turn. Anchoring on the
// PRECEDING turn rather than the following one is what makes the placement
// decidable when canonical has inserted foreign turns since: the row stays
// immediately after the row it already sat behind, and turns the file never saw
// land after it.
type claudeBridgeRow struct {
	line      string
	uuid      string
	afterTurn int
}

// claudeRebuildUUIDs assigns one uuid per planned turn, reusing the uuid a
// contained mirror row already carried wherever containment matched one.
//
// Preserved uuids are reserved FIRST, so a generated uuid can never collide
// with one: a preserved uuid is a deterministic uuid from the row's ORIGINAL
// index, and canonical may have inserted turns since, so the same value can be
// the natural default for a different index. A duplicate uuid makes
// parseClaudeVisibleLeaf reject the whole file, which would convert the repair
// into the exact permanent failure it exists to end. Carried bridge uuids are
// reserved for the same reason and are just as capable of colliding, since they
// come from the same file and the same generator.
func claudeRebuildUUIDs(sessionID string, count int, contained claudeContainment) []string {
	if count <= 0 {
		return nil
	}
	out := make([]string, count)
	used := make(map[string]bool, count+len(contained.bridges))
	for _, bridge := range contained.bridges {
		if bridge.uuid != "" {
			used[bridge.uuid] = true
		}
	}
	for index, uuid := range contained.preserved {
		if index < 0 || index >= count || uuid == "" || used[uuid] {
			continue
		}
		out[index] = uuid
		used[uuid] = true
	}
	for i := range out {
		if out[i] != "" {
			continue
		}
		candidate := deterministicUUID(sessionID, i)
		for attempt := 1; used[candidate]; attempt++ {
			candidate = deterministicUUID(fmt.Sprintf("%s:rebuild:%d", sessionID, attempt), i)
		}
		out[i] = candidate
		used[candidate] = true
	}
	return out
}

// claudeChainRows threads the regenerated turn rows and the carried bridge rows
// into ONE parentUuid chain, in the order the source file itself held them, and
// returns the rendered lines plus the uuid of the chain's tip.
//
// turnRow renders the planned turn at index with the parent it must hang from;
// it returns nil to abort. A bridge is emitted immediately after the turn it
// followed in the source file, so the only rows that move are the ones canonical
// inserted, and every uuid the file held is still in the file afterwards. That
// second property is the one that matters: Claude Code appends a child of the
// leaf it holds IN MEMORY, and containment-passing transcripts can have a
// conversational row parented at an attachment or system row,
// so a rebuild that dropped bridge uuids would strand the very next append on a
// missing parent and make the transcript permanently unparseable.
//
// ok is false if a carried row cannot be re-encoded, which fails the rebuild
// closed rather than committing a file with a row missing.
func claudeChainRows(
	uuids []string,
	bridges []claudeBridgeRow,
	turnRow func(index int, parentUUID string) map[string]any,
) (lines []string, leaf string, ok bool) {
	byAnchor := make(map[int][]claudeBridgeRow, len(bridges))
	for _, bridge := range bridges {
		byAnchor[bridge.afterTurn] = append(byAnchor[bridge.afterTurn], bridge)
	}
	parent := ""
	emitBridges := func(anchor int) bool {
		for _, bridge := range byAnchor[anchor] {
			line, encoded := reparentClaudeRow(bridge.line, parent)
			if !encoded {
				return false
			}
			lines = append(lines, line)
			parent = bridge.uuid
		}
		return true
	}
	if !emitBridges(-1) {
		return nil, "", false
	}
	for index := range uuids {
		row := turnRow(index, parent)
		if row == nil {
			return nil, "", false
		}
		encoded, err := json.Marshal(row)
		if err != nil {
			return nil, "", false
		}
		lines = append(lines, string(encoded))
		parent = uuids[index]
		if !emitBridges(index) {
			return nil, "", false
		}
	}
	return lines, parent, true
}

// reparentClaudeRow rewrites ONE carried row's parentUuid and changes nothing
// else about it.
//
// The decode uses json.Number so an integer field cannot come back as a float
// with a different spelling, and the row is otherwise round-tripped as-is: key
// order is not preserved (Go marshals maps sorted) but every key and every
// value is. Re-encoding rather than string-splicing is what keeps a body
// containing the literal text "parentUuid" from being corrupted.
func reparentClaudeRow(line, parentUUID string) (string, bool) {
	decoder := json.NewDecoder(strings.NewReader(line))
	decoder.UseNumber()
	var row map[string]any
	if err := decoder.Decode(&row); err != nil || row == nil {
		return "", false
	}
	row["parentUuid"] = parentOrNil(parentUUID)
	encoded, err := json.Marshal(row)
	if err != nil {
		return "", false
	}
	return string(encoded), true
}

// claudeRowCanonicalEvents canonicalizes ONE already-validated JSONL row.
// EncodeCanonical's error is unconditionally nil today, but a future change
// that started returning one must not degrade into "this row carries no turn":
// that reading would hand the row to the enumerated non-content check, which
// can accept it. ok=false makes the caller decline instead.
func claudeRowCanonicalEvents(line []byte) ([]acf.ConversationEvent, bool) {
	events, err := EncodeCanonical(append(append([]byte(nil), line...), '\n'))
	if err != nil {
		return nil, false
	}
	return events, true
}

// claudeNonContentConversationalRow enumerates the ONLY conversational rows a
// rebuild may drop. Both are shapes that carry no conversation on either side
// of the comparison, so regenerating the file without them changes nothing a
// user or an agent can observe:
//
//  1. Claude Desktop's reserved <synthetic> reply, which it appends when it
//     indexes an imported transcript whose newest turn is still a prompt.
//     encodeCanonicalInto skips it outright, so it is not canonical content,
//     and parseClaudeVisibleLeaf already treats it as a parent bridge.
//  2. The signature-only interleaved-thinking record Claude Code >= 2.1.204
//     splits off the visible text record that follows it. Its thinking text is
//     the EMPTY string (the record carries only a signature, which is not
//     content), encodeCanonicalInto skips it for exactly that reason, and its
//     sibling text row is matched separately.
//
// The empty-thinking requirement is load-bearing, not pedantry. This mirror is
// CO-OWNED: Claude Code appends to it live, and with extended thinking on it
// writes real reasoning into thinking blocks — canonical.go preserves any
// non-empty thinking as content precisely because a user can read it. Accepting
// a thinking-only row without checking the text would let the rebuild, which
// emits text turns only, delete paragraphs of visible reasoning.
//
// Everything else — a user row that normalizes to empty, an image-only row, a
// tool_use or tool_result row — is content this rebuild cannot reproduce and is
// NOT listed here. Its presence declines the repair.
func claudeNonContentConversationalRow(row claudeMirrorRow) bool {
	if row.Type != "assistant" {
		return false
	}
	if row.Message != nil && row.Message.Model == "<synthetic>" {
		return true
	}
	content := row.contentBlocks()
	var blocks []claudeMirrorBlock
	if len(content) == 0 || json.Unmarshal(content, &blocks) != nil || len(blocks) == 0 {
		return false
	}
	for _, block := range blocks {
		if block.Type != "thinking" || block.Thinking != "" {
			return false
		}
	}
	return true
}

// repairForkedClaudeMirror commits the forked-graph rebuild. It runs only
// behind claudeMirrorRowsContained and only against the deterministic synthetic
// pathname the policy names.
//
// Repair-pass budget (design rule 3 — a repair pass must stay under the failure
// budget of the systems it drives). The quarantine breaker is 3 failures / 10
// minutes per adapter and blocks ALL materialization, live sync included:
//
//   - Failures charged to the breaker by this pass: ZERO. The breaker is fed
//     from exactly one call site, the Export loop in fanOut. The conversation
//     session pass reaches this code through writeConversationSession, which
//     records an adapter error string for status and never calls
//     Quarantine.RecordFailure. 0 < 3.
//   - Commit attempts per materialization pass: at most ONE.
//     rebuildDivergedClaudeMirror is reached only from the !appendable branch
//     of writeClaudeConversationSession, and every path out of that branch
//     returns, so the four-iteration snapshot-race loop cannot re-enter it.
//   - Passes per artifact: bounded by the deferral backoff, which doubles to a
//     15-minute ceiling, and by the convergence sweep's own 15-minute floor.
//     Even if the breaker did count these, one artifact yields <= 1 attempt per
//     10 minutes.
//   - A successful repair is terminal: the next pass satisfies
//     claudeSessionMatches and writes nothing, so the repair cannot loop.
func repairForkedClaudeMirror(
	path string,
	snapshot claudeSessionSnapshot,
	fullSession []byte,
	plannedTurns []acf.TextTurn,
	sessionID, threadID, branchID string,
	policy claudeMirrorRepairPolicy,
) (bool, error) {
	if !policy.repairForkedMirror || policy.syntheticDest == "" ||
		filepath.Clean(policy.syntheticDest) != filepath.Clean(path) {
		return false, nil
	}
	// Read-before-clobber: re-read the destination and require it to still be
	// the exact inode and the exact bytes the containment proof was computed
	// from. A continuation written since the snapshot aborts the repair, and
	// the caller defers exactly as it does today.
	current, exists, changed, err := readClaudeSessionSnapshot(path)
	if err != nil || !exists || changed ||
		!os.SameFile(current.info, snapshot.info) ||
		!bytes.Equal(current.raw, snapshot.raw) {
		return false, err
	}
	// Truncate-and-rewrite is not crash-atomic the way atomicfile.WriteFile's
	// rename is, and preserving the inode is the whole point here, so the
	// pre-repair bytes are made DURABLE first — fsynced, and its directory entry
	// fsynced — before the destructive truncate is issued. Ordering alone is not
	// enough: atomicfile.WriteFile fsyncs the temp file but explicitly leaves
	// the parent-directory sync to its caller, and a brand-new preimage
	// directory has never been fsynced at all, so a crash could otherwise lose
	// the only copy while the destination was already truncated.
	//
	// The in-process failure path is handled by
	// rewriteClaudeSessionIfSnapshotCurrent, which restores the original bytes
	// if anything fails after the truncate. This pre-image covers what that
	// cannot: a crash, a SIGKILL, or a power loss between the two syscalls.
	if err := writeClaudeMirrorPreimage(
		policy.preimageDir, sessionID, claudeMirrorPreimageForked, current.raw); err != nil {
		return false, err
	}
	written, raced, err := rewriteClaudeSessionIfSnapshotCurrent(path, current, fullSession)
	if err != nil {
		return false, err
	}
	if raced || !written {
		return false, nil
	}
	post, postExists, postChanged, err := readClaudeSessionSnapshot(path)
	if err != nil {
		return false, err
	}
	if !postExists || postChanged || !os.SameFile(post.info, snapshot.info) {
		// Something replaced the pathname while we held it open. Report the
		// repair as not taken so the caller defers and re-reads.
		return false, nil
	}
	return claudeSessionMatches(post.raw, plannedTurns, sessionID, threadID, branchID)
}

// rewriteClaudeSessionIfSnapshotCurrent replaces a session file's whole
// contents WITHOUT replacing its inode, under the same authorization
// appendClaudeSessionIfSnapshotCurrent demands of an append: the complete
// bytes, the inode, and the length must still match the snapshot the new
// content was derived from, verified both through the open descriptor and
// through the pathname, before and after the commit.
//
// Claude Code opens-appends-closes per write and holds no descriptor on a
// session file, so inode preservation is insurance rather than a load-bearing
// assumption. It is cheap insurance, and it keeps this mutation in the same
// house style as every other mutation the adapter is allowed to make.
func rewriteClaudeSessionIfSnapshotCurrent(
	path string, snapshot claudeSessionSnapshot, content []byte,
) (written, changed bool, err error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
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
	if _, err = f.Seek(0, io.SeekStart); err != nil {
		return false, false, err
	}
	existing, err := io.ReadAll(f)
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
		verifiedInfo.Size() != int64(len(existing)) || !bytes.Equal(existing, snapshot.raw) {
		return false, true, nil
	}
	// From here the file's contents are destroyed until the new bytes land. Any
	// failure below puts the snapshot back on the SAME inode, so an ENOSPC or an
	// I/O error can never leave the mirror empty — a state the adapter could not
	// recover from and which makes the thread vanish from /resume.
	restore := func(cause error) error { return restoreClaudeSessionBytes(f, snapshot.raw, cause) }
	if err = f.Truncate(0); err != nil {
		return false, false, err
	}
	if _, err = f.Seek(0, io.SeekStart); err != nil {
		return false, false, restore(err)
	}
	n, err := f.Write(content)
	if err != nil {
		return false, false, restore(err)
	}
	if n != len(content) {
		return false, false, restore(io.ErrShortWrite)
	}
	if err = f.Sync(); err != nil {
		return false, false, restore(err)
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
	if !os.SameFile(verifiedInfo, finalInfo) || !os.SameFile(finalInfo, pathInfo) {
		// The pathname now names a different inode than the one we just wrote.
		// The bytes are on the old inode; report a race so the caller defers.
		return false, true, nil
	}
	return true, false, nil
}

// restoreClaudeSessionBytes rewrites raw over an already-truncated descriptor
// and returns cause, joined with whatever went wrong during the rollback.
//
// It exists because the inode-preserving commit has a window in which the file
// holds nothing: preserving the inode means the destination cannot be published
// by rename, so truncate-then-write is the only shape available. Leaving that
// window unhandled is what made a failed commit terminal — an empty session
// file has no thread stamp, so it can be neither authenticated, appended to,
// nor rebuilt.
func restoreClaudeSessionBytes(f *os.File, raw []byte, cause error) error {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return errors.Join(cause, err)
	}
	if err := f.Truncate(0); err != nil {
		return errors.Join(cause, err)
	}
	if _, err := f.Write(raw); err != nil {
		return errors.Join(cause, err)
	}
	if err := f.Sync(); err != nil {
		return errors.Join(cause, err)
	}
	return cause
}

// writeClaudeMirrorPreimage preserves the pre-repair bytes outside ~/.claude,
// beside the sessions bestEffortQuarantineClaudeThreadDuplicates already
// quarantines. Two properties matter: a quarantined copy is invisible to
// /resume, so preserving one can never create a second session for the thread;
// and the name is derived from the pre-image's own content hash, so repairing
// the same state twice reuses one file while two genuinely different pre-images
// never collide.
//
// kind names which repair took the copy, so a native transcript's pre-image is
// distinguishable from a synthetic mirror's at a glance. The two populations
// need very different recovery instructions: one is Aplexica's own file, the
// other is the user's.
func writeClaudeMirrorPreimage(dir, sessionID, kind string, raw []byte) error {
	if dir == "" {
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	dest := filepath.Join(dir, fmt.Sprintf("%s-%s.%s.jsonl", sessionID, shortHashBytes(raw), kind))
	if _, err := os.Stat(dest); err == nil {
		return nil
	}
	if err := atomicfile.WriteFile(dest, raw, claudeSessionFileMode); err != nil {
		return err
	}
	// atomicfile fsyncs the bytes and renames atomically, but per its own
	// contract the rename's effect on the parent directory is the caller's to
	// make durable — and MkdirAll above may have created that directory for the
	// first time. Without this, a crash immediately after the truncate could
	// find the destination emptied and the pre-image's directory entry
	// unrecovered: both copies gone. The whole point of taking a pre-image is
	// that it is on disk BEFORE the destructive write, so its durability is
	// load-bearing, not best-effort.
	return fsyncClaudeMirrorDir(dir)
}

// fsyncClaudeMirrorDir makes a directory entry durable. A directory fsync is
// not supported on every platform or filesystem (notably Windows, where opening
// a directory handle for sync is rejected); those known-benign cases are
// tolerated rather than failing an otherwise-successful write. Genuine I/O
// errors are returned, because on this path a lost pre-image is a lost
// transcript.
func fsyncClaudeMirrorDir(dir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	syncErr := d.Sync()
	closeErr := d.Close()
	if syncErr != nil {
		if errors.Is(syncErr, errors.ErrUnsupported) ||
			errors.Is(syncErr, os.ErrInvalid) ||
			errors.Is(syncErr, syscall.EINVAL) ||
			errors.Is(syncErr, syscall.ENOTSUP) {
			return nil
		}
		return syncErr
	}
	return closeErr
}
