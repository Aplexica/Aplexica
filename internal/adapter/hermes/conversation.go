package hermes

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter"
	"github.com/aplexica/aplexica/internal/hermesdb"
)

// SessionBundleFormat is the Format string used in ConversationPayload for
// Hermes session bundles. Bumping this string is a breaking change — adapters
// MUST refuse to decode an unknown format and surface a clear error.
// SessionBundleFormat aliases acf.ConversationFormatHermesBundle so
// cross-agent materializers can recognize hermes payloads without
// importing this package.
const SessionBundleFormat = acf.ConversationFormatHermesBundle

// canonicalImportSource marks sessions the daemon itself wrote into the
// hermes DB (TickInbound / ExportConversationsToDB). The import path skips
// them: re-importing our own export would mint a duplicate hermes-sourced
// artifact for every cross-agent conversation synced into hermes (E2E F5).
const canonicalImportSource = hermesdb.AplexicaCanonicalImportSource

// ImportConversationsFromDB reads every session from the Hermes DB at dbPath
// with started_at > sinceUnixSeconds (pass 0 for "all"), and creates one ACF
// conversation artifact per session. Identity reconciliation keys on
// SourcePath = "<abs dbPath>#session=<session-id>", so re-importing the same
// session appends an "update" event instead of creating a duplicate.
//
// No-change skip: if a session's encoded SessionBundle is byte-identical to
// the most recent create/update event on the existing artifact, no event is
// appended (and the artifact's UpdatedAt is not touched). The artifact ID is
// still returned so callers (e.g. hermeswatch) can track it. This suppresses
// spurious "update" events on daemon-restart full re-scans of unchanged DBs.
func (a *Adapter) ImportConversationsFromDB(ctx context.Context, store *acf.Store, dbPath string, sinceUnixSeconds float64) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("hermes: import-conversations cancelled: %w", err)
	}
	abs, err := filepath.Abs(dbPath)
	if err != nil {
		return nil, fmt.Errorf("hermes: resolve db path: %w", err)
	}
	bundles, err := hermesdb.ListSessions(abs, sinceUnixSeconds)
	if err != nil {
		return nil, fmt.Errorf("hermes: list sessions: %w", err)
	}

	var ids []string
	for _, b := range bundles {
		// Bidirectional thread merge (mirrors claudecode/codex dispatch). A
		// session the daemon materialized into this DB carries the canonical
		// THREAD id as its session id. Merge by text turns:
		//   - unchanged echo of our own export → no-op (loop break), and
		//   - a turn the user ADDED by resuming the materialized session in
		//     Hermes → append a continuation to the ORIGINAL conversation so it
		//     fans out to every other agent and publishes back to other devices.
		// This replaces the old blanket `source==canonical-import` skip, which
		// stranded such continuations in the Hermes DB forever. Native hermes
		// sessions (id is not a canonical thread) fall through to a path-keyed
		// import.
		events := EncodeBundleAsCanonical(b)
		if mergedIDs, handled, merr := adapter.MergeConversationByThread(
			ctx, store, a.opaqueParams(), b.Session.ID, events, adapter.EncodeCanonicalConversationPayload,
		); handled {
			if merr != nil {
				return ids, merr
			}
			ids = append(ids, mergedIDs...) // continuation id, or empty on a no-op loop break
			continue
		}
		// Not a known canonical thread. A daemon-written echo whose artifact we
		// can't reconcile (e.g. the thread was pruned, or this is a stale export)
		// must NOT mint a duplicate hermes-native artifact — skip it, preserving
		// the original echo-suppression. Only genuinely native sessions get a
		// path-keyed import.
		if b.Session.Source == canonicalImportSource {
			continue
		}
		id, err := a.importOneSession(ctx, store, abs, b)
		if err != nil {
			return ids, err // partial-progress: return what we got so far
		}
		ids = append(ids, id)
	}
	return ids, nil
}

