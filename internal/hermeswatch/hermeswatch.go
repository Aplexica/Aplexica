// Package hermeswatch polls a Hermes state.db on an interval and exports
// new/updated sessions to the canonical store via the Hermes adapter.
//
// Not a filesystem watcher: SQLite WAL mode writes are noisy at the
// filesystem layer (the -wal and -shm sidecar files churn on every commit),
// and the WAL-checkpoint timing is not portable across platforms. Polling is
// simple, portable, and good enough at the default 5-second interval — the
// canonical store sync target is a human-perceptible "few seconds late",
// not millisecond-real-time.
//
// The watcher keeps a single high-water mark (HWM) in memory. It does NOT
// persist HWM to disk: on daemon restart, the first tick re-scans every
// session — which is safe because hermes.ImportConversationsFromDB does
// identity reconciliation by SourcePath and content-hash dedupes messages,
// so a full re-scan after restart is a no-op for unchanged sessions.
package hermeswatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter/hermes"
	"github.com/aplexica/aplexica/internal/atomicfile"
	"github.com/aplexica/aplexica/internal/hermesdb"
)

// Direction selects which sync direction(s) a Watcher runs.
type Direction string

const (
	// DirectionOutbound: poll the Hermes state.db and export new/updated
	// sessions to the canonical store.
	DirectionOutbound Direction = "outbound"
	// DirectionInbound: poll the canonical store for Hermes-format conversation
	// artifacts whose head event hash changed and INSERT each one into the
	// target Hermes state.db.
	DirectionInbound Direction = "inbound"
	// DirectionBoth: outbound then inbound on every tick. Default.
	DirectionBoth Direction = "both"
)

// defaultReconcileEvery is the tick count between full inbound reconciliations
// when Watcher.ReconcileEvery is unset. At the default 5s interval this is one
// reconcile per hour. Fresh inbound work is handled by ordinary head-change
// ticks; the full sweep is only a recovery net for rare transient skip states
// (see ReconcileInbound). Keeping it rare prevents a large historical store from
// re-verifying and re-materializing hundreds of already-known sessions every
// minute.
const defaultReconcileEvery = 720

// Watcher polls a Hermes state.db at an interval and exports changed
// sessions to the canonical store. Not safe for concurrent Tick() calls.
type Watcher struct {
	Adapter   *hermes.Adapter
	Store     *acf.Store
	DBPath    string
	Interval  time.Duration // default applied in Run if zero
	Direction Direction     // default DirectionBoth when zero
	StateFile string        // optional; empty means in-memory only

	// ReconcileEvery is the number of ticks between full inbound
	// reconciliations: every Nth tick Run clears the seenHeads cache so
	// TickInbound re-evaluates (and re-exports any missing) conversation
	// artifacts. This recovers conversations a prior tick skipped while the
	// store record was in a transient state (mid-rebase during cross-device
	// sync) — seenHeads is persisted, so without this they stay permanently
	// absent from Hermes. <= 0 applies defaultReconcileEvery in Run.
	ReconcileEvery int

	// Logger is optional. If nil, Watcher is silent. The control loop logs
	// each tick result; tests prefer nil.
	Logger Logger

	// OnImported, if set, is invoked after TickOutbound imports/updates
	// conversation artifacts from the Hermes DB, with their ids. The daemon
	// wires it to the orchestrator's fan-out + remote-forward so a turn a user
	// adds by resuming a materialized conversation in Hermes propagates to
	// sibling agents and other devices LIVE — the hermeswatch import path
	// bypasses handleEvent's fan-out/forward tail, so without this hook the turn
	// only crosses on the next restart's catch-up scan. nil disables it (tests,
	// inbound-only watchers).
	OnImported func(ctx context.Context, ids []string)

	mu          sync.Mutex
	hwm         float64
	seenHeads   map[string]string // artifactID -> last-processed HeadEventHash for successful/skipped inbound work
	failedHeads map[string]string // artifactID -> HeadEventHash for terminal inbound export failures that should survive reconciliation
	// catalogModNano is the last fully processed conversation-artifact catalog
	// generation. WriteArtifact atomically replaces files in that directory, so
	// an unchanged generation lets the five-second inbound poll skip the full
	// ListArtifacts walk. stateDirty records cache-only progress too (including
	// foreign formats that produced no materialized ID), so it survives restart.
	catalogModNano int64
	stateDirty     bool

	// intervalChange signals the Run loop to swap its ticker to the
	// current w.Interval value. Buffered size 1: SetInterval performs a
	// non-blocking send; coalesced multi-edits within a single tick are
	// fine because the Run loop re-reads w.Interval under the lock when
	// it acts on the signal. Allocated by Run; nil before Run starts (or
	// after it returns), in which case SetInterval just updates the
	// field with no signal — the next Run will pick up the new value.
	intervalChange chan struct{}
}

