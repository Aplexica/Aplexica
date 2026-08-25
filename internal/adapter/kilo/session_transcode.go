package kilo

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter"
	"github.com/aplexica/aplexica/internal/filelock"
	"github.com/aplexica/aplexica/internal/privatefs"
)

// syncedSessionIDPrefix marks Kilo sessions the daemon itself imported via
// `kilo import`. ImportConversationsFromDB skips sessions carrying it so a
// materialized foreign conversation never round-trips back into the store
// as a new kilo-sourced artifact (the echo class fixed for hermes with the
// aplexica:canonical-import source marker — kilo sessions have no source
// column, so the marker lives in the id).
const syncedSessionIDPrefix = "ses_aplx"

// kiloImportTimeout bounds the `kilo import` subprocess. The CLI does a
// local SQLite write; anything beyond this is a hang, not work.
const kiloImportTimeout = 60 * time.Second

// kiloImportLockName serializes the CLI import plus exact-row cleanup across
// overlapping Aplexica processes. The in-process mutex below avoids needless
// local lock contention; this file lock is the authoritative cross-process
// guard. It intentionally lives in Aplexica's private state directory rather
// than Kilo's watched data directory.
const kiloImportLockName = ".kilo-session-import.lock"

// kiloImportMu serializes `kilo import` subprocesses; the CLI writes one local
// DB and concurrent imports can otherwise burn CPU while contending.
var kiloImportMu sync.Mutex

// Id-segment lengths mirror the shape of native kilo identifiers
// (ses_/msg_/prt_ + base62 tail) — wire-format constants, not tunables.
const (
	sessionIDSeedLen    = 24
	slugSeedLen         = 10
	msgIDSeedLen        = 16
	generatedIDIndexLen = 6
)

// Synthesized sessions reference kilo's free auto model — any valid
// provider/model pair satisfies the import schema; the session is a
// read-mostly mirror, not a continuation target with billing.
const (
	syncedProviderID = "kilo"
	syncedModelID    = "kilo-auto/free"
)

// Kilo has no documented general write surface for kilo.db (a Drizzle-managed
// schema that migrates between releases), but it DOES ship a version-stable
// session interchange format: `kilo export` / `kilo import <json>`.
// MaterializeConversationSession therefore delegates every create/update to
// `kilo import`. The CLI is idempotent on the session id but only upserts rows;
// session_cleanup.go follows a successful import with a schema-gated,
// transactionally exact deletion of obsolete Aplexica-owned rows. It never
// constructs native rows or mutates a user-created session.
//
// The structs below mirror the observed `kilo export` shape; field set kept
// close to a real export so the CLI's schema validation passes.
type kiloExportFile struct {
	Info     kiloExportInfo      `json:"info"`
	Messages []kiloExportMessage `json:"messages"`
}

type kiloExportInfo struct {
	ID        string          `json:"id"`
	Slug      string          `json:"slug"`
	ProjectID string          `json:"projectID"`
	Directory string          `json:"directory"`
	Path      string          `json:"path"`
	Title     string          `json:"title"`
	Agent     string          `json:"agent"`
	Model     kiloExportModel `json:"model"`
	Version   string          `json:"version"`
	Summary   struct {
		Additions int `json:"additions"`
		Deletions int `json:"deletions"`
		Files     int `json:"files"`
	} `json:"summary"`
	Permission []struct{} `json:"permission"`
	Time       struct {
		Created int64 `json:"created"`
		Updated int64 `json:"updated"`
	} `json:"time"`
}

type kiloExportModel struct {
	ID         string `json:"id"`
	ProviderID string `json:"providerID"`
	Variant    string `json:"variant,omitempty"`
}

type kiloExportMessage struct {
	// Info is schema-mirroring: kilo validates ROLE-SPECIFIC shapes (a
	// user message carries model+summary; an assistant message carries
	// parentID/mode/path/cost/tokens/modelID/providerID/finish). The CLI
	// silently DROPS messages that fail validation while still exiting 0
	// and creating the session row — which is how the first cut shipped
	// sessions with titles but empty transcripts.
	Info  map[string]any   `json:"info"`
	Parts []kiloExportPart `json:"parts"`
}

