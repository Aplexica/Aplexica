package openclaw

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter"
	"github.com/aplexica/aplexica/internal/atomicfile"
)

// OpenClaw's LLM backend is configurable (codex today, Claude API tomorrow),
// so sync must NOT target the backend's store (e.g. the codex-home rollouts
// under agents/<id>/agent/) — those are backend-specific. The backend-AGNOSTIC
// surface is OpenClaw's own session store at ~/.openclaw/agents/<id>/sessions/:
//
//   - sessions.json — index keyed "agent:<agentId>:<name>", one entry per
//     session (sessionId, sessionFile, timestamps, model attribution).
//   - <sessionId>.jsonl — transcript in OpenClaw's versioned session format
//     ({"type":"session","version":3,...} header, then {"type":"message",...}
//     lines chained by parentId).
//
// The gateway reads this store regardless of backend: `openclaw sessions`
// lists injected entries, the agent's sessions_list/sessions_history tools see
// them, and `openclaw agent --session-key <key>` CONTINUES them with full
// context while the session is inside OpenClaw's idle-reset window — which
// freshly-synced conversations always are. All of this was validated live
// against OpenClaw 2026.6.5 before this implementation (including that the
// running gateway preserves foreign index entries across its own rewrites).
// There is no import CLI to shell out to: `openclaw migrate` providers cover
// config/skills/credentials only, and `openclaw sessions` is read-only.

// aplexicaImportMarker is written into the session header line as
// "_aplexica" so ImportConversation can recognize transcripts the daemon
// itself materialized and skip them (echo guard — the class fixed for hermes
// with the aplexica:canonical-import source marker). The extra header field
// is tolerated by OpenClaw's parser (verified live).
const aplexicaImportMarker = "canonical-import"

// syncedSessionIDPrefix starts every materialized sessionId (and therefore
// transcript filename). Native ids are UUIDs, so the prefix can't collide;
// it doubles as visible provenance in `openclaw sessions list`.
const syncedSessionIDPrefix = "aplx"

// sessionFormatVersion mirrors the "version" field of native session headers
// written by OpenClaw 2026.6.5.
const sessionFormatVersion = 3

// Deterministic-id geometry — wire-format constants, not tunables. The seed
// is the artifact id with dashes stripped, padded to seedLen hex chars; the
// sessionId regroups it 8-4-4-4-12 (native UUID shape) with the 4-char
// "aplx" prefix consuming the head of the first group.
const (
	seedLen      = 32
	idCut1       = 4  // prefix + seed[:idCut1] = first 8-char group
	idCut2       = 8  // second group
	idCut3       = 12 // third group
	idCut4       = 16 // fourth group
	idCut5       = 28 // fifth group end (12 chars)
	msgIDSeedLen = 12 // seed chars embedded in per-message ids
	// Index-key slug limits: the key is the only label `openclaw sessions
	// list` shows, so it carries a readable slug plus the seed's random
	// TAIL as a uniqueness suffix (UUIDv7 heads are timestamp-shared).
	keySlugMax     = 48
	keySuffixStart = 24
)

// MaterializeConversationSession (adapter.ConversationSessionTarget)
// transcodes a foreign conversation into OpenClaw's native session store so
// it appears in `openclaw sessions` and is continuable via its session key.
// Opts out (supports=false) when no agent session store exists yet or the
// payload has no text turns. Returns the transcript path written.
// MaterializeConversationSessionReason (adapter.ConversationSessionDeclineReporter)
// is MaterializeConversationSession plus the typed reason for a decline.
// OpenClaw rewrites its transcript wholesale rather than appending to a file a
// live agent co-owns, so it has no race or prefix relation to report: every
// supports=false result without an error is the permanent opt-out from
// conversationSessionPlan (no session store, an unsupported payload format, or
// no text turns).
func (a *Adapter) MaterializeConversationSessionReason(
	art acf.Artifact, head acf.Event, sourceAgent string,
) (string, bool, adapter.SessionDeclineReason, error) {
	path, ok, err := a.MaterializeConversationSession(art, head, sourceAgent)
	if err != nil || ok {
		return path, ok, adapter.SessionDeclineUnspecified, err
	}
	return path, false, adapter.SessionDeclineOptOut, nil
}