const inboundFormatGateVersion = 3

// persistedState is the on-disk shape of LoadState/SaveState. JSON tags use
// snake_case for readability when inspecting the state file by hand.
type persistedState struct {
	HWM                      float64           `json:"hwm"`
	InboundFormatGateVersion int               `json:"inbound_format_gate_version,omitempty"`
	ConversationCatalogNano  int64             `json:"conversation_catalog_mod_nano,omitempty"`
	SeenHeads                map[string]string `json:"seen_heads"`
	FailedHeads              map[string]string `json:"failed_heads,omitempty"`
}

// LoadState reads StateFile (if non-empty) and seeds hwm + seenHeads. A
// missing file is NOT an error — it's how a first-run watcher behaves.
func (w *Watcher) LoadState() error {
	if w.StateFile == "" {
		return nil
	}
	data, err := os.ReadFile(w.StateFile)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("hermeswatch: read state file: %w", err)
	}
	var s persistedState
	if err := json.Unmarshal(data, &s); err != nil {
		return fmt.Errorf("hermeswatch: parse state file: %w", err)
	}
	w.mu.Lock()
	w.hwm = s.HWM
	if s.InboundFormatGateVersion == inboundFormatGateVersion && s.SeenHeads != nil {
		w.seenHeads = s.SeenHeads
	}
	if s.InboundFormatGateVersion == inboundFormatGateVersion && s.FailedHeads != nil {
		w.failedHeads = s.FailedHeads
	}
	if s.InboundFormatGateVersion == inboundFormatGateVersion {
		w.catalogModNano = s.ConversationCatalogNano
		// Migration: an existing persisted head cache already represents the
		// pre-upgrade catalog. Establish the current directory generation as its
		// baseline rather than replaying every large history once more merely to
		// discover the same hashes. The hourly reconciliation still clears this
		// cursor and performs a full recovery pass.
		if w.catalogModNano == 0 && len(w.seenHeads) > 0 && w.Store != nil {
			if mt, statErr := w.Store.ArtifactCatalogModTime(acf.KindConversation); statErr == nil {
				w.catalogModNano = mt.UnixNano()
				w.stateDirty = true
			}
		}
	}
	w.mu.Unlock()
	return nil
}

// SaveState writes the current hwm + seenHeads to StateFile via atomicfile.
// Silent no-op when StateFile is empty.
func (w *Watcher) SaveState() error {
	if w.StateFile == "" {
		return nil
	}
	w.mu.Lock()
	s := persistedState{
		HWM:                      w.hwm,
		InboundFormatGateVersion: inboundFormatGateVersion,
		ConversationCatalogNano:  w.catalogModNano,
		SeenHeads:                map[string]string{},
		FailedHeads:              map[string]string{},
	}
	for k, v := range w.seenHeads {
		s.SeenHeads[k] = v
	}
	for k, v := range w.failedHeads {
		s.FailedHeads[k] = v
	}
	w.mu.Unlock()
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("hermeswatch: marshal state: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(w.StateFile), 0o755); err != nil {
		return fmt.Errorf("hermeswatch: mkdir state dir: %w", err)
	}
	if err := atomicfile.WriteFile(w.StateFile, data, 0o644); err != nil {
		return err
	}
	w.mu.Lock()
	w.stateDirty = false
	w.mu.Unlock()
	return nil
}

// Logger is a tiny interface that matches both *slog.Logger and a no-op stub.
// Kept narrow so callers don't have to import slog when they don't need to log.
type Logger interface {
	Info(msg string, args ...any)
	Error(msg string, args ...any)
}