func (a *Adapter) importOneSession(ctx context.Context, store *acf.Store, dbAbsPath string, b hermesdb.SessionBundle) (string, error) {
	sourcePath := dbAbsPath + "#session=" + b.Session.ID
	name := b.Session.ID
	if b.Session.Title != nil && *b.Session.Title != "" {
		name = *b.Session.Title
	}

	payloadJSON, err := a.encodeConversationPayload(b)
	if err != nil {
		return "", err
	}

	existing, found, err := store.FindBySourcePath(acf.KindConversation, sourcePath)
	if err != nil {
		return "", fmt.Errorf("hermes: find by source path: %w", err)
	}

	// No-change skip: if the artifact already exists AND its current payload
	// (most recent create/update event) is byte-identical to what we're about
	// to write, do nothing. Avoids spurious "update" events on every daemon
	// restart full re-scan. Identity reconciliation still returns the artifact
	// ID so the caller can track it.
	if found {
		same, err := payloadMatchesLatest(store, existing.ArtifactID, payloadJSON)
		if err != nil {
			return "", fmt.Errorf("hermes: compare payload: %w", err)
		}
		if same {
			return existing.ArtifactID, nil
		}
		// Anti-revert (see adapter.WouldRevertThread): a stale shorter copy must
		// not overwrite a newer continuation that arrived from another agent.
		if adapter.WouldRevertThread(store, existing.ArtifactID, acf.ExtractTextTurns(EncodeBundleAsCanonical(b))) {
			return existing.ArtifactID, nil
		}
	}

	now := time.Now().UTC()
	var id, parentHash string
	var eventType acf.EventType
	createdNew := false

	if found {
		id = existing.ArtifactID
		eventType = acf.EventTypeUpdate
		var herr error
		parentHash, herr = adapter.RefreshMainBranchHead(store, acf.KindConversation, &existing)
		if herr != nil {
			return "", herr
		}
		existing.UpdatedAt = now
		existing.Name = name // refresh title if it changed
		if werr := store.WriteArtifact(existing); werr != nil {
			return "", werr
		}
	} else {
		id = acf.NewID()
		eventType = acf.EventTypeCreate
		artifact := acf.Artifact{
			AcfSchemaVersion: acf.SchemaVersion,
			ArtifactID:       id,
			Kind:             acf.KindConversation,
			Scope:            acf.ScopeGlobal, // sessions live under ~/.hermes/
			Name:             name,
			SourcePath:       sourcePath,
			CreatedAt:        now,
			UpdatedAt:        now,
		}
		if werr := store.WriteArtifact(artifact); werr != nil {
			return "", werr
		}
		createdNew = true
	}

	event := acf.Event{
		EventID:    acf.NewID(),
		ArtifactID: id,
		Type:       eventType,
		Timestamp:  now,
		Provenance: acf.Provenance{
			DeviceID:       a.deviceID(),
			SourceAgent:    a.Name(),
			AgentVersion:   acf.UnknownAgentVersion,
			AdapterVersion: a.Version(),
		},
		Payload:    payloadJSON,
		ParentHash: parentHash,
	}
	if aerr := store.AppendEvent(acf.KindConversation, event); aerr != nil {
		if createdNew {
			_ = store.DeleteArtifact(acf.KindConversation, id)
		}
		return "", aerr
	}
	return id, nil
}

