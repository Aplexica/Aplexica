package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/atomicfile"
	"github.com/aplexica/aplexica/internal/project"
)

// OpaqueParams bundles the per-adapter values that Import/Export need.
type OpaqueParams struct {
	DeviceID       string
	SourceAgent    string // e.g. "claude-code", "codex"
	AdapterVersion string // semver of the adapter
	InferScope     func(absPath string) acf.Scope
	// InferProject (v0.56.0; BRD-02 §4.13) returns the rich project
	// identity for an absolute import path, or nil when the path is
	// global-scope (under the adapter's user-level state directory)
	// or when project detection fails. Only called when the resolved
	// Scope == ScopeProject.
	//
	// nil callback is allowed: the artifact's Project field stays
	// nil even when scope is project. This is the v0.55.0-era behavior
	// (project detection not yet wired) and is the default for adapters
	// that haven't been updated.
	InferProject func(absPath string) *project.ProjectInfo
	// Registry (BRD-02 §4.13.5; collision fix) is the user's project
	// registry. When an import path lives under a folder the user has
	// explicitly REGISTERED as a "local" project, the ad-hoc->global
	// downgrade is SKIPPED — the registration is an explicit "this is a
	// project" declaration, so its files stay project-scoped keyed by
	// the registered path even without a git/hg marker.
	//
	// nil is allowed and is the default for callers that haven't wired a
	// registry: behavior is then identical to pre-collision-fix (the
	// registered branch is never taken; DowngradeAdHocToGlobal runs as
	// before).
	Registry *project.Registry
}

// DefaultInferProject is the shared per-path project-identity
// resolver every adapter should plug into OpaqueParams.InferProject
// when the import path is project-scoped. v0.56.0.
//
// Implementation: call project.Detect on the file's parent directory.
// Detect's walk-up handles repo-root resolution. nil return when
// detection fails (Detect's error path; very rare).
func DefaultInferProject(absPath string) *project.ProjectInfo {
	info, err := project.Detect(filepath.Dir(absPath))
	if err != nil {
		return nil
	}
	return &info
}

// ProjectFromScope is the convenience wrapper for tool.go and other
// non-ImportOpaque code paths: returns DefaultInferProject(absPath)
// when scope == ScopeProject, nil otherwise. v0.56.0; mirrors the
// "only resolve project info for project-scoped artifacts" rule
// baked into ImportOpaque.
func ProjectFromScope(scope acf.Scope, absPath string) *project.ProjectInfo {
	if scope != acf.ScopeProject {
		return nil
	}
	return DefaultInferProject(absPath)
}

// DowngradeAdHocToGlobal applies BRD-02 §4.13.5's "ad-hoc directories
// default to global scope" rule (v0.61.0). Given a (scope, projInfo)
// pair just resolved by an adapter's Import path, returns the
// effective (scope, projInfo) after the rule fires.
//
// Rule: when scope is ScopeProject but the resolved project info
// reports VCS == "none" (no .git/.hg walk-up found), the path is
// considered ad-hoc — `~/scratch/`, `/tmp/play/`, Downloads, etc.
// Downgrade to ScopeGlobal and clear projInfo. The user can promote
// back to project-scope explicitly via `aplexica project init`
// (today's registry-aware override is a future enhancement; see the
// limitation note in ImportOpaque).
//
// Idempotent: calling twice has the same effect as calling once.
// No-op for ScopeGlobal / ScopeNamespace.
func DowngradeAdHocToGlobal(scope acf.Scope, projInfo *project.ProjectInfo) (acf.Scope, *project.ProjectInfo) {
	if scope == acf.ScopeProject && projInfo != nil && projInfo.VCS == "none" {
		return acf.ScopeGlobal, nil
	}
	return scope, projInfo
}