// HWM returns the current high-water mark (max observed message timestamp).
func (w *Watcher) HWM() float64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.hwm
}

// SetHWM seeds the high-water mark. Used by tests and by the --since CLI flag.
func (w *Watcher) SetHWM(t float64) {
	w.mu.Lock()
	w.hwm = t
	w.mu.Unlock()
}

// SetInterval updates the poll interval. If a Run loop is currently active
// (intervalChange channel registered), it is signaled to re-arm its ticker
// with the new value on the next select iteration; in-flight tick processing
// is unaffected. If Run is not active, SetInterval just updates the field —
// the next Run() will pick it up at startup.
//
// Live-setter half of the v0.27.x SIGHUP config-reload story for
// hermesWatchInterval. Safe for concurrent callers; the non-blocking send
// coalesces rapid successive edits (the Run loop re-reads w.Interval under
// lock when it processes the signal).
func (w *Watcher) SetInterval(d time.Duration) {
	w.mu.Lock()
	w.Interval = d
	ch := w.intervalChange
	w.mu.Unlock()
	if ch == nil {
		return
	}
	select {
	case ch <- struct{}{}:
	default:
	}
}

// Tick runs whichever direction(s) the Watcher is configured for. Returns
// the union of artifact IDs touched in this tick (outbound's import IDs +
// inbound's materialized IDs).
func (w *Watcher) Tick(ctx context.Context) ([]string, error) {
	dir := w.Direction
	if dir == "" {
		dir = DirectionBoth
	}
	var all []string
	if dir == DirectionOutbound || dir == DirectionBoth {
		ids, err := w.TickOutbound(ctx)
		if err != nil {
			return all, err
		}
		all = append(all, ids...)
	}
	if dir == DirectionInbound || dir == DirectionBoth {
		ids, err := w.TickInbound(ctx)
		if err != nil {
			return all, err
		}
		all = append(all, ids...)
	}
	return all, nil
}

