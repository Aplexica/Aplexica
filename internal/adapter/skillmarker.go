package adapter

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/atomicfile"
)

// Skill fan-out copies live INSIDE other agents' watched skills trees
// (~/.codex/skills/<name>/, ~/.kilo/skills/<name>/, …), so the watcher
// re-imports them — and path-keyed identity reconciliation can mint a
// duplicate artifact per copy. The fix mirrors how conversations solved the same
// echo class with thread-keys: cross-agent skill exports carry a provenance
// marker naming the canonical artifact, and skill imports route marked files
// back to that artifact — a no-op when the content is unchanged, an update
// event when the user edited the copy (which is what makes skill sync
// bidirectional from ANY agent's copy).
//
// The marker is a trailing HTML comment: inert in markdown, invisible to
// every agent's skill loader, and stripped before content is stored so the
// canonical payload stays marker-free.
const (
	skillMarkerPrefix = "<!-- aplexica:skill:"
	skillMarkerSuffix = " -->"
)

// AppendSkillMarker returns content with a trailing provenance-marker line.
func AppendSkillMarker(content []byte, artifactID string) []byte {
	out := bytes.TrimRight(content, "\n")
	return append(out, []byte("\n\n"+skillMarkerPrefix+artifactID+skillMarkerSuffix+"\n")...)
}

// ParseSkillMarker extracts the canonical artifact id from a marked skill
// file and returns the content with the marker stripped. found=false for
// unmarked content (stripped is then the input unchanged).
func ParseSkillMarker(content []byte) (artifactID string, stripped []byte, found bool) {
	trimmed := bytes.TrimRight(content, "\n")
	idx := bytes.LastIndex(trimmed, []byte(skillMarkerPrefix))
	if idx < 0 {
		return "", content, false
	}
	rest := trimmed[idx+len(skillMarkerPrefix):]
	end := bytes.Index(rest, []byte(skillMarkerSuffix))
	// The marker must be the file's last line — a prefix match mid-document
	// (e.g. the skill DOCUMENTS the marker format) is not a marker.
	if end < 0 || len(bytes.TrimSpace(rest[end+len(skillMarkerSuffix):])) != 0 {
		return "", content, false
	}
	id := strings.TrimSpace(string(rest[:end]))
	if id == "" {
		return "", content, false
	}
	return id, append(bytes.TrimRight(trimmed[:idx], "\n"), '\n'), true
}

// SkillPathVendorHidden reports whether a SKILL.md path sits under a
// dot-prefixed directory INSIDE a skills tree — vendor-internal skill
// libraries an agent ships for itself, not user content. Such directories
// must not be imported and fanned out when a skills tree is watched. Segments
// BEFORE the last "skills" segment are exempt (the tree root itself lives
// under dot-dirs like ~/.codex).
func SkillPathVendorHidden(path string) bool {
	segs := strings.Split(filepath.ToSlash(filepath.Clean(path)), "/")
	last := -1
	for i, s := range segs {
		if s == "skills" {
			last = i
		}
	}
	if last < 0 {
		return false
	}
	for _, s := range segs[last+1 : len(segs)-1] { // between skills root and filename
		if strings.HasPrefix(s, ".") {
			return true
		}
	}
	return false
}

