package syncd

import (
	"errors"
	"fmt"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter"
)

// ConversationRetryClass groups adapter decline reasons by what a retry can
// plausibly achieve. It exists because the deferral queue holds structurally
// different populations — a live agent racing us, and a projection that will
// never converge — and pacing both identically is what turned a projection
// failure into an indefinite silent retry.
//
// The class is carried on ConversationDeclineError rather than recomputed by
// each consumer, so the daemon's status surface and the queue's give-up
// accounting can never disagree about what a decline meant.
type ConversationRetryClass string

const (
	// ConversationRetryUnknown is what an adapter that reports no reason gets.
	// It is deliberately distinct from every other class: nothing may read an
	// unclassified decline as evidence that retrying is pointless.
	ConversationRetryUnknown ConversationRetryClass = "unknown"

	// ConversationRetryRace covers declines that the next pass is expected to
	// resolve on its own — a snapshot race, or a native session that is
	// currently ahead of canonical and whose pending import will converge it.
	// A conversation left open in a TUI all day lives here, which is why this
	// class must never be terminated on elapsed time alone.
	ConversationRetryRace ConversationRetryClass = "race"

	// ConversationRetryStructural covers declines that re-reading the same
	// bytes will reach again: genuine divergence, a forked graph, or a session
	// that cannot be authenticated as the thread it claims to be. Retrying
	// changes nothing, so this is the only class for which giving up is honest.
	ConversationRetryStructural ConversationRetryClass = "structural"

	// ConversationRetryOptOut is a permanent, non-failure refusal: the payload
	// or the runtime is simply not materializable in this agent.
	ConversationRetryOptOut ConversationRetryClass = "opt_out"
)

// conversationRetryClassFor routes a typed adapter reason to its retry class.
// Every reason is enumerated explicitly so a new one cannot silently inherit a
// class it was never reviewed for.
func conversationRetryClassFor(reason adapter.SessionDeclineReason) ConversationRetryClass {
	switch reason {
	case adapter.SessionDeclineRace, adapter.SessionDeclineNativeAhead:
		return ConversationRetryRace
	case adapter.SessionDeclineDiverged,
		adapter.SessionDeclineMirrorDiverged,
		adapter.SessionDeclineForkedMirror,
		adapter.SessionDeclineChainUnspanned,
		adapter.SessionDeclineGraphMalformed:
		return ConversationRetryStructural
	case adapter.SessionDeclineOptOut:
		return ConversationRetryOptOut
	default:
		return ConversationRetryUnknown
	}
}

// conversationDeclineExplanation renders a reason for a human reading a CLI
// error. `aplexica materialize` used to say only that the agent "did not
// materialize the conversation", which is exactly the uninformative phrasing
// that let a permanently diverged session look like a transient one.
func conversationDeclineExplanation(reason adapter.SessionDeclineReason) string {
	switch reason {
	case adapter.SessionDeclineOptOut:
		return "this payload is not materializable in that agent"
	case adapter.SessionDeclineRace:
		return "its native session was being written; retry once it settles"
	case adapter.SessionDeclineNativeAhead:
		return "its native session is ahead of the canonical conversation and must be imported first"
	case adapter.SessionDeclineDiverged:
		return "its native session has diverged from the canonical conversation"
	case adapter.SessionDeclineMirrorDiverged:
		return "the copy Aplexica maintains for that agent holds a turn the canonical conversation has not imported"
	case adapter.SessionDeclineForkedMirror:
		return "its session file holds rows the agent's own resume walk cannot reach"
	case adapter.SessionDeclineChainUnspanned:
		return "its session file holds a conversational row its own resume walk did not visit, " +
			"and the graph is not forked"
	case adapter.SessionDeclineGraphMalformed:
		return "its session file could not be authenticated as this conversation"
	default:
		return "the agent reported no reason"
	}
}

// ConversationDeclineError is the cause a conversation adapter's decline
// carries out through the strict fan-out funnel. It wraps
// ErrInboundNativeMaterialization so every existing errors.Is caller — the
// durable inbound finalizer, the deferral drain, the remote materializer — is
// unaffected, while errors.As recovers the reason and its retry class.
//
// It is content-free by construction: agent, artifact id, branch, reason and a
// path already run through redactPaths. Nothing the user wrote can reach it.
type ConversationDeclineError struct {
	Agent      string
	ArtifactID string
	Branch     string
	Reason     adapter.SessionDeclineReason
	RetryClass ConversationRetryClass
	// Path is the redacted destination the adapter refused to write. It exists
	// so an operator can tell WHICH session declined without the daemon ever
	// recording a real user path.
	Path string

	// dest is the same destination unredacted. It stays unexported so it can
	// reach neither JSON, nor Error(), nor any consumer outside this package,
	// and exists for exactly one purpose: the deferral drain stats it on the
	// next retry to tell whether the agent has touched the file since. That
	// stat is the quiescence signal the escalation rule runs on, and without a
	// real path there is nothing to stat.
	dest string
}

func newConversationDeclineError(
	agent, artifactID, branch string,
	reason adapter.SessionDeclineReason,
	path string,
) *ConversationDeclineError {
	return &ConversationDeclineError{
		Agent:      agent,
		ArtifactID: artifactID,
		Branch:     branch,
		Reason:     reason,
		RetryClass: conversationRetryClassFor(reason),
		Path:       redactPaths(path),
		dest:       path,
	}
}

