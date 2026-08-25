package syncd

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter"
	"github.com/aplexica/aplexica/internal/syncrules"
)

// DefaultConvBackfill is the per-agent cap, when none is configured, on how many
// of the most-recent conversations are materialized into an agent the first time
// it's enabled for sync. Bounds the "enable codex → replicate my entire
// claude-code history into codex" flood while still seeding recent context.
const DefaultConvBackfill = 10

// capConvBackfill decides, for conversations ordered NEWEST-FIRST, which target
// agents each conversation may be materialized into so that each target receives
// at most limit(convIdx, target) conversations (resolved per conversation, so a
// routing rule's route.historicalSyncDepth can vary the cap). A negative limit
// means unlimited. A
// conversation is never backfilled into the agent that authored it. Returns a
// slice parallel to convSources: the allowed target set for each conversation.
//
// Pure (no IO) so the cap arithmetic is unit-tested directly; the orchestrator
// feeds it source agents in recency order and turns each result into a per-call
// fan-out target filter.
func capConvBackfill(convSources []string, targets []string, limit func(convIdx int, target string) int) [][]string {
	return capConvBackfillWithSourcePolicy(convSources, targets, false, limit)
}

// capConvBackfillWithSourcePolicy is the late-device variant of
// capConvBackfill. Ordinary local backfill excludes the author because its
// native session is already present. A runtime installed after remote events
// arrived must include a same-named author: that author was on another device,
// and this device's newly available surface has no native session yet.
func capConvBackfillWithSourcePolicy(convSources []string, targets []string, includeSource bool, limit func(convIdx int, target string) int) [][]string {
	return capConvBackfillWithPerConversationSourcePolicy(convSources, targets, func(int) bool { return includeSource }, limit)
}

// capConvBackfillWithPerConversationSourcePolicy applies the same bounded
// planning while deciding same-agent eligibility per conversation. Late
// runtime activation uses this to admit only peer-device heads: locally
// authored sessions already exist in the shared CLI/Desktop storage and must
// neither be synthesized again nor consume the newest-N budget.
func capConvBackfillWithPerConversationSourcePolicy(convSources []string, targets []string, includeSource func(convIdx int) bool, limit func(convIdx int, target string) int) [][]string {
	counts := make(map[string]int, len(targets))
	out := make([][]string, len(convSources))
	for i, src := range convSources {
		var allowed []string
		for _, t := range targets {
			if t == src && (includeSource == nil || !includeSource(i)) {
				continue // never materialize a conversation into its own author
			}
			lim := limit(i, t)
			if lim < 0 || counts[t] < lim {
				allowed = append(allowed, t)
				if lim >= 0 {
					counts[t]++
				}
			}
		}
		out[i] = allowed
	}
	return out
}

// SetConvBackfill updates the per-agent conversation-backfill caps (from
// SyncConfig.ConvBackfill). Missing agent → DefaultConvBackfill; a negative
// value → unlimited. Safe for concurrent callers (daemon config reload).
func (o *Orchestrator) SetConvBackfill(m map[string]int) {
	o.mu.Lock()
	o.convBackfill = m
	o.mu.Unlock()
}

// convBackfillLimit returns the conversation-backfill cap for an agent.
func (o *Orchestrator) convBackfillLimit(agent string) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.convBackfill != nil {
		if n, ok := o.convBackfill[agent]; ok {
			return n
		}
	}
	return DefaultConvBackfill
}

// resolveConvBackfillDepths returns the per-target conversation-backfill depth
// for one artifact, preferring a matching routing rule's
// route.historicalSyncDepth over the global sync.convBackfill map and
// DefaultConvBackfill. Evaluated per artifact so a rule that matches only a
// subset of conversations (by tag/type/etc.) caps just those. Falls back to the
// global cap when the rules engine is absent or sets no depth for a target.
func (o *Orchestrator) resolveConvBackfillDepths(art acf.Artifact, originAgent string, targets []string) map[string]int {
	out := make(map[string]int, len(targets))
	if eng := o.rulesEngine(); eng != nil {
		names := make([]string, 0, len(o.cfg.Adapters))
		for _, ad := range o.cfg.Adapters {
			names = append(names, ad.Name())
		}
		for _, target := range targets {
			branch := selectedBranchForAgent(art, target)
			dec := eng.Evaluate(ruleInputFor(art, originAgent, branch), syncrules.EvaluateOpts{InstalledAgents: names})
			if n, ok := dec.HistoricalSyncDepth[target]; ok {
				out[target] = n
				continue
			}
			out[target] = o.convBackfillLimit(target)
		}
		return out
	}
	for _, t := range targets {
		out[t] = o.convBackfillLimit(t)
	}
	return out
}