func (a *Adapter) MaterializeConversationSession(art acf.Artifact, head acf.Event, sourceAgent string) (string, bool, error) {
	plan, ok, err := a.conversationSessionPlan(art, head, sourceAgent)
	if err != nil || !ok {
		return "", ok, err
	}
	if err := os.MkdirAll(plan.dir, 0o700); err != nil {
		return "", false, fmt.Errorf("openclaw: session store dir: %w", err)
	}
	// 0600 matches the perms OpenClaw itself writes session files with.
	if err := atomicfile.WriteFile(plan.sessionPath, plan.doc.transcript, 0o600); err != nil {
		return "", false, fmt.Errorf("openclaw: write session transcript: %w", err)
	}
	plan.doc.index.SessionFile = plan.sessionPath
	if err := upsertSessionIndex(filepath.Join(plan.dir, "sessions.json"), plan.key, plan.doc.index); err != nil {
		return "", false, err
	}
	return plan.sessionPath, true, nil
}

// ConversationSessionPath lets the orchestrator recursion-guard OpenClaw's
// deterministic session transcript before MaterializeConversationSession
// writes it.
func (a *Adapter) ConversationSessionPath(art acf.Artifact, head acf.Event, sourceAgent string) (string, bool, error) {
	path, ok, _, err := a.ConversationSessionPathReason(art, head, sourceAgent)
	return path, ok, err
}

// ConversationSessionPathReason (adapter.ConversationSessionPathDeclineReporter)
// is ConversationSessionPath plus the typed reason; the planner's only decline
// is the same permanent opt-out the writer reports.
func (a *Adapter) ConversationSessionPathReason(
	art acf.Artifact, head acf.Event, sourceAgent string,
) (string, bool, adapter.SessionDeclineReason, error) {
	plan, ok, err := a.conversationSessionPlan(art, head, sourceAgent)
	if err != nil {
		return "", false, adapter.SessionDeclineUnspecified, err
	}
	if !ok {
		return "", false, adapter.SessionDeclineOptOut, nil
	}
	return plan.sessionPath, true, adapter.SessionDeclineUnspecified, nil
}

type openclawConversationSessionPlan struct {
	dir         string
	sessionPath string
	key         string
	doc         openclawSessionDoc
}

func (a *Adapter) conversationSessionPlan(art acf.Artifact, head acf.Event, sourceAgent string) (openclawConversationSessionPlan, bool, error) {
	dir, agentID, ok := a.sessionStoreDir()
	if !ok {
		return openclawConversationSessionPlan{}, false, nil
	}
	p, err := acf.DecodeConversationPayload(head)
	if err != nil {
		return openclawConversationSessionPlan{}, false, err
	}
	var turns []acf.TextTurn
	switch p.Format {
	case acf.ConversationFormatV1:
		turns = acf.ExtractTextTurns(p.Events)
	case acf.ConversationFormatHermesBundle:
		turns = acf.TurnsFromHermesBundleJSON(p.Content)
	default:
		return openclawConversationSessionPlan{}, false, nil
	}
	if len(turns) == 0 {
		return openclawConversationSessionPlan{}, false, nil
	}

	doc := buildOpenclawSessionForBranch(art, turns, sourceAgent, a.workspaceDir(), head.Branch)
	sessionPath := filepath.Join(dir, doc.sessionID+".jsonl")
	key := "agent:" + agentID + ":" + doc.keyName
	return openclawConversationSessionPlan{dir: dir, sessionPath: sessionPath, key: key, doc: doc}, true, nil
}

