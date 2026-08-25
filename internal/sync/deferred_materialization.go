package syncd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter"
	"github.com/aplexica/aplexica/internal/atomicfile"
	"github.com/aplexica/aplexica/internal/syncrules"
)

// Keep startup-safety deferrals small even if a broken adapter remains blocked
// for a long time. Overflow never drops correctness: it coalesces to one
// target-only reconciliation of the canonical store after the adapter clears.
const deferredMaterializationLimit = 4096

const (
	deferredMaterializationDirtyName    = ".deferred-materialization-dirty.json"
	deferredMaterializationDirtyVersion = 2
)

// Per-entry retry pacing. The delay doubles from RetryMin so a genuinely
// transient block (adapter activating, a session mid-write) still clears in
// well under a second, then keeps doubling to RetryMax so an entry that can
// never converge costs four log lines an hour instead of twelve a minute.
const (
	deferredMaterializationRetryMin = 250 * time.Millisecond
	deferredMaterializationRetryMax = 15 * time.Minute
)

// Retry budget. An entry is abandoned only when it has BOTH burned its
// attempt budget and been queued long enough that no plausible open-session
// race explains it: a Claude or Codex session that stays open all day must
// still converge when its user finally quits, so the age floor does the real
// gating and the attempt floor only guarantees we actually tried.
//
// The canonical cause of a permanently stuck entry is an artifact whose
// canonical head duplicated while its native mirror did not; those never
// converge until the head itself is repaired, so retrying past this budget
// buys nothing but log volume.
const (
	deferredMaterializationMaxAttempts = 64
	deferredMaterializationMaxAge      = 24 * time.Hour
	// Bound the retained give-up records per target so the journal cannot
	// grow without limit on a device that keeps producing stuck heads.
	deferredMaterializationAbandonedMax = 64
)

// errDeferredMaterializationWithheld marks an attempt that never reached the
// target adapter because a reversible, user-controlled gate was closed — a
// pause, a quarantine, an unresolved conflict, a disabled sync gate, a project
// no longer in scope, or an adapter that is momentarily unavailable. Such an
// attempt is paced like a failure but must NOT spend the retry budget:
// charging it would let an operator forfeit every queued write simply by
// pausing sync for a day, which is precisely what
// deferredMaterializationTargetWithheld exists to prevent.
var errDeferredMaterializationWithheld = errors.New("syncd: native materialization withheld by policy")

// deferredMaterializationWithheld reports whether a failed attempt should be
// paced but not charged. Shutdown counts: a cancelled scan never reached the
// adapter either.
func deferredMaterializationWithheld(err error) bool {
	return errors.Is(err, errDeferredMaterializationWithheld) || errors.Is(err, context.Canceled)
}

type deferredMaterializationEntry struct {
	version        uint64
	includePrimary bool
	mirrorsOnly    bool
	originAgent    string

	// Retry accounting. attempts counts only attempts that actually reached
	// the adapter, and together with firstDeferred is persisted so the budget
	// survives restarts. nextAttempt deliberately is not persisted, so every
	// daemon start grants one immediate retry (a restart is itself evidence
	// that the blocking condition may be gone).
	attempts      int
	firstDeferred time.Time
	nextAttempt   time.Time
	lastErr       string

	// withheldAttempts paces retries that a closed policy gate turned back
	// before the adapter saw them. It feeds the backoff so a long pause does
	// not spin, and never feeds the give-up budget. Not persisted: a restart
	// re-evaluates every gate from scratch.
	withheldAttempts int

	// Quiescence accounting (see materialization_escalation.go). lastHeadHash
	// and destWitness are the two inputs that decide this write's outcome;
	// quietSince is when either of them last changed and is the only clock the
	// age-based escalation may consult. quietSince and lastHeadHash ARE
	// persisted: restarts happen often enough to re-arm the startup safety gate
	// for every adapter, so a clock a restart resets would make escalation as
	// unreachable as the attempt budget it replaces.
	lastHeadHash string
	quietSince   time.Time
	destWitness  sessionWriteWitness
	// destObserved records that destWitness holds a real observation made in
	// THIS process. destPath is memory-only, so after a restart the first pass
	// has no destination to witness; without this flag the first pass that
	// finally learns one would read "zero witness → real witness" as "the
	// destination changed" and restart the quiescence clock on every daemon
	// start. Restart frequency must never control whether escalation is
	// reachable.
	destObserved bool

	// destPath is the destination the target last refused. It is memory-only —
	// never logged, never persisted, never published; every surface outside
	// this process sees the redactPaths form — and exists so the next attempt
	// can tell whether the agent has touched the file since.
	destPath string
	// declineObserved records that the last outcome was a typed adapter
	// decline that named its own destination — the one case where the two
	// observed inputs are provably what decided it. Every other outcome (a
	// transient adapter error, a closed policy gate, a restart) clears it, so
	// the short circuit can never mistake "we did not look" for "looking again
	// is pointless". Memory-only, so a restart always earns a real attempt.
	declineObserved bool

	// declineReason and withheldReason are the content-free classifications the
	// last failure carried. They select the needs_attention row's explanation
	// and remedy, which before this were one generic sentence and one command
	// that repairs nothing.
	declineReason  adapter.SessionDeclineReason
	withheldReason SuppressionReason

	// escalationHeld records that this entry qualified to escalate and the
	// per-device rate budget turned it back. It is reported, never silently
	// dropped, and clears as soon as the entry's inputs move again. Persisted:
	// a restart that forgot it would re-run the whole hold decision, and the
	// held population is paced as a POPULATION (see heldRetryDueLocked), which
	// only works if the daemon remembers who is held.
	escalationHeld bool

	// lastEscalatedAt / escalatedReason / escalatedGate record that this device
	// has ALREADY raised this write for a human, and for what. They survive both
	// the convergence sweep's re-admission and a restart.
	//
	// Two jobs. First, the rate budget counts them, so self-heal cannot refund
	// the device's daily allowance every 15 minutes. Second, and more
	// importantly, they make escalation TERMINAL per (artifact, reason): an
	// entry that re-declines for the same reason is returned to the standing set
	// silently instead of spending a fresh escalation. Without that, a device
	// with a hundred permanently stuck writes emits three give-up events a day
	// forever — the rate budget bounds the RATE but nothing bounded the TOTAL.
	lastEscalatedAt time.Time
	escalatedReason adapter.SessionDeclineReason
	escalatedGate   SuppressionReason
}

// alreadyEscalatedFor reports whether this entry has been raised before for the
// same classification, i.e. whether raising it again would tell the operator
// something they have already been told.
func (e deferredMaterializationEntry) alreadyEscalatedFor(
	reason adapter.SessionDeclineReason, gate SuppressionReason,
) bool {
	return !e.lastEscalatedAt.IsZero() && e.escalatedReason == reason && e.escalatedGate == gate
}

// backoffAttempts is the pacing input: everything that has made this entry
// wait, whether or not it was charged.
func (e deferredMaterializationEntry) backoffAttempts() int {
	return e.attempts + e.withheldAttempts
}

// abandonedMaterialization is the diagnostic left behind when an entry
// exhausts its retry budget. It is what `aplexica repair materialization`
// and the status surface report; the daemon never retries it again on its
// own.
type abandonedMaterialization struct {
	artifactID    string
	originAgent   string
	attempts      int
	firstDeferred time.Time
	abandonedAt   time.Time
	lastErr       string
	// includePrimary and mirrorsOnly preserve the WRITE the record stands for,
	// so re-admission retries that write and not a differently-shaped one. The
	// load-bearing case is an origin-session write (originAgent == target):
	// includePrimary is the one flag that lets it past fan-out's same-source
	// exclusion, and a re-admission that lost it would plan ZERO writes, read
	// as success, and retire this record with the divergence untouched — a
	// flag nothing but success may lower, lowered by a write that wrote
	// nothing.
	includePrimary bool
	mirrorsOnly    bool
	// declineReason and withheldReason carry the classification forward so the
	// row can name a remedy that resolves THIS class. Both are content-free
	// enums, safe to persist.
	declineReason  adapter.SessionDeclineReason
	withheldReason SuppressionReason
}

type deferredMaterializationQueue struct {
	ids               []string
	entries           map[string]deferredMaterializationEntry
	generation        uint64
	overflow          bool
	conversationsOnly bool
	draining          bool
	wake              chan struct{}

	// Retry accounting for the overflow (whole-target reconciliation) mode,
	// which has no per-artifact entries to carry it.
	overflowAttempts         int
	overflowWithheldAttempts int
	overflowFirstDeferred    time.Time
	overflowNextAttempt      time.Time
	overflowLastErr          string

	// nextHeldSlot serializes the retries of entries the escalation rate budget
	// turned back. See heldRetryDueLocked: without it, N held entries produce
	// N retries per hour, and every one of them is a full fan-out that can
	// charge the quarantine breaker.
	nextHeldSlot time.Time

	abandoned []abandonedMaterialization
}