// ExportConversationsToDB replays the conversation artifact's events back into
// a SessionBundle, then INSERTs it into the SQLite DB at dbPath. Idempotent
// (uses hermesdb.InsertSession). The DB at dbPath must already exist with the
// canonical Hermes schema — typically the user has run `hermes` at least once
// on the destination machine to initialize ~/.hermes/state.db.
//
// Retention/prune resilience (FR-02.32): once retention.CreateSnapshot +
// PruneArtifact run on a main-only conversation, every pre-snapshot create/
// update event moves into .compacted and the ACTIVE log holds only the
// snapshot event. We first try to materialize from the active log alone (the
// common, cheap path). If that yields no exportable payload — because the
// snapshot is payload-less, or because the active-only log can't even
// VerifyChain across the prune boundary (the snapshot's ParentHash references
// the now-compacted pre-snapshot head) — we fall back to
// Store.ReadEventsIncludingCompacted, which re-merges the active + compacted
// layers into a log that both VerifyChains and contains the create/update
// payload. Defense in depth: a payload-bearing snapshot (the root fix) is a
// self-contained checkpoint the active-log walk decodes directly, while the
// compacted fallback re-materializes even a payload-less snapshot.
func (a *Adapter) ExportConversationsToDB(ctx context.Context, store *acf.Store, artifactID, dbPath string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("hermes: export-conversations cancelled: %w", err)
	}
	// Hot path for Codex/Claude prompt and answer updates: materialize backward
	// from the newest self-contained payload instead of reading and decoding the
	// artifact's complete active JSONL history (live transcripts can exceed
	// 100 MiB). If Hermes already has the exact timestamp+role+content rows, no
	// historical identities are needed and the portable upsert is complete.
	// Only the head-bound in-process cache is eligible here. It is populated
	// after a native import or verified remote delivery commits successfully,
	// and its validator re-reads and hashes the persisted main head. A generic
	// backward materialization is intentionally not enough: it can decode a
	// self-contained tail from a manually damaged chain before VerifyChain has
	// rejected that chain (see the corrupt-chain regression below).
	if payload, ok, cacheErr := store.ValidatedCachedMaterializedConversationPayload(artifactID); cacheErr != nil {
		return fmt.Errorf("hermes: validate cached portable payload: %w", cacheErr)
	} else if ok && payload.Format == acf.ConversationFormatV1 {
		head, found, headErr := store.LastEvent(acf.KindConversation, artifactID)
		if headErr != nil {
			return fmt.Errorf("hermes: read cached portable head: %w", headErr)
		}
		if !found {
			return fmt.Errorf("hermes: cached portable head missing for artifact %s", artifactID)
		}
		encoded, encodeErr := acf.EncodePayload(payload)
		if encodeErr != nil {
			return fmt.Errorf("hermes: encode materialized portable payload: %w", encodeErr)
		}
		head.Payload = encoded
		current, _, portable, decodeErr := decodeAnyConversationFormat(head, artifactID)
		if decodeErr != nil {
			return decodeErr
		}
		if portable {
			if len(current.Messages) == 0 {
				return nil
			}
			needsHistory, preflightErr := hermesdb.PortableRepairNeedsHistory(dbPath, current)
			if preflightErr != nil {
				return fmt.Errorf("hermes: preflight portable repair: %w", preflightErr)
			}
			if !needsHistory {
				return hermesdb.InsertPortableSession(dbPath, current, nil)
			}
		}
	}
	events, err := store.ReadEvents(acf.KindConversation, artifactID)
	if err != nil {
		return fmt.Errorf("hermes: read events: %w", err)
	}
	if len(events) == 0 {
		return fmt.Errorf("hermes: no events for artifact %s", artifactID)
	}

	current, obsolete, redacted, portable, err := exportableBundleFromActiveLog(events, artifactID)
	if err != nil {
		return err
	}
	if current == nil && !redacted {
		// The active log has no exportable payload and is not an explicit
		// redaction — most commonly a snapshot-only log after an on-snapshot
		// prune. Re-merge the .compacted layer so the create/update payload
		// (and a VerifyChain-able chain) come back into view, then replay that.
		merged, merr := store.ReadEventsIncludingCompacted(acf.KindConversation, artifactID)
		if merr != nil {
			return fmt.Errorf("hermes: read events including compacted: %w", merr)
		}
		// Surface a genuine corruption clearly: if even the merged log fails
		// VerifyChain, the active-only VerifyChain failure was NOT a benign
		// prune-boundary artifact but real chain damage. Report it as the hard
		// "event log invalid" error the pre-fallback code did, rather than the
		// generic "no exportable payload" below.
		if verr := acf.VerifyChain(merged); verr != nil {
			return fmt.Errorf("hermes: event log invalid: %w", verr)
		}
		current, obsolete, redacted, portable, err = exportableBundleFromActiveLog(merged, artifactID)
		if err != nil {
			return err
		}
	} else if current != nil && portable {
		// Avoid replaying/decompressing the complete historical log on every
		// prompt and answer export. Exact current Hermes row identities need only
		// the active portable projection; visible divergence (including identical
		// text at legacy timestamps) requests history for one-time precise repair.
		needsHistory, perr := hermesdb.PortableRepairNeedsHistory(dbPath, *current)
		if perr != nil {
			return fmt.Errorf("hermes: preflight portable repair: %w", perr)
		}
		if needsHistory {
			merged, merr := store.ReadEventsIncludingCompacted(acf.KindConversation, artifactID)
			if merr != nil {
				return fmt.Errorf("hermes: read events including compacted for portable repair: %w", merr)
			}
			if verr := acf.VerifyChain(merged); verr != nil {
				return fmt.Errorf("hermes: event log invalid: %w", verr)
			}
			current, obsolete, redacted, portable, err = exportableBundleFromActiveLog(merged, artifactID)
			if err != nil {
				return err
			}
		}
	}
	if current == nil {
		return fmt.Errorf("hermes: artifact %s is redacted or has no exportable payload; nothing to export", artifactID)
	}
	if len(current.Messages) == 0 {
		// Hidden-only artifacts (for example a historical Claude /model
		// command row) are valid no-op exports. Writing them would create a
		// blank/ID-only Hermes session that the watcher can re-import.
		return nil
	}
	if portable {
		return hermesdb.InsertPortableSession(dbPath, *current, obsolete)
	}
	return hermesdb.InsertSession(dbPath, *current)
}

