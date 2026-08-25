package retention

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/blobstore"
)

// GC action Op values (FR-03.22/23). Each retention mutation reports its
// would-be effect through one of these named ops so a dry-run report renders
// a stable, human-auditable vocabulary.
const (
	// OpEvictAttachment marks a single conversation attachment as evicted
	// (append-only marker event). Emitted by PlanEvictAttachments.
	OpEvictAttachment = "evict-attachment"
	// OpPruneEvents moves a pre-snapshot event out of the active log into
	// the .compacted layer. Emitted by PlanPruneArtifact.
	OpPruneEvents = "prune-events"
	// OpDeleteCompacted deletes a .compacted file whose mtime is past the
	// grace deadline. Emitted by PlanPruneArtifact.
	OpDeleteCompacted = "delete-compacted"
	// OpGCBlob deletes an unreferenced blob past the GC grace window.
	// Emitted by PlanGCBlobs.
	OpGCBlob = "gc-blob"
)

// GCAction is a single would-be (or applied) retention mutation. It is the
// unit of a GCReport: PLAN paths emit GCActions describing exactly what apply
// would do, so dry-run and apply share one wire shape.
type GCAction struct {
	Kind       acf.Kind `json:"kind"`
	ArtifactID string   `json:"artifactId"`
	// Op is one of OpEvictAttachment, OpPruneEvents, OpDeleteCompacted,
	// OpGCBlob.
	Op string `json:"op"`
	// Detail is a short human-readable qualifier (e.g. an event hash, a
	// content hash, or an attachment slot). Free-text; not load-bearing.
	Detail string `json:"detail,omitempty"`
	// BytesSaved is the bytes this action would reclaim. Zero when the
	// action does not directly free bytes (e.g. moving an event to the
	// compacted layer keeps it on disk).
	BytesSaved int64 `json:"bytesSaved"`
}

// GCReport accumulates GCActions for a store or artifact and tracks the total
// bytes the actions would reclaim. DryRun records whether the report came from
// a plan-only pass (no writes) or from an apply pass.
type GCReport struct {
	Actions    []GCAction `json:"actions"`
	BytesSaved int64      `json:"bytesSaved"`
	DryRun     bool       `json:"dryRun"`
}

// AddAction appends a GCAction and accumulates its BytesSaved into the
// report total.
func (r *GCReport) AddAction(a GCAction) {
	r.Actions = append(r.Actions, a)
	r.BytesSaved += a.BytesSaved
}

// markdownHeader / markdownDivider are the fixed table header and rule for
// MarshalMarkdown. The render is intentionally stable (golden-tested) so
// dry-run output diffs cleanly.
const markdownHeader = "| Kind | Artifact | Op | Detail | Bytes |"
const markdownDivider = "| --- | --- | --- | --- | --- |"

// decimalBase is the radix passed to strconv.FormatInt when rendering byte
// counts in the report — base-10 decimal.
const decimalBase = 10

// MarshalMarkdown renders the report as a stable Markdown table: one row per
// action in insertion order, followed by a total row. The column order is
// Kind | Artifact | Op | Detail | Bytes. Output is deterministic for a given
// action sequence.
func (r *GCReport) MarshalMarkdown() []byte {
	var buf bytes.Buffer
	buf.WriteString(markdownHeader)
	buf.WriteByte('\n')
	buf.WriteString(markdownDivider)
	buf.WriteByte('\n')
	for _, a := range r.Actions {
		buf.WriteString("| ")
		buf.WriteString(string(a.Kind))
		buf.WriteString(" | ")
		buf.WriteString(a.ArtifactID)
		buf.WriteString(" | ")
		buf.WriteString(a.Op)
		buf.WriteString(" | ")
		buf.WriteString(a.Detail)
		buf.WriteString(" | ")
		buf.WriteString(strconv.FormatInt(a.BytesSaved, decimalBase))
		buf.WriteString(" |\n")
	}
	buf.WriteString("| **Total** | | | | ")
	buf.WriteString(strconv.FormatInt(r.BytesSaved, decimalBase))
	buf.WriteString(" |\n")
	return buf.Bytes()
}

// gcReportWire is the stable JSON wire shape for a GCReport. A nil Actions
// slice marshals as an empty array (not null) so the wire shape is identical
// whether or not any actions were recorded.
type gcReportWire struct {
	Actions    []GCAction `json:"actions"`
	BytesSaved int64      `json:"bytesSaved"`
	DryRun     bool       `json:"dryRun"`
}

// MarshalJSON renders the report in a stable wire shape. Actions is always a
// JSON array (empty, never null) so consumers can rely on the field type.
func (r *GCReport) MarshalJSON() ([]byte, error) {
	w := gcReportWire{
		Actions:    r.Actions,
		BytesSaved: r.BytesSaved,
		DryRun:     r.DryRun,
	}
	if w.Actions == nil {
		w.Actions = []GCAction{}
	}
	out, err := json.Marshal(w)
	if err != nil {
		return nil, fmt.Errorf("retention: marshal gc report: %w", err)
	}
	return out, nil
}

// PlanGC assembles a dry-run GCReport for one conversation artifact by running
// the three retention plan paths (prune, attachment eviction, blob GC) WITHOUT
// mutating, and folding each plan's would-be effects into GCActions. The
// returned report has DryRun set. It is the plan-assembly helper the later
// gc --dry-run command renders; it is not wired to any command here.
//
// Blob GC is store-wide (a blob may be shared across conversations), so the
// blob actions reflect every collectible blob in the store, not only those
// referenced by artifactID. graceDeadline gates the prune compacted-delete
// decision exactly as PruneArtifact's grace check does.
func PlanGC(ctx context.Context, store *acf.Store, blobs *blobstore.Store, kind acf.Kind, artifactID string, opts EvictOpts, graceDeadline time.Time) (GCReport, error) {
	report := GCReport{DryRun: true}

	prunePlan, err := PlanPruneArtifact(ctx, store, kind, artifactID, graceDeadline)
	if err != nil {
		return report, err
	}
	for _, e := range prunePlan.ToMove {
		report.AddAction(GCAction{
			Kind:       kind,
			ArtifactID: artifactID,
			Op:         OpPruneEvents,
			Detail:     e.Hash,
		})
	}
	if prunePlan.DeleteCompacted {
		report.AddAction(GCAction{
			Kind:       kind,
			ArtifactID: artifactID,
			Op:         OpDeleteCompacted,
			Detail:     artifactID + ".jsonl.gz",
			BytesSaved: prunePlan.CompactedBytes,
		})
	}

	evictPlan, err := PlanEvictAttachments(ctx, store, kind, artifactID, opts)
	if err != nil {
		return report, err
	}
	for _, ev := range evictPlan.Events {
		report.AddAction(GCAction{
			Kind:       kind,
			ArtifactID: artifactID,
			Op:         OpEvictAttachment,
			Detail:     fmt.Sprintf("%d attachment(s)", ev.AttachmentsEvicted),
			BytesSaved: ev.BytesReclaimable,
		})
	}

	blobEntries, err := PlanGCBlobs(ctx, store, blobs)
	if err != nil {
		return report, err
	}
	for _, b := range blobEntries {
		report.AddAction(GCAction{
			Kind:       acf.KindConversation,
			ArtifactID: "",
			Op:         OpGCBlob,
			Detail:     b.Hash,
			BytesSaved: b.Bytes,
		})
	}

	return report, nil
}