// ResolveRegisteredScope reports whether absPath lives under a registered
// project folder. When it does, it returns that project's scope-as-ACF
// (ScopeProject for "local") and the project ID. A registered local
// project means the ad-hoc->global downgrade is skipped: the user has
// explicitly declared this directory a project. ScopeGlobal is returned
// for a registered "global" project (handled by a later plan); callers
// in this plan only act on the local case.
//
// Projects can nest (e.g. both ~/ and ~/Projects/Foo registered). When
// multiple registered folders are prefixes of absPath, the LONGEST (most-
// specific) match wins, so a file under ~/Projects/Foo keys to Foo, not
// to the broader home registration.
func ResolveRegisteredScope(reg *project.Registry, absPath string) (acf.Scope, string, bool) {
	if reg == nil {
		return acf.ScopeProject, "", false
	}
	clean := filepath.Clean(absPath)
	var best project.Entry
	bestLen := -1
	for _, e := range reg.List() {
		root := filepath.Clean(e.Path)
		if clean == root || strings.HasPrefix(clean, root+string(filepath.Separator)) {
			if len(root) > bestLen {
				best = e
				bestLen = len(root)
			}
		}
	}
	if bestLen < 0 {
		return acf.ScopeProject, "", false
	}
	if best.EffectiveScope() == "local" {
		return acf.ScopeProject, best.ID, true
	}
	return acf.ScopeGlobal, best.ID, true
}

// OpaqueEncoder converts raw file bytes into a typed payload's JSON form.
// Each adapter+kind provides its own encoder (e.g. one that wraps the bytes
// in MemoryPayload{Format: "markdown", Content: <bytes>}).
type OpaqueEncoder func(content []byte) (json.RawMessage, error)

// OpaqueDecoder extracts the string content from an event payload. Caller is
// responsible for choosing the right decoder for the kind (MemoryPayload's
// Content for memory, SkillPayload's Content for skill, etc.).
type OpaqueDecoder func(e acf.Event) (string, error)

// ImportOpaque is the shared "read file → ACF artifact + event" pipeline used
// by every opaque-bytes artifact kind across every adapter. It is NOT used by
// tool import (which has secrets extraction).
//
// Identity reconciliation: looks up an existing artifact by absolute SourcePath
// before minting a new UUIDv7. If found, appends an "update" event instead of
// creating. Per ADR-0027 (stable UUIDv7 IDs).
//
// Transactional rollback: if an error occurs after a fresh WriteArtifact
// (the "create" path), the orphan artifact is removed before returning.
// The update path leaves the pre-existing artifact intact.
//
// Errors are wrapped with "adapter: " prefix.
func ImportOpaque(
	ctx context.Context,
	store *acf.Store,
	kind acf.Kind,
	params OpaqueParams,
	nativePath string,
	encoder OpaqueEncoder,
) ([]string, error) {
	ids, _, err := ImportOpaqueWithHeadEvent(ctx, store, kind, params, nativePath, encoder)
	return ids, err
}

// ImportOpaqueWithHeadEvent is ImportOpaque plus the exact committed/equal
// head event identity. See ImportOpaqueContentWithHeadEvent.
func ImportOpaqueWithHeadEvent(
	ctx context.Context,
	store *acf.Store,
	kind acf.Kind,
	params OpaqueParams,
	nativePath string,
	encoder OpaqueEncoder,
) ([]string, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", fmt.Errorf("adapter: import cancelled: %w", err)
	}
	abs, err := filepath.Abs(nativePath)
	if err != nil {
		return nil, "", fmt.Errorf("adapter: resolve path: %w", err)
	}
	content, err := os.ReadFile(abs)
	if err != nil {
		return nil, "", fmt.Errorf("adapter: read %s: %w", abs, err)
	}
	return ImportOpaqueContentWithHeadEvent(ctx, store, kind, params, abs, content, encoder)
}