// exportableBundleFromActiveLog verifies the event chain and walks BACKWARD to
// the single effective payload that materializes the session ("last write
// wins"), returning the decoded bundle.
//
// The LATEST materialized payload determines the exported transcript. Before
// folding to that state, canonical payloads in the available history are also
// decoded into exact Hermes row identities. Rows absent from the current
// portable bundle are stale repair candidates (including system/tool traffic,
// retimestamped visible rows, and assistant commentary removed by a sanitizer).
// Foreign historical formats are ignored for candidate derivation but continue
// to participate in normal latest-state materialization.
//
// A payload-bearing EventTypeSnapshot (FR-02.32) is a self-contained
// checkpoint: it carries the materialized payload, so the walk decodes it and
// stops, exactly as it would a create/update. A payload-less snapshot is
// skipped (keep walking), preserving the legacy behavior for snapshots written
// before the FR-02.32 change.
//
// Return contract: (bundle, obsolete, redacted, portable, err).
//   - (non-nil, obsolete, false, true, nil): a canonical portable payload was found.
//   - (non-nil, nil, false, false, nil): a native Hermes bundle was found.
//   - (nil, nil, true, false, nil): the latest mutating event is a redaction — caller must
//     NOT fall back to the compacted layer (the redaction is authoritative).
//   - (nil, nil, false, false, nil): no exportable payload in this slice — caller may
//     re-try against ReadEventsIncludingCompacted.
//
// VerifyChain errors are NOT fatal to the caller: an active-only slice can fail
// verification at the prune boundary (the snapshot's ParentHash points at the
// now-compacted head). That is reported as (nil, false, false, nil) so the caller
// retries against the merged log, which verifies cleanly. A verification
// failure of the MERGED log surfaces as a hard error from that retry.
func exportableBundleFromActiveLog(events []acf.Event, artifactID string) (*hermesdb.SessionBundle, []hermesdb.MessageRow, bool, bool, error) {
	if err := acf.VerifyChain(events); err != nil {
		// Can't trust this slice — treat as "no exportable payload here" so
		// the caller falls back to the merged (active + compacted) log.
		return nil, nil, false, false, nil
	}
	// Redaction barrier (hermes-owned policy; acf.LatestPayloadEvent is policy-
	// free): a redaction authoritatively removes content, so a payload at or
	// before it must NOT be resurrected. Bound the payload walk to events NEWER
	// than the latest redaction — otherwise the delegate would walk past it.
	window := events
	hasRedaction := false
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Type == acf.EventTypeRedaction {
			window = events[i+1:]
			hasRedaction = true
			break
		}
	}
	e, ok := acf.LatestPayloadEvent(window)
	if !ok {
		// hasRedaction && !ok: the latest mutating event is a redaction →
		// signal redacted==true so the caller does NOT fall back to compacted.
		return nil, nil, hasRedaction, false, nil
	}
	// This must happen before MaterializedConversationPayloadBytes replaces old
	// full payloads with the latest state. Once folded, identities emitted by a
	// polluted predecessor can no longer be reconstructed.
	historicalRows, herr := historicalCanonicalMessageRows(window, artifactID)
	if herr != nil {
		return nil, nil, false, false, herr
	}
	materialized, mok, merr := acf.MaterializedConversationPayloadBytes(conversationMaterializationSuffix(window))
	if merr != nil {
		return nil, nil, false, false, merr
	}
	if mok {
		e.Payload = materialized
	}
	b, obsolete, portable, derr := decodeAnyConversationFormat(e, artifactID)
	if derr != nil {
		if e.Type == acf.EventTypeSnapshot {
			return nil, nil, false, false, fmt.Errorf("hermes: decode snapshot event %s: %w", e.EventID, derr)
		}
		return nil, nil, false, false, fmt.Errorf("hermes: decode event %s: %w", e.EventID, derr)
	}
	if portable {
		obsolete = historicalRowsAbsentFromCurrent(historicalRows, b.Messages)
	}
	return &b, obsolete, false, portable, nil
}

