package adapter

import "github.com/aplexica/aplexica/internal/acf"

// SessionDeclineReason explains WHY a ConversationSessionTarget returned
// supports=false. A decline is not an error: the adapter refused to write a
// native session file it does not currently own outright, which is the
// invariant that keeps an unimported agent-side continuation safe.
//
// The orchestrator needs the reason because the deferral queue holds three
// structurally different populations — a live agent racing us, a projection
// that can still converge, and one that never will — and retrying all three
// identically is what produced an indefinite, silent retry loop. Adapters
// already compute the facts that separate them and then discard them.
//
// Reasons are content-free: each names a structural relation between the
// canonical plan and the bytes on disk, never anything the user wrote.
type SessionDeclineReason string

const (
	// SessionDeclineUnspecified is what an adapter that does not implement the
	// reporting refinements below is treated as reporting. It classifies as
	// unknown and must never be read as evidence that a retry is hopeless.
	SessionDeclineUnspecified SessionDeclineReason = ""

	// SessionDeclineOptOut is a permanent, non-failure refusal: this payload or
	// this runtime is simply not materializable here — no home directory, no
	// installed session store, an unsupported conversation format, or no text
	// turns at all.
	SessionDeclineOptOut SessionDeclineReason = "opt_out"

	// SessionDeclineRace means the destination was being written while we
	// looked at it: a torn trailing row, a lost optimistic-append race, or a
	// file whose bytes or inode changed between snapshot and commit. The next
	// pass converges, so this must never terminate a queued write.
	SessionDeclineRace SessionDeclineReason = "race"

	// SessionDeclineNativeAhead means the agent's own file already holds every
	// turn the canonical plan holds, and more. That pending watcher import is
	// the only authority allowed to move canonical forward, so the write waits
	// for it rather than reordering the thread.
	SessionDeclineNativeAhead SessionDeclineReason = "native_ahead"

	// SessionDeclineDiverged means neither side is a prefix of the other: the
	// file holds at least one turn the plan lacks AND the plan holds at least
	// one the file lacks. No append repairs that, and picking a winner is a
	// human decision.
	//
	// It names the case where the file is the agent's OWN transcript — the
	// user's native session — so canonical is the side that can be repaired.
	SessionDeclineDiverged SessionDeclineReason = "diverged"

	// SessionDeclineMirrorDiverged is the same relation on an Aplexica-owned
	// MIRROR: the mirror holds a turn canonical never saw. It is a distinct
	// reason because the two have opposite remedies — canonical divergence is
	// resolved by collapsing a duplicated canonical head, while a mirror holding
	// a foreign turn is blocked until that turn is imported, and offering the
	// canonical repair for it costs the operator the attempt to discover it does
	// not apply. This can be a common retained population when mirrors hold
	// turns absent from canonical.
	SessionDeclineMirrorDiverged SessionDeclineReason = "mirror_diverged"

	// SessionDeclineForkedMirror means conversational rows exist in the file
	// that the agent's own resume walk cannot reach BECAUSE the parent graph
	// forked: some node has more than one child subtree holding a conversational
	// row, so no single chain can include both. Nothing in the file is missing
	// and nothing may be appended to it; only a rebuild can repair it.
	//
	// It is reported from a DIRECT fork measurement. It used to be reported from
	// "the whole-file node count disagrees with the leaf-chain turn count",
	// which is a different question — that mismatch is also produced by shapes
	// with no fork anywhere in them, and those now report the reason below.
	SessionDeclineForkedMirror SessionDeclineReason = "forked_mirror"

	// SessionDeclineChainUnspanned means the file holds a conversational row the
	// agent's own resume walk did not visit, and the graph is provably NOT
	// forked: no node has two conversational branches. This covers multi-root
	// files with several independent conversation trees each parented at null
	// in one file, which is two threads sharing a pathname rather than one thread
	// that branched. Naming it separately is what keeps these files out of the
	// fork's remedy, whose whole premise is one chain and one leaf. Structural:
	// re-reading the same bytes reaches the same conclusion.
	SessionDeclineChainUnspanned SessionDeclineReason = "chain_unspanned"

	// SessionDeclineGraphMalformed means the file could not be proven to be the
	// session it claims to be — an unparseable graph, a session id that does
	// not match its own pathname, or an Aplexica thread stamp on a file the
	// artifact records as its pristine native source. Retrying re-reads the
	// same bytes and reaches the same conclusion.
	SessionDeclineGraphMalformed SessionDeclineReason = "graph_malformed"
)