// ImportOpaqueContent is ImportOpaque with the file bytes supplied by the
// caller instead of read from disk. It lets an adapter store a SYNTHESIZED
// artifact whose on-disk identity (SourcePath) is a real native file but whose
// content is composed from multiple inputs.
//
// The codex adapter uses this to fold ~/.codex/memories/*.md into the single
// AGENTS.md-keyed global-memory artifact: the trigger may be any memories file,
// but the artifact's SourcePath is always AGENTS.md, so memories edits update
// the one canonical memory rather than minting colliding peer artifacts that
// would clobber each other on a single-file NativePath fan-out.
//
// sourcePath MUST be an absolute path (it becomes the artifact's SourcePath and
// the FindBySourcePath reconciliation key). Identity reconciliation, scope
// inference, transactional rollback, and provenance are identical to
// ImportOpaque.
func ImportOpaqueContent(
	ctx context.Context,
	store *acf.Store,
	kind acf.Kind,
	params OpaqueParams,
	sourcePath string,
	content []byte,
	encoder OpaqueEncoder,
) (ids []string, err error) {
	return importOpaqueContent(ctx, store, kind, params, sourcePath, content, encoder, nil)
}

// ImportOpaqueContentWithHeadEvent is ImportOpaqueContent plus the event ID of
// the exact head it committed or proved byte-identical. Canonical conversation
// importers use that identity to bind their parsed full-state cache seed; a
// concurrent append before priming then causes a safe cache miss instead of
// associating stale turns with the newer head.
func ImportOpaqueContentWithHeadEvent(
	ctx context.Context,
	store *acf.Store,
	kind acf.Kind,
	params OpaqueParams,
	sourcePath string,
	content []byte,
	encoder OpaqueEncoder,
) (ids []string, headEventID string, err error) {
	ids, err = importOpaqueContent(ctx, store, kind, params, sourcePath, content, encoder, &headEventID)
	return ids, headEventID, err
}