func historicalCanonicalMessageRows(events []acf.Event, artifactID string) ([]hermesdb.MessageRow, error) {
	var rows []hermesdb.MessageRow
	for _, event := range events {
		switch event.Type {
		case acf.EventTypeCreate, acf.EventTypeUpdate, acf.EventTypeResolution,
			acf.EventTypeSnapshot, acf.EventTypeBaseline:
		default:
			continue
		}
		if !acf.HasPayload(event.Payload) {
			continue
		}
		payload, err := acf.DecodeConversationPayload(event)
		if err != nil {
			// Candidate collection is best-effort for superseded history. If an
			// undecodable event still contributes to the current state, the normal
			// materialization below includes it and returns the hard error there.
			continue
		}
		if payload.Format != acf.ConversationFormatV1 && payload.Format != acf.ConversationDeltaFormatV1 {
			continue
		}
		bundle := DecodeBundleFromCanonical(artifactID, event.Provenance.SourceAgent, payload.Events)
		rows = append(rows, bundle.Messages...)
	}
	return rows, nil
}

// conversationMaterializationSuffix starts replay at the newest decodable
// full-state payload. Events before that anchor are superseded and must not
// make a valid latest canonical replacement fail merely because an old foreign
// payload cannot be decoded as ConversationPayload. An undecodable event at or
// after the selected anchor remains in the suffix, so corruption that can
// affect the current state still surfaces as a hard materialization error.
func conversationMaterializationSuffix(events []acf.Event) []acf.Event {
	for i := len(events) - 1; i >= 0; i-- {
		event := events[i]
		switch event.Type {
		case acf.EventTypeCreate, acf.EventTypeUpdate, acf.EventTypeResolution,
			acf.EventTypeSnapshot, acf.EventTypeBaseline:
		default:
			continue
		}
		if !acf.HasPayload(event.Payload) {
			continue
		}
		payload, err := acf.DecodeConversationPayload(event)
		if err == nil && payload.Format != acf.ConversationDeltaFormatV1 {
			return events[i:]
		}
	}
	return events
}

type hermesMessageIdentity struct {
	Timestamp  float64 `json:"timestamp"`
	Role       string  `json:"role"`
	Content    string  `json:"content"`
	ToolCalls  string  `json:"tool_calls"`
	ToolCallID string  `json:"tool_call_id"`
	ToolName   string  `json:"tool_name"`
}

func historicalRowsAbsentFromCurrent(historical, current []hermesdb.MessageRow) []hermesdb.MessageRow {
	currentIdentities := make(map[string]struct{}, len(current))
	for _, message := range current {
		currentIdentities[hermesMessageIdentityKey(message)] = struct{}{}
	}
	seen := make(map[string]struct{}, len(historical))
	obsolete := make([]hermesdb.MessageRow, 0)
	for _, message := range historical {
		key := hermesMessageIdentityKey(message)
		if _, keep := currentIdentities[key]; keep {
			continue
		}
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		obsolete = append(obsolete, message)
	}
	return obsolete
}

func hermesMessageIdentityKey(message hermesdb.MessageRow) string {
	identity := hermesMessageIdentity{
		Timestamp:  message.Timestamp,
		Role:       message.Role,
		Content:    stringValue(message.Content),
		ToolCalls:  stringValue(message.ToolCalls),
		ToolCallID: stringValue(message.ToolCallID),
		ToolName:   stringValue(message.ToolName),
	}
	encoded, _ := json.Marshal(identity)
	return string(encoded)
}

