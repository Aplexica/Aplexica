// Package syncgate gates outbound cross-agent fan-out per participating agent.
//
// FR-03.3 / user decision (2026-05-28): after the daemon DISCOVERS installed
// agents it imports their state into the canonical store and SHOWS it, but it
// must NOT move data between agents until the user explicitly enables sync.
// This is the "discover + show, await config" default. The orchestrator
// consults the gate for both the source and target: an agent whose sync is not
// enabled may still import into the canonical store for local visibility, but
// it neither feeds nor receives cross-agent fan-out.
//
// It mirrors the existing ProjectRegistry / RulesEngine / PauseStore gates in
// internal/sync: a small, side-effect-free predicate the orchestrator
// consults in fanOut.
package syncgate

// Config is the persisted enablement state (bridged from daemon.SyncConfig).
type Config struct {
	// All enables fan-out to every installed agent.
	All bool
	// Agents holds per-agent overrides. A present key wins over All
	// (true force-enables, false force-excludes).
	Agents map[string]bool
}

// Gate answers "may this agent participate in cross-agent fan-out?".
type Gate struct{ cfg Config }

// New returns a Gate for cfg. The zero Config denies every agent — the
// await-config default.
func New(cfg Config) *Gate { return &Gate{cfg: cfg} }

// Enabled reports whether fan-out involving agentName is allowed. Resolution order:
// explicit per-agent override, then All, else deny. A nil *Gate is permissive
// (no gating configured — pre-Slice-3 behavior) so callers that never set a
// gate keep their previous fan-out semantics.
func (g *Gate) Enabled(agentName string) bool {
	if g == nil {
		return true
	}
	if v, ok := g.cfg.Agents[agentName]; ok {
		return v
	}
	return g.cfg.All
}