func importOpaqueContent(
	ctx context.Context,
	store *acf.Store,
	kind acf.Kind,
	params OpaqueParams,
	sourcePath string,
	content []byte,
	encoder OpaqueEncoder,
	headEventIDOut *string,
) (ids []string, err error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("adapter: import cancelled: %w", err)
	}
	abs, err := filepath.Abs(sourcePath)
	if err != nil {
		return nil, fmt.Errorf("adapter: resolve path: %w", err)
	}
	scope := acf.ScopeProject
	if params.InferScope != nil {
		scope = params.InferScope(abs)
	}
	// Resolve rich project info only when scope is project AND a
	// resolver is configured. nil result is OK — the artifact's
	// Project field stays nil and the orchestrator will treat the
	// artifact as "project-scoped but identity-unknown."
	var projInfo *project.ProjectInfo
	if scope == acf.ScopeProject && params.InferProject != nil {
		projInfo = params.InferProject(abs)
	}
	// A path the adapter's InferScope already classified as ScopeGlobal is the
	// agent's OWN global-state root (~/.claude/, ~/.codex/, ...). It stays
	// global regardless of any registered project that happens to be an
	// ancestor on disk: the daemon registers its --dir as an implicit local
	// project, and when --dir is $HOME that registration's path is an
	// ancestor of ~/.claude/. Without this guard, ResolveRegisteredScope would
	// recapture every ~/.claude/projects/<cwd>/<session>.jsonl conversation
	// (and ~/.claude skills / user config) into the home project's local
	// scope — stranding them in `pending` on any device that hasn't linked the
	// home project, so they never materialize there. The registry override and
	// the ad-hoc downgrade therefore only apply to non-global paths.
	if scope != acf.ScopeGlobal {
		// BRD-02 §4.13.5 collision fix: if the path lives under a folder the
		// user has explicitly REGISTERED as a "local" project, that
		// registration is an explicit "this is a project" declaration —
		// keep it project-scoped (keyed by the registered path) and SKIP the
		// ad-hoc->global downgrade. Otherwise apply the "ad-hoc directories
		// default to global scope" rule (see DowngradeAdHocToGlobal's godoc).
		//
		// When params.Registry is nil (every pre-collision-fix caller), the
		// registered branch is never taken and behavior is unchanged.
		if regScope, projID, registered := ResolveRegisteredScope(params.Registry, abs); registered && regScope == acf.ScopeProject {
			scope = acf.ScopeProject
			if entry, ok := params.Registry.Get(projID); ok {
				projInfo = &project.ProjectInfo{
					ID:        entry.ID,
					Path:      entry.Path,
					VCS:       entry.VCS,
					Ephemeral: entry.Ephemeral,
				}
			}
		} else {
			scope, projInfo = DowngradeAdHocToGlobal(scope, projInfo)
		}
	}

	// Identity reconciliation: if an artifact with this SourcePath already
	// exists in this kind, append an "update" event to it instead of minting
	// a new ID. ADR-0027 (stable UUIDv7 IDs).
	existing, found, err := store.FindBySourcePath(kind, abs)
	if err != nil {
		return nil, err
	}

	// Encode the payload up front so the unchanged-content loop break (and the
	// create path's encode-failure handling) run before any store write.
	payload, encErr := encoder(content)
	if encErr != nil {
		return nil, fmt.Errorf("adapter: encode payload: %w", encErr)
	}

	now := time.Now().UTC()
	var id string
	var eventType acf.EventType
	var parentHash string
	createdNew := false // true only if this call freshly wrote a new artifact
	if found {
		// Skip-if-equal guard: when the new payload is byte-identical to the
		// artifact's current head, this is a redundant re-import — e.g. the
		// daemon's startup InitialScan re-reading a native file whose content
		// hasn't changed. Append no event (and skip the UpdatedAt bump) but
		// still reuse the artifact id, so identity reconciliation holds and the
		// fan-out contract is unchanged (the recursion guard suppresses the
		// resulting echo, exactly as in steady state). Without this, every
		// restart wrote a redundant "update" event per artifact, bloating the
		// event log and flooding the events feed with a same-second burst of
		// "synced" rows. Mirrors the hermes ImportConversation guard.
		head, hasHead, uerr := store.LastEvent(kind, existing.ArtifactID)
		if uerr != nil {
			return nil, uerr
		}
		if hasHead && bytes.Equal(head.Payload, payload) {
			if headEventIDOut != nil {
				*headEventIDOut = head.EventID
			}
			return []string{existing.ArtifactID}, nil
		}
		id = existing.ArtifactID
		eventType = acf.EventTypeUpdate
		var herr error
		parentHash, herr = RefreshMainBranchHead(store, kind, &existing)
		if herr != nil {
			return nil, herr
		}
		// AppendEvent updates UpdatedAt from the committed event while holding the
		// per-artifact append lock. Do not write this earlier artifact snapshot:
		// a concurrent append after RefreshMainBranchHead could otherwise have its
		// new head bookkeeping overwritten before the ParentHash CAS runs.
	} else {
		id = acf.NewID()
		eventType = acf.EventTypeCreate
		parentHash = ""
		artifact := acf.Artifact{
			AcfSchemaVersion: acf.SchemaVersion,
			ArtifactID:       id,
			Kind:             kind,
			Scope:            scope,
			Project:          projInfo,
			Name:             filepath.Base(abs),
			SourcePath:       abs,
			CreatedAt:        now,
			UpdatedAt:        now,
		}
		if werr := store.WriteArtifact(artifact); werr != nil {
			return nil, werr
		}
		createdNew = true
	}

	// Transactional rollback: if anything below returns an error AND we just
	// created a brand-new artifact in this call (not an update path), undo
	// the WriteArtifact so we don't leave an orphan in the store.
	defer func() {
		if err != nil && createdNew {
			_ = store.DeleteArtifact(kind, id)
		}
	}()

	event := acf.Event{
		EventID:    acf.NewID(),
		ArtifactID: id,
		Type:       eventType,
		Timestamp:  now,
		Provenance: acf.Provenance{
			DeviceID:       params.DeviceID,
			SourceAgent:    params.SourceAgent,
			AgentVersion:   acf.UnknownAgentVersion,
			AdapterVersion: params.AdapterVersion,
			CausedBy:       causedByFromContext(ctx),
		},
		Payload:    payload,
		ParentHash: parentHash,
	}
	if aerr := store.AppendEventWithRefreshedParent(kind, event); aerr != nil {
		err = aerr
		return nil, err
	}
	if headEventIDOut != nil {
		*headEventIDOut = event.EventID
	}
	return []string{id}, nil
}