func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func encodeSessionBundle(b hermesdb.SessionBundle) (json.RawMessage, error) {
	inner, err := json.Marshal(b)
	if err != nil {
		return nil, fmt.Errorf("hermes: marshal session bundle: %w", err)
	}
	return acf.EncodePayload(acf.ConversationPayload{
		Format:  SessionBundleFormat,
		Content: string(inner),
	})
}

// payloadMatchesLatest returns true iff the artifact's most-recent create/update
// event payload is byte-identical to candidate. Used to suppress no-op
// re-imports (most commonly: daemon restart full-scans an unchanged session).
func payloadMatchesLatest(store *acf.Store, artifactID string, candidate json.RawMessage) (bool, error) {
	events, err := store.ReadEvents(acf.KindConversation, artifactID)
	if err != nil {
		return false, err
	}
	// Walk backward; first create/update/resolution wins.
	for i := len(events) - 1; i >= 0; i-- {
		e := events[i]
		if e.Type != acf.EventTypeCreate && e.Type != acf.EventTypeUpdate && e.Type != acf.EventTypeResolution {
			// EventTypeResolution (v0.34.0) carries a full payload.
			continue
		}
		// json.RawMessage compares byte-by-byte. The encoder always produces
		// the same canonical JSON for the same SessionBundle (Go's json.Marshal
		// is deterministic for struct ordering; map keys are sorted).
		return bytes.Equal(e.Payload, candidate), nil
	}
	// No create/update events found — treat as different so we write one.
	return false, nil
}

func decodeSessionBundle(e acf.Event) (hermesdb.SessionBundle, error) {
	p, err := acf.DecodeConversationPayload(e)
	if err != nil {
		return hermesdb.SessionBundle{}, err
	}
	if p.Format != SessionBundleFormat {
		return hermesdb.SessionBundle{}, fmt.Errorf("hermes: unsupported conversation format %q (expected %q)", p.Format, SessionBundleFormat)
	}
	var b hermesdb.SessionBundle
	if err := json.Unmarshal([]byte(p.Content), &b); err != nil {
		return hermesdb.SessionBundle{}, fmt.Errorf("hermes: unmarshal session bundle: %w", err)
	}
	return b, nil
}

// encodeConversationPayload picks the wire format based on the adapter's
// CanonicalConversations flag. Legacy (default) wraps the SessionBundle in
// SessionBundleFormat; canonical mode emits acf.ConversationFormatV1 with
// the structured Events field populated via EncodeBundleAsCanonical.
func (a *Adapter) encodeConversationPayload(b hermesdb.SessionBundle) (json.RawMessage, error) {
	if a.CanonicalConversations {
		return acf.EncodePayload(acf.ConversationPayload{
			Format: acf.ConversationFormatV1,
			Events: EncodeBundleAsCanonical(b),
		})
	}
	return encodeSessionBundle(b)
}

// decodeAnyConversationFormat dispatches by payload Format. Legacy
// acf.hermes.session.v1 → embedded SessionBundle JSON unwrapped. Canonical
// acf.conversation.v1 → portable user/assistant projection with the artifactID
// as the synthetic session ID. The returned bool reports that portable mode.
func decodeAnyConversationFormat(e acf.Event, artifactID string) (hermesdb.SessionBundle, []hermesdb.MessageRow, bool, error) {
	p, err := acf.DecodeConversationPayload(e)
	if err != nil {
		return hermesdb.SessionBundle{}, nil, false, err
	}
	switch p.Format {
	case SessionBundleFormat:
		var b hermesdb.SessionBundle
		if err := json.Unmarshal([]byte(p.Content), &b); err != nil {
			return hermesdb.SessionBundle{}, nil, false, fmt.Errorf("hermes: unmarshal session bundle: %w", err)
		}
		return b, nil, false, nil
	case acf.ConversationFormatV1:
		// Canonical cross-agent materialization is always a portable visible
		// transcript. Provenance belongs to the latest update, not necessarily
		// to the artifact's original agent; using it as a fidelity switch made a
		// Hermes continuation re-expose old Codex system/tool internals.
		portable := DecodePortableBundleFromCanonical(artifactID, e.Provenance.SourceAgent, p.Events)
		return portable, nil, true, nil
	}
	return hermesdb.SessionBundle{}, nil, false, fmt.Errorf("hermes: unsupported conversation format %q", p.Format)
}