// TickInbound enumerates conversation artifacts in the store whose latest
// materializable payload is a Hermes-decodable format, replays each to its
// current SessionBundle, and INSERTs the bundle into w.DBPath (idempotent —
// hermesdb.InsertSession dedupes by content hash). Skips artifacts whose
// HeadEventHash has not changed since the previous TickInbound call. Returns
// the artifact IDs that were actually materialized in this tick.
func (w *Watcher) TickInbound(ctx context.Context) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("hermeswatch: tick-inbound cancelled: %w", err)
	}
	catalogTime, err := w.Store.ArtifactCatalogModTime(acf.KindConversation)
	if err != nil {
		return nil, fmt.Errorf("hermeswatch: stat conversation catalog: %w", err)
	}
	catalogNano := catalogTime.UnixNano()
	w.mu.Lock()
	processedCatalogNano := w.catalogModNano
	w.mu.Unlock()
	if processedCatalogNano != 0 && processedCatalogNano == catalogNano {
		return nil, nil
	}
	arts, err := w.Store.ListArtifacts(acf.KindConversation)
	if err != nil {
		return nil, fmt.Errorf("hermeswatch: list conversation artifacts: %w", err)
	}
	// Prioritize fresh remote baselines over historical baggage. A single old,
	// broken chain can be expensive to verify; new conversations should still
	// materialize into Hermes promptly.
	sort.SliceStable(arts, func(i, j int) bool {
		if !arts[i].UpdatedAt.Equal(arts[j].UpdatedAt) {
			return arts[i].UpdatedAt.After(arts[j].UpdatedAt)
		}
		if !arts[i].CreatedAt.Equal(arts[j].CreatedAt) {
			return arts[i].CreatedAt.After(arts[j].CreatedAt)
		}
		return arts[i].ArtifactID > arts[j].ArtifactID
	})

	w.mu.Lock()
	if w.seenHeads == nil {
		w.seenHeads = make(map[string]string)
	}
	if w.failedHeads == nil {
		w.failedHeads = make(map[string]string)
	}
	w.mu.Unlock()

	var written []string
	for _, art := range arts {
		if err := ctx.Err(); err != nil {
			return written, fmt.Errorf("hermeswatch: tick-inbound cancelled: %w", err)
		}
		// Cache check: if head hash matches last observed, skip.
		w.mu.Lock()
		seen, ok := w.seenHeads[art.ArtifactID]
		failed, failedOK := w.failedHeads[art.ArtifactID]
		w.mu.Unlock()
		if ok && seen == art.HeadEventHash {
			continue
		}
		if failedOK && failed == art.HeadEventHash {
			continue
		}

		// Format filter: peek at the latest create/update event payload.
		events, err := w.Store.ReadEvents(acf.KindConversation, art.ArtifactID)
		if err != nil {
			w.logError("hermeswatch: read events failed", "id", art.ArtifactID, "err", err)
			continue
		}
		if !isHermesBundleArtifact(events) {
			// Cache this artifact's head so we don't re-check it every tick.
			w.mu.Lock()
			w.seenHeads[art.ArtifactID] = art.HeadEventHash
			delete(w.failedHeads, art.ArtifactID)
			w.stateDirty = true
			w.mu.Unlock()
			continue
		}

		// Replay to the bundle, then INSERT.
		if err := w.Adapter.ExportConversationsToDB(ctx, w.Store, art.ArtifactID, w.DBPath); err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return written, fmt.Errorf("hermeswatch: tick-inbound cancelled: %w", err)
			}
			// Cache the head in failedHeads so a persistently un-exportable
			// artifact (malformed or untranslatable payload) is logged ONCE per
			// head, not re-attempted by normal ticks or full reconciliations. A
			// genuinely new event changes the head and triggers exactly one fresh
			// attempt. Without this, a single bad historical artifact can burn CPU
			// replaying/verifying its chain every reconciliation interval.
			w.logError("hermeswatch: inbound export failed", "id", art.ArtifactID, "err", err)
			w.mu.Lock()
			w.seenHeads[art.ArtifactID] = art.HeadEventHash
			w.failedHeads[art.ArtifactID] = art.HeadEventHash
			w.stateDirty = true
			w.mu.Unlock()
			continue
		}
		w.mu.Lock()
		w.seenHeads[art.ArtifactID] = art.HeadEventHash
		delete(w.failedHeads, art.ArtifactID)
		w.stateDirty = true
		w.mu.Unlock()
		written = append(written, art.ArtifactID)
	}
	w.mu.Lock()
	w.catalogModNano = catalogNano
	w.stateDirty = true
	w.mu.Unlock()
	return written, nil
}

// resetSeenHeads drops the inbound dedup cache so the next TickInbound
// re-evaluates every conversation artifact from scratch, except artifacts with
// unchanged terminal failures in failedHeads.
func (w *Watcher) resetSeenHeads() {
	w.mu.Lock()
	w.seenHeads = map[string]string{}
	w.catalogModNano = 0
	w.stateDirty = true
	w.mu.Unlock()
}

// ReconcileInbound clears the inbound dedup cache (seenHeads), then runs one
// TickInbound — forcing every non-terminal-failed conversation artifact to be
// re-evaluated and (re-)exported into the Hermes DB.
//
// Why this is needed: TickInbound records an artifact's head in seenHeads even
// when it SKIPS the artifact (e.g. it observed the store record in a transient
// state — empty/partial events mid-rebase during cross-device sync — so
// isHermesBundleArtifact returned false) or when an export did not durably
// land. seenHeads is persisted, so such an entry survives restarts and the
// conversation stays permanently absent from Hermes: the plain tick never
// retries because the cached head never changes. Periodically reconciling
// recovers it. Terminal export failures are tracked in failedHeads per head
// hash, so a malformed historical artifact does not get retried by every full
// reconcile until its head changes. ExportConversationsToDB ->
// hermesdb.InsertSession dedupes by content hash, so re-exporting
// already-present sessions is a no-op.
func (w *Watcher) ReconcileInbound(ctx context.Context) ([]string, error) {
	w.resetSeenHeads()
	return w.TickInbound(ctx)
}