type kiloExportPart struct {
	ID        string `json:"id"`
	SessionID string `json:"sessionID"`
	MessageID string `json:"messageID"`
	Type      string `json:"type"`
	Text      string `json:"text"`
}

// MaterializeConversationSession (adapter.ConversationSessionTarget)
// transcodes a foreign conversation into Kilo's session store via the
// `kilo import` CLI so it appears in Kilo's session list. Opts out
// (supports=false) when the kilo binary isn't findable or the payload has
// no text turns. The returned path is the interchange file written under
// the daemon's temp dir; the actual store is kilo.db, which the CLI owns.
// MaterializeConversationSessionReason (adapter.ConversationSessionDeclineReporter)
// is MaterializeConversationSession plus the typed reason for a decline.
// `kilo import` either succeeds or fails loudly, so this adapter never observes
// a snapshot race or a prefix relation it could report: every supports=false
// result without an error is one of the permanent opt-outs at the top of
// MaterializeConversationSession (no kilo binary, an unsupported payload
// format, or no text turns).
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
	bin := kiloBinary(a.HomeDir)
	if bin == "" {
		return "", false, nil
	}
	p, err := acf.DecodeConversationPayload(head)
	if err != nil {
		return "", false, err
	}
	var turns []acf.TextTurn
	switch p.Format {
	case acf.ConversationFormatV1:
		turns = acf.ExtractTextTurns(p.Events)
	case acf.ConversationFormatHermesBundle:
		turns = acf.TurnsFromHermesBundleJSON(p.Content)
	default:
		return "", false, nil
	}
	if len(turns) == 0 {
		return "", false, nil
	}

	exportDoc := buildKiloExportForBranch(art, turns, sourceAgent, a.HomeDir, head.Branch)
	doc, err := json.Marshal(exportDoc)
	if err != nil {
		return "", false, fmt.Errorf("kilo: marshal import file: %w", err)
	}
	tmp, err := os.CreateTemp("", "aplexica-kilo-import-*.json")
	if err != nil {
		return "", false, fmt.Errorf("kilo: temp import file: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(doc); err != nil {
		tmp.Close()
		return "", false, fmt.Errorf("kilo: write import file: %w", err)
	}
	tmp.Close()

	kiloImportMu.Lock()
	defer kiloImportMu.Unlock()
	lock, err := a.acquireKiloImportLock()
	if err != nil {
		return "", false, fmt.Errorf("kilo: acquire import lock: %w", err)
	}
	defer lock.Close()

	ctx, cancel := context.WithTimeout(context.Background(), kiloImportTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "import", tmp.Name())
	hideImportWindow(cmd)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", false, fmt.Errorf("kilo: import CLI failed: %w (output: %s)", err, truncateOut(out))
	}
	// The CLI exits 0 even when schema validation DROPS messages (it
	// still creates the session row) — surface that as an error so the
	// failure is visible in AdapterLastErrors instead of producing
	// title-only sessions with empty transcripts.
	if s := strings.ToLower(string(out)); strings.Contains(s, "missing key") || strings.Contains(s, "error") {
		return "", false, fmt.Errorf("kilo: import validation rejected content: %s", truncateOut(out))
	}
	// `kilo import` is an upsert, not an exact replacement. When a canonical
	// thread shrinks during repair, deterministic rows from the older, longer
	// projection otherwise survive forever. Import first so a crash can only
	// leave the old extra rows (never a missing session), then prune obsolete
	// Aplexica-owned rows in one SQLite transaction. Native Kilo rows are never
	// removed, including continuations made by the user in a synced session.
	if err := a.cleanupImportedKiloSession(exportDoc); err != nil {
		return "", false, err
	}
	return tmp.Name(), true, nil
}