// heldRetryDueLocked allocates the next admission slot for an escalation-held
// entry. The caller holds deferredMaterializeMu.
//
// Design rule 3, restated in terms of the RETAINED population rather than the
// cost of one decision. A held entry stays in the retry queue by design (it is
// counted, not dropped), so the queue keeps driving it; per-entry pacing alone
// makes the aggregate rate O(N):
//
//	100 held entries x 1 retry/hour = 16.7 retries per 10 minutes
//
// Each retry is a real fanOutWithOptions — the decline short circuit only arms
// for a typed adapter decline that names its own destination, so most failure
// classes run the whole pass — and an Export failure charges the quarantine
// breaker, which trips at 3 failures / 10 minutes and blocks ALL
// materialization including live sync. 16.7 > 3, so the retained population
// alone would recreate the permanent quarantine cycle convergence.go exists to
// prevent.
//
// Serializing admission makes it O(1): at most one held retry per target per
// deferredEscalationHeldAdmit, i.e. 0.67 per 10 minutes per adapter, and the
// breaker is per-adapter. 0.67 < 3 however many entries are held.
func (q *deferredMaterializationQueue) heldRetryDueLocked(now time.Time) time.Time {
	due := now.Add(deferredEscalationHeldRetry)
	if q.nextHeldSlot.After(due) {
		due = q.nextHeldSlot
	}
	q.nextHeldSlot = due.Add(deferredEscalationHeldAdmit)
	return due
}

type deferredMaterializationEntryDisk struct {
	ArtifactID     string    `json:"artifactId"`
	IncludePrimary bool      `json:"includePrimary,omitempty"`
	MirrorsOnly    bool      `json:"mirrorsOnly,omitempty"`
	OriginAgent    string    `json:"originAgent,omitempty"`
	Attempts       int       `json:"attempts,omitempty"`
	FirstDeferred  time.Time `json:"firstDeferred,omitzero"`
	LastError      string    `json:"lastError,omitempty"`
	// QuietSince and LastHeadHash carry the quiescence clock across restarts.
	// DestPath deliberately has no disk field: it is a real user path.
	QuietSince      time.Time                    `json:"quietSince,omitzero"`
	LastHeadHash    string                       `json:"lastHeadHash,omitempty"`
	DeclineReason   adapter.SessionDeclineReason `json:"declineReason,omitempty"`
	WithheldReason  SuppressionReason            `json:"withheldReason,omitempty"`
	LastEscalatedAt time.Time                    `json:"lastEscalatedAt,omitzero"`
	EscalatedReason adapter.SessionDeclineReason `json:"escalatedReason,omitempty"`
	EscalatedGate   SuppressionReason            `json:"escalatedGate,omitempty"`
	EscalationHeld  bool                         `json:"escalationHeld,omitempty"`
}

type abandonedMaterializationDisk struct {
	ArtifactID     string                       `json:"artifactId"`
	OriginAgent    string                       `json:"originAgent,omitempty"`
	Attempts       int                          `json:"attempts,omitempty"`
	FirstDeferred  time.Time                    `json:"firstDeferred,omitzero"`
	AbandonedAt    time.Time                    `json:"abandonedAt,omitzero"`
	LastError      string                       `json:"lastError,omitempty"`
	IncludePrimary bool                         `json:"includePrimary,omitempty"`
	MirrorsOnly    bool                         `json:"mirrorsOnly,omitempty"`
	DeclineReason  adapter.SessionDeclineReason `json:"declineReason,omitempty"`
	WithheldReason SuppressionReason            `json:"withheldReason,omitempty"`
}

type deferredMaterializationQueueDisk struct {
	Target                string                             `json:"target"`
	Overflow              bool                               `json:"overflow,omitempty"`
	ConversationsOnly     bool                               `json:"conversationsOnly,omitempty"`
	Entries               []deferredMaterializationEntryDisk `json:"entries,omitempty"`
	OverflowAttempts      int                                `json:"overflowAttempts,omitempty"`
	OverflowFirstDeferred time.Time                          `json:"overflowFirstDeferred,omitzero"`
	OverflowLastError     string                             `json:"overflowLastError,omitempty"`
	Abandoned             []abandonedMaterializationDisk     `json:"abandoned,omitempty"`
}

// deferredMaterializationBackoff returns the delay before retry number
// attempts+1 (attempts counts failures so far).
func deferredMaterializationBackoff(attempts int) time.Duration {
	delay := deferredMaterializationRetryMin
	for range attempts {
		if delay >= deferredMaterializationRetryMax {
			return deferredMaterializationRetryMax
		}
		delay *= 2
	}
	if delay > deferredMaterializationRetryMax {
		return deferredMaterializationRetryMax
	}
	return delay
}

// deferredMaterializationExhausted reports whether an entry has spent its
// retry budget and should be abandoned with a diagnostic.
func deferredMaterializationExhausted(attempts int, firstDeferred, now time.Time) bool {
	if attempts < deferredMaterializationMaxAttempts {
		return false
	}
	if firstDeferred.IsZero() {
		return false
	}
	return now.Sub(firstDeferred) >= deferredMaterializationMaxAge
}

type deferredMaterializationDirtyDisk struct {
	Version int                                `json:"version"`
	Targets []string                           `json:"targets,omitempty"` // v1 compatibility
	Queues  []deferredMaterializationQueueDisk `json:"queues,omitempty"`
}

func newDeferredMaterializationQueue() *deferredMaterializationQueue {
	return &deferredMaterializationQueue{
		entries:           map[string]deferredMaterializationEntry{},
		conversationsOnly: true,
		wake:              make(chan struct{}, 1),
	}
}

func deferredMaterializationProjectionMigrationNeeded(storeRoot string) bool {
	if storeRoot == "" {
		return false
	}
	data, err := os.ReadFile(filepath.Join(storeRoot, deferredMaterializationDirtyName))
	if os.IsNotExist(err) {
		return true
	}
	if err != nil {
		return false
	}
	var disk struct {
		Version int `json:"version"`
	}
	if json.Unmarshal(data, &disk) != nil {
		return false
	}
	return disk.Version < deferredMaterializationDirtyVersion
}

// loadDeferredMaterializationQueues restores exact artifact IDs for v2 state.
// The v1 target-only marker remains supported and deliberately restarts in
// overflow mode because that format did not persist the exact missed IDs.
func loadDeferredMaterializationQueues(storeRoot string) (map[string]*deferredMaterializationQueue, error) {
	queues := map[string]*deferredMaterializationQueue{}
	if storeRoot == "" {
		return queues, nil
	}
	data, err := os.ReadFile(filepath.Join(storeRoot, deferredMaterializationDirtyName))
	if os.IsNotExist(err) {
		return queues, nil
	}
	if err != nil {
		return queues, fmt.Errorf("read deferred materialization state: %w", err)
	}
	var disk deferredMaterializationDirtyDisk
	if err := json.Unmarshal(data, &disk); err != nil {
		return queues, fmt.Errorf("decode deferred materialization state: %w", err)
	}
	switch disk.Version {
	case 1:
		for _, target := range disk.Targets {
			if target == "" {
				continue
			}
			queue := newDeferredMaterializationQueue()
			queue.generation = 1
			queue.overflow = true
			queue.conversationsOnly = false
			queues[target] = queue
		}
		return queues, nil
	case deferredMaterializationDirtyVersion:
	default:
		return queues, fmt.Errorf("unsupported deferred materialization state version %d", disk.Version)
	}
	loadedAt := time.Now().UTC()
	for _, saved := range disk.Queues {
		if saved.Target == "" {
			continue
		}
		queue := newDeferredMaterializationQueue()
		queue.overflow = saved.Overflow
		queue.conversationsOnly = saved.ConversationsOnly
		queue.overflowAttempts = saved.OverflowAttempts
		queue.overflowFirstDeferred = saved.OverflowFirstDeferred
		queue.overflowLastErr = saved.OverflowLastError
		for _, savedEntry := range saved.Entries {
			if savedEntry.ArtifactID == "" {
				continue
			}
			if _, duplicate := queue.entries[savedEntry.ArtifactID]; duplicate {
				continue
			}
			queue.generation++
			queue.ids = append(queue.ids, savedEntry.ArtifactID)
			entry := deferredMaterializationEntry{
				version:         queue.generation,
				includePrimary:  savedEntry.IncludePrimary,
				mirrorsOnly:     savedEntry.MirrorsOnly,
				originAgent:     savedEntry.OriginAgent,
				attempts:        savedEntry.Attempts,
				firstDeferred:   savedEntry.FirstDeferred,
				lastErr:         savedEntry.LastError,
				quietSince:      savedEntry.QuietSince,
				lastHeadHash:    savedEntry.LastHeadHash,
				declineReason:   savedEntry.DeclineReason,
				withheldReason:  savedEntry.WithheldReason,
				lastEscalatedAt: savedEntry.LastEscalatedAt,
				escalatedReason: savedEntry.EscalatedReason,
				escalatedGate:   savedEntry.EscalatedGate,
				escalationHeld:  savedEntry.EscalationHeld,
				// nextAttempt is intentionally left zero: a restart earns one
				// immediate retry before the persisted backoff resumes.
				// declineObserved, destObserved and destPath stay zero: the
				// restored quiescence clock is trusted, but the destination must
				// be re-learned by a real attempt before the short circuit may
				// fire again.
			}
			if entry.escalationHeld {
				// A held entry is paced as a population, and that pacing lives in
				// memory. Re-issue its slot at load, or a restart would make every
				// held entry due at once — the burst the pacing exists to prevent.
				entry.nextAttempt = queue.heldRetryDueLocked(loadedAt)
			}
			queue.entries[savedEntry.ArtifactID] = entry
		}
		for _, savedAbandoned := range saved.Abandoned {
			// Unlike a pending entry, whose artifact ID is its retry token, an
			// abandoned record with no ID is the legitimate whole-target
			// (overflow) give-up marker. Skipping it here would discard the
			// only surviving evidence that the target was given up on.
			queue.abandoned = append(queue.abandoned, abandonedMaterialization{
				artifactID:     savedAbandoned.ArtifactID,
				originAgent:    savedAbandoned.OriginAgent,
				attempts:       savedAbandoned.Attempts,
				firstDeferred:  savedAbandoned.FirstDeferred,
				abandonedAt:    savedAbandoned.AbandonedAt,
				lastErr:        savedAbandoned.LastError,
				includePrimary: savedAbandoned.IncludePrimary,
				mirrorsOnly:    savedAbandoned.MirrorsOnly,
				declineReason:  savedAbandoned.DeclineReason,
				withheldReason: savedAbandoned.WithheldReason,
			})
		}
		if queue.overflow || len(queue.ids) > 0 || len(queue.abandoned) > 0 {
			queues[saved.Target] = queue
		}
	}
	return queues, nil
}

