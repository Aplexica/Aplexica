// Package onboarding tracks the OSS daemon's first-time-user setup
// progress for the local web UI's /api/onboarding/state endpoint
// (spec §6.9 / §9 W7). State is computed on demand from the daemon's
// runtime — adapter count, first-sync flag — rather than persisted,
// so a fresh-clone OSS install starts from "nothing complete" without
// requiring a migration.
package onboarding

import "time"

// StepID enumerates the steps the web UI walks the user through on
// first run. Per the spec §9 W7 onboarding wizard:
//
//   - install-daemon   : trivially complete by virtue of the API
//     responding (the daemon must be running for
//     the SPA to load).
//   - detect-agents    : ≥1 adapter is installed and detected.
//   - first-sync       : the orchestrator has observed at least one
//     sync event since the daemon started. This is
//     per-daemon-process: the orchestrator's
//     activity is in-memory, so a restart resets it
//     until the next sync event (onboarding state is
//     not persisted).
//
// Cloud-mode adds: pair-device, connect-agents. Those steps are
// surfaced by the Cloud plugin's own onboarding tracker; the local
// surface knows nothing about them.
type StepID string

// The three local-mode step IDs. Stable across releases so the SPA's
// step labels can be keyed off them.
const (
	StepInstallDaemon StepID = "install-daemon"
	StepDetectAgents  StepID = "detect-agents"
	StepFirstSync     StepID = "first-sync"
)

// Step is the wire shape for a single onboarding step.
type Step struct {
	ID          StepID    `json:"id"`
	Complete    bool      `json:"complete"`
	CompletedAt time.Time `json:"completedAt,omitempty"`
}

// State is the wire shape returned by GET /api/onboarding/state.
type State struct {
	Steps []Step `json:"steps"`
}

// Inputs aggregates everything the tracker needs to compute the
// current state. Filled by the daemon's runtime before each call —
// the tracker is stateless.
type Inputs struct {
	// AdapterCount is the number of adapters currently registered
	// with the daemon's orchestrator. Empty means the user hasn't
	// installed any of Aplexica's supported agents (Claude Code,
	// Codex, Hermes, OpenClaw, Kilo) yet.
	AdapterCount int

	// LastSyncActivity is the timestamp of the most recent sync
	// event the orchestrator observed since startup. Zero means no
	// sync activity yet.
	LastSyncActivity time.Time
}

// Compute derives the State from current daemon inputs. Stateless
// function; safe to call from the HTTP handler on every request.
func Compute(in Inputs) State {
	now := time.Now().UTC()
	steps := []Step{
		{
			// install-daemon is true by construction: the API
			// can only respond if the daemon process is running.
			ID:          StepInstallDaemon,
			Complete:    true,
			CompletedAt: now,
		},
		{
			ID:       StepDetectAgents,
			Complete: in.AdapterCount > 0,
			// We don't have first-detection timing; report now()
			// when complete and zero-time when not. The SPA renders
			// just the boolean for now.
			CompletedAt: completeAt(in.AdapterCount > 0, now),
		},
		{
			ID:          StepFirstSync,
			Complete:    !in.LastSyncActivity.IsZero(),
			CompletedAt: in.LastSyncActivity,
		},
	}
	return State{Steps: steps}
}

// completeAt returns t when the step is complete, zero time otherwise.
// Small helper so Compute reads top-to-bottom without branching.
func completeAt(done bool, t time.Time) time.Time {
	if !done {
		return time.Time{}
	}
	return t
}
