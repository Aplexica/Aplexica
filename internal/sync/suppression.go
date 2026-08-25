package syncd

import (
	"sort"
	"sync"
	"time"
)

// Suppression accounting: the record of every decision NOT to write a
// materialization target.
//
// Why this exists. A device can run with no ~/.aplexica/rules.toml. The rules
// engine is fail-closed by design, so every fan-out target is
// denied — correctly. But each denial was a bare `continue` in fanOut with no
// log line, no metric and no queue entry, after which fanOut returned nil
// (success) having written nothing. Import, publish, receive and canonical
// store writes can keep working, so every surface may report health while all
// cross-agent sync is unavailable.
//
// The ledger's contract: every code path that declines to write a target
// either records a suppression here, or is a structural self-exclusion on the
// explicit allowlist in suppression_census_test.go. That test is the real
// deliverable — the census is only worth having if it cannot silently regress.
//
// The ledger OBSERVES. It never retries (that is the deferred queue), never
// repairs (that is the convergence reconciler) and never renders a verdict
// (that is synchealth). Keeping those four jobs in four layers is what stops
// this from growing into a second, competing sync engine.
//
// ZERO-KNOWLEDGE: artifact ids, counts, timestamps and reasons only. Never a
// path, never a title, never body content.

// SuppressionClass determines both the operator experience and the recovery
// path, and is the most important distinction in this package.
//
// A policy suppression is CORRECT behaviour that the user configured. It must
// be shown ("not synced, because you configured it that way") and must never
// be "repaired" — auto-repairing it would override the user's own choice, and
// for a rule that excludes secrets from an agent that would be a security
// regression. A defect suppression is a fault: it self-heals or it escalates.
type SuppressionClass uint8

const (
	// ClassPolicy is user configuration. Expected, surfaced, never retried.
	ClassPolicy SuppressionClass = iota + 1
	// ClassDefect is a fault or transient failure. Retried, then escalated.
	ClassDefect
	// ClassCapability means the target structurally cannot hold this artifact
	// (no such adapter feature). Surfaced once; retrying cannot help.
	ClassCapability
)

func (c SuppressionClass) String() string {
	switch c {
	case ClassPolicy:
		return "policy"
	case ClassDefect:
		return "defect"
	case ClassCapability:
		return "capability"
	default:
		return "unknown"
	}
}

// SuppressionReason is the stable wire identity of a drop site. These strings
// appear in status JSON, the web API and operator tooling, so they are API:
// add freely, never rename or repurpose.
type SuppressionReason string

const (
	// ── policy ──────────────────────────────────────────────────────────
	ReasonNoRulesConfigured     SuppressionReason = "no_rules_configured"
	ReasonRulesDenied           SuppressionReason = "rules_denied"
	ReasonTargetSyncDisabled    SuppressionReason = "target_sync_disabled"
	ReasonSourceSyncDisabled    SuppressionReason = "source_sync_disabled"
	ReasonProjectAgentNotListed SuppressionReason = "project_agent_not_listed"
	ReasonProjectUnregistered   SuppressionReason = "project_unregistered"
	ReasonPaused                SuppressionReason = "paused"
	ReasonSkillModeStrict       SuppressionReason = "skill_mode_strict"
	ReasonBackfillDepth         SuppressionReason = "backfill_depth_exhausted"

	// ── defect ──────────────────────────────────────────────────────────
	ReasonAdapterBlockedSafety SuppressionReason = "adapter_blocked_safety"
	ReasonSourceAdapterBlocked SuppressionReason = "source_adapter_blocked"
	ReasonQuarantined          SuppressionReason = "quarantined"
	ReasonConflictUnresolved   SuppressionReason = "conflict_unresolved"
	ReasonArtifactMissing      SuppressionReason = "artifact_missing"
	ReasonHeadReadFailed       SuppressionReason = "head_read_failed"
	ReasonExportFailed         SuppressionReason = "export_failed"
	ReasonDestChangedUnderUs   SuppressionReason = "dest_changed_under_us"
	ReasonMirrorFirstContact   SuppressionReason = "mirror_first_contact_unsafe"
	ReasonSessionWriteFailed   SuppressionReason = "conversation_session_write_failed"
	ReasonDocWriteFailed       SuppressionReason = "conversation_doc_write_failed"
	ReasonArtifactReadFailed   SuppressionReason = "artifact_read_failed"
	ReasonArtifactWriteFailed  SuppressionReason = "artifact_write_failed"

	// ── capability ──────────────────────────────────────────────────────
	ReasonAdapterNotInstalled   SuppressionReason = "adapter_not_installed"
	ReasonFormatUnsupported     SuppressionReason = "format_unsupported"
	ReasonNativePathUnsupported SuppressionReason = "native_path_unsupported"
	ReasonSessionOptOut         SuppressionReason = "conversation_session_opt_out"
	ReasonDocDirUnavailable     SuppressionReason = "conversation_doc_dir_unavailable"
	ReasonDocTargetUnsupported  SuppressionReason = "conversation_target_unsupported"
)