// persistDeferredMaterializationLocked is the write-ahead journal for exact
// pending IDs. The caller holds deferredMaterializeMu. Conversation payloads
// are never copied here: canonical artifact IDs are the retry tokens, and each
// retry re-reads the newest selected head from the canonical store.
func (o *Orchestrator) persistDeferredMaterializationLocked() error {
	if o == nil || o.cfg.Store == nil || o.cfg.Store.Root == "" {
		return nil
	}
	return writeDeferredMaterializationQueues(o.cfg.Store.Root, o.deferredMaterialize)
}

// writeDeferredMaterializationQueues serializes queue state to the store's
// dirty journal. Shared by the live orchestrator and the offline repair path
// so both write exactly the same shape.
func writeDeferredMaterializationQueues(storeRoot string, byTarget map[string]*deferredMaterializationQueue) error {
	targets := make([]string, 0, len(byTarget))
	for target, queue := range byTarget {
		if target != "" && queue != nil && (queue.overflow || len(queue.ids) > 0 || len(queue.abandoned) > 0) {
			targets = append(targets, target)
		}
	}
	sort.Strings(targets)
	queues := make([]deferredMaterializationQueueDisk, 0, len(targets))
	for _, target := range targets {
		queue := byTarget[target]
		saved := deferredMaterializationQueueDisk{
			Target:                target,
			Overflow:              queue.overflow,
			ConversationsOnly:     queue.conversationsOnly,
			OverflowAttempts:      queue.overflowAttempts,
			OverflowFirstDeferred: queue.overflowFirstDeferred,
			OverflowLastError:     queue.overflowLastErr,
		}
		if !queue.overflow {
			saved.OverflowAttempts = 0
			saved.OverflowFirstDeferred = time.Time{}
			saved.OverflowLastError = ""
			for _, artifactID := range queue.ids {
				entry, ok := queue.entries[artifactID]
				if !ok {
					continue
				}
				saved.Entries = append(saved.Entries, deferredMaterializationEntryDisk{
					ArtifactID:      artifactID,
					IncludePrimary:  entry.includePrimary,
					MirrorsOnly:     entry.mirrorsOnly,
					OriginAgent:     entry.originAgent,
					Attempts:        entry.attempts,
					FirstDeferred:   entry.firstDeferred,
					LastError:       entry.lastErr,
					QuietSince:      entry.quietSince,
					LastHeadHash:    entry.lastHeadHash,
					DeclineReason:   entry.declineReason,
					WithheldReason:  entry.withheldReason,
					LastEscalatedAt: entry.lastEscalatedAt,
					EscalatedReason: entry.escalatedReason,
					EscalatedGate:   entry.escalatedGate,
					EscalationHeld:  entry.escalationHeld,
				})
			}
		}
		for _, record := range queue.abandoned {
			saved.Abandoned = append(saved.Abandoned, abandonedMaterializationDisk{
				ArtifactID:     record.artifactID,
				OriginAgent:    record.originAgent,
				Attempts:       record.attempts,
				FirstDeferred:  record.firstDeferred,
				AbandonedAt:    record.abandonedAt,
				LastError:      record.lastErr,
				IncludePrimary: record.includePrimary,
				MirrorsOnly:    record.mirrorsOnly,
				DeclineReason:  record.declineReason,
				WithheldReason: record.withheldReason,
			})
		}
		queues = append(queues, saved)
	}
	data, err := json.Marshal(deferredMaterializationDirtyDisk{
		Version: deferredMaterializationDirtyVersion,
		Queues:  queues,
	})
	if err != nil {
		return err
	}
	return atomicfile.WriteFile(filepath.Join(storeRoot, deferredMaterializationDirtyName), data, 0o600)
}

// deferMaterialization records one otherwise-eligible target write that could
// not safely complete. The canonical artifact ID is the retry token: a later
// drain always reads the newest selected head, so repeated updates coalesce
// safely without persisting conversation bodies. conversationsOnly controls
// the scope only if an overflow reconciliation is already pending.
func (o *Orchestrator) deferMaterialization(agent, artifactID, originAgent string, includePrimary, mirrorsOnly, conversationsOnly bool) {
	if o == nil || agent == "" || artifactID == "" {
		return
	}
	o.deferredMaterializeMu.Lock()
	if o.deferredMaterialize == nil {
		o.deferredMaterialize = map[string]*deferredMaterializationQueue{}
	}
	queue := o.deferredMaterialize[agent]
	if queue == nil {
		queue = newDeferredMaterializationQueue()
		o.deferredMaterialize[agent] = queue
	}
	now := time.Now().UTC()
	queue.generation++
	if queue.overflow {
		if !conversationsOnly {
			queue.conversationsOnly = false
		}
		if queue.overflowFirstDeferred.IsZero() {
			queue.overflowFirstDeferred = now
		}
		if err := o.persistDeferredMaterializationLocked(); err != nil && o.cfg.Logger != nil {
			o.cfg.Logger.Warn("persist native materialization deferral", "agent", agent, "err", err)
		}
		o.deferredMaterializeMu.Unlock()
		o.scheduleDeferredMaterializationDrain(agent)
		return
	}
	if len(queue.ids) == 0 {
		queue.conversationsOnly = conversationsOnly
	} else {
		queue.conversationsOnly = queue.conversationsOnly && conversationsOnly
	}
	if entry, exists := queue.entries[artifactID]; exists {
		entry.version = queue.generation
		entry.includePrimary = entry.includePrimary || includePrimary
		// A full canonical destination request subsumes a mirror-only request.
		entry.mirrorsOnly = entry.mirrorsOnly && mirrorsOnly
		if originAgent != "" {
			entry.originAgent = originAgent
		}
		// Re-deferral deliberately preserves attempts, firstDeferred, and
		// nextAttempt. The live fan-out re-defers on every failed cycle, so
		// resetting the budget here would make it unreachable for exactly the
		// artifacts that fail most often.
		if entry.firstDeferred.IsZero() {
			entry.firstDeferred = now
		}
		// A re-deferral is a fresh request for this write, so the next pass
		// must actually run rather than be short-circuited on a stale
		// observation. The quiescence clock is left alone: wanting the write
		// again is not evidence that anything it depends on moved.
		entry.declineObserved = false
		queue.entries[artifactID] = entry
	} else if len(queue.ids) < deferredMaterializationLimit {
		// A fresh deferral gives this artifact a full retry budget again — but
		// it does NOT retire the give-up record.
		//
		// It used to. That made ordinary sync activity an eraser: the live
		// fan-out re-defers on every failed cycle, so on a device under
		// continuous fan-out the give-up records for exactly the artifacts that
		// keep failing were dropped routinely. Two consequences, both bad. The
		// operator-facing flag this whole surface exists to raise was cleared
		// before anyone could see it (one `aplexica daemon reload` wiped the
		// lot), and the record is also the evidence the rate budget counts, so
		// the daily cap could be refilled many times a day. A give-up record is
		// retired when the write SUCCEEDS, or when an operator drops it.
		fresh := deferredMaterializationEntry{
			version:        queue.generation,
			includePrimary: includePrimary,
			mirrorsOnly:    mirrorsOnly,
			originAgent:    originAgent,
			firstDeferred:  now,
		}
		queue.carryEscalationStampLocked(&fresh, artifactID)
		queue.ids = append(queue.ids, artifactID)
		queue.entries[artifactID] = fresh
	} else {
		queue.overflow = true
		queue.overflowFirstDeferred = now
		queue.overflowAttempts = 0
		queue.overflowNextAttempt = time.Time{}
		if o.cfg.Logger != nil {
			o.cfg.Logger.Warn("native materialization deferrals coalesced to target reconciliation", "agent", agent)
		}
	}
	if err := o.persistDeferredMaterializationLocked(); err != nil && o.cfg.Logger != nil {
		o.cfg.Logger.Warn("persist native materialization deferral", "agent", agent, "err", err)
	}
	o.deferredMaterializeMu.Unlock()

	// Clear can race between fanOut's Blocked check and this enqueue. Recheck
	// after the record is visible so that race cannot strand the new entry.
	o.scheduleDeferredMaterializationDrain(agent)
}