// backfillConversations re-fans the conversations already in the store to the
// currently-enabled target agents, but caps each agent at its configured
// most-recent-N (see capConvBackfill). Unlike non-conversation kinds — which
// are few and fully back-filled by RefanOutAll — conversation history can be
// huge, so an uncapped backfill floods a newly-enabled agent's session store
// with another agent's entire history. Live (going-forward) fan-out is never
// capped. Returns the number of conversations processed.
func (o *Orchestrator) backfillConversations(ctx context.Context) int {
	_, processed := o.backfillConversationsRun(ctx, backfillRunOpts{apply: true})
	return processed
}

// backfillConversationsInto replays bounded history into only the requested
// logical adapters. A nil target set preserves the ordinary all-target
// backfill. includeSource is reserved for late runtime activation, where the
// provenance author may have the same adapter name but ran on another device.
func (o *Orchestrator) backfillConversationsInto(ctx context.Context, only map[string]struct{}, includeSource bool) int {
	_, processed := o.backfillConversationsRun(ctx, backfillRunOpts{
		only:          only,
		includeSource: includeSource,
		apply:         true,
	})
	return processed
}

// backfillRunOpts parameterizes one conversation-backfill pass.
type backfillRunOpts struct {
	// only limits the pass to these logical adapters; nil = every enabled one.
	only map[string]struct{}
	// includeSource is reserved for late runtime activation (see
	// backfillConversationsInto).
	includeSource bool
	// depthOverride, when non-nil, replaces the per-conversation depth
	// resolution (route.historicalSyncDepth → sync.convBackfill →
	// DefaultConvBackfill) with one uniform per-target depth. Negative =
	// unlimited. Routing-rule allow/deny decisions still apply: an override
	// changes HOW MUCH history a permitted target receives, never WHICH
	// targets a conversation may reach.
	depthOverride *int
	// apply=false plans without materializing anything (a dry run).
	apply bool
	// flushLarge + holdRestoreGate are set by the forced pass: large
	// conversations write inline (no coalescing-timer storm), and each
	// conversation's fan-out runs under the native-restore read lock so a
	// concurrently started native restore cannot interleave with a
	// multi-minute full-store write. Per-conversation, not pass-wide, so the
	// restore writer's TryLock loop is never starved for the whole pass.
	// The ordinary enable-time callers already run under RefanOutAll's
	// pass-wide read lock and must NOT re-acquire it here: an RWMutex read
	// lock is not reentrant once a writer is waiting.
	flushLarge      bool
	holdRestoreGate bool
}

// ForcedBackfillResult reports what a conversation-backfill pass planned. The
// counts are planning-time: an applied pass may still defer individual
// materializations through the ordinary retry queue.
type ForcedBackfillResult struct {
	// Conversations counts conversations with at least one planned target.
	Conversations int `json:"conversations"`
	// PerAgent counts planned conversation materializations per target agent.
	PerAgent map[string]int `json:"perAgent,omitempty"`
	// Targets is the resolved set of enabled conversation-capable agents the
	// pass considered (after the only-filter).
	Targets []string `json:"targets,omitempty"`
	// DryRun records whether the pass only planned.
	DryRun bool `json:"dryRun"`
	// Truncated reports that an applied pass stopped early because the daemon
	// began shutting down; the counts then cover only the attempted prefix.
	Truncated bool `json:"truncated,omitempty"`
}