// sessionStoreDir locates the session store of the default agent: prefers
// "main" (OpenClaw's default agent id), else the first agent dir sorted.
// ok=false when OpenClaw has no agents dir at all (never initialized).
func (a *Adapter) sessionStoreDir() (dir, agentID string, ok bool) {
	if a.HomeDir == "" {
		return "", "", false
	}
	agentsRoot := filepath.Join(a.HomeDir, ".openclaw", "agents")
	entries, err := os.ReadDir(agentsRoot)
	if err != nil {
		return "", "", false
	}
	var ids []string
	for _, e := range entries {
		if e.IsDir() {
			ids = append(ids, e.Name())
		}
	}
	if len(ids) == 0 {
		return "", "", false
	}
	sort.Strings(ids)
	agentID = ids[0]
	for _, id := range ids {
		if id == "main" {
			agentID = "main"
		}
	}
	return filepath.Join(agentsRoot, agentID, "sessions"), agentID, true
}

func (a *Adapter) workspaceDir() string {
	if a.HomeDir == "" {
		return ""
	}
	return filepath.Join(a.HomeDir, ".openclaw", "workspace")
}

// openclawSessionDoc is the assembled materialization: the transcript bytes,
// the deterministic ids, and the index entry to upsert.
type openclawSessionDoc struct {
	sessionID  string
	keyName    string // the part after "agent:<id>:" in the index key
	transcript []byte
	index      sessionIndexEntry
}

// sessionIndexEntry mirrors the fields of a native sessions.json entry that
// the CLI/gateway read (verified live: this minimal set lists and resumes).
type sessionIndexEntry struct {
	SessionID         string `json:"sessionId"`
	UpdatedAt         int64  `json:"updatedAt"`
	SessionStartedAt  int64  `json:"sessionStartedAt"`
	LastInteractionAt int64  `json:"lastInteractionAt"`
	SessionFile       string `json:"sessionFile"`
	ModelProvider     string `json:"modelProvider"`
	Model             string `json:"model"`
	AbortedLastRun    bool   `json:"abortedLastRun"`
	InputTokens       int    `json:"inputTokens"`
	OutputTokens      int    `json:"outputTokens"`
	TotalTokens       int    `json:"totalTokens"`
}

// Transcript line shapes, mirroring native files written by OpenClaw 2026.6.5.
type sessionHeaderLine struct {
	Type             string `json:"type"`
	Version          int    `json:"version"`
	ID               string `json:"id"`
	Timestamp        string `json:"timestamp"`
	Cwd              string `json:"cwd"`
	Aplexica         string `json:"_aplexica"`
	AplexicaThreadID string `json:"aplexicaThreadId,omitempty"`
	AplexicaBranchID string `json:"aplexicaBranchId,omitempty"`
}

type sessionMessageLine struct {
	Type      string         `json:"type"`
	ID        string         `json:"id"`
	ParentID  *string        `json:"parentId"` // null for the first message
	Timestamp string         `json:"timestamp"`
	Message   map[string]any `json:"message"`
}

// buildOpenclawSession assembles the materialization. All ids derive
// deterministically from the artifact id so re-materialization overwrites the
// SAME session (same file, same index key) instead of accumulating
// duplicates. Timestamps come from the artifact's UpdatedAt: a just-synced
// conversation is stamped "now", which keeps it inside OpenClaw's idle-reset
// window and therefore continuable.
// openclawSyncedSessionID is the deterministic OpenClaw sessionId the daemon
// assigns when it materializes a canonical conversation: syncedSessionIDPrefix +
// the dash-stripped artifact id regrouped into UUID shape. The seed is TRUNCATED
// to seed[:idCut5] (28 < a UUID's 32 hex chars), so the id can't be parsed back;
// the import path recomputes it per artifact (a forward map) to recover the
// canonical thread.
func openclawSyncedSessionID(artifactID string) string {
	return openclawSyncedSessionIDForBranch(artifactID, acf.MainBranch)
}

func openclawSyncedSessionIDForBranch(artifactID, branchID string) string {
	seed := padSeed(openclawSessionSeed(artifactID, branchID), seedLen)
	return syncedSessionIDPrefix + seed[:idCut1] + "-" + seed[idCut1:idCut2] + "-" + seed[idCut2:idCut3] + "-" + seed[idCut3:idCut4] + "-" + seed[idCut4:idCut5]
}