// reopenDeferredMaterializationForDest retracts the evidence that lets the
// drain skip a pass, for every queued entry whose destination is this path.
//
// The short circuit watches exactly two inputs — the canonical head and the
// destination bytes — and fires only on positive evidence that those two are
// what decided the last outcome. A native IMPORT of the destination moves
// neither of them and yet changes the answer: the adapter declines to append a
// canonical suffix to a native session while that session holds an unimported
// continuation, so "this file has now been imported" is a third input the two
// witnesses cannot see.
//
// Before this existed, the retry after an import worked only by accident. The
// first pass that learned a destination compared a real witness against the
// zero value and read it as "the destination changed", which bought exactly one
// free real attempt — the same defect that made a daemon restart reset the
// quiescence clock. Fixing that removed the accident, so the signal is now
// stated explicitly by the code that knows it.
func (o *Orchestrator) reopenDeferredMaterializationForDest(path string) {
	if o == nil || path == "" {
		return
	}
	clean := filepath.Clean(path)
	o.deferredMaterializeMu.Lock()
	defer o.deferredMaterializeMu.Unlock()
	for _, queue := range o.deferredMaterialize {
		if queue == nil {
			continue
		}
		for artifactID, entry := range queue.entries {
			if !entry.declineObserved || entry.destPath == "" ||
				filepath.Clean(entry.destPath) != clean {
				continue
			}
			entry.declineObserved = false
			queue.entries[artifactID] = entry
		}
	}
}

// scheduleDeferredMaterializationDrain starts at most one joined drain per
// adapter. AdapterBlocker.Clear invokes this synchronously, but all native IO
// stays in the orchestrator's background lifecycle.
func (o *Orchestrator) scheduleDeferredMaterializationDrain(agent string) {
	if o == nil || agent == "" {
		return
	}
	configured := false
	for _, ad := range o.cfg.Adapters {
		if ad.Name() == agent {
			configured = true
			break
		}
	}
	// Retain a recovered marker for an adapter that is not part of this
	// process. A later daemon start that discovers the adapter can reconcile it;
	// treating an empty targeted fan-out as success would erase the only retry.
	if !configured {
		return
	}
	// A blocked adapter deliberately does NOT stop the drain any more. The drain
	// is the only thing that folds an entry's quiescence clock and evaluates its
	// escalation rule, so refusing to run while a gate is closed made the
	// terminal state structurally unreachable for exactly the population D2 is
	// about: startNativeStartupSafety re-arms the block for every agent on every
	// daemon start, and an agent whose safety snapshot never verifies stays
	// blocked indefinitely. deferredMaterializationTargetWithheld reports the
	// block as ReasonAdapterBlockedSafety before anything touches the adapter,
	// so the pass is paced and never charged — identical to every other gate.
	o.deferredMaterializeMu.Lock()
	queue := o.deferredMaterialize[agent]
	if queue == nil || (!queue.overflow && len(queue.ids) == 0) {
		o.deferredMaterializeMu.Unlock()
		return
	}
	if queue.wake == nil {
		queue.wake = make(chan struct{}, 1)
	}
	if queue.draining {
		select {
		case queue.wake <- struct{}{}:
		default:
		}
		o.deferredMaterializeMu.Unlock()
		return
	}
	queue.draining = true
	o.deferredMaterializeMu.Unlock()

	if !o.beginBackground() {
		o.deferredMaterializeMu.Lock()
		queue.draining = false
		o.deferredMaterializeMu.Unlock()
		return
	}
	go o.runDeferredMaterializationDrain(agent, queue)
}

func (o *Orchestrator) runDeferredMaterializationDrain(agent string, queue *deferredMaterializationQueue) {
	defer o.endBackground()
	defer func() {
		o.deferredMaterializeMu.Lock()
		queue.draining = false
		hasWork := queue.overflow || len(queue.ids) > 0
		o.deferredMaterializeMu.Unlock()
		if hasWork {
			o.scheduleDeferredMaterializationDrain(agent)
		}
	}()

	for {
		if o.closingNow() {
			return
		}
		// No blocked-adapter early return here either — see
		// scheduleDeferredMaterializationDrain. The gate is enforced per
		// artifact, inside materializeDeferredArtifact, where it can be observed
		// and explained instead of silently suspending the queue.

		o.deferredMaterializeMu.Lock()
		overflow := queue.overflow
		conversationsOnly := queue.conversationsOnly
		generation := queue.generation
		var artifactID string
		var entry deferredMaterializationEntry
		var due time.Time
		hasWork := false
		if overflow {
			hasWork = true
			due = queue.overflowNextAttempt
		} else {
			artifactID, entry, due, hasWork = queue.nextDueLocked()
		}
		o.deferredMaterializeMu.Unlock()
		if !hasWork {
			return
		}
		// Selecting the earliest-due entry rather than the queue head is also
		// what keeps one permanently open or divergent native conversation
		// from starving its siblings: a failed attempt pushes that entry's due
		// time out, so every other pending artifact runs before it returns.
		if wait := time.Until(due); wait > 0 {
			if !o.waitDeferredMaterializationRetry(queue, wait) {
				return
			}
			continue
		}

		var err error
		if overflow {
			err = o.reconcileDeferredMaterializationTarget(context.Background(), agent, conversationsOnly)
		} else {
			err = o.materializeDeferredArtifact(context.Background(), agent, artifactID, entry)
		}
		if err != nil {
			o.recordDeferredMaterializationFailure(agent, queue, overflow, artifactID, entry, err)
			continue
		}

		o.deferredMaterializeMu.Lock()
		if overflow {
			// If another deferral arrived during the scan, repeat: the scan may
			// already have passed that artifact before its new head committed.
			if queue.overflow && queue.generation == generation {
				queue.overflow = false
				queue.conversationsOnly = false
				queue.ids = nil
				clear(queue.entries)
				queue.overflowAttempts = 0
				queue.overflowFirstDeferred = time.Time{}
				queue.overflowNextAttempt = time.Time{}
				queue.overflowLastErr = ""
			}
		} else if current, ok := queue.entries[artifactID]; ok && current.version == entry.version {
			delete(queue.entries, artifactID)
			queue.removeIDLocked(artifactID)
			// The write landed, so the condition the give-up record described is
			// genuinely resolved. This is the ONE place a standing record is
			// retired automatically: everything else — a re-deferral, the
			// convergence sweep — leaves it raised, because a flag nothing but
			// success can lower is the only kind an operator can trust.
			queue.dropAbandoned(artifactID)
		}
		if err := o.persistDeferredMaterializationLocked(); err != nil && o.cfg.Logger != nil {
			o.cfg.Logger.Warn("persist native materialization completion", "agent", agent, "err", err)
		}
		o.deferredMaterializeMu.Unlock()
	}
}