// ConversationSessionDeclineReporter is an OPTIONAL refinement for
// ConversationSessionTarget (type-asserted, so it neither widens the Adapter
// interface nor the out-of-process plugin protocol). An adapter implements it
// to name the reason for a decline it already decided; the plain
// MaterializeConversationSession stays the compatibility entry point and is
// expected to delegate here so the two can never disagree.
type ConversationSessionDeclineReporter interface {
	// MaterializeConversationSessionReason is MaterializeConversationSession
	// plus the typed reason. reason is meaningful only when supports=false; a
	// successful write reports SessionDeclineUnspecified.
	MaterializeConversationSessionReason(
		art acf.Artifact, head acf.Event, sourceAgent string,
	) (path string, supports bool, reason SessionDeclineReason, err error)
}

// ConversationMirrorRepairReporter is an OPTIONAL refinement for an adapter
// that can automatically rebuild a forked mirror of its own sessions.
//
// It exists so the orchestrator's operator-facing surface can tell the two
// forked-mirror outcomes apart. "This build has no repair for that" and "this
// build has a repair and it is switched off" call for opposite messages, and
// printing the first when the second is true is the maximum-user-involvement
// outcome on the one decline class that has an automatic fix.
type ConversationMirrorRepairReporter interface {
	// ForkedMirrorRepairEnabled reports whether the adapter is currently
	// authorized to rebuild a forked synthetic mirror.
	ForkedMirrorRepairEnabled() bool
}

// ConversationSessionPathDeclineReporter is the same refinement for
// ConversationSessionPathTarget. The path planner declines independently of the
// writer — a native-origin session whose visible turns diverged is refused
// before any write is attempted — so its reason has to be reported separately.
type ConversationSessionPathDeclineReporter interface {
	// ConversationSessionPathReason is ConversationSessionPath plus the typed
	// reason, with the same supports/reason contract as
	// MaterializeConversationSessionReason.
	ConversationSessionPathReason(
		art acf.Artifact, head acf.Event, sourceAgent string,
	) (path string, supports bool, reason SessionDeclineReason, err error)
}

// ConversationSessionSourceInspector is an OPTIONAL refinement for
// ConversationSessionTarget adapters that own an artifact's native SOURCE
// transcript. It answers, READ-ONLY, the one question no other surface can:
// does the origin session file, exactly as it stands on disk, present the
// canonical head on the agent's own resume walk?
//
// It exists for the origin-session fork. When the artifact's own agent appends
// at a stale in-memory leaf, its import commits fine and canonical converges —
// but fan-out same-source-excludes the origin adapter, so no materialize
// attempt is ever made toward the forked file: no attempt, no decline, no
// queue entry, no repair. The path planner cannot stand in for this check
// either, in either direction: it compares turns in FILE ORDER, so a fork
// whose physical rows equal the canonical plan reads as perfectly writable
// (only the WRITER discovers the graph cannot be walked), and with the
// forked-mirror repair authorized it deliberately reports a repairable
// divergence as supports=true so the write path can reach the rebuild.
//
//   - applicable=false: the artifact/head does not name a native-origin source
//     session this adapter owns (a synthetic mirror, a remote shell, a fork
//     branch, an unsupported payload). The caller imposes nothing.
//   - reusable=true: the file already presents every canonical turn on the
//     agent's own resume walk, or is a spanning prefix the ordinary append
//     converges. Nothing needs to be queued.
//   - reusable=false: the file, as it stands, cannot come to present the
//     canonical conversation through the ordinary routes. reason classifies
//     why, with the same vocabulary the write path declines with, so the
//     caller can tell a transient race from a structural fork.
type ConversationSessionSourceInspector interface {
	InspectConversationSessionSource(
		art acf.Artifact, head acf.Event,
	) (reusable, applicable bool, reason SessionDeclineReason, err error)
}