// isHermesBundleArtifact returns true iff the latest materializable event in the
// log has a ConversationPayload Hermes can materialize into a SessionBundle.
// The shared LatestEventFormat helper intentionally includes payload-bearing
// snapshots and baselines; retained-lane remote sync delivers new conversations
// to other devices as baseline checkpoints, not create/update events.
// Foreign formats like claude-code.session.jsonl / codex.session.jsonl are
// excluded because those would need a translator the hermes adapter doesn't
// carry.
func isHermesBundleArtifact(events []acf.Event) bool {
	format, ok := acf.LatestEventFormat(events)
	if !ok {
		return false
	}
	return format == hermes.SessionBundleFormat ||
		format == acf.ConversationFormatV1 ||
		format == acf.ConversationDeltaFormatV1
}

// TickOutbound performs one synchronous outbound poll cycle: it reads the
// Hermes state.db, exports any new/updated sessions to the canonical store,
// and advances the high-water mark. Returns artifact IDs written (or
// updated) in this tick. Safe to call from outside Run for testing.
func (w *Watcher) TickOutbound(ctx context.Context) ([]string, error) {
	w.mu.Lock()
	hwm := w.hwm
	w.mu.Unlock()

	abs, err := filepath.Abs(w.DBPath)
	if err != nil {
		return nil, fmt.Errorf("hermeswatch: resolve db path: %w", err)
	}

	bundles, newHWM, err := hermesdb.ListChangedSessions(abs, hwm)
	if err != nil {
		return nil, fmt.Errorf("hermeswatch: list changed sessions: %w", err)
	}
	if len(bundles) == 0 {
		// Still advance HWM in case the query observed activity past the
		// previous mark (e.g. external HWM seed was set artificially high).
		if newHWM > hwm {
			w.mu.Lock()
			w.hwm = newHWM
			w.mu.Unlock()
		}
		return nil, nil
	}

	// Reuse ImportConversationsFromDB to do the full pipeline (identity
	// reconciliation + AppendEvent). Its internal filter uses
	// ListSessions(since), which keys on session.started_at — NOT on
	// max(message.timestamp) like ListChangedSessions does. So we must
	// pass an importSince low enough that every bundle we just enumerated
	// is re-selected: min(StartedAt) - 1 across the changed bundles.
	// (Passing `hwm` directly would drop sessions whose only change is a
	// new message appended to an old session — which is the most common
	// real-world case.)
	//
	// NB: this performs the same SQLite read twice per tick. That's fine for
	// the v0.12.0 cadence (5s default); future revisions can hoist the
	// inner read out by exposing a more granular adapter method.
	importSince := bundles[0].Session.StartedAt - 1
	for _, b := range bundles[1:] {
		if b.Session.StartedAt-1 < importSince {
			importSince = b.Session.StartedAt - 1
		}
	}
	ids, err := w.Adapter.ImportConversationsFromDB(ctx, w.Store, abs, importSince)
	if err != nil {
		return nil, fmt.Errorf("hermeswatch: import: %w", err)
	}

	w.mu.Lock()
	if newHWM > w.hwm {
		w.hwm = newHWM
	}
	w.mu.Unlock()

	// Bridge to the orchestrator's fan-out + remote-forward (handleEvent's tail,
	// which the hermeswatch import path bypasses) so a continuation a user added
	// in Hermes propagates to sibling agents and other devices LIVE.
	if len(ids) > 0 && w.OnImported != nil {
		w.OnImported(ctx, ids)
	}
	return ids, nil
}