// recordDeferredMaterializationFailure charges one failed attempt against the
// entry's retry budget, schedules the next attempt, and abandons the entry
// once the budget is spent. Logging is throttled: a stuck entry that retries
// for a day must leave a usable trail without burning tens of thousands of
// log lines, which is exactly what an unbounded fixed-interval retry did.
func (o *Orchestrator) recordDeferredMaterializationFailure(
	agent string,
	queue *deferredMaterializationQueue,
	overflow bool,
	artifactID string,
	entry deferredMaterializationEntry,
	cause error,
) {
	now := time.Now().UTC()
	reason := redactPaths(cause.Error())
	// A closed policy gate turned this attempt back before the adapter saw
	// it. Pace it, but never charge it: the give-up budget is for writes that
	// the target itself refused, not for a sync the user paused.
	withheld := deferredMaterializationWithheld(cause)
	// Read the nudge target from the cause BEFORE the lock; the nudge itself
	// runs after the unlock, because it does file IO and must never hold the
	// queue mutex across it.
	nudgeDest := divergedNativeDest(cause)

	o.deferredMaterializeMu.Lock()
	attempts := 0
	paced := 0
	var firstDeferred time.Time
	var backoff time.Duration
	abandoned := false
	// held marks an entry the per-device escalation budget turned back. It is
	// reported rather than dropped because one `aplexica daemon reload` could
	// otherwise raise a large retained population at once.
	held := false
	// standing marks a give-up that was already raised for this same cause, so
	// it is returned to the standing set without spending a new escalation.
	standing := false
	// quiescenceMoved marks a pass that changed persisted escalation state even
	// though it never reached the adapter.
	quiescenceMoved := false
	if overflow {
		if withheld {
			queue.overflowWithheldAttempts++
		} else {
			queue.overflowAttempts++
		}
		if queue.overflowFirstDeferred.IsZero() {
			queue.overflowFirstDeferred = now
		}
		queue.overflowLastErr = reason
		attempts = queue.overflowAttempts
		paced = queue.overflowAttempts + queue.overflowWithheldAttempts
		firstDeferred = queue.overflowFirstDeferred
		backoff = deferredMaterializationBackoff(paced)
		queue.overflowNextAttempt = now.Add(backoff)
		// Whole-target reconciliation has no per-artifact quiescence to observe,
		// so it keeps the attempt budget as its only trigger.
		//
		// The give-up is UNCONDITIONAL. Gating it on the escalation rate budget
		// made a terminal state reachable only while an unrelated quota happened
		// to be free — and in the design's own steady state that quota is spent
		// for ~24h at a time, so the overflow never cleared and re-ran its
		// fail-fast whole-target reconciliation at the 15-minute ceiling
		// forever. A terminating loop must not be made non-terminating by a
		// budget. There is no wall-of-alarms risk here either: an overflow row
		// is at most one per target, so the population is bounded by the number
		// of adapters, not by the store.
		if !withheld && deferredMaterializationExhausted(attempts, firstDeferred, now) {
			abandoned = true
			queue.recordAbandonedLocked(abandonedMaterialization{
				attempts:      attempts,
				firstDeferred: firstDeferred,
				abandonedAt:   now,
				lastErr:       reason,
			})
			queue.overflow = false
			queue.conversationsOnly = false
			queue.ids = nil
			clear(queue.entries)
			queue.overflowAttempts = 0
			queue.overflowWithheldAttempts = 0
			queue.overflowFirstDeferred = time.Time{}
			queue.overflowNextAttempt = time.Time{}
			queue.overflowLastErr = ""
		}
	} else if current, ok := queue.entries[artifactID]; ok {
		if withheld {
			current.withheldAttempts++
		} else {
			current.attempts++
		}
		if current.firstDeferred.IsZero() {
			current.firstDeferred = now
		}
		current.lastErr = reason
		classifyDeferredMaterializationCause(&current, cause)
		attempts = current.attempts
		paced = current.backoffAttempts()
		firstDeferred = current.firstDeferred
		backoff = deferredMaterializationBackoff(paced)
		current.nextAttempt = now.Add(backoff)
		// Escalation is evaluated for a withheld pass too. That is the whole
		// point of the regression: entries can be withheld on every single pass,
		// so a rule that only ran for charged attempts left
		// them with no terminal state available at all. Withholding still never
		// spends the ATTEMPT budget — only age plus quiescence can raise them,
		// and a target the user merely paused keeps moving nothing, which is
		// exactly the condition an operator should be told about.
		if deferredMaterializationEscalates(current, now) {
			// Already raised for THIS cause: returning it to the standing set is
			// bookkeeping, not news. It spends no budget, emits no event and
			// logs no warning, which is what makes escalation terminal per
			// (artifact, reason) instead of a permanent 3-a-day drip that says
			// the same thing about the same artifacts forever.
			reraise := current.alreadyEscalatedFor(current.declineReason, current.withheldReason)
			switch {
			case reraise || escalationsInWindow(o.deferredMaterialize, now) < deferredEscalationsPerWindow:
				abandoned = true
				standing = reraise
				current.escalationHeld = false
				raisedAt := now
				if reraise && !current.lastEscalatedAt.IsZero() {
					// Keep the FIRST-raised timestamp. The rate budget counts
					// give-up records inside a rolling window, so a standing
					// record must age out of that window and stop consuming the
					// allowance a genuinely new cause needs.
					raisedAt = current.lastEscalatedAt
				}
				queue.recordAbandonedLocked(abandonedMaterialization{
					artifactID:     artifactID,
					originAgent:    current.originAgent,
					attempts:       attempts,
					firstDeferred:  firstDeferred,
					abandonedAt:    raisedAt,
					lastErr:        reason,
					includePrimary: current.includePrimary,
					mirrorsOnly:    current.mirrorsOnly,
					declineReason:  current.declineReason,
					withheldReason: current.withheldReason,
				})
				delete(queue.entries, artifactID)
				queue.removeIDLocked(artifactID)
			default:
				// Over the device's rolling budget. The entry stays queued at a
				// low frequency and is COUNTED — a truncated list would recreate
				// the silence this whole surface exists to end. Its retry is
				// admitted against the per-target held slot, so the aggregate
				// rate stays O(1) however many entries are held.
				current.escalationHeld = true
				held = true
				current.nextAttempt = queue.heldRetryDueLocked(now)
				backoff = time.Until(current.nextAttempt)
			}
		}
		if !abandoned {
			queue.entries[artifactID] = current
		}
		// The quiescence clock and the hold decision are the state the whole
		// age-based escalation rests on, and observeDeferredMaterializationInputs
		// sets them in MEMORY only. A pass that is purely withheld used to skip
		// the journal entirely, so for the D2 population — withheld on every
		// single pass — they never reached disk at all and every restart began
		// the 2h quiescence window again. Restart frequency must never decide
		// whether the system can give up.
		quiescenceMoved = !current.quietSince.Equal(entry.quietSince) ||
			current.lastHeadHash != entry.lastHeadHash ||
			current.escalationHeld != entry.escalationHeld ||
			current.declineReason != entry.declineReason ||
			current.withheldReason != entry.withheldReason
	} else {
		// The entry was dropped or superseded while the attempt ran; nothing
		// to charge.
		o.deferredMaterializeMu.Unlock()
		return
	}
	// A withheld attempt that changed nothing persisted skips the journal write
	// rather than rewriting it every cycle for the whole of a pause. A withheld
	// attempt that escalated, or that moved the quiescence clock or the hold
	// decision, is NOT that case: those are precisely the fields a restart would
	// otherwise reset, and the population they exist for is withheld on every
	// single pass.
	var persistErr error
	if !withheld || abandoned || quiescenceMoved {
		persistErr = o.persistDeferredMaterializationLocked()
	}
	o.deferredMaterializeMu.Unlock()

	if persistErr != nil && o.cfg.Logger != nil {
		o.cfg.Logger.Warn("persist native materialization deferral", "agent", agent, "err", persistErr)
	}
	// Break the import/materialize deadlock this decline just proved exists.
	// Placed before every early return below on purpose: an entry that was
	// escalated in this very pass still needs canonical to learn the turns its
	// destination holds, and a raised flag is not a reason to stop repairing.
	if nudgeDest != "" && o.nudgeDivergedNativeImport(agent, artifactID, nudgeDest) {
		// The device-wide budget turned it back. Retract this entry's decline
		// short circuit so its next paced pass reproduces the decline and
		// re-offers the nudge; otherwise the evidence is discarded AND the
		// trigger with it, because a short-circuited pass never reaches the
		// adapter and so never produces another typed decline.
		o.reofferDivergedNativeNudge(agent, artifactID)
	}
	if abandoned {
		if standing {
			// Same artifact, same cause, already raised. Say so once at INFO
			// rather than re-warning and re-publishing: repeating a give-up the
			// operator has already been told about is noise that trains them to
			// ignore the surface.
			if o.cfg.Logger != nil {
				o.cfg.Logger.Info("native materialization still needs attention",
					"agent", agent, "artifact_id", artifactID, "attempts", attempts)
			}
			return
		}
		if o.cfg.Logger != nil {
			o.cfg.Logger.Warn("native materialization abandoned after retry budget",
				"agent", agent, "artifact_id", artifactID, "attempts", attempts,
				"queued_for", now.Sub(firstDeferred).Round(time.Second).String(), "err", reason)
		}
		o.publishEvent("conversation.materialize_gave_up", map[string]any{
			"artifact_id": artifactID,
			"agent":       agent,
			"attempts":    attempts,
			"reason":      reason,
		})
		return
	}
	if held {
		// Logged once per hour-long hold rather than sampled: the whole point of
		// the cap is that these are NOT individually surfaced, so the log is the
		// only place a specific held entry is nameable.
		if o.cfg.Logger != nil {
			o.cfg.Logger.Info("native materialization escalation deferred by rate budget",
				"agent", agent, "artifact_id", artifactID,
				"per_window", deferredEscalationsPerWindow,
				"window", deferredEscalationWindow.String(), "retry_in", backoff.String())
		}
		return
	}
	if o.cfg.Logger == nil || !deferredMaterializationShouldLogAttempt(paced) {
		return
	}
	if withheld {
		o.cfg.Logger.Info("native materialization withheld",
			"agent", agent, "artifact_id", artifactID, "waits", paced,
			"retry_in", backoff.String(), "err", reason)
		return
	}
	o.cfg.Logger.Info("native materialization retry deferred",
		"agent", agent, "artifact_id", artifactID, "attempts", attempts,
		"retry_in", backoff.String(), "err", reason)
}

// Attempt-log sampling: keep every one of the first failures, where a human
// is likely watching a live sync, then take one in every
// deferredMaterializationLogEvery so a day-long stuck entry produces roughly
// ten lines instead of ten thousand.
const (
	deferredMaterializationLogFirst = 3
	deferredMaterializationLogEvery = 8
)

func deferredMaterializationShouldLogAttempt(attempts int) bool {
	return attempts <= deferredMaterializationLogFirst ||
		attempts%deferredMaterializationLogEvery == 0
}

// nextDueLocked returns the pending entry whose retry is due soonest.
func (q *deferredMaterializationQueue) nextDueLocked() (string, deferredMaterializationEntry, time.Time, bool) {
	var (
		bestID    string
		bestEntry deferredMaterializationEntry
		bestDue   time.Time
		found     bool
	)
	for _, artifactID := range q.ids {
		entry, ok := q.entries[artifactID]
		if !ok {
			continue
		}
		if !found || entry.nextAttempt.Before(bestDue) {
			bestID, bestEntry, bestDue, found = artifactID, entry, entry.nextAttempt, true
		}
	}
	return bestID, bestEntry, bestDue, found
}

func (q *deferredMaterializationQueue) removeIDLocked(artifactID string) {
	for i, id := range q.ids {
		if id == artifactID {
			q.ids = append(q.ids[:i], q.ids[i+1:]...)
			return
		}
	}
}

func (q *deferredMaterializationQueue) recordAbandonedLocked(record abandonedMaterialization) {
	q.dropAbandoned(record.artifactID)
	q.abandoned = append(q.abandoned, record)
	if excess := len(q.abandoned) - deferredMaterializationAbandonedMax; excess > 0 {
		q.abandoned = append([]abandonedMaterialization(nil), q.abandoned[excess:]...)
	}
}