func buildOpenclawSession(art acf.Artifact, turns []acf.TextTurn, sourceAgent, workspaceDir string) openclawSessionDoc {
	return buildOpenclawSessionForBranch(art, turns, sourceAgent, workspaceDir, acf.MainBranch)
}

func buildOpenclawSessionForBranch(art acf.Artifact, turns []acf.TextTurn, sourceAgent, workspaceDir, branchID string) openclawSessionDoc {
	branchID = normalizeOpenClawBranchID(branchID)
	seed := padSeed(openclawSessionSeed(art.ArtifactID, branchID), seedLen)
	sessionID := openclawSyncedSessionIDForBranch(art.ArtifactID, branchID)

	base := art.UpdatedAt.UTC()
	if base.IsZero() {
		base = art.CreatedAt.UTC()
	}
	baseMS := base.UnixMilli()

	var lines [][]byte
	header, _ := json.Marshal(sessionHeaderLine{
		Type:             "session",
		Version:          sessionFormatVersion,
		ID:               sessionID,
		Timestamp:        base.Format("2006-01-02T15:04:05.000Z"),
		Cwd:              workspaceDir,
		Aplexica:         aplexicaImportMarker,
		AplexicaThreadID: art.ArtifactID,
		AplexicaBranchID: branchID,
	})
	lines = append(lines, header)

	var prevID *string
	for i, t := range turns {
		msgID := fmt.Sprintf("%s-%s-%06d", syncedSessionIDPrefix, seed[:msgIDSeedLen], i)
		ts := base.Add(time.Duration(i) * time.Millisecond)
		tsMS := baseMS + int64(i)
		// Role-specific body shapes per native files: user content is a
		// plain string; assistant content is a text-block array. The
		// sourceChannel/provider fields carry provenance into OpenClaw's
		// own rendering ("model: <sourceAgent>" in `sessions list`).
		var msg map[string]any
		if t.Role == "assistant" {
			msg = map[string]any{
				"role":       "assistant",
				"content":    []any{map[string]any{"type": "text", "text": t.Text}},
				"provider":   "aplexica",
				"model":      sourceAgent,
				"timestamp":  tsMS,
				"stopReason": "stop",
			}
		} else {
			msg = map[string]any{
				"role":          "user",
				"content":       t.Text,
				"timestamp":     tsMS,
				"sourceChannel": "aplexica-sync",
			}
		}
		line, _ := json.Marshal(sessionMessageLine{
			Type:      "message",
			ID:        msgID,
			ParentID:  prevID,
			Timestamp: ts.Format("2006-01-02T15:04:05.000Z"),
			Message:   msg,
		})
		lines = append(lines, line)
		id := msgID
		prevID = &id
	}

	return openclawSessionDoc{
		sessionID:  sessionID,
		keyName:    sessionKeyName(sourceAgent, turns, branchID, seed),
		transcript: append([]byte(strings.Join(stringsOf(lines), "\n")), '\n'),
		index: sessionIndexEntry{
			SessionID:         sessionID,
			UpdatedAt:         baseMS + int64(len(turns)),
			SessionStartedAt:  baseMS,
			LastInteractionAt: baseMS + int64(len(turns)),
			ModelProvider:     "aplexica",
			Model:             sourceAgent,
		},
	}
}

func openclawSessionSeed(artifactID, branchID string) string {
	branchID = normalizeOpenClawBranchID(branchID)
	if branchID == acf.MainBranch {
		return strings.ReplaceAll(artifactID, "-", "")
	}
	sum := sha256.Sum256([]byte(artifactID + ":" + branchID))
	return hex.EncodeToString(sum[:])
}

func normalizeOpenClawBranchID(branchID string) string {
	if branchID == "" {
		return acf.MainBranch
	}
	norm, err := acf.NormalizeBranchName(branchID)
	if err != nil {
		return acf.MainBranch
	}
	return norm
}