// EventPayloadUnchanged reports whether newPayload is byte-identical to the
// payload of the artifact's current head event. Import paths use it as a loop
// break: skip appending a redundant "update" event when a re-import produces
// the same payload the artifact already holds — most commonly the daemon's
// startup InitialScan re-reading an unchanged native file, or a fan-out write
// echoing back through the watcher.
//
// Returns false (caller should append) when the artifact has no events yet. A
// read error is returned so the caller surfaces it rather than silently
// skipping the dedup. Comparison is on the raw payload bytes: the opaque
// payloads this guards (memory/skill/conversation) are produced by
// acf.EncodePayload (json.Marshal of a content-only struct), so identical
// content yields byte-identical payloads. A redaction/tombstone head — whose
// payload shape differs from a create/update payload — correctly registers as
// changed, so re-adding content after a redaction still records an event.
//
// NOT safe for tool artifacts: their payload is the REDACTED config, so a
// secret rotation (which must still fan out) leaves the payload byte-identical.
// Tool imports therefore intentionally do not use this guard.
func EventPayloadUnchanged(store *acf.Store, kind acf.Kind, artifactID string, newPayload json.RawMessage) (bool, error) {
	head, ok, err := store.LastEvent(kind, artifactID)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, nil
	}
	return bytes.Equal(head.Payload, newPayload), nil
}

// ExportOpaque is the shared "replay events → write native file" pipeline.
// Reads all events for the artifact, verifies the hash chain, replays them
// to compute the current content, then writes to destPath.
//
// The replay rules match what every opaque exporter currently does:
//   - "create"/"update" → decode payload, replace current content
//   - "redaction" → blank the content (preserves format string in caller's
//     decoder logic; here we just write empty bytes)
//   - other types → ignored
//
// Tombstone semantics (v0.18.0): if the artifact's most recent event is a
// redaction, the export does NOT write a (zero-byte) file. It returns the
// sentinel ErrArtifactTombstoned so callers can choose to skip silently,
// write to a .redacted sidecar, or surface a clear error. Check with
// errors.Is(err, adapter.ErrArtifactTombstoned).
func ExportOpaque(
	ctx context.Context,
	store *acf.Store,
	kind acf.Kind,
	artifactID, destPath string,
	decoder OpaqueDecoder,
) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("adapter: export cancelled: %w", err)
	}
	current, tombstoned, err := ReplayOpaqueContent(store, kind, artifactID, decoder)
	if err != nil {
		return err
	}
	if tombstoned {
		return ErrArtifactTombstoned
	}
	if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
		return fmt.Errorf("adapter: mkdir dest: %w", err)
	}
	return atomicfile.WriteFile(destPath, []byte(current), 0o644)
}