// carryEscalationStampLocked copies "this device already raised this write, and
// for what" from a standing give-up record onto a freshly created entry.
//
// Without it, every path that installs a fresh entry — the live fan-out's
// re-deferral, the convergence sweep's re-admission, `aplexica materialize` —
// hands the artifact a clean escalation history, so the same permanently stuck
// write can spend the device's daily allowance again and again.
func (q *deferredMaterializationQueue) carryEscalationStampLocked(
	entry *deferredMaterializationEntry, artifactID string,
) {
	for _, record := range q.abandoned {
		if record.artifactID != artifactID {
			continue
		}
		entry.lastEscalatedAt = record.abandonedAt
		entry.escalatedReason = record.declineReason
		entry.escalatedGate = record.withheldReason
		return
	}
}

func (q *deferredMaterializationQueue) dropAbandoned(artifactID string) {
	for i, record := range q.abandoned {
		if record.artifactID == artifactID {
			q.abandoned = append(q.abandoned[:i], q.abandoned[i+1:]...)
			return
		}
	}
}

// resumeDeferredMaterializationAfterUnblock clears the accumulated backoff for
// a target whose AdapterBlocker entry just cleared. An unblock is positive
// evidence that the condition every queued write was waiting on is gone, so
// those writes should run now rather than after a backoff earned while the
// adapter was unreachable.
func (o *Orchestrator) resumeDeferredMaterializationAfterUnblock(agent string) {
	if o == nil || agent == "" {
		return
	}
	o.deferredMaterializeMu.Lock()
	if queue := o.deferredMaterialize[agent]; queue != nil {
		queue.overflowNextAttempt = time.Time{}
		queue.overflowWithheldAttempts = 0
		for artifactID, entry := range queue.entries {
			if entry.escalationHeld {
				// A held entry is paced as a POPULATION, and an unblock fires on
				// every daemon start. Clearing its slot here would collapse the
				// whole held backlog into one burst at every start — the exact
				// O(N) failure rate the pacing exists to bound.
				continue
			}
			entry.nextAttempt = time.Time{}
			entry.withheldAttempts = 0
			queue.entries[artifactID] = entry
		}
	}
	o.deferredMaterializeMu.Unlock()
	o.scheduleDeferredMaterializationDrain(agent)
}

// DeferredMaterializations reports every native materialization the daemon is
// still retrying, plus every one it has abandoned. Rows are plain maps for the
// same reason PendingProjects uses them: the daemon control package must be
// able to serialize this without importing internal/sync.
//
// Row shape: {agent, artifactId, state, attempts, firstDeferredAt,
// nextAttemptAt, abandonedAt, lastError}. state is "pending", "overflow"
// (a whole-target reconciliation standing in for coalesced entries), or
// "abandoned". An empty artifactId on an "overflow" row is expected — that
// mode does not track individual IDs.
func (o *Orchestrator) DeferredMaterializations() []map[string]any {
	if o == nil {
		return nil
	}
	mirrorRepair := o.mirrorRepairByAgent()
	o.deferredMaterializeMu.Lock()
	defer o.deferredMaterializeMu.Unlock()
	return deferredMaterializationRows(o.deferredMaterialize, mirrorRepair)
}

// mirrorRepairByAgent reports, per target, whether the adapter is currently
// authorized to rebuild a forked synthetic mirror.
//
// The needs_attention wording depends on it: a build that SHIPS the repair must
// never tell an operator "no shipped command repairs that" merely because the
// flag is off, and a build with the flag ON must not offer to turn it on again.
// It is resolved through an optional interface rather than an import of the
// adapter package, exactly as every other adapter refinement in this package is.
func (o *Orchestrator) mirrorRepairByAgent() map[string]materializationSurface {
	if o == nil {
		return nil
	}
	var out map[string]materializationSurface
	for _, ad := range o.cfg.Adapters {
		reporter, ok := ad.(adapter.ConversationMirrorRepairReporter)
		if !ok {
			// A target that does not implement the reporter has no rebuild at
			// all, which is a DIFFERENT state from having one that is switched
			// off. Recording only the boolean collapsed the two, and codex —
			// which reports the same `diverged` reason and is not repairable by
			// this flag — was then handed a remedy naming it. Absence from this
			// map is what mirrorRepairSupported reads as "no rebuild exists".
			continue
		}
		if out == nil {
			out = map[string]materializationSurface{}
		}
		out[ad.Name()] = materializationSurface{
			mirrorRepairSupported: true,
			mirrorRepairEnabled:   reporter.ForkedMirrorRepairEnabled(),
		}
	}
	return out
}

// Suppressions reports every declined materialization target aggregated by
// (agent, reason). This is the surface that answers "why is nothing syncing?"
// — a question that otherwise had no actionable answer beyond an INFO log line.
//
// Rows are bounded by agents x reasons and carry no body content: artifact
// ids, counts, timestamps and a remedy string only.
func (o *Orchestrator) Suppressions() []SuppressionSnapshot {
	if o == nil {
		return nil
	}
	return o.suppressions.SnapshotAt(time.Now().UTC(), o.suppressionConditionLive)
}

// suppressionConditionLive re-verifies, at read time, whether the condition a
// suppression row describes is still true.
//
// This is the liveness the ledger lacked. Every reason answered here names a
// live gate the orchestrator can simply ask about, so a resolved condition
// stops being reported the moment it resolves rather than waiting for a
// successful write to that agent — which, for a blocked agent, may never come.
// Reasons that name an EVENT rather than a condition return known=false and are
// judged on their own recurrence cadence instead.
func (o *Orchestrator) suppressionConditionLive(agent string, reason SuppressionReason) (bool, bool) {
	if o == nil || agent == "" {
		return false, false
	}
	switch reason {
	case ReasonAdapterBlockedSafety, ReasonSourceAdapterBlocked:
		_, blocked := o.adapterBlocked(agent)
		return blocked, true
	case ReasonQuarantined:
		if o.cfg.Quarantine == nil {
			return false, true
		}
		return o.cfg.Quarantine.IsQuarantined(agent, time.Now()), true
	case ReasonPaused:
		if o.cfg.PauseStore == nil {
			return false, true
		}
		paused, _ := o.cfg.PauseStore.IsPaused(agent, time.Now().UTC())
		return paused, true
	case ReasonNoRulesConfigured:
		return o.rulesEngineIsEmpty(), true
	case ReasonTargetSyncDisabled, ReasonSourceSyncDisabled:
		gate := o.syncGate()
		if gate == nil {
			return false, true
		}
		return !gate.Enabled(agent), true
	case ReasonAdapterNotInstalled:
		for _, ad := range o.cfg.Adapters {
			if ad.Name() == agent {
				// Deliberately the side-effect-FREE probe. This runs from a
				// status read; runtimeAdapterAvailable would fire the runtime
				// activation hook, which on this daemon verifies and possibly
				// rebuilds native safety snapshots and can block the adapter.
				return !o.runtimeAdapterInstalled(ad), true
			}
		}
		return true, true
	}
	return false, false
}

// SyncSuppressions renders the ledger as the map rows the daemon control
// surface carries (map[string]any for the same import-cycle reason as
// PendingProjects: daemon must not import sync).
func (o *Orchestrator) SyncSuppressions() []map[string]any {
	if o == nil {
		return nil
	}
	snaps := o.suppressions.SnapshotAt(time.Now().UTC(), o.suppressionConditionLive)
	if len(snaps) == 0 {
		return nil
	}
	rows := make([]map[string]any, 0, len(snaps))
	for _, s := range snaps {
		row := map[string]any{
			"agent":   s.Agent,
			"reason":  s.Reason,
			"class":   s.Class,
			"count":   s.Count,
			"explain": s.Explain,
		}
		if s.Stale {
			row["stale"] = true
		}
		if s.FirstAt != "" {
			row["firstAt"] = s.FirstAt
		}
		if s.LastAt != "" {
			row["lastAt"] = s.LastAt
		}
		if s.Remedy != "" {
			row["remedy"] = s.Remedy
		}
		if len(s.Exemplars) > 0 {
			row["exemplars"] = s.Exemplars
		}
		rows = append(rows, row)
	}
	return rows
}

// SyncStructurallyDisabled reports whether this device cannot fan out at all
// because it holds a non-nil rules engine with zero rules — the fail-closed
// state a fresh (or wiped) install lands in. It is deliberately a separate,
// cheap predicate so the status surface can lead with the consequence instead
// of making the operator infer it from an empty suppression table.
func (o *Orchestrator) SyncStructurallyDisabled() bool {
	if o == nil {
		return false
	}
	return o.rulesEngineIsEmpty()
}

// LoadDeferredMaterializationJournal reads the persisted retry queue directly
// from a canonical store, for operator tooling that runs while the daemon is
// stopped. Rows have the same shape as Orchestrator.DeferredMaterializations.
func LoadDeferredMaterializationJournal(storeRoot string) ([]map[string]any, error) {
	queues, err := loadDeferredMaterializationQueues(storeRoot)
	if err != nil {
		return nil, err
	}
	// No orchestrator, so no adapter to ask: report the default-off wording,
	// which is also the state a stopped daemon's journal most likely reflects.
	// Naming the switch is safe advice either way.
	return deferredMaterializationRows(queues, nil), nil
}