// Run blocks until ctx is canceled. Ticks every w.Interval; each tick logs
// "no changes" or "synced N sessions". Returns nil on ctx.Done(); poll
// errors are logged but do NOT terminate the loop (transient DB locks or
// network-mounted-FS hiccups should not kill the watcher).
func (w *Watcher) Run(ctx context.Context) error {
	interval := w.Interval
	if interval <= 0 {
		interval = 5 * time.Second
	}
	if err := w.LoadState(); err != nil {
		w.logError("hermeswatch: load state failed (continuing with fresh state)", "err", err)
	}
	// Persist immediately so migrations that drop stale inbound caches survive
	// even if the first historical sweep is interrupted before it can save.
	if err := w.SaveState(); err != nil {
		w.logError("hermeswatch: initial state save failed", "err", err)
	}
	defer func() {
		if err := w.SaveState(); err != nil {
			w.logError("hermeswatch: save state on shutdown failed", "err", err)
		}
	}()

	w.logInfo("hermeswatch starting", "db", w.DBPath, "interval", interval, "direction", string(w.directionOrDefault()), "state_file", w.StateFile)

	// Register the intervalChange channel so SetInterval can signal us
	// to swap the ticker. Clear it on exit so a post-Run SetInterval
	// degrades to "field update only, no signal" (matches its docs).
	w.mu.Lock()
	w.intervalChange = make(chan struct{}, 1)
	w.mu.Unlock()
	defer func() {
		w.mu.Lock()
		w.intervalChange = nil
		w.mu.Unlock()
	}()

	t := time.NewTicker(interval)
	// NOTE: deliberately NOT using `defer t.Stop()` — the intervalChange
	// branch reassigns t, so a deferred Stop on the original ticker would
	// be a stale reference. Cleanup is performed explicitly in the
	// ctx.Done branch below (and at the top of the intervalChange branch
	// before reassignment).

	reconcileEvery := w.ReconcileEvery
	if reconcileEvery <= 0 {
		reconcileEvery = defaultReconcileEvery
	}
	tickCount := 0
	// runTick reports whether the loop should STOP. A tick that fails with a
	// permanent ErrNotHermesDB (the target file is missing/empty/wrong-schema)
	// disables the watcher rather than retrying — and re-logging — every
	// interval forever, which is what flooded the daemon log.
	runTick := func() bool {
		tickCount++
		// Every Nth tick, clear the inbound dedup cache so a conversation a
		// prior tick skipped (observed mid-rebase, or marked seen without a
		// durable export) is re-evaluated and re-exported to Hermes. The first
		// tick (tickCount==1) is exempt so startup honors the persisted cache.
		if reconcileEvery > 0 && tickCount%reconcileEvery == 0 {
			w.resetSeenHeads()
		}
		err := w.tickAndLog(ctx)
		if err == nil {
			return false
		}
		if errors.Is(err, hermesdb.ErrNotHermesDB) {
			w.logError("hermeswatch disabled: target is not a Hermes state.db; stopping poll loop", "db", w.DBPath, "err", err)
			return true
		}
		// Transient failure (locked DB, FS hiccup): log and keep polling.
		w.logError("hermeswatch tick failed", "err", err)
		return false
	}

	// First tick fires immediately, not after the first interval.
	if runTick() {
		t.Stop()
		return nil
	}

	for {
		select {
		case <-ctx.Done():
			t.Stop()
			w.logInfo("hermeswatch exiting", "reason", ctx.Err())
			return nil
		case <-t.C:
			if runTick() {
				t.Stop()
				return nil
			}
		case <-w.intervalChange:
			t.Stop()
			w.mu.Lock()
			newInterval := w.Interval
			w.mu.Unlock()
			if newInterval <= 0 {
				newInterval = 5 * time.Second
			}
			t = time.NewTicker(newInterval)
			w.logInfo("hermeswatch interval updated", "new", newInterval)
		}
	}
}

// tickAndLog performs one tick, logs+saves on success, and RETURNS any tick
// error so the caller (runTick) can decide whether the failure is permanent
// (disable) or transient (log + keep polling).
func (w *Watcher) tickAndLog(ctx context.Context) error {
	ids, err := w.Tick(ctx)
	if err != nil {
		return err
	}
	w.mu.Lock()
	dirty := w.stateDirty
	w.mu.Unlock()
	if len(ids) == 0 && !dirty {
		return nil
	}
	if len(ids) > 0 {
		w.logInfo("hermeswatch synced artifacts", "count", len(ids), "direction", string(w.directionOrDefault()), "hwm", w.HWM())
	}
	if err := w.SaveState(); err != nil {
		w.logError("hermeswatch: incremental state save failed", "err", err)
	}
	return nil
}

func (w *Watcher) directionOrDefault() Direction {
	if w.Direction == "" {
		return DirectionBoth
	}
	return w.Direction
}

func (w *Watcher) logInfo(msg string, args ...any) {
	if w.Logger != nil {
		w.Logger.Info(msg, args...)
	}
}

func (w *Watcher) logError(msg string, args ...any) {
	if w.Logger != nil {
		w.Logger.Error(msg, args...)
	}
}