func (a *Adapter) acquireKiloImportLock() (*filelock.Lock, error) {
	if a.HomeDir == "" {
		return nil, fmt.Errorf("home directory is empty")
	}
	stateDir := filepath.Join(a.HomeDir, ".aplexica", "state")
	if err := privatefs.EnsureDir(stateDir, privatefs.DirPolicy{
		Access:        privatefs.AccessPrivate,
		RepairOwned:   true,
		AllowExisting: true,
	}); err != nil {
		return nil, err
	}
	return filelock.Acquire(filepath.Join(stateDir, kiloImportLockName), kiloImportTimeout)
}

// kiloSyncedSessionID is the deterministic kilo session id the daemon assigns
// when it materializes a canonical conversation into kilo: syncedSessionIDPrefix
// + the dash-stripped artifact id, padded/truncated to sessionIDSeedLen. Because
// the seed is TRUNCATED (24 < 32), the id can't be parsed back to the artifact
// id; the import path recomputes it per artifact (a forward map) to map a
// re-imported synced session to its canonical thread.
func kiloSyncedSessionID(artifactID string) string {
	return kiloSyncedSessionIDForBranch(artifactID, acf.MainBranch)
}

func kiloSyncedSessionIDForBranch(artifactID, branchID string) string {
	return syncedSessionIDPrefix + pad(kiloSessionSeed(artifactID, branchID), sessionIDSeedLen)
}

func kiloSessionSeed(artifactID, branchID string) string {
	branchID = normalizeKiloBranchID(branchID)
	if branchID == acf.MainBranch {
		return strings.ReplaceAll(artifactID, "-", "")
	}
	sum := sha256.Sum256([]byte(artifactID + ":" + branchID))
	return hex.EncodeToString(sum[:])
}

// buildKiloExport assembles the interchange document. All ids derive
// deterministically from the artifact id so re-materialization targets the
// SAME kilo session (the CLI dedupes on session id).
func buildKiloExport(art acf.Artifact, turns []acf.TextTurn, sourceAgent, homeDir string) kiloExportFile {
	return buildKiloExportForBranch(art, turns, sourceAgent, homeDir, acf.MainBranch)
}