// suppressionMeta is the operator-facing description of a reason. Explain
// states the CONSEQUENCE in the user's terms; Remedy is the exact action.
// Neither may describe only the mechanism — "rules engine rebuilt (0 rules)"
// does not tell the operator how to resolve the condition.
type suppressionMeta struct {
	Class   SuppressionClass
	Explain string
	Remedy  string
}

var suppressionCatalog = map[SuppressionReason]suppressionMeta{
	ReasonNoRulesConfigured: {ClassPolicy,
		"No sync rules are configured, so nothing is copied between agents on this device.",
		"aplexica rules add <file.toml>"},
	ReasonRulesDenied: {ClassPolicy,
		"Your sync rules do not route this artifact to this agent.",
		"aplexica rules test <artifact-id>"},
	ReasonTargetSyncDisabled: {ClassPolicy,
		"Sync is turned off for this agent.",
		"aplexica config set sync.agents.<agent> true"},
	ReasonSourceSyncDisabled: {ClassPolicy,
		"Sync is turned off for the agent this artifact came from.",
		"aplexica config set sync.agents.<agent> true"},
	ReasonProjectAgentNotListed: {ClassPolicy,
		"This project is limited to specific agents and this one is not listed.",
		"aplexica project list --json"},
	ReasonProjectUnregistered: {ClassPolicy,
		"This artifact belongs to a project that is not registered on this device.",
		"aplexica project link <id> <path>"},
	ReasonPaused: {ClassPolicy,
		"Sync is paused for this agent.",
		"aplexica sync resume"},
	ReasonSkillModeStrict: {ClassPolicy,
		"Strict skill mode is on and this agent cannot represent the skill without loss.",
		"aplexica rules edit  (set route.skillMode)"},
	ReasonBackfillDepth: {ClassPolicy,
		"This artifact is older than the backfill depth configured for this agent.",
		"aplexica backfill --agent <agent>"},

	// The remedies below name `aplexica daemon restart` because that is what
	// actually re-runs the startup safety verification; the `aplexica backups
	// verify` they used to name has never existed as a command. A remedy that
	// cannot be typed is worse than none — the operator spends the attempt
	// before learning that.
	ReasonAdapterBlockedSafety: {ClassDefect,
		"Writes to this agent are blocked until its safety snapshot finishes verifying.",
		"aplexica daemon restart  (re-runs the verification; it also clears on its own)"},
	ReasonSourceAdapterBlocked: {ClassDefect,
		"The source agent is blocked until its safety snapshot finishes verifying.",
		"aplexica daemon restart  (re-runs the verification; it also clears on its own)"},
	ReasonQuarantined: {ClassDefect,
		"This agent is quarantined after repeated failures.",
		"aplexica status  (quarantine clears automatically)"},
	ReasonConflictUnresolved: {ClassDefect,
		"This artifact has an unresolved conflict, so it is not written anywhere.",
		"aplexica conflicts list"},
	ReasonArtifactMissing: {ClassDefect,
		"The artifact was not found in the canonical store when the write was attempted.",
		"aplexica doctor"},
	ReasonHeadReadFailed: {ClassDefect,
		"The artifact's event history could not be read.",
		"aplexica doctor"},
	ReasonExportFailed: {ClassDefect,
		"Writing the artifact into this agent failed.",
		"aplexica status  (retried automatically)"},
	ReasonDestChangedUnderUs: {ClassDefect,
		"The destination file changed while it was being written.",
		"aplexica status  (retried automatically)"},
	ReasonMirrorFirstContact: {ClassDefect,
		"Refused to overwrite existing agent data Aplexica has never read.",
		"aplexica daemon restart  (its startup scan imports the agent's own copy first)"},
	// `aplexica repair materialization` only lists and drops entries, so it
	// could never resolve a failed write. These are retried automatically.
	ReasonSessionWriteFailed: {ClassDefect,
		"Writing the conversation into this agent's session store failed.",
		"aplexica status  (retried automatically)"},
	ReasonDocWriteFailed: {ClassDefect,
		"Writing the conversation transcript for this agent failed.",
		"aplexica status  (retried automatically)"},
	ReasonArtifactReadFailed: {ClassDefect,
		"The artifact record could not be read while crediting the write.",
		"aplexica doctor"},
	ReasonArtifactWriteFailed: {ClassDefect,
		"The artifact record could not be updated after the write.",
		"aplexica doctor"},

	ReasonAdapterNotInstalled: {ClassCapability,
		"This agent is not installed on this device.",
		""},
	ReasonFormatUnsupported: {ClassCapability,
		"This agent cannot read this artifact's format.",
		""},
	ReasonNativePathUnsupported: {ClassCapability,
		"This agent has no place to store this kind of artifact.",
		""},
	ReasonSessionOptOut: {ClassCapability,
		"This agent declined to store the conversation as a native session.",
		""},
	ReasonDocDirUnavailable: {ClassCapability,
		"This agent has no transcript directory configured.",
		""},
	ReasonDocTargetUnsupported: {ClassCapability,
		"This agent cannot hold conversations in any supported form.",
		""},
}