// ReplayOpaqueContent replays an opaque artifact's event log to its current
// materialized content WITHOUT writing anything. Returns tombstoned=true (and
// empty content) when the head event is a redaction, mirroring ExportOpaque's
// tombstone semantics so callers can skip the write.
//
// Adapters that need to post-process content before materializing it — e.g.
// codex stripping memories-folder entries out of AGENTS.md to avoid
// duplication — use this to get the bytes, transform them, and write
// themselves.
//
// Retention/prune resilience (FR-02.32): once retention.CreateSnapshot +
// PruneArtifact run on a main-only artifact, every pre-snapshot create/update
// event moves into .compacted and the ACTIVE log holds only the snapshot
// event. We first replay the active log alone (the common, cheap path). A
// payload-bearing snapshot (the root fix) is a self-contained checkpoint the
// active-log walk decodes directly. If the active log yields no exportable
// content — because the snapshot is payload-less (legacy), or because the
// active-only log can't even VerifyChain across the prune boundary (the
// snapshot's ParentHash references the now-compacted pre-snapshot head) — and
// the artifact is NOT redaction-tombstoned, we fall back to
// Store.ReadEventsIncludingCompacted, which re-merges the active + compacted
// layers into a log that both VerifyChains and contains the create/update
// payload. A redaction is authoritative: a tombstoned artifact returns empty
// and never resurrects pre-redaction content from the compacted layer.
func ReplayOpaqueContent(
	store *acf.Store,
	kind acf.Kind,
	artifactID string,
	decoder OpaqueDecoder,
) (content string, tombstoned bool, err error) {
	// Look up the artifact to check the tombstone flag BEFORE composing the
	// content. We still ReadArtifact even when the chain happens to be empty so
	// a missing artifact surfaces a clear "read artifact" error rather than the
	// looser "no events" one below. A redaction head is authoritative: return
	// the tombstone WITHOUT touching the compacted layer, so a redacted
	// artifact never resurrects its pre-redaction payload from compacted
	// history.
	art, aerr := store.ReadArtifact(kind, artifactID)
	if aerr != nil {
		return "", false, fmt.Errorf("adapter: read artifact: %w", aerr)
	}
	if art.Tombstoned {
		return "", true, nil
	}
	events, err := store.ReadEvents(kind, artifactID)
	if err != nil {
		return "", false, fmt.Errorf("adapter: read events: %w", err)
	}
	if len(events) == 0 {
		return "", false, fmt.Errorf("adapter: no events for artifact %s", artifactID)
	}

	// Try the active log first. VerifyChain failure here is NOT fatal: an
	// active-only slice can fail verification at the prune boundary (the
	// snapshot's ParentHash points at the now-compacted head). That is reported
	// as found=false so we retry against the merged log below.
	current, found := replayOpaqueFromLog(events, decoder)
	if derr := current.err; derr != nil {
		return "", false, derr
	}
	if found {
		return current.content, false, nil
	}

	// The active log has no exportable payload and is not an explicit redaction
	// (a redaction head was already handled by art.Tombstoned above) — most
	// commonly a snapshot-only log after an on-snapshot prune. Re-merge the
	// .compacted layer so the create/update payload (and a VerifyChain-able
	// chain) come back into view, then replay that.
	merged, merr := store.ReadEventsIncludingCompacted(kind, artifactID)
	if merr != nil {
		return "", false, fmt.Errorf("adapter: read events including compacted: %w", merr)
	}
	// Surface genuine corruption clearly: if even the merged log fails
	// VerifyChain, the active-only failure was NOT a benign prune-boundary
	// artifact but real chain damage. Report it as the hard "event log is
	// invalid" error the pre-fallback code did.
	if verr := acf.VerifyChain(merged); verr != nil {
		return "", false, fmt.Errorf("adapter: event log is invalid: %w", verr)
	}
	current, _ = replayOpaqueFromLog(merged, decoder)
	if current.err != nil {
		return "", false, current.err
	}
	return current.content, false, nil
}

// replayOpaqueResult bundles the forward-replay output so replayOpaqueFromLog
// can distinguish a decode error from a clean "no exportable content".
type replayOpaqueResult struct {
	content string
	err     error
}

