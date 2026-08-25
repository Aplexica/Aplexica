package kilo

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter"
	"github.com/aplexica/aplexica/internal/project"
)

// ReadOnlyConversationSession is a decoded native Kilo session. It exists for
// read-only verification and diagnostics that must inspect an Aplexica-owned
// synced session without feeding that session back through the import/echo
// suppression pipeline.
type ReadOnlyConversationSession struct {
	SessionID string
	Events    []acf.ConversationEvent
}

// ReadConversationSessions decodes native Kilo sessions updated after
// sinceMillis without mutating a canonical store. ImportConversationsFromDB
// intentionally suppresses Aplexica-owned session IDs to prevent sync echoes;
// callers that are verifying the native projection need this read-only view.
func ReadConversationSessions(ctx context.Context, dbPath string, sinceMillis int64) ([]ReadOnlyConversationSession, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("kilo: read conversations cancelled: %w", err)
	}
	abs, err := filepath.Abs(dbPath)
	if err != nil {
		return nil, fmt.Errorf("kilo: resolve db path: %w", err)
	}
	bundles, err := listKiloSessionBundles(abs, sinceMillis)
	if err != nil {
		return nil, fmt.Errorf("kilo: list sessions: %w", err)
	}
	out := make([]ReadOnlyConversationSession, 0, len(bundles))
	for _, bundle := range bundles {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("kilo: read conversations cancelled: %w", err)
		}
		events, err := encodeKiloBundleAsCanonical(bundle)
		if err != nil {
			return nil, err
		}
		out = append(out, ReadOnlyConversationSession{SessionID: bundle.Session.ID, Events: events})
	}
	return out, nil
}