// DropDeferredMaterializationJournal is the stopped-daemon counterpart of
// Orchestrator.DropDeferredMaterializations. It must not run against a live
// daemon: the daemon's in-memory queue is authoritative and would overwrite
// the edit on its next persist.
func DropDeferredMaterializationJournal(storeRoot, agent, artifactID string) (int, error) {
	// Rewriting the journal stamps the current version, and the version is
	// also the one-shot watermark for the local-projection repair migration.
	// Editing a pre-migration journal offline would silently consume that
	// watermark and the repair would never run, so refuse instead.
	if deferredMaterializationProjectionMigrationNeeded(storeRoot) {
		if _, err := os.Stat(filepath.Join(storeRoot, deferredMaterializationDirtyName)); err == nil {
			return 0, fmt.Errorf(
				"deferred materialization journal predates per-artifact tracking; " +
					"start the daemon once to migrate it, then retry")
		}
	}
	queues, err := loadDeferredMaterializationQueues(storeRoot)
	if err != nil {
		return 0, err
	}
	dropped := dropDeferredMaterializationsIn(queues, agent, artifactID)
	if dropped == 0 {
		return 0, nil
	}
	return dropped, writeDeferredMaterializationQueues(storeRoot, queues)
}

func deferredMaterializationRows(
	byTarget map[string]*deferredMaterializationQueue, mirrorRepair map[string]materializationSurface,
) []map[string]any {
	agents := make([]string, 0, len(byTarget))
	for agent, queue := range byTarget {
		if agent == "" || queue == nil {
			continue
		}
		agents = append(agents, agent)
	}
	sort.Strings(agents)
	var out []map[string]any
	for _, agent := range agents {
		queue := byTarget[agent]
		if queue.overflow {
			row := map[string]any{
				"agent":    agent,
				"state":    "overflow",
				"attempts": queue.overflowAttempts,
			}
			addDeferredMaterializationTimes(row, queue.overflowFirstDeferred, queue.overflowNextAttempt, time.Time{})
			if queue.overflowLastErr != "" {
				row["lastError"] = queue.overflowLastErr
			}
			out = append(out, row)
		}
		// Entering overflow does not clear the per-artifact ids already in
		// memory, but the drain stops consulting them and the journal stops
		// persisting them. Reporting them would make the live view and the
		// on-disk view disagree by up to deferredMaterializationLimit rows
		// across a restart, for work that is no longer individually tracked.
		if queue.overflow {
			out = append(out, deferredMaterializationAbandonedRows(agent, queue, mirrorRepair[agent])...)
			continue
		}
		standing := make(map[string]struct{}, len(queue.abandoned))
		for _, record := range queue.abandoned {
			if record.artifactID != "" {
				standing[record.artifactID] = struct{}{}
			}
		}
		for _, artifactID := range queue.ids {
			entry, ok := queue.entries[artifactID]
			if !ok {
				continue
			}
			if _, flagged := standing[artifactID]; flagged {
				// A re-admitted write: the give-up record is retained until the
				// write SUCCEEDS, so the artifact is both queued and flagged.
				// Report the flag, not a second pending row for the same
				// artifact — the needs_attention row already says the daemon
				// resumes it automatically, and double-reporting would inflate
				// every count the status surface prints.
				continue
			}
			row := map[string]any{
				"agent":      agent,
				"artifactId": artifactID,
				"state":      "pending",
				"attempts":   entry.attempts,
			}
			if entry.originAgent != "" {
				row["originAgent"] = entry.originAgent
			}
			if entry.escalationHeld {
				// This entry qualified to be raised and the device's rolling
				// escalation budget turned it back. Saying so is the difference
				// between a bounded surface and a silently truncated one.
				row["escalationDeferred"] = true
			}
			addDeferredMaterializationTimes(row, entry.firstDeferred, entry.nextAttempt, time.Time{})
			if entry.lastErr != "" {
				row["lastError"] = entry.lastErr
			}
			out = append(out, row)
		}
		out = append(out, deferredMaterializationAbandonedRows(agent, queue, mirrorRepair[agent])...)
	}
	return out
}

func deferredMaterializationAbandonedRows(
	agent string, queue *deferredMaterializationQueue, surface materializationSurface,
) []map[string]any {
	out := make([]map[string]any, 0, len(queue.abandoned))
	for _, record := range queue.abandoned {
		row := map[string]any{
			"agent":      agent,
			"artifactId": record.artifactID,
			// "needs_attention", not "abandoned". The write is not forgotten:
			// a new canonical commit for this artifact re-enqueues it, and an
			// operator can force it with `aplexica materialize`. Calling it
			// "abandoned" told the operator a true thing (we stopped trying)
			// in a way that implied a false one (nothing will ever fix it) and
			// offered no action, allowing entries to accumulate unnoticed.
			"state":    "needs_attention",
			"attempts": record.attempts,
			// The explanation and the remedy are per class. Every row previously
			// carried the same sentence and the same
			// command — and that command, `repair materialization --agent`,
			// only lists and drops. Handing an operator a command that repairs
			// nothing costs them the time to discover that it does not.
			"explain": escalatedMaterializationExplain(record.declineReason, record.withheldReason, surface) +
				" Aplexica resumes this write automatically if the artifact changes again.",
		}
		if remedy := escalatedMaterializationRemedy(
			agent, record.artifactID, record.declineReason, record.withheldReason, surface); remedy != "" {
			row["remedy"] = remedy
		}
		if record.declineReason != "" {
			row["reason"] = string(record.declineReason)
			row["retryClass"] = string(conversationRetryClassFor(record.declineReason))
		} else if record.withheldReason != "" {
			row["reason"] = string(record.withheldReason)
		}
		if record.originAgent != "" {
			row["originAgent"] = record.originAgent
		}
		addDeferredMaterializationTimes(row, record.firstDeferred, time.Time{}, record.abandonedAt)
		if record.lastErr != "" {
			row["lastError"] = record.lastErr
		}
		out = append(out, row)
	}
	return out
}

func addDeferredMaterializationTimes(row map[string]any, firstDeferred, nextAttempt, abandonedAt time.Time) {
	if !firstDeferred.IsZero() {
		row["firstDeferredAt"] = firstDeferred.UTC().Format(time.RFC3339)
	}
	if !nextAttempt.IsZero() {
		row["nextAttemptAt"] = nextAttempt.UTC().Format(time.RFC3339)
	}
	if !abandonedAt.IsZero() {
		row["abandonedAt"] = abandonedAt.UTC().Format(time.RFC3339)
	}
}

// DropDeferredMaterializations removes queued and abandoned materialization
// records, returning how many it removed. An empty agent matches every target
// and an empty artifactID matches every artifact for the selected targets;
// abandoned-only drops are how an operator clears the diagnostic backlog after
// repairing the underlying canonical heads.
//
// Dropping a pending entry forfeits that write: the artifact is not
// rematerialized until something defers it again (a new canonical commit, a
// startup reconciliation, or an explicit `aplexica materialize`).
func (o *Orchestrator) DropDeferredMaterializations(agent, artifactID string) (int, error) {
	if o == nil {
		return 0, nil
	}
	o.deferredMaterializeMu.Lock()
	dropped := dropDeferredMaterializationsIn(o.deferredMaterialize, agent, artifactID)
	err := o.persistDeferredMaterializationLocked()
	o.deferredMaterializeMu.Unlock()
	return dropped, err
}

func dropDeferredMaterializationsIn(byTarget map[string]*deferredMaterializationQueue, agent, artifactID string) int {
	dropped := 0
	for name, queue := range byTarget {
		if queue == nil || (agent != "" && name != agent) {
			continue
		}
		for _, id := range append([]string(nil), queue.ids...) {
			if artifactID != "" && id != artifactID {
				continue
			}
			delete(queue.entries, id)
			queue.removeIDLocked(id)
			dropped++
		}
		for _, record := range append([]abandonedMaterialization(nil), queue.abandoned...) {
			if artifactID != "" && record.artifactID != artifactID {
				continue
			}
			queue.dropAbandoned(record.artifactID)
			dropped++
		}
		// A whole-target reconciliation carries no artifact ID, so only an
		// unqualified drop for that target can clear it.
		if queue.overflow && artifactID == "" {
			queue.overflow = false
			queue.conversationsOnly = false
			queue.overflowAttempts = 0
			queue.overflowFirstDeferred = time.Time{}
			queue.overflowNextAttempt = time.Time{}
			queue.overflowLastErr = ""
			dropped++
		}
	}
	return dropped
}

func (o *Orchestrator) waitDeferredMaterializationRetry(queue *deferredMaterializationQueue, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-queue.wake:
		return true
	case <-o.bgDone:
		return false
	}
}