// materializationGateError is the cause a drain pass carries when a reversible
// policy gate turned it back BEFORE the target adapter saw it. It names the
// gate using the suppression ledger's own vocabulary, so a queued write that
// never once reached its adapter can be explained — and given a remedy that
// opens that specific gate — instead of reporting the same opaque "inbound
// native materialization incomplete" as every genuine refusal.
type materializationGateError struct {
	Reason SuppressionReason
}

func newMaterializationGateError(reason SuppressionReason) *materializationGateError {
	return &materializationGateError{Reason: reason}
}

func (e *materializationGateError) Error() string {
	if e.Reason == "" {
		return ErrInboundNativeMaterialization.Error() + " (" + errDeferredMaterializationWithheld.Error() + ")"
	}
	return fmt.Sprintf("%s (%s: %s)",
		ErrInboundNativeMaterialization.Error(), errDeferredMaterializationWithheld.Error(), e.Reason)
}

// Unwrap reports BOTH sentinels: callers outside the drain keep matching on
// ErrInboundNativeMaterialization, while the drain reads the withheld marker
// and declines to spend this entry's attempt budget.
func (e *materializationGateError) Unwrap() []error {
	return []error{ErrInboundNativeMaterialization, errDeferredMaterializationWithheld}
}

// classifyDeferredMaterializationCause folds one failed attempt's typed cause
// into the entry, so the give-up row can name what actually refused the write.
// Before this the queue kept only a redacted error string, so needs_attention
// rows carried one indistinguishable sentence.
func classifyDeferredMaterializationCause(entry *deferredMaterializationEntry, cause error) {
	if errors.Is(cause, errDeferredMaterializationUnchanged) {
		// The short circuit deliberately observed nothing new, so it must not
		// disturb the classification the last real attempt produced — otherwise
		// it would retract its own evidence and the drain would alternate
		// between skipping and re-asking forever.
		return
	}
	var decline *ConversationDeclineError
	if errors.As(cause, &decline) {
		entry.declineReason = decline.Reason
		// The target itself answered, so no gate is the current explanation.
		entry.withheldReason = ""
		if decline.dest != "" {
			entry.destPath = decline.dest
			entry.declineObserved = true
			// Witness the destination NOW, at the moment we first learn which
			// file the target refused. Deferring that to the next pass would make
			// the first real observation read as "the destination changed", which
			// restarts the quiescence clock — and destPath is memory-only, so
			// that first observation happens again after every daemon restart.
			// A clock a restart resets is the exact defect the age+quiescence
			// rule replaced.
			entry.destWitness = witnessSessionFile(decline.dest)
			entry.destObserved = true
		}
		return
	}
	// Anything else — a transient adapter error, a closed policy gate, a
	// shutdown — leaves us without proof that the observed inputs are what
	// decided this outcome, so the next pass must actually run.
	entry.declineObserved = false
	var gate *materializationGateError
	if errors.As(cause, &gate) {
		entry.withheldReason = gate.Reason
	}
}

func (e *ConversationDeclineError) Error() string {
	reason := string(e.Reason)
	if reason == "" {
		reason = "unreported"
	}
	return fmt.Sprintf("%s: %s declined conversation %s (%s, %s)",
		ErrInboundNativeMaterialization.Error(), e.Agent, e.ArtifactID, reason, e.RetryClass)
}

// Unwrap keeps the sentinel matchable. Callers outside this package have always
// tested errors.Is(err, ErrInboundNativeMaterialization) and must keep working
// byte-for-byte.
func (e *ConversationDeclineError) Unwrap() error { return ErrInboundNativeMaterialization }

// planConversationSessionPath resolves an adapter's deterministic destination,
// preferring the reason-carrying refinement when the adapter implements it. An
// adapter that predates the refinement reports SessionDeclineUnspecified, which
// classifies as unknown rather than as anything terminal.
func planConversationSessionPath(
	planner adapter.ConversationSessionPathTarget,
	art acf.Artifact,
	head acf.Event,
	sourceAgent string,
) (string, bool, adapter.SessionDeclineReason, error) {
	if reporter, ok := planner.(adapter.ConversationSessionPathDeclineReporter); ok {
		return reporter.ConversationSessionPathReason(art, head, sourceAgent)
	}
	path, supported, err := planner.ConversationSessionPath(art, head, sourceAgent)
	return path, supported, adapter.SessionDeclineUnspecified, err
}

// materializeConversationSessionInto is planConversationSessionPath's sibling
// for the write itself.
func materializeConversationSessionInto(
	st adapter.ConversationSessionTarget,
	art acf.Artifact,
	head acf.Event,
	sourceAgent string,
) (string, bool, adapter.SessionDeclineReason, error) {
	if reporter, ok := st.(adapter.ConversationSessionDeclineReporter); ok {
		return reporter.MaterializeConversationSessionReason(art, head, sourceAgent)
	}
	path, supported, err := st.MaterializeConversationSession(art, head, sourceAgent)
	return path, supported, adapter.SessionDeclineUnspecified, err
}