// ImportSkillReconciled is the skill-kind import pipeline: ImportOpaque plus
// the two skill-specific guards (vendor-hidden skip, marker reconciliation).
// Every adapter's ImportSkill routes through here.
func ImportSkillReconciled(
	ctx context.Context,
	store *acf.Store,
	params OpaqueParams,
	nativePath string,
	encoder OpaqueEncoder,
) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("adapter: import cancelled: %w", err)
	}
	abs, err := filepath.Abs(nativePath)
	if err != nil {
		return nil, fmt.Errorf("adapter: resolve path: %w", err)
	}
	if SkillPathVendorHidden(abs) {
		return nil, nil
	}
	content, err := os.ReadFile(abs)
	if err != nil {
		return nil, fmt.Errorf("adapter: read %s: %w", abs, err)
	}

	markerID, stripped, marked := ParseSkillMarker(content)
	if !marked {
		return ImportOpaqueContent(ctx, store, acf.KindSkill, params, abs, content, encoder)
	}

	art, aerr := store.ReadArtifact(acf.KindSkill, markerID)
	if aerr != nil {
		// Marker names an artifact this store doesn't have (deleted, or the
		// file was copied from another machine). Import as a fresh skill,
		// WITHOUT the stale marker.
		return ImportOpaqueContent(ctx, store, acf.KindSkill, params, abs, stripped, encoder)
	}

	payload, encErr := encoder(stripped)
	if encErr != nil {
		return nil, fmt.Errorf("adapter: encode payload: %w", encErr)
	}
	unchanged, uerr := EventPayloadUnchanged(store, acf.KindSkill, markerID, payload)
	if uerr != nil {
		return nil, uerr
	}
	if unchanged {
		// Pure echo of our own materialization — same loop break as
		// ImportOpaqueContent's skip-if-equal guard. A marked copy can live at
		// a different SourcePath from the canonical artifact, so returning the
		// artifact id would make callers that rely on SourcePath heads mistake
		// the echo for a fresh commit and fan it out again.
		return nil, nil
	}

	// The user edited THIS copy: append an update to the canonical artifact
	// (provenance = the editing agent) so the change fans out to the source
	// file and every other copy. The artifact's SourcePath stays the origin
	// agent's file.
	now := time.Now().UTC()
	parentHash, herr := RefreshMainBranchHead(store, acf.KindSkill, &art)
	if herr != nil {
		return nil, herr
	}
	art.UpdatedAt = now
	if werr := store.WriteArtifact(art); werr != nil {
		return nil, werr
	}
	event := acf.Event{
		EventID:    acf.NewID(),
		ArtifactID: markerID,
		Type:       acf.EventTypeUpdate,
		Timestamp:  now,
		Provenance: acf.Provenance{
			DeviceID:       params.DeviceID,
			SourceAgent:    params.SourceAgent,
			AgentVersion:   acf.UnknownAgentVersion,
			AdapterVersion: params.AdapterVersion,
			CausedBy:       CausedByFromContext(ctx),
		},
		Payload:    payload,
		ParentHash: parentHash,
	}
	if aerr := store.AppendEvent(acf.KindSkill, event); aerr != nil {
		return nil, aerr
	}
	return []string{markerID}, nil
}

// ExportSkillMarked is the skill-kind export pipeline: ExportOpaque plus the
// provenance marker on CROSS-AGENT materializations and on additional native
// mirrors of the same logical agent. Exports to the origin agent's canonical
// source path stay byte-identical, while a Desktop-worktree copy keeps the
// marker needed to reconcile any later copy edit with the original artifact.
func ExportSkillMarked(
	ctx context.Context,
	store *acf.Store,
	agentName, artifactID, destPath string,
	decoder OpaqueDecoder,
) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("adapter: export cancelled: %w", err)
	}
	current, tombstoned, err := ReplayOpaqueContent(store, acf.KindSkill, artifactID, decoder)
	if err != nil {
		return err
	}
	if tombstoned {
		return ErrArtifactTombstoned
	}
	out := []byte(current)
	if origin, oerr := skillOriginAgent(store, artifactID); oerr == nil && origin != "" && (origin != agentName || IsNativeMirror(ctx)) {
		out = AppendSkillMarker(out, artifactID)
	}
	if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
		return fmt.Errorf("adapter: mkdir dest: %w", err)
	}
	return atomicfile.WriteFile(destPath, out, 0o644)
}

// skillOriginAgent returns the SourceAgent of the artifact's create event —
// the agent whose native tree owns the skill's source file.
func skillOriginAgent(store *acf.Store, artifactID string) (string, error) {
	events, err := store.ReadEvents(acf.KindSkill, artifactID)
	if err != nil {
		return "", err
	}
	if len(events) == 0 {
		return "", nil
	}
	return events[0].Provenance.SourceAgent, nil
}