func buildKiloExportForBranch(art acf.Artifact, turns []acf.TextTurn, sourceAgent, homeDir, branchID string) kiloExportFile {
	seed := kiloSessionSeed(art.ArtifactID, branchID)
	sessionID := kiloSyncedSessionIDForBranch(art.ArtifactID, branchID)
	// Last-activity stamp, not creation: Kilo's session panel orders by
	// recency, and a creation-time stamp buried an actively-continuing
	// conversation deep in the list. UpdatedAt bumps on every imported
	// turn. Mirrors the openclaw and claude-code session materializers.
	base := art.UpdatedAt.UTC()
	if base.IsZero() {
		base = art.CreatedAt.UTC()
	}
	baseMS := base.UnixMilli()

	title := adapter.ConversationBranchDisplayTitle(
		"↪ "+titleWord(sourceAgent)+": "+firstUserLine(turns),
		branchID,
	)

	var f kiloExportFile
	f.Info = kiloExportInfo{
		ID:        sessionID,
		Slug:      "aplexica-" + pad(seed, slugSeedLen),
		ProjectID: "global",
		Directory: homeDir,
		Path:      strings.TrimPrefix(homeDir, "/"),
		Title:     title,
		Agent:     "code",
		Model:     kiloExportModel{ID: syncedModelID, ProviderID: syncedProviderID, Variant: "default"},
		Version:   "aplexica-sync",
	}
	f.Info.Permission = []struct{}{}
	f.Info.Time.Created = baseMS
	f.Info.Time.Updated = baseMS + int64(len(turns))

	lastUserID := ""
	// When the visible thread begins with an assistant turn (its leading user
	// turn was injected context and filtered out by ExtractTextTurns), there is
	// no user message to parent it to. Emit a structural root user message so
	// the leading assistant chains to a valid parentID instead of "" (which
	// risks Kilo dropping the turn), preserving the assistant turn losslessly.
	if len(turns) > 0 && turns[0].Role == "assistant" {
		rootID := "msg_aplxroot" + pad(seed, msgIDSeedLen)
		f.Messages = append(f.Messages, kiloExportMessage{
			Info: map[string]any{
				"role":      "user",
				"time":      map[string]any{"created": baseMS},
				"agent":     "code",
				"model":     map[string]any{"providerID": syncedProviderID, "modelID": syncedModelID},
				"summary":   map[string]any{"diffs": []any{}},
				"id":        rootID,
				"sessionID": sessionID,
			},
			Parts: []kiloExportPart{{
				ID:        "prt_aplxroot" + pad(seed, msgIDSeedLen),
				SessionID: sessionID,
				MessageID: rootID,
				Type:      "text",
				Text:      "",
			}},
		})
		lastUserID = rootID
	}
	for i, t := range turns {
		msgID := "msg_aplx" + pad(seed, msgIDSeedLen) + fmt.Sprintf("%06d", i)
		created := baseMS + int64(i)
		var info map[string]any
		if t.Role == "assistant" {
			info = map[string]any{
				"parentID":   lastUserID,
				"role":       "assistant",
				"mode":       "code",
				"agent":      "code",
				"path":       map[string]any{"cwd": homeDir, "root": "/"},
				"cost":       0,
				"tokens":     map[string]any{"total": 0, "input": 0, "output": 0, "reasoning": 0, "cache": map[string]any{"write": 0, "read": 0}},
				"modelID":    syncedModelID,
				"providerID": syncedProviderID,
				"time":       map[string]any{"created": created, "completed": created},
				"finish":     "stop",
				"id":         msgID,
				"sessionID":  sessionID,
			}
		} else {
			info = map[string]any{
				"role":      "user",
				"time":      map[string]any{"created": created},
				"agent":     "code",
				"model":     map[string]any{"providerID": syncedProviderID, "modelID": syncedModelID},
				"summary":   map[string]any{"diffs": []any{}},
				"id":        msgID,
				"sessionID": sessionID,
			}
			lastUserID = msgID
		}
		m := kiloExportMessage{Info: info}
		m.Parts = []kiloExportPart{{
			ID:        "prt_aplx" + pad(seed, msgIDSeedLen) + fmt.Sprintf("%06d", i),
			SessionID: sessionID,
			MessageID: msgID,
			Type:      "text",
			Text:      t.Text,
		}}
		f.Messages = append(f.Messages, m)
	}
	return f
}

func normalizeKiloBranchID(branchID string) string {
	if branchID == "" {
		return acf.MainBranch
	}
	norm, err := acf.NormalizeBranchName(branchID)
	if err != nil {
		return acf.MainBranch
	}
	return norm
}

// kiloBinary locates the kilo CLI: PATH first, then the newest VS Code
// extension bundle (where Kilo Code ships it). Empty string = not found.
func kiloBinary(homeDir string) string {
	if p, err := exec.LookPath("kilo"); err == nil {
		return p
	}
	if homeDir == "" {
		return ""
	}
	matches, _ := filepath.Glob(filepath.Join(homeDir, ".vscode", "extensions", "kilocode.kilo-code-*", "bin", "kilo"))
	if len(matches) == 0 {
		return ""
	}
	sort.Strings(matches)
	return matches[len(matches)-1]
}

func firstUserLine(turns []acf.TextTurn) string {
	for _, t := range turns {
		if t.Role == "user" && !acf.IsHiddenUserContext(t.Text) {
			return truncateTitle(t.Text)
		}
	}
	return truncateTitle(turns[0].Text)
}

func titleWord(s string) string {
	if s == "" {
		return "Agent"
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func truncateTitle(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	r := []rune(s)
	const max = 56
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

func pad(s string, n int) string {
	if len(s) >= n {
		return s[:n]
	}
	return s + strings.Repeat("0", n-len(s))
}

func truncateOut(b []byte) string {
	s := strings.TrimSpace(string(b))
	const max = 300
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