// sessionKeyName builds the human-readable index-key tail. The key is the
// only label `openclaw sessions list` shows, so it carries the origin agent
// and the first real user message as a slug, plus a seed suffix so distinct
// conversations with identical openings don't collide:
// "aplx-codex-what-is-the-capital-of-france-8812d7f1". The suffix comes from
// the seed's TAIL: artifact ids are UUIDv7-style, whose head is
// timestamp-derived and shared by everything created in the same few-hour
// window (a head suffix collapsed same-slug conversations onto one key in
// the first live backfill); the tail bits are random.
func sessionKeyName(sourceAgent string, turns []acf.TextTurn, branchID, seed string) string {
	lead := turns[0].Text
	for _, t := range turns {
		if t.Role == "user" && !acf.IsHiddenUserContext(t.Text) {
			lead = t.Text
			break
		}
	}
	label := sourceAgent + " " + lead
	branchID = normalizeOpenClawBranchID(branchID)
	if branchID != acf.MainBranch {
		label = branchID + " " + label
	}
	return syncedSessionIDPrefix + "-" + slugify(label, keySlugMax) + "-" + seed[keySuffixStart:]
}

// sessionIndexMu serializes sessions.json read-modify-write cycles within
// this process. The orchestrator materializes many conversations concurrently
// (startup scan, gate-toggle backfill), and unserialized upserts otherwise lose
// one another's entries wholesale.
var sessionIndexMu sync.Mutex

// upsertSessionIndex sets one key in sessions.json, preserving every other
// entry byte-for-byte (json.RawMessage) — the index is shared with the live
// gateway, which keeps its own entries' rich fields (skillsSnapshot,
// systemPromptReport, …) that we must not strip. A parse failure aborts
// rather than clobbering a file we can't faithfully rewrite.
func upsertSessionIndex(indexPath, key string, entry sessionIndexEntry) error {
	sessionIndexMu.Lock()
	defer sessionIndexMu.Unlock()
	idx := map[string]json.RawMessage{}
	if b, err := os.ReadFile(indexPath); err == nil {
		if err := json.Unmarshal(b, &idx); err != nil {
			return fmt.Errorf("openclaw: sessions.json unparseable, refusing to rewrite: %w", err)
		}
	}
	raw, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("openclaw: marshal index entry: %w", err)
	}
	idx[key] = raw
	out, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return fmt.Errorf("openclaw: marshal sessions.json: %w", err)
	}
	return atomicfile.WriteFile(indexPath, out, 0o600)
}

// sessionFileIsCanonicalImport reports whether the transcript at path was
// materialized by this adapter (header line carries the _aplexica marker).
// ImportConversation skips such files so a synced foreign conversation never
// round-trips back into the store as a new openclaw-sourced artifact.
func sessionFileIsCanonicalImport(path string) bool {
	hdr, ok := readOpenclawSessionHeader(path)
	return ok && hdr.Aplexica == aplexicaImportMarker
}

func readOpenclawSessionHeader(path string) (sessionHeaderLine, bool) {
	f, err := os.Open(path)
	if err != nil {
		return sessionHeaderLine{}, false
	}
	defer f.Close()
	line, err := bufio.NewReader(f).ReadString('\n')
	if err != nil && line == "" {
		return sessionHeaderLine{}, false
	}
	var hdr sessionHeaderLine
	if json.Unmarshal([]byte(line), &hdr) != nil {
		return sessionHeaderLine{}, false
	}
	return hdr, hdr.Type == "session"
}

func slugify(s string, max int) string {
	var b strings.Builder
	lastDash := true // suppress a leading dash
	for _, r := range strings.ToLower(s) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
		}
		if b.Len() >= max {
			break
		}
	}
	return strings.TrimRight(b.String(), "-")
}

func padSeed(s string, n int) string {
	if len(s) >= n {
		return s[:n]
	}
	return s + strings.Repeat("0", n-len(s))
}

func stringsOf(lines [][]byte) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = string(l)
	}
	return out
}