// backfillConversationsRun is the single implementation behind every
// conversation-backfill entry point. It returns the plan and the legacy
// processed-conversation count (every conversation iterated, matching the
// historical RefanOutAll return semantics).
func (o *Orchestrator) backfillConversationsRun(ctx context.Context, opts backfillRunOpts) (ForcedBackfillResult, int) {
	only, includeSource := opts.only, opts.includeSource
	result := ForcedBackfillResult{PerAgent: map[string]int{}, DryRun: !opts.apply}
	if o.cfg.Store == nil {
		return result, 0
	}
	convs, err := o.cfg.Store.ListArtifacts(acf.KindConversation)
	if err != nil {
		return result, 0
	}
	// Most-recent first so the cap keeps the freshest context.
	sort.SliceStable(convs, func(i, j int) bool {
		return convs[i].UpdatedAt.After(convs[j].UpdatedAt)
	})

	// Enabled agents that can receive a conversation (native session or
	// markdown doc). Only enabled targets get a backfill, so the cap is
	// counted against — and the loop stops once exhausted for — exactly the
	// agents that actually materialize.
	gate := o.syncGate()
	var targets []string
	for _, ad := range o.cfg.Adapters {
		name := ad.Name()
		if only != nil {
			if _, requested := only[name]; !requested {
				continue
			}
		}
		if gate != nil && !gate.Enabled(name) {
			continue
		}
		if _, ok := ad.(adapter.ConversationSessionTarget); ok {
			targets = append(targets, name)
			continue
		}
		if _, ok := ad.(adapter.ConversationDocTarget); ok {
			targets = append(targets, name)
		}
	}

	// Resolve each conversation's source agent (its author) for the cap.
	primaries := make([]adapter.Adapter, len(convs))
	sources := make([]string, len(convs))
	inboundEligible := make([]bool, len(convs))
	for i, art := range convs {
		primaries[i], sources[i] = o.backfillPrimary(art)
		// Inbound activation semantics are eligible only for the durable shell
		// shape (native imports always set Name and SourcePath). Prefer explicit
		// peer-device ancestry. After retention's compacted-history grace period,
		// a self-contained snapshot may be the only surviving event and carry no
		// device provenance; the inbound-shell identity is the fail-safe fallback
		// only when no device identity remains. Local-only provenance is rejected.
		if includeSource && art.SourcePath == "" && art.Name == "" && o.localDeviceID() != "" {
			branch := selectedBranchForAgent(art, sources[i])
			_, materializable, headErr := conversationHeadForBranch(o.cfg.Store, art.ArtifactID, branch)
			inboundEligible[i] = headErr == nil && materializable &&
				art.RemoteOriginDeviceID != "" && art.RemoteOriginDeviceID != o.localDeviceID()
		}
	}

	// Resolve each conversation's per-agent backfill depth from the routing
	// rules (route.historicalSyncDepth), falling back to the global
	// sync.convBackfill then DefaultConvBackfill. A forced pass replaces that
	// whole chain with one uniform depth — but the rule allow/deny pass below
	// still runs unconditionally: forcing history depth must never widen WHERE
	// a conversation may go.
	depths := make([]map[string]int, len(convs))
	for i, art := range convs {
		if opts.depthOverride != nil {
			depths[i] = make(map[string]int, len(targets))
			for _, target := range targets {
				depths[i][target] = *opts.depthOverride
			}
		} else {
			depths[i] = o.resolveConvBackfillDepths(art, sources[i], targets)
		}
		origin := sources[i]
		for _, target := range targets {
			branch := selectedBranchForAgent(art, target)
			if !o.conversationRuleAllowsTarget(art, origin, target, branch) {
				// A denied conversation must not consume this target's
				// newest-N budget; an older allowed conversation may still be
				// the freshest history the route permits.
				depths[i][target] = 0
			}
		}
	}
	plan := capConvBackfillWithPerConversationSourcePolicy(sources, targets, func(convIdx int) bool {
		return inboundEligible[convIdx]
	}, func(convIdx int, target string) int {
		// A dispatch fallback is not proof of authorship. During runtime
		// activation, an unknown-source conversation that is not a durable
		// inbound shell is therefore unsafe to synthesize into any late target.
		// Excluding it at planning time also keeps it from consuming the cap so
		// older proven peer history can still catch up.
		if includeSource && sources[convIdx] == "" && !inboundEligible[convIdx] {
			return 0
		}
		return depths[convIdx][target]
	})

	result.Targets = append([]string(nil), targets...)
	n := 0
	for i, art := range convs {
		allowed := make(map[string]struct{}, len(plan[i]))
		for _, t := range plan[i] {
			allowed[t] = struct{}{}
		}
		if len(allowed) == 0 {
			n++
			continue // every target already at its cap for older conversations
		}
		// A forced full backfill can cover thousands of conversations. Checking
		// the shutdown signal between conversations bounds Close's background
		// join to one in-flight fan-out, mirroring the per-file scan loops. The
		// check runs BEFORE this conversation is counted so a truncated pass
		// reports exactly what it attempted, and the truncation is never silent.
		if opts.apply && o.closingNow() {
			result.Truncated = true
			break
		}
		result.Conversations++
		for _, t := range plan[i] {
			result.PerAgent[t]++
		}
		if !opts.apply {
			n++
			continue
		}
		if opts.holdRestoreGate {
			o.nativeRestoreGate.RLock()
		}
		ctxDir := ""
		if art.Scope == acf.ScopeProject && art.Project != nil {
			ctxDir = art.Project.Path
		}
		// Every target of a proven remote shell needs inbound semantics. The
		// source adapter may be absent or disabled locally, so applying the local
		// source gate here would strand peer history when a different harness is
		// installed later. The already-bounded target set and target gates still
		// constrain the catch-up pass.
		if inboundEligible[i] && primaries[i] != nil {
			requested := make(map[string]struct{}, len(allowed))
			for target := range allowed {
				requested[target] = struct{}{}
			}
			origin := sources[i]
			o.fanOutWithOptions(ctx, primaries[i], []string{art.ArtifactID}, ctxDir, art.SourcePath, true,
				fanOutOptions{targets: requested, originAgent: &origin, flushLarge: opts.flushLarge})
			clear(allowed)
		}
		if len(allowed) > 0 {
			requested := make(map[string]struct{}, len(allowed))
			for target := range allowed {
				requested[target] = struct{}{}
			}
			origin := sources[i]
			o.fanOutWithOptions(ctx, primaries[i], []string{art.ArtifactID}, ctxDir, art.SourcePath, false,
				fanOutOptions{targets: requested, originAgent: &origin, flushLarge: opts.flushLarge})
		}
		if opts.holdRestoreGate {
			o.nativeRestoreGate.RUnlock()
		}
		n++
	}
	return result, n
}