// materializeDeferredArtifact replays exactly one artifact into exactly one
// target. strict=true is used only as an error-reporting contract here: the
// item came from the legacy/native path, and stays queued until every eligible
// write succeeds. Durable finalization never enters this queue.
func (o *Orchestrator) materializeDeferredArtifact(ctx context.Context, agent, artifactID string, entry deferredMaterializationEntry) error {
	o.nativeRestoreGate.RLock()
	defer o.nativeRestoreGate.RUnlock()
	art, found := o.findArtifact(artifactID)
	if !found {
		return nil
	}
	now := time.Now().UTC()
	primary, origin := o.backfillPrimary(art)
	if entry.originAgent != "" {
		origin = entry.originAgent
		primary = nil
		for _, ad := range o.cfg.Adapters {
			if ad.Name() == origin {
				primary = ad
				break
			}
		}
	}
	if gate, withheld := o.deferredMaterializationTargetWithheld(agent, art, origin, entry.includePrimary); withheld {
		// Fold the quiescence clock even here, and only that: an entry the gate
		// turns back on every pass can never charge an attempt, so this is the
		// ONLY place its age can ever be judged. The returned short-circuit
		// verdict is deliberately discarded — this pass never looked at the
		// target, and the gate opening is a change it could not have seen.
		o.observeDeferredMaterializationInputs(agent, art, now)
		// Callers outside the drain still match on
		// ErrInboundNativeMaterialization, while the drain reads the withheld
		// marker and declines to spend this entry's retry budget.
		return newMaterializationGateError(gate)
	}
	if o.observeDeferredMaterializationInputs(agent, art, now) {
		// Neither the canonical head nor the destination has moved since the
		// previous attempt, so the target would reach the identical conclusion.
		//
		// This short circuit lives HERE, in the drain, and nowhere else.
		// materializeConversationSession is shared with the large-artifact flush
		// and with the strict durable inbound finalizer, and skipping a write
		// for either of those would turn "we chose not to look" into "the write
		// is complete". The drain is the only caller whose contract is retry
		// pacing.
		return fmt.Errorf("%w: %w", ErrInboundNativeMaterialization, errDeferredMaterializationUnchanged)
	}
	contextDir := ""
	if art.Scope == acf.ScopeProject && art.Project != nil {
		contextDir = art.Project.Path
	}
	return o.fanOutWithOptions(ctx, primary, []string{artifactID}, contextDir, art.SourcePath, entry.includePrimary,
		fanOutOptions{
			targets:     map[string]struct{}{agent: {}},
			originAgent: &origin,
			mirrorsOnly: entry.mirrorsOnly,
			strict:      true,
		})
}

// deferredMaterializationTargetWithheld re-evaluates every reversible gate on
// each retry. A queued write must not bypass policy merely because it was
// eligible when first enqueued, and a temporary pause/quarantine/conflict must
// not be mistaken for successful delivery and dropped.
//
// It names WHICH gate held the write, using the same reasons the suppression
// ledger publishes. That is what lets an entry that never once reached its
// adapter — the whole D2 population — be explained and remediated instead of
// reported as a generic "materialization incomplete".
func (o *Orchestrator) deferredMaterializationTargetWithheld(agent string, art acf.Artifact, origin string, includePrimary bool) (SuppressionReason, bool) {
	var target adapter.Adapter
	for _, ad := range o.cfg.Adapters {
		if ad.Name() == agent {
			target = ad
			break
		}
	}
	if target == nil || !o.runtimeAdapterAvailable(target) {
		return ReasonAdapterNotInstalled, true
	}
	if _, blocked := o.adapterBlocked(agent); blocked {
		return ReasonAdapterBlockedSafety, true
	}
	if o.inUnresolvedConflict(art.ArtifactID) {
		return ReasonConflictUnresolved, true
	}
	if o.cfg.ProjectRegistry != nil && art.Scope == acf.ScopeProject && art.Project != nil {
		entry, known := o.cfg.ProjectRegistry.Get(art.Project.ID)
		if !known {
			return ReasonProjectUnregistered, true
		}
		if entry.EffectiveScope() == "local" && len(entry.Agents) > 0 {
			allowed := false
			for _, name := range entry.Agents {
				if name == agent {
					allowed = true
					break
				}
			}
			if !allowed {
				return ReasonProjectAgentNotListed, true
			}
		}
	}
	if gate := o.syncGate(); gate != nil {
		if !gate.Enabled(agent) {
			return ReasonTargetSyncDisabled, true
		}
		if !includePrimary && origin != "" && !gate.Enabled(origin) {
			return ReasonSourceSyncDisabled, true
		}
	}
	if !includePrimary && origin != "" {
		if _, blocked := o.adapterBlocked(origin); blocked {
			return ReasonSourceAdapterBlocked, true
		}
	}
	if o.cfg.PauseStore != nil {
		if paused, _ := o.cfg.PauseStore.IsPaused(agent, time.Now().UTC()); paused {
			return ReasonPaused, true
		}
	}
	if o.cfg.Quarantine != nil && o.cfg.Quarantine.IsQuarantined(agent, time.Now()) {
		return ReasonQuarantined, true
	}
	branch := selectedBranchForAgent(art, agent)
	if art.Kind == acf.KindConversation {
		if o.conversationRuleAllowsTarget(art, origin, agent, branch) {
			return "", false
		}
		return o.conversationRuleReason(), true
	}
	if eng := o.rulesEngine(); eng != nil {
		names := make([]string, 0, len(o.cfg.Adapters))
		for _, ad := range o.cfg.Adapters {
			names = append(names, ad.Name())
		}
		decision := eng.Evaluate(ruleInputFor(art, origin, branch), syncrules.EvaluateOpts{
			InstalledAgents: names,
		})
		allowed := map[string]struct{}{}
		for _, name := range decision.AllowedAgents {
			allowed[name] = struct{}{}
		}
		if _, ok := allowed[agent]; ok {
			return "", false
		}
		return o.rulesSuppressionReason(allowed), true
	}
	return "", false
}

// seedLocalConversationProjectionRepairs is the one-time migration for
// releases that could discard a safe native-session decline. It queues only
// recent locally-owned conversations whose selected canonical head was
// authored by a different agent. Exact adapter materializers then decide
// idempotently whether the native projection is actually behind.
func (o *Orchestrator) seedLocalConversationProjectionRepairs() {
	if o == nil || o.cfg.Store == nil {
		return
	}
	conversations, err := o.cfg.Store.ListArtifacts(acf.KindConversation)
	if err != nil {
		return
	}
	sort.SliceStable(conversations, func(i, j int) bool {
		return conversations[i].UpdatedAt.After(conversations[j].UpdatedAt)
	})
	counts := map[string]int{}
	for _, art := range conversations {
		if art.SourcePath == "" || art.RemoteOriginDeviceID != "" {
			continue
		}
		owners := o.pathOwners(art.SourcePath)
		if len(owners) != 1 {
			continue
		}
		targetName := ""
		for name := range owners {
			targetName = name
		}
		var target adapter.Adapter
		for _, ad := range o.cfg.Adapters {
			if ad.Name() == targetName {
				target = ad
				break
			}
		}
		if target == nil {
			continue
		}
		if _, ok := target.(adapter.ConversationSessionTarget); !ok {
			continue
		}
		if limit := o.convBackfillLimit(targetName); limit >= 0 && counts[targetName] >= limit {
			continue
		}
		branch := selectedBranchForAgent(art, targetName)
		head, ok, headErr := conversationHeadForBranch(o.cfg.Store, art.ArtifactID, branch)
		if headErr != nil || !ok || head.Provenance.SourceAgent == "" || head.Provenance.SourceAgent == targetName {
			continue
		}
		origin := head.Provenance.SourceAgent
		if _, withheld := o.deferredMaterializationTargetWithheld(targetName, art, origin, false); withheld {
			continue
		}
		counts[targetName]++
		o.deferMaterialization(targetName, art.ArtifactID, origin, false, false, true)
	}
}

// reconcileDeferredMaterializationTarget is the bounded-queue overflow path.
// It may scan the canonical index, but every fan-out remains target-only; no
// healthy sibling adapter is rewritten. Current head provenance reconstructs
// inbound include-primary semantics for a same-named adapter on this device.
func (o *Orchestrator) reconcileDeferredMaterializationTarget(ctx context.Context, agent string, conversationsOnly bool) error {
	available := false
	for _, ad := range o.cfg.Adapters {
		if ad.Name() != agent {
			continue
		}
		available = o.runtimeAdapterAvailable(ad)
		break
	}
	if !available {
		// Reversible: the adapter's binary may reappear. Do not charge this
		// against the target's give-up budget.
		return fmt.Errorf("native materialization target %q is unavailable (%w)",
			agent, errDeferredMaterializationWithheld)
	}
	// Runtime discovery's activation hook may install a safety block. Recheck
	// after availability probing so the reconciliation never crosses it.
	if _, blocked := o.adapterBlocked(agent); blocked {
		return fmt.Errorf("%w (%w)", ErrInboundNativeMaterialization, errDeferredMaterializationWithheld)
	}
	kinds := []acf.Kind{acf.KindMemory, acf.KindSkill, acf.KindTool, acf.KindConversation}
	if conversationsOnly {
		kinds = []acf.Kind{acf.KindConversation}
	}
	for _, kind := range kinds {
		artifacts, err := o.cfg.Store.ListArtifacts(kind)
		if err != nil {
			return err
		}
		for _, art := range artifacts {
			if o.closingNow() {
				return context.Canceled
			}
			includePrimary := false
			originAgent := ""
			if head, ok, headErr := o.cfg.Store.LastEvent(kind, art.ArtifactID); headErr != nil {
				return headErr
			} else if ok {
				originAgent = head.Provenance.SourceAgent
				if o.localDeviceID() != "" && head.Provenance.DeviceID != "" && head.Provenance.DeviceID != o.localDeviceID() {
					includePrimary = true
				}
			}
			if kind == acf.KindConversation {
				branch := selectedBranchForAgent(art, agent)
				if head, ok, headErr := conversationHeadForBranch(o.cfg.Store, art.ArtifactID, branch); headErr != nil {
					return headErr
				} else if ok && head.Provenance.SourceAgent != "" {
					originAgent = head.Provenance.SourceAgent
				}
			}
			if err := o.materializeDeferredArtifact(ctx, agent, art.ArtifactID, deferredMaterializationEntry{
				includePrimary: includePrimary,
				originAgent:    originAgent,
			}); err != nil {
				return err
			}
		}
	}
	return nil
}