// ImportConversationsFromDB reads Kilo sessions from dbPath with
// time_updated > sinceMillis (pass 0 for all current sessions) and creates one
// canonical ACF conversation artifact per session. This is intentionally
// read-only: Kilo still does not expose a stable conversation export surface.
func (a *Adapter) ImportConversationsFromDB(ctx context.Context, store *acf.Store, dbPath string, sinceMillis int64) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("kilo: import-conversations cancelled: %w", err)
	}
	abs, err := filepath.Abs(dbPath)
	if err != nil {
		return nil, fmt.Errorf("kilo: resolve db path: %w", err)
	}
	bundles, err := listKiloSessionBundles(abs, sinceMillis)
	if err != nil {
		return nil, fmt.Errorf("kilo: list sessions: %w", err)
	}

	ids := make([]string, 0, len(bundles))
	var syncedThreads map[string]adapter.ThreadRef // synced session id → canonical thread ref; built lazily
	for _, b := range bundles {
		if err := ctx.Err(); err != nil {
			return ids, fmt.Errorf("kilo: import-conversations cancelled: %w", err)
		}
		// A session the daemon materialized into kilo (synced echo). If the user
		// CONTINUED it in kilo, route the new turns through the loop-safe thread
		// merge so the continuation propagates to the other agents (mirrors the
		// hermes fix). The synced session id is a truncated forward hash of the
		// artifact id, so we recover the canonical thread via a recomputed forward
		// map. Unchanged echoes (and ids we can't map) are skipped — never
		// re-minted as a duplicate kilo-native artifact.
		if strings.HasPrefix(b.Session.ID, syncedSessionIDPrefix) {
			if syncedThreads == nil {
				syncedThreads = kiloSyncedThreadRefMap(store)
			}
			ref := syncedThreads[b.Session.ID]
			if ref.ArtifactID == "" {
				continue
			}
			events, eerr := encodeKiloBundleAsCanonical(b)
			if eerr != nil {
				continue
			}
			mergedIDs, handled, merr := adapter.MergeConversationByThreadRef(ctx, store, a.opaqueParams(), ref, events, adapter.EncodeCanonicalConversationPayload)
			if !handled {
				continue
			}
			if merr != nil {
				return ids, merr
			}
			ids = append(ids, mergedIDs...)
			continue
		}
		id, err := a.importOneKiloSession(store, abs, b)
		if err != nil {
			return ids, err
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// kiloSyncedThreadMap maps each conversation artifact's deterministic kilo synced
// session id to its artifact id, so a re-imported synced session is reconciled to
// its canonical thread. Built once per import call (only when a synced session is
// present). nil on a store-read error (the caller then skips the synced session).
func kiloSyncedThreadMap(store *acf.Store) map[string]string {
	refMap := kiloSyncedThreadRefMap(store)
	if refMap == nil {
		return nil
	}
	out := make(map[string]string, len(refMap))
	for sessionID, ref := range refMap {
		if ref.BranchID == acf.MainBranch {
			out[sessionID] = ref.ArtifactID
		}
	}
	return out
}

func kiloSyncedThreadRefMap(store *acf.Store) map[string]adapter.ThreadRef {
	if store == nil {
		return nil
	}
	arts, err := store.ListArtifacts(acf.KindConversation)
	if err != nil {
		return nil
	}
	out := make(map[string]adapter.ThreadRef, len(arts))
	for _, art := range arts {
		for _, branchID := range kiloKnownBranches(store, art) {
			out[kiloSyncedSessionIDForBranch(art.ArtifactID, branchID)] = adapter.ThreadRef{
				ArtifactID: art.ArtifactID,
				BranchID:   branchID,
			}
		}
	}
	return out
}

func kiloKnownBranches(store *acf.Store, art acf.Artifact) []string {
	seen := map[string]struct{}{acf.MainBranch: {}}
	branches, err := store.ListBranches(acf.KindConversation, art.ArtifactID, true)
	if err == nil {
		for _, branch := range branches {
			if norm, nerr := acf.NormalizeBranchName(branch.Name); nerr == nil {
				seen[norm] = struct{}{}
			}
		}
	}
	for _, branch := range art.MaterializedBranchByAgent {
		if norm, nerr := acf.NormalizeBranchName(branch); nerr == nil {
			seen[norm] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for branch := range seen {
		out = append(out, branch)
	}
	sort.Strings(out)
	return out
}

func (a *Adapter) importOneKiloSession(store *acf.Store, dbAbsPath string, b kiloSessionBundle) (string, error) {
	sourcePath := dbAbsPath + "#session=" + b.Session.ID
	name := b.Session.ID
	if b.Session.Title != "" {
		name = b.Session.Title
	}
	scope, projectInfo := kiloConversationScope(b.Session.Directory)

	payloadJSON, err := encodeKiloConversationPayload(b)
	if err != nil {
		return "", err
	}

	existing, found, err := store.FindBySourcePath(acf.KindConversation, sourcePath)
	if err != nil {
		return "", fmt.Errorf("kilo: find by source path: %w", err)
	}
	if found {
		same, err := kiloPayloadMatchesLatest(store, existing.ArtifactID, payloadJSON)
		if err != nil {
			return "", fmt.Errorf("kilo: compare payload: %w", err)
		}
		if same {
			return existing.ArtifactID, nil
		}
		// Anti-revert: when a conversation was continued in ANOTHER agent (e.g.
		// Hermes), kilo's own session for that thread is a STALE shorter copy. Its
		// turns are a strict PREFIX of the canonical thread, so appending it would
		// REVERT the conversation and lose the newer turns — skip the write. A real
		// local edit in kilo is longer or divergent and still appends.
		if events, eerr := encodeKiloBundleAsCanonical(b); eerr == nil &&
			adapter.WouldRevertThread(store, existing.ArtifactID, acf.ExtractTextTurns(events)) {
			return existing.ArtifactID, nil
		}
	}

	now := time.Now().UTC()
	var id, parentHash string
	eventType := acf.EventTypeCreate
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
		existing.Name = name
		existing.Scope = scope
		existing.Project = projectInfo
		if err := store.WriteArtifact(existing); err != nil {
			return "", err
		}
	} else {
		id = acf.NewID()
		artifact := acf.Artifact{
			AcfSchemaVersion: acf.SchemaVersion,
			ArtifactID:       id,
			Kind:             acf.KindConversation,
			Scope:            scope,
			Name:             name,
			SourcePath:       sourcePath,
			CreatedAt:        now,
			UpdatedAt:        now,
			Project:          projectInfo,
		}
		if err := store.WriteArtifact(artifact); err != nil {
			return "", err
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
	if err := store.AppendEvent(acf.KindConversation, event); err != nil {
		if createdNew {
			_ = store.DeleteArtifact(acf.KindConversation, id)
		}
		return "", err
	}
	return id, nil
}

func encodeKiloConversationPayload(b kiloSessionBundle) (json.RawMessage, error) {
	events, err := encodeKiloBundleAsCanonical(b)
	if err != nil {
		return nil, err
	}
	return acf.EncodePayload(acf.ConversationPayload{
		Format: acf.ConversationFormatV1,
		Events: events,
	})
}

func kiloConversationScope(directory string) (acf.Scope, *project.ProjectInfo) {
	if directory == "" {
		return acf.ScopeGlobal, nil
	}
	info, err := project.Detect(directory)
	if err != nil || info.VCS == "none" {
		return acf.ScopeGlobal, nil
	}
	return acf.ScopeProject, &info
}

func kiloPayloadMatchesLatest(store *acf.Store, artifactID string, candidate json.RawMessage) (bool, error) {
	events, err := store.ReadEvents(acf.KindConversation, artifactID)
	if err != nil {
		return false, err
	}
	for i := len(events) - 1; i >= 0; i-- {
		e := events[i]
		if e.Type != acf.EventTypeCreate && e.Type != acf.EventTypeUpdate && e.Type != acf.EventTypeResolution {
			continue
		}
		return bytes.Equal(e.Payload, candidate), nil
	}
	return false, nil
}