// ErrForcedBackfillRunning reports that a forced conversation backfill is
// already in flight; only one runs at a time so two full-history passes can't
// interleave their materializations.
var ErrForcedBackfillRunning = errors.New("syncd: a forced conversation backfill is already running")

// ForcedConversationBackfillPlan computes, without materializing anything,
// what a forced LOCAL conversation backfill would do: which enabled agents it
// reaches and how many conversations each would receive at the given depth
// (negative = full history). agents narrows the target set; empty = every
// enabled conversation-capable agent. Routing-rule allow/deny still applies.
func (o *Orchestrator) ForcedConversationBackfillPlan(agents []string, depth int) (ForcedBackfillResult, error) {
	opts, err := forcedBackfillOpts(agents, depth, false)
	if err != nil {
		return ForcedBackfillResult{}, err
	}
	result, _ := o.backfillConversationsRun(context.Background(), opts)
	if err := validateForcedBackfillTargets(agents, result.Targets); err != nil {
		return result, err
	}
	return result, nil
}

// StartForcedConversationBackfill plans a forced LOCAL conversation backfill,
// then materializes it on a background goroutine joined by Close. The
// returned plan is what the pass will attempt (eligibility counts, not
// pending-write counts — the planner does not consult materialization state,
// so an already-backfilled store re-plans the same numbers); individual
// materializations may still defer through the ordinary retry queue.
//
// The pass ITSELF is local: its fan-out writes native agent files and
// publishes nothing. Its side effects flow through ordinary sync like any
// other local activity — in particular, when a freshly materialized copy is
// re-imported and reveals a provable legacy corruption in the canonical head,
// the resulting repair event replicates to peers exactly as a hand-authored
// repair would. That replicates corrections to conversations peers already
// hold; it never ships history a peer lacks.
func (o *Orchestrator) StartForcedConversationBackfill(agents []string, depth int) (ForcedBackfillResult, error) {
	opts, err := forcedBackfillOpts(agents, depth, true)
	if err != nil {
		return ForcedBackfillResult{}, err
	}
	if !o.forcedBackfillActive.CompareAndSwap(false, true) {
		return ForcedBackfillResult{}, ErrForcedBackfillRunning
	}
	planOpts := opts
	planOpts.apply = false
	plan, _ := o.backfillConversationsRun(context.Background(), planOpts)
	if err := validateForcedBackfillTargets(agents, plan.Targets); err != nil {
		o.forcedBackfillActive.Store(false)
		return plan, err
	}
	if !o.beginBackground() {
		o.forcedBackfillActive.Store(false)
		return plan, errors.New("syncd: orchestrator is shutting down")
	}
	// The returned plan describes the run being started, not a dry run.
	plan.DryRun = false
	go func() {
		defer o.endBackground()
		defer o.forcedBackfillActive.Store(false)
		result, _ := o.backfillConversationsRun(context.Background(), opts)
		o.publishEvent("backfill.forced_completed", map[string]any{
			"conversations": result.Conversations,
			"perAgent":      result.PerAgent,
			"depth":         depth,
			"truncated":     result.Truncated,
		})
	}()
	return plan, nil
}