// Class reports the reason's class. An unregistered reason is treated as a
// defect: an unknown suppression is a bug until proven otherwise, and
// defaulting to policy would hide it (policy rows are never retried).
func (r SuppressionReason) Class() SuppressionClass {
	if m, ok := suppressionCatalog[r]; ok {
		return m.Class
	}
	return ClassDefect
}

// Explain returns the consequence in the operator's terms.
func (r SuppressionReason) Explain() string {
	if m, ok := suppressionCatalog[r]; ok {
		return m.Explain
	}
	return "This write was declined for an unrecognized reason."
}

// Remedy returns the exact action, or "" when none applies (capability
// suppressions are facts about the agent, not problems to solve).
func (r SuppressionReason) Remedy() string {
	if m, ok := suppressionCatalog[r]; ok {
		return m.Remedy
	}
	return "aplexica doctor"
}

// suppressionExemplarsPerKey bounds the artifact ids retained per
// (agent, reason). Enough to recognize a pattern; small enough that the whole
// ledger stays a fixed cost regardless of store size.
const suppressionExemplarsPerKey = 16

type suppressionKey struct {
	Agent  string
	Reason SuppressionReason
}

type suppressionRow struct {
	Count     uint64
	FirstAt   time.Time
	LastAt    time.Time
	exemplars []string
}

// suppressionLedger is bounded by construction: it aggregates by
// (agent, reason), so its cardinality is len(adapters) x len(reasons) no matter
// how many artifacts exist. That fixed
// bound is what makes it safe to call on the fan-out hot path.
type suppressionLedger struct {
	mu   sync.Mutex
	rows map[suppressionKey]*suppressionRow
}

func newSuppressionLedger() *suppressionLedger {
	return &suppressionLedger{rows: map[suppressionKey]*suppressionRow{}}
}

// record notes one declined write. Safe on a nil ledger so call sites need no
// guard: a daemon built without a ledger must still fan out.
func (l *suppressionLedger) record(agent string, reason SuppressionReason, artifactID string, now time.Time) {
	if l == nil || agent == "" || reason == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	k := suppressionKey{Agent: agent, Reason: reason}
	row := l.rows[k]
	if row == nil {
		row = &suppressionRow{FirstAt: now}
		l.rows[k] = row
	}
	row.Count++
	row.LastAt = now
	if artifactID != "" {
		row.exemplars = appendExemplar(row.exemplars, artifactID)
	}
}

// appendExemplar keeps the most recent ids, deduped, capped. Ring semantics:
// the oldest id is evicted, so the sample always reflects current behaviour.
func appendExemplar(list []string, id string) []string {
	for _, existing := range list {
		if existing == id {
			return list
		}
	}
	list = append(list, id)
	if len(list) > suppressionExemplarsPerKey {
		list = list[len(list)-suppressionExemplarsPerKey:]
	}
	return list
}

// clearDefects drops defect rows for an agent once a write to it succeeds.
// Policy and capability rows are steady-state facts about configuration and
// adapter shape, not failures, so a success does not retract them — they are
// recomputed on the next evaluation instead.
func (l *suppressionLedger) clearDefects(agent string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for k := range l.rows {
		if k.Agent == agent && k.Reason.Class() == ClassDefect {
			delete(l.rows, k)
		}
	}
}

// SuppressionSnapshot is one aggregated row, wire-shaped for status JSON and
// the local web API.
type SuppressionSnapshot struct {
	Agent   string `json:"agent"`
	Reason  string `json:"reason"`
	Class   string `json:"class"`
	Count   uint64 `json:"count"`
	FirstAt string `json:"firstAt,omitempty"`
	LastAt  string `json:"lastAt,omitempty"`
	// Stale marks a row whose condition is no longer current. The row is kept
	// — the count and the timestamps are real history — but it must not be
	// rendered as a problem the operator still has.
	Stale     bool     `json:"stale,omitempty"`
	Explain   string   `json:"explain"`
	Remedy    string   `json:"remedy,omitempty"`
	Exemplars []string `json:"exemplars,omitempty"`
}