// replayOpaqueFromLog forward-replays a single event slice to its materialized
// content. It returns found=false (and leaves the caller to fall back to the
// compacted layer) when the slice cannot be trusted or carries no exportable
// payload:
//   - the slice fails acf.VerifyChain (e.g. an active-only log across a prune
//     boundary whose snapshot ParentHash points at the now-compacted head); or
//   - it contains no content-bearing event (create/update/resolution, or an
//     FR-02.32 payload-bearing snapshot).
//
// A payload-bearing EventTypeSnapshot (FR-02.32) is a self-contained checkpoint
// carrying the materialized content, so the forward walk decodes it like a
// create/update and marks the content found. A payload-LESS snapshot (legacy)
// carries no body and is skipped, preserving the pre-FR-02.32 behavior.
// EventTypeRedaction blanks the content (last-write-wins) and is itself a
// materializing event — a redaction head therefore yields ("", found=true), so
// the caller does NOT fall back to compacted history and resurrect a
// pre-redaction payload.
//
// A decode error is returned in replayOpaqueResult.err with found=false; the
// caller surfaces it rather than masking it via the fallback.
func replayOpaqueFromLog(events []acf.Event, decoder OpaqueDecoder) (replayOpaqueResult, bool) {
	if err := acf.VerifyChain(events); err != nil {
		// Can't trust this slice — treat as "no exportable content here" so the
		// caller falls back to the merged (active + compacted) log.
		return replayOpaqueResult{}, false
	}
	var current string
	found := false
	for _, e := range events {
		switch e.Type {
		case acf.EventTypeCreate, acf.EventTypeUpdate, acf.EventTypeResolution:
			// EventTypeResolution (v0.34.0) re-asserts a winning payload
			// after conflict resolution — replay behavior is identical to
			// create/update.
			decoded, derr := decoder(e)
			if derr != nil {
				return replayOpaqueResult{err: fmt.Errorf("adapter: %w", derr)}, false
			}
			current = decoded
			found = true
		case acf.EventTypeSnapshot:
			// FR-02.32: a payload-bearing snapshot is a self-contained
			// checkpoint carrying the materialized content. After an
			// on-snapshot prune the active log can be snapshot-only, so the
			// replay must decode it. A payload-LESS snapshot (legacy) carries
			// no body — skip it (the compacted fallback re-materializes it).
			if !acf.HasPayload(e.Payload) {
				continue
			}
			decoded, derr := decoder(e)
			if derr != nil {
				return replayOpaqueResult{err: fmt.Errorf("adapter: %w", derr)}, false
			}
			current = decoded
			found = true
		case acf.EventTypeRedaction:
			current = ""
			found = true
		}
	}
	return replayOpaqueResult{content: current}, found
}

// ReplayToolPayload replays a tool artifact's event log to its current
// materialized acf.ToolPayload WITHOUT writing anything or expanding secrets.
// Every adapter's ExportTool routes through this so tool export shares the SAME
// retention/prune resilience as the opaque (memory/skill/conversation) path —
// tool replay is otherwise a typed parallel of ReplayOpaqueContent (it decodes
// the canonical ToolPayload and applies the tool-specific redaction blanking)
// and would otherwise carry the identical export-after-prune defect.
//
// Replay rules (unchanged from the per-adapter inline loops they replace):
//   - create/update/resolution → decode and replace the current payload.
//   - redaction → blank Content but KEEP the Format string (a redacted tool
//     still exports as a valid, empty-server .mcp.json; it is NOT tombstone-
//     skipped the way an opaque artifact is). A redaction is a materializing
//     event, so a redaction head suppresses the compacted fallback — a redacted
//     tool never resurrects its pre-redaction servers from compacted history.
//
// Retention/prune resilience (FR-02.32): after retention.CreateSnapshot +
// PruneArtifact on a main-only tool artifact the active log holds only the
// snapshot. A payload-bearing snapshot (FR-02.32) is decoded directly; an
// active log that yields no payload — payload-less legacy snapshot, or an
// active-only log that can't VerifyChain across the prune boundary — falls back
// to Store.ReadEventsIncludingCompacted. Genuine corruption (the merged log
// also fails VerifyChain) still surfaces as the hard "event log is invalid".
func ReplayToolPayload(store *acf.Store, artifactID string) (acf.ToolPayload, error) {
	events, err := store.ReadEvents(acf.KindTool, artifactID)
	if err != nil {
		return acf.ToolPayload{}, fmt.Errorf("adapter: read events: %w", err)
	}
	if len(events) == 0 {
		return acf.ToolPayload{}, fmt.Errorf("adapter: no events for artifact %s", artifactID)
	}

	current, found := replayToolFromLog(events)
	if current.err != nil {
		return acf.ToolPayload{}, current.err
	}
	if found {
		return current.payload, nil
	}

	merged, merr := store.ReadEventsIncludingCompacted(acf.KindTool, artifactID)
	if merr != nil {
		return acf.ToolPayload{}, fmt.Errorf("adapter: read events including compacted: %w", merr)
	}
	if verr := acf.VerifyChain(merged); verr != nil {
		return acf.ToolPayload{}, fmt.Errorf("adapter: event log is invalid: %w", verr)
	}
	current, _ = replayToolFromLog(merged)
	if current.err != nil {
		return acf.ToolPayload{}, current.err
	}
	return current.payload, nil
}