// validateForcedBackfillTargets rejects a request whose target set is empty or
// whose --agent filter names an agent that is not an enabled
// conversation-capable target, so a typo alongside a valid name fails loudly
// instead of being silently ignored.
func validateForcedBackfillTargets(requested, resolved []string) error {
	if len(resolved) == 0 {
		return errors.New("syncd: no enabled conversation-capable agent matches the request")
	}
	have := make(map[string]struct{}, len(resolved))
	for _, name := range resolved {
		have[name] = struct{}{}
	}
	for _, name := range requested {
		if _, ok := have[name]; !ok {
			return fmt.Errorf("syncd: agent %q is not an enabled conversation-capable target", name)
		}
	}
	return nil
}

// forcedBackfillOpts validates and assembles the options for a forced pass.
func forcedBackfillOpts(agents []string, depth int, apply bool) (backfillRunOpts, error) {
	if depth == 0 {
		return backfillRunOpts{}, errors.New("syncd: backfill depth 0 plans nothing; use a positive depth or -1 for full history")
	}
	var only map[string]struct{}
	if len(agents) > 0 {
		only = make(map[string]struct{}, len(agents))
		for _, name := range agents {
			only[name] = struct{}{}
		}
	}
	return backfillRunOpts{
		only:            only,
		depthOverride:   &depth,
		apply:           apply,
		flushLarge:      true,
		holdRestoreGate: true,
	}, nil
}

// backfillPrimary resolves the source ("primary") adapter for an artifact from
// its most-recent event's provenance, mirroring RefanOutAll's resolution so
// fanOut skips the author when iterating destinations.
func (o *Orchestrator) backfillPrimary(art acf.Artifact) (adapter.Adapter, string) {
	var primary adapter.Adapter
	srcName := ""
	// Native imports retain their source path on the artifact. A unique
	// configured path owner is durable local evidence of authorship and avoids
	// decoding the artifact's entire append history merely to recover the same
	// adapter name. This matters for long conversations whose superseded full
	// payload events can total multiple gigabytes.
	if art.SourcePath != "" {
		owners := o.pathOwners(art.SourcePath)
		if len(owners) == 1 {
			for name := range owners {
				srcName = name
			}
		}
	}
	if srcName == "" {
		srcName = art.RemoteSourceAgent
	}
	if srcName == "" {
		// Remote shells have no native source path. Their current append-order
		// head normally carries provenance, so inspect only that tail event.
		if head, ok, err := o.cfg.Store.LastEvent(art.Kind, art.ArtifactID); err == nil && ok {
			srcName = head.Provenance.SourceAgent
		}
	}
	// Do not fall back to replaying compacted history. Old shells without the
	// durable fields above remain unknown and fail closed for same-source late
	// activation; their next authenticated inbound event fills the fields.
	if srcName != "" {
		for _, ad := range o.cfg.Adapters {
			if ad.Name() == srcName {
				primary = ad
				break
			}
		}
	}
	if primary == nil && len(o.cfg.Adapters) > 0 {
		primary = o.cfg.Adapters[0]
	}
	return primary, srcName
}