// suppressionLiveness answers, for a reason whose condition is cheaply
// observable right now, whether that condition is STILL true.
//
// It exists because clearDefects retires a row only when a write to that agent
// succeeds, which for a blocked agent may never happen. That can leave
// `aplexica status` reporting a safety block after the daemon has verified that
// the block is cleared.
//
// known=false means the reason names an EVENT (an export that failed, a
// destination that moved), not a condition; there is nothing to re-verify and
// the cadence rule below decides instead.
type suppressionLiveness func(agent string, reason SuppressionReason) (live bool, known bool)

// suppressionStaleFloor is the shortest a row may sit unobserved before it is
// treated as history. It is comfortably above the 15-minute deferred-retry
// ceiling, so any condition that is genuinely still producing suppressions
// re-records well inside it.
const suppressionStaleFloor = 30 * time.Minute

// suppressionCadenceStaleFactor multiplies a row's own mean recurrence interval
// to get its staleness horizon. A row that recurs hourly is judged on hours; a
// one-off is judged on the floor. Measuring a row against its own cadence — not
// against a fixed TTL — is what keeps a rare but real fault rendering.
const suppressionCadenceStaleFactor = 4

// stale reports whether this row has gone quiet relative to its own recurrence
// cadence. Only applied when no verifier could answer for the reason.
func (r *suppressionRow) stale(now time.Time) bool {
	if r.LastAt.IsZero() {
		return false
	}
	horizon := suppressionStaleFloor
	if r.Count > 1 && r.LastAt.After(r.FirstAt) {
		interval := r.LastAt.Sub(r.FirstAt) / time.Duration(r.Count-1)
		if cadence := suppressionCadenceStaleFactor * interval; cadence > horizon {
			horizon = cadence
		}
	}
	return now.Sub(r.LastAt) > horizon
}

// Snapshot returns the ledger with no liveness re-verification. Kept for tests
// and callers that hold no orchestrator; the daemon's surfaces use SnapshotAt.
func (l *suppressionLedger) Snapshot() []SuppressionSnapshot {
	return l.SnapshotAt(time.Now().UTC(), nil)
}

// SnapshotAt returns the ledger sorted deterministically (agent, then reason)
// so status output and tests are stable, with each row's liveness resolved.
//
// Resolution order is deliberate: an authoritative answer from the verifier
// always wins, because it is a statement about the world right now. Only when
// the reason names an event rather than a condition does elapsed time decide.
func (l *suppressionLedger) SnapshotAt(now time.Time, live suppressionLiveness) []SuppressionSnapshot {
	if l == nil {
		return nil
	}
	// Copy the rows out under the lock and release it BEFORE asking the
	// verifier anything.
	//
	// l.mu is on the fan-out hot path — every drop site calls record() — and the
	// verifier is not a pure read: resolving adapter_not_installed probes the
	// filesystem, and any future verifier is free to be just as expensive. A
	// status read must never be able to stall all cross-agent sync for the
	// duration of a discovery probe, and holding a hot mutex across a callback
	// this package does not own is how that happens.
	type snapshotSource struct {
		key suppressionKey
		row suppressionRow
	}
	l.mu.Lock()
	sources := make([]snapshotSource, 0, len(l.rows))
	for k, row := range l.rows {
		copied := *row
		copied.exemplars = append([]string(nil), row.exemplars...)
		sources = append(sources, snapshotSource{key: k, row: copied})
	}
	l.mu.Unlock()

	out := make([]SuppressionSnapshot, 0, len(sources))
	for _, source := range sources {
		k, row := source.key, source.row
		snap := SuppressionSnapshot{
			Agent:     k.Agent,
			Reason:    string(k.Reason),
			Class:     k.Reason.Class().String(),
			Count:     row.Count,
			Explain:   k.Reason.Explain(),
			Remedy:    k.Reason.Remedy(),
			Exemplars: row.exemplars,
		}
		if !row.FirstAt.IsZero() {
			snap.FirstAt = row.FirstAt.UTC().Format(time.RFC3339)
		}
		if !row.LastAt.IsZero() {
			snap.LastAt = row.LastAt.UTC().Format(time.RFC3339)
		}
		if live != nil {
			if current, known := live(k.Agent, k.Reason); known {
				snap.Stale = !current
				out = append(out, snap)
				continue
			}
		}
		// Policy and capability rows count writes that were correctly not
		// copied. They are historical facts about what the user's own
		// configuration did, not conditions that resolve, so they never age
		// out on their own.
		if k.Reason.Class() == ClassDefect {
			snap.Stale = row.stale(now)
		}
		out = append(out, snap)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Agent != out[j].Agent {
			return out[i].Agent < out[j].Agent
		}
		return out[i].Reason < out[j].Reason
	})
	return out
}