// replayToolResult bundles the typed forward-replay output so replayToolFromLog
// can distinguish a decode error from a clean "no exportable payload".
type replayToolResult struct {
	payload acf.ToolPayload
	err     error
}

// replayToolFromLog is the typed (acf.ToolPayload) analogue of
// replayOpaqueFromLog: it forward-replays a single event slice and returns
// found=false when the slice fails acf.VerifyChain or carries no materializing
// event, so the caller falls back to the merged (active + compacted) log. The
// snapshot and redaction handling mirror replayOpaqueFromLog, with the
// tool-specific redaction blanking (keep Format, clear Content).
func replayToolFromLog(events []acf.Event) (replayToolResult, bool) {
	if err := acf.VerifyChain(events); err != nil {
		return replayToolResult{}, false
	}
	var current acf.ToolPayload
	found := false
	for _, e := range events {
		switch e.Type {
		case acf.EventTypeCreate, acf.EventTypeUpdate, acf.EventTypeResolution:
			// EventTypeResolution (v0.34.0) re-asserts a winning payload after
			// conflict resolution — replay identical to create/update.
			decoded, derr := acf.DecodeToolPayload(e)
			if derr != nil {
				return replayToolResult{err: fmt.Errorf("adapter: %w", derr)}, false
			}
			current = decoded
			found = true
		case acf.EventTypeSnapshot:
			// FR-02.32: a payload-bearing snapshot is a self-contained
			// checkpoint. A payload-LESS snapshot (legacy) is skipped — the
			// compacted fallback re-materializes it.
			if !acf.HasPayload(e.Payload) {
				continue
			}
			decoded, derr := acf.DecodeToolPayload(e)
			if derr != nil {
				return replayToolResult{err: fmt.Errorf("adapter: %w", derr)}, false
			}
			current = decoded
			found = true
		case acf.EventTypeRedaction:
			// Blank the secrets-bearing Content but keep the Format so the
			// artifact still exports as a valid (empty) tool config.
			current = acf.ToolPayload{Format: current.Format}
			found = true
		}
	}
	return replayToolResult{payload: current}, found
}

// causedByCtxKey is the unexported context key for the source event hash.
// Lives in this package (not internal/sync) to avoid a cyclic import:
// internal/adapter cannot depend on internal/sync because the orchestrator
// already imports internal/adapter. The orchestrator calls
// adapter.WithCausedBy(ctx, hash) before each fan-out Export; ImportOpaque
// reads the value back via causedByFromContext and stamps it on the new
// event's Provenance.CausedBy.
type causedByCtxKey struct{}

// WithCausedBy returns a derived context that carries the source event
// hash. Used by the sync orchestrator to propagate the originating event's
// hash to whatever ImportOpaque call ultimately writes the fan-out event.
// Empty hash is a no-op (returns ctx unchanged) so callers can pass through
// the result of an upstream lookup without a nil check.
func WithCausedBy(ctx context.Context, hash string) context.Context {
	if hash == "" {
		return ctx
	}
	return context.WithValue(ctx, causedByCtxKey{}, hash)
}

// CausedByFromContext returns the source event hash previously set by
// WithCausedBy, or "" if none. Used by ImportOpaque to stamp
// Provenance.CausedBy and exported so future receive-side guard
// implementations (and tests) can inspect what the orchestrator
// propagated.
func CausedByFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(causedByCtxKey{}).(string); ok {
		return v
	}
	return ""
}

// causedByFromContext is the internal shorthand used inside this package.
// Kept as a function alias so ImportOpaque reads naturally without an
// awkward capital letter mid-struct-literal.
func causedByFromContext(ctx context.Context) string { return CausedByFromContext(ctx) }
