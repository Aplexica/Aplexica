// Package syncrules implements the BRD-05 selective-sync rule engine:
// a TOML-defined declarative ruleset that the orchestrator consults at
// fan-out time to decide which artifacts may flow to which agents.
//
// Rule shape mirrors BRD-05 §5.1. Evaluation is deterministic and
// additive across matching rules (§5.6).
package syncrules

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/BurntSushi/toml"
)

// MatchKindAll / MatchKindAny are the two combinator modes for
// MatchSpec. Default is "all".
const (
	MatchKindAll = "all"
	MatchKindAny = "any"
)

// Reserved skill modes (BRD-05 §5.4.1 / FR-05.16).
const (
	SkillModeLossy  = "lossy"
	SkillModeStrict = "strict"
)

// Reserved sync modes (BRD-05 §5.4).
const (
	ModeLive      = "live"
	ModeScheduled = "scheduled"
	ModeManual    = "manual"
)

// DefaultScheduledIntervalSeconds is the default cadence for a rule with
// mode="scheduled" when no explicit per-rule cadence is set.
const DefaultScheduledIntervalSeconds = 15 * 60

// AgentTokenOriginating is the special token used by the
// "fork-respects-origin" default rule (BRD-05 §6 #2).
const AgentTokenOriginating = "__originatingAgent__"

// MatchSpec is the §5.1 match block.
type MatchSpec struct {
	Kind           string   `toml:"kind" json:"kind,omitempty"`
	Tag            []string `toml:"tag" json:"tag,omitempty"`
	Type           []string `toml:"type" json:"type,omitempty"`
	ToolKind       []string `toml:"toolKind" json:"toolKind,omitempty"`
	ToolCapability []string `toml:"toolCapability" json:"toolCapability,omitempty"`
	AgentSource    []string `toml:"agentSource" json:"agentSource,omitempty"`
	DeviceSource   []string `toml:"deviceSource" json:"deviceSource,omitempty"`
	Size           string   `toml:"size" json:"size,omitempty"`
	Path           string   `toml:"path" json:"path,omitempty"`
	// BranchName matches a conversation's head branch as a REGEX (BRD-05 §5.2).
	// The pattern is compiled+validated at rule load (New/Validate/Parse reject
	// an un-compilable pattern) and evaluated with regexp.MatchString in
	// matches(); the orchestrator's ruleInputFor populates Artifact.BranchName
	// from the artifact's normalized head branch, so the predicate is live in
	// both the local fan-out and outbound-remote paths. A rule with no
	// branchName carries no branch condition (deny-by-default is unaffected).
	BranchName string `toml:"branchName" json:"branchName,omitempty"`

	// Scope is the nested match.scope.{kind, project: {id, ephemeral,
	// namespace}}. TOML decoder uses the nested table.
	Scope MatchScopeSpec `toml:"scope" json:"scope"`
}

// MatchScopeSpec is the nested match.scope.* block.
type MatchScopeSpec struct {
	Kind      []string              `toml:"kind" json:"kind,omitempty"`
	Project   MatchScopeProjectSpec `toml:"project" json:"project"`
	Namespace []string              `toml:"namespace" json:"namespace,omitempty"`
}

// MatchScopeProjectSpec is the nested match.scope.project.* block.
type MatchScopeProjectSpec struct {
	ID        []string `toml:"id" json:"id,omitempty"`
	Ephemeral *bool    `toml:"ephemeral" json:"ephemeral,omitempty"`
}

// RouteSpec is the §5.3 route block. Agents is the union of positive
// and negative ("!name") patterns. Remote=="exclude" blocks any remote
// transport. SkillMode is FR-05.16. IncludeSecrets is a documented
// per-rule override over the secrets-handling layer's default (the
// `tool-secrets-default-local` default rule sets this to false; the
// security layer enforces the actual gate per BRD-05 §6).
type RouteSpec struct {
	Agents         []string `toml:"agents" json:"agents,omitempty"`
	Remote         string   `toml:"remote" json:"remote,omitempty"`
	SkillMode      string   `toml:"skillMode" json:"skillMode,omitempty"`
	IncludeSecrets *bool    `toml:"includeSecrets" json:"includeSecrets,omitempty"`

	// Devices scopes a rule to specific paired devices in the cloud
	// deployment (BRD-05 cloud §4.2/§5.4): omitted or ["*"] = all
	// devices; ["<id>", …] = only those devices. It is a CLOUD-stage
	// predicate resolved by the relay / cloud plugin against the
	// receiving device. The LOCAL engine deliberately IGNORES it (a
	// single-device install is always in scope); it is stored, preserved
	// on Parse round-trip, and surfaced verbatim through the rules API.
	Devices []string `toml:"devices" json:"devices,omitempty"`

	// HistoricalSyncDepth caps, per target agent, how many of the most-recent
	// conversations are back-filled into that agent when a rule first routes to
	// it (the "historical sync depth"). Keyed by agent name (the same tokens as
	// route.agents). -1 = all history; 0 = none; an agent omitted here falls back
	// to the global sync.convBackfill setting, then DefaultConvBackfill (10).
	// Backfill-only — live (going-forward) fan-out is never capped. BRD-05 §5.4 /
	// FR-05.17.
	HistoricalSyncDepth map[string]int `toml:"historicalSyncDepth" json:"historicalSyncDepth,omitempty"`
}

// AssignSpec is the §5.5 assign block — tag-assigning rules.
type AssignSpec struct {
	Tags []string `toml:"tags" json:"tags,omitempty"`
}

// Rule is one §5.1 rule.
type Rule struct {
	Name                     string     `toml:"name"`
	Match                    MatchSpec  `toml:"match"`
	Route                    RouteSpec  `toml:"route"`
	Assign                   AssignSpec `toml:"assign"`
	Mode                     string     `toml:"mode"`
	ScheduledIntervalSeconds int        `toml:"scheduledIntervalSeconds,omitempty" json:"ScheduledIntervalSeconds,omitempty"`

	// Enabled is the per-rule on/off toggle (BRD-05 §6.5). A pointer so
	// the absence of the key (nil) means "enabled" — this preserves
	// back-compat with rules written before the field existed and with
	// the shipped defaults, which omit it. When Enabled != nil && !*Enabled
	// the rule is INACTIVE: the evaluator early-continues it so it
	// contributes no allow/deny/tags/mode (see evaluate). A portal PATCH
	// {"enabled":false} disables a rule without deleting it. The TOML
	// round-trip (writeUserRules) preserves it via the toml tag; omitempty
	// on the JSON tag keeps the wire form clean when nil.
	Enabled *bool `toml:"enabled" json:"enabled,omitempty"`

	// Source is a display-only provenance tag the daemon's rules API sets
	// when listing rules: "local" for the user's rules.toml, "cloud" for a
	// rule pushed down from the cloud account. It is NEVER persisted to
	// TOML (toml:"-") and is ignored by the evaluator + Validate; it only
	// rides the JSON the local portal renders so cloud rules can be shown
	// read-only. Empty for rules that haven't been tagged. The other Rule
	// fields carry no json tag (so they marshal PascalCase: Name, Match,
	// …); keep Source PascalCase too for a consistent wire shape.
	Source string `toml:"-" json:"Source,omitempty"`
}

// Config holds the [[sync.rules]] table — a list of rules in
// declaration order. Used as the layer between toml.Decode and the
// evaluator.
type Config struct {
	Sync SyncSection `toml:"sync"`
}

// SyncSection is the `[[sync.rules]]` container.
type SyncSection struct {
	Rules []Rule `toml:"rules"`
}

// Parse reads a TOML byte slice and returns the parsed Config plus any
// validation errors. Each rule is validated independently after the
// overall TOML parses.
func Parse(data []byte) (Config, error) {
	var cfg Config
	md, err := toml.Decode(string(data), &cfg)
	if err != nil {
		return cfg, fmt.Errorf("syncrules: parse: %w", err)
	}
	if err := Validate(cfg.Sync.Rules); err != nil {
		return cfg, err
	}
	// Reject unrecognized keys. toml.Decode silently ignores keys with no
	// destination field, so a misspelled match key (e.g. match.tags for the
	// documented match.tag) would otherwise drop to nothing — turning a
	// narrowly-scoped rule into a no-predicate, all-artifacts fan-out. Fail
	// loudly so the rule never silently contradicts what the user wrote
	// (FR-05.6 determinism/explainability). Undecoded() lists keys in
	// document order.
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, len(undecoded))
		for i, k := range undecoded {
			keys[i] = k.String()
		}
		return cfg, fmt.Errorf("syncrules: parse: unrecognized key(s): %s", strings.Join(keys, ", "))
	}
	return cfg, nil
}

// Validate checks each rule for shape errors (FR-05.14 contradictory
// patterns, duplicate names, invalid enums). Returns the first error
// found.
func Validate(rules []Rule) error {
	seen := map[string]int{}
	for i, r := range rules {
		if r.Name == "" {
			return fmt.Errorf("syncrules: rule[%d] missing name", i)
		}
		if prior, ok := seen[r.Name]; ok {
			return fmt.Errorf("syncrules: duplicate rule name %q at index %d (first at %d)", r.Name, i, prior)
		}
		seen[r.Name] = i
		// FR-05.14: positive vs. negative pattern contradictions inside a
		// single rule.
		if err := validateAgentsPatterns(r.Route.Agents); err != nil {
			return fmt.Errorf("syncrules: rule %q: %w", r.Name, err)
		}
		switch r.Match.Kind {
		case "", MatchKindAll, MatchKindAny:
		default:
			return fmt.Errorf("syncrules: rule %q: invalid match.kind %q (allowed: \"all\", \"any\")", r.Name, r.Match.Kind)
		}
		switch r.Mode {
		case "", ModeLive, ModeScheduled, ModeManual:
		default:
			return fmt.Errorf("syncrules: rule %q: invalid mode %q (allowed: \"live\", \"scheduled\", \"manual\")", r.Name, r.Mode)
		}
		if r.ScheduledIntervalSeconds < 0 {
			return fmt.Errorf("syncrules: rule %q: scheduledIntervalSeconds must be >= 0", r.Name)
		}
		switch r.Route.SkillMode {
		case "", SkillModeLossy, SkillModeStrict:
		default:
			return fmt.Errorf("syncrules: rule %q: invalid route.skillMode %q (allowed: \"lossy\", \"strict\")", r.Name, r.Route.SkillMode)
		}
		if r.Route.Remote != "" && r.Route.Remote != "exclude" {
			return fmt.Errorf("syncrules: rule %q: invalid route.remote %q (only \"exclude\" supported in V1)", r.Name, r.Route.Remote)
		}
		// match.size is documented by BRD-05 §5.2 but is not yet evaluated by
		// matches() (and end-to-end support also needs the orchestrator to
		// populate Artifact.SizeBytes and fold it into evaluateCacheKey). A
		// rule carrying match.size would otherwise carry NO size condition and
		// silently match every artifact — the opposite of the safe restrictive
		// intent. Reject it loudly at config-load rather than accept a no-op.
		if r.Match.Size != "" {
			return fmt.Errorf("syncrules: rule %q: match.size not supported in V1", r.Name)
		}
		// match.branchName is a regex (BRD-05 §5.2). Reject an un-compilable
		// pattern at load time rather than let it silently never match (which
		// would be the opposite of the rule writer's intent — a branch-scoped
		// rule that fans out to nobody). Compiled once for real use in New.
		if _, err := compileBranchName(r.Match.BranchName); err != nil {
			return fmt.Errorf("syncrules: rule %q: %w", r.Name, err)
		}
	}
	return nil
}

// compileBranchName compiles a match.branchName pattern into a regexp. An empty
// pattern yields a nil regexp and no error — it carries no branch predicate.
// A non-empty pattern that does not compile is an error (named clearly so
// Validate can surface which rule is at fault).
func compileBranchName(pattern string) (*regexp.Regexp, error) {
	if pattern == "" {
		return nil, nil
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("invalid match.branchName regex %q: %w", pattern, err)
	}
	return re, nil
}

// validateAgentsPatterns enforces FR-05.14: a single rule cannot
// contradict itself by listing both "X" and "!X". Empty list is fine.
func validateAgentsPatterns(agents []string) error {
	if len(agents) == 0 {
		return nil
	}
	positives, negatives := SplitAgentPatterns(agents)
	posSet := map[string]struct{}{}
	for _, p := range positives {
		posSet[p] = struct{}{}
	}
	for _, n := range negatives {
		if _, ok := posSet[n]; ok {
			return fmt.Errorf("contradictory pattern: route.agents contains both %q and %q", n, "!"+n)
		}
	}
	return nil
}

func effectiveScheduledIntervalSeconds(r Rule) int {
	if r.ScheduledIntervalSeconds > 0 {
		return r.ScheduledIntervalSeconds
	}
	return DefaultScheduledIntervalSeconds
}

// SplitAgentPatterns separates a route.agents list into positive
// patterns and negative ("!name") patterns. Returns (positives,
// negatives). The leading "!" is stripped on negatives.
func SplitAgentPatterns(agents []string) (positives, negatives []string) {
	for _, a := range agents {
		if strings.HasPrefix(a, "!") {
			negatives = append(negatives, strings.TrimPrefix(a, "!"))
		} else {
			positives = append(positives, a)
		}
	}
	return positives, negatives
}

// Artifact is the minimal projection of acf.Artifact + event-derived
// metadata that the rule evaluator needs. Keeping this separate from
// acf.Artifact avoids an import cycle (acf imports nothing from
// syncrules; the orchestrator builds an Artifact at fan-out time).
type Artifact struct {
	ArtifactID       string
	Kind             string // "memory" | "skill" | "tool" | "conversation"
	Tags             []string
	Type             string // mirrors Kind for match.type predicates
	ToolKind         string // only meaningful when Kind=="tool"
	ToolCapabilities []string
	ScopeKind        string // "global" | "project" | "namespace"
	ProjectID        string
	ProjectEphemeral bool
	Namespace        string
	OriginAgent      string
	OriginDevice     string
	SizeBytes        int64
	NativePath       string
	BranchName       string
}

// Decision is the per-artifact resolved routing decision (FR-05.6).
// AllowedAgents is the set of installed-and-enabled agent names the
// orchestrator may fan-out to AFTER all rules have been merged.
// DeniedAgents records every adapter excluded by a negative pattern in
// any matching rule. AssignedTags is the union of tags contributed by
// tag-assigning rules. RemoteAllowed reflects route.remote="exclude"
// across matching rules. Mode is the highest-precedence mode wins.
// SkillMode is the highest-precedence skillMode if any matched rule
// specified one ("strict" beats "lossy").
type Decision struct {
	AllowedAgents            []string
	DeniedAgents             []string
	AssignedTags             []string
	RemoteAllowed            bool
	Mode                     string
	ScheduledIntervalSeconds int
	SkillMode                string
	MatchedRules             []string // names of rules that contributed
	// IncludeSecrets is advisory / explainability-only: it surfaces the
	// resolved route.includeSecrets (last-non-nil-wins) so `rules test` can
	// explain it, but nothing routes off it — secret-VALUE gating is enforced
	// independently by the secrets/security layer (BRD-05 §6 #4, ADR-0042). Do
	// not wire routing decisions off this field.
	IncludeSecrets *bool

	// HistoricalSyncDepth is the resolved per-agent conversation-backfill depth
	// from matching rules' route.historicalSyncDepth (last contribution wins per
	// agent, rule order). Consumed by the conversation backfill cap; an agent
	// absent here falls back to the global sync.convBackfill / DefaultConvBackfill.
	// nil when no matching rule set it.
	HistoricalSyncDepth map[string]int
}

// Engine owns a parsed ruleset and exposes Evaluate.
//
// An Engine is immutable per rules-set: the orchestrator swaps it
// wholesale on every rule Add/Update/Delete, so the memoization cache
// below needs no explicit invalidation — a new ruleset is a new Engine
// with an empty cache, and a different artifact tag set yields a
// different cache key (tags are part of the key). The mutex guards the
// cache map only; the rules slice is never mutated after New.
type Engine struct {
	rules []Rule
	// branchRE holds the compiled match.branchName regex for each rule,
	// parallel to rules (branchRE[i] corresponds to rules[i]). A nil entry
	// means the rule has no branchName predicate. Compiled once in New so the
	// per-artifact evaluate hot path never recompiles; immutable thereafter
	// (a new ruleset is a new Engine, like rules itself).
	branchRE []*regexp.Regexp

	mu    sync.RWMutex
	cache map[string]Decision
}

// cacheMaxEntries bounds the per-Engine memoization cache. At M-scale
// artifact counts a plain map is fine; this cap is a memory backstop —
// when exceeded the cache is cleared and repopulated lazily. It is a
// structural limit, not a tunable behavioural knob.
const cacheMaxEntries = 10000

// New constructs an Engine. Validate(rules) is run again as a guard;
// returns the first validation error.
func New(rules []Rule) (*Engine, error) {
	if err := Validate(rules); err != nil {
		return nil, err
	}
	copied := append([]Rule(nil), rules...)
	// Compile each rule's match.branchName ONCE here (Validate already proved
	// they compile, so this cannot error) and keep the *regexp.Regexp parallel
	// to the rules slice. matches() consults branchRE[i] instead of recompiling
	// per artifact on the fan-out hot path.
	branchRE := make([]*regexp.Regexp, len(copied))
	for i := range copied {
		re, err := compileBranchName(copied[i].Match.BranchName)
		if err != nil {
			return nil, err
		}
		branchRE[i] = re
	}
	return &Engine{
		rules:    copied,
		branchRE: branchRE,
		cache:    map[string]Decision{},
	}, nil
}

// Rules returns a defensive copy of the configured rules.
func (e *Engine) Rules() []Rule {
	out := make([]Rule, len(e.rules))
	copy(out, e.rules)
	return out
}

// EvaluateOpts captures per-evaluation context the orchestrator
// supplies (installed agents, current device id, etc.).
type EvaluateOpts struct {
	InstalledAgents []string // universe of agents the orchestrator knows about
}

// Evaluate runs every rule against the artifact and returns the merged
// Decision. Matching rules contribute positives + negatives, tags,
// mode, and skillMode per §5.6.
//
// FR-05.6: the decision MUST be deterministic and explainable. The
// returned Decision.MatchedRules lists every rule that matched, in
// declaration order; the orchestrator and the `rules test` command
// both consume this.
func (e *Engine) Evaluate(a Artifact, opts EvaluateOpts) Decision {
	key := evaluateCacheKey(a, opts)
	e.mu.RLock()
	cached, ok := e.cache[key]
	e.mu.RUnlock()
	if ok {
		return cloneDecision(cached)
	}

	dec := e.evaluate(a, opts)

	e.mu.Lock()
	if len(e.cache) >= cacheMaxEntries {
		// Memory backstop: discard the whole cache rather than evict
		// individually. A new ruleset is a new Engine, so this only fires
		// under a pathological artifact-key explosion within one ruleset.
		e.cache = map[string]Decision{}
	}
	e.cache[key] = cloneDecision(dec)
	e.mu.Unlock()
	return cloneDecision(dec)
}

// evaluate runs the uncached rule merge. Evaluate wraps it with the
// memoization cache.
func (e *Engine) evaluate(a Artifact, opts EvaluateOpts) Decision {
	var dec Decision
	dec.RemoteAllowed = true
	dec.SkillMode = SkillModeLossy
	allowedSet := map[string]struct{}{}
	deniedSet := map[string]struct{}{}
	tagSet := map[string]struct{}{}
	for _, t := range a.Tags {
		tagSet[t] = struct{}{}
	}
	universe := opts.InstalledAgents
	var hasPositive bool
	for i := range e.rules {
		r := e.rules[i]
		// A disabled rule (Enabled != nil && !*Enabled) is inactive: it
		// contributes nothing to the decision (BRD-05 §6.5). nil means
		// enabled, preserving back-compat with rules + defaults that omit
		// the field.
		if r.Enabled != nil && !*r.Enabled {
			continue
		}
		if !matches(r.Match, e.branchRE[i], a) {
			continue
		}
		dec.MatchedRules = append(dec.MatchedRules, r.Name)

		// Routing — accumulate positives / negatives.
		pos, neg := SplitAgentPatterns(r.Route.Agents)
		if len(pos) == 0 && len(neg) == 0 {
			// No route.agents specified: implies the universe.
			for _, name := range universe {
				if matchPattern("*", name) {
					allowedSet[name] = struct{}{}
				}
			}
			hasPositive = true
		} else {
			if len(pos) > 0 {
				hasPositive = true
				for _, p := range pos {
					expanded := expandAgentToken(p, a, universe)
					for _, e := range expanded {
						for _, name := range universe {
							if matchPattern(e, name) {
								allowedSet[name] = struct{}{}
							}
						}
					}
				}
			} else {
				// Negative-only — universe is implied.
				hasPositive = true
				for _, name := range universe {
					allowedSet[name] = struct{}{}
				}
			}
			for _, n := range neg {
				expanded := expandAgentToken(n, a, universe)
				for _, e := range expanded {
					for _, name := range universe {
						if matchPattern(e, name) {
							deniedSet[name] = struct{}{}
						}
					}
				}
			}
		}

		// Tags assigned by this rule (§5.5).
		for _, t := range r.Assign.Tags {
			tagSet[t] = struct{}{}
		}

		// route.remote="exclude" — any matching rule excludes remote.
		if r.Route.Remote == "exclude" {
			dec.RemoteAllowed = false
		}

		// Mode precedence (§5.6 #3): live > scheduled > manual. If
		// scheduled wins, carry the effective per-rule cadence forward for
		// transport plugins; if live later wins, clear the cadence because it
		// no longer applies.
		dec.Mode = pickMode(dec.Mode, r.Mode)
		if dec.Mode == ModeScheduled && r.Mode == ModeScheduled {
			dec.ScheduledIntervalSeconds = effectiveScheduledIntervalSeconds(r)
		}
		if dec.Mode != ModeScheduled {
			dec.ScheduledIntervalSeconds = 0
		}

		// Skill mode — strict wins over lossy across any matching rule.
		if r.Route.SkillMode == SkillModeStrict {
			dec.SkillMode = SkillModeStrict
		}

		// IncludeSecrets — last non-nil contribution wins (rule order).
		if r.Route.IncludeSecrets != nil {
			v := *r.Route.IncludeSecrets
			dec.IncludeSecrets = &v
		}

		// HistoricalSyncDepth — per-agent backfill cap; last contribution wins
		// per agent (rule order).
		for agent, depth := range r.Route.HistoricalSyncDepth {
			if dec.HistoricalSyncDepth == nil {
				dec.HistoricalSyncDepth = map[string]int{}
			}
			dec.HistoricalSyncDepth[agent] = depth
		}
	}
	if !hasPositive {
		// No rule matched at all → "all-to-all" default per §6 #1 is
		// expected to be the catch-all; if even that didn't match (e.g.
		// the user replaced defaults), the artifact goes nowhere.
		dec.AllowedAgents = nil
		dec.DeniedAgents = mapKeys(deniedSet)
		dec.AssignedTags = setToSorted(tagSet)
		if dec.Mode == "" {
			dec.Mode = ModeLive
		}
		return dec
	}
	for name := range deniedSet {
		delete(allowedSet, name)
	}
	dec.AllowedAgents = mapKeys(allowedSet)
	dec.DeniedAgents = mapKeys(deniedSet)
	dec.AssignedTags = setToSorted(tagSet)
	if dec.Mode == "" {
		dec.Mode = ModeLive
	}
	return dec
}

// evaluateCacheKey builds a deterministic memoization key from the
// matching-relevant Artifact fields plus the installed-agents universe.
// Slice-valued inputs (Tags, ToolCapabilities, InstalledAgents) are
// sorted into copies first so ordering differences never produce
// distinct keys. Fields that do not influence matching or routing
// (e.g. SizeBytes, NativePath beyond Path predicates) are still
// included where they feed a predicate; ProjectEphemeral is folded in
// because match.scope.project.ephemeral consults it.
func evaluateCacheKey(a Artifact, opts EvaluateOpts) string {
	tags := append([]string(nil), a.Tags...)
	sort.Strings(tags)
	caps := append([]string(nil), a.ToolCapabilities...)
	sort.Strings(caps)
	agents := append([]string(nil), opts.InstalledAgents...)
	sort.Strings(agents)

	// Use record + unit separators that cannot appear in the joined
	// field values to avoid cross-field collisions.
	const (
		fieldSep = "\x1f"
		listSep  = "\x1e"
	)
	parts := []string{
		a.ArtifactID,
		a.Kind,
		a.Type,
		a.ToolKind,
		a.ScopeKind,
		a.ProjectID,
		fmt.Sprintf("%t", a.ProjectEphemeral),
		a.Namespace,
		a.OriginAgent,
		a.OriginDevice,
		a.BranchName,
		a.NativePath,
		strings.Join(tags, listSep),
		strings.Join(caps, listSep),
		strings.Join(agents, listSep),
	}
	return strings.Join(parts, fieldSep)
}

// cloneDecision deep-copies the slice fields of a Decision so the cache
// and its callers never alias the same backing arrays. IncludeSecrets
// is a *bool that the evaluator only ever rebinds to a fresh value, so
// a shallow pointer copy is safe.
func cloneDecision(d Decision) Decision {
	out := d
	out.AllowedAgents = append([]string(nil), d.AllowedAgents...)
	out.DeniedAgents = append([]string(nil), d.DeniedAgents...)
	out.AssignedTags = append([]string(nil), d.AssignedTags...)
	out.MatchedRules = append([]string(nil), d.MatchedRules...)
	if d.HistoricalSyncDepth != nil {
		out.HistoricalSyncDepth = make(map[string]int, len(d.HistoricalSyncDepth))
		for k, v := range d.HistoricalSyncDepth {
			out.HistoricalSyncDepth[k] = v
		}
	}
	return out
}

// pickMode returns the higher-precedence mode of cur and next per
// §5.6 #3: live > scheduled > manual.
func pickMode(cur, next string) string {
	if next == "" {
		return cur
	}
	if cur == "" {
		return next
	}
	// Mode precedence ranks per BRD-05 §5.6 #3. These are spec constants,
	// not tunables — they capture the rule "live > scheduled > manual".
	const (
		rankLive      = 3
		rankScheduled = 2
		rankManual    = 1
	)
	rank := map[string]int{
		ModeLive: rankLive, ModeScheduled: rankScheduled, ModeManual: rankManual,
	}
	if rank[next] > rank[cur] {
		return next
	}
	return cur
}

// expandAgentToken resolves special tokens like
// "__originatingAgent__" (BRD-05 §6 #2) to concrete agent names; bare
// names pass through unchanged.
func expandAgentToken(token string, a Artifact, universe []string) []string {
	if token == AgentTokenOriginating {
		if a.OriginAgent != "" {
			return []string{a.OriginAgent}
		}
		// No origin known — falls through to no expansion (nothing
		// added to allowed set).
		return nil
	}
	return []string{token}
}

// matchPattern compares pattern against name. Pattern "*" matches
// every name; bare names match by exact equality. Glob patterns may
// be added in V2; V1 supports "*" and exact only.
func matchPattern(pattern, name string) bool {
	if pattern == "*" {
		return true
	}
	if strings.ContainsAny(pattern, "*?[]") {
		// Best-effort glob via path.Match for forward-compat use cases
		// (e.g. "claude-*" if rule writers experiment); silent on
		// errors.
		matched, err := path.Match(pattern, name)
		if err == nil && matched {
			return true
		}
	}
	return pattern == name
}

// matches reports whether artifact a satisfies the rule's match block. branchRE
// is the rule's pre-compiled match.branchName regex (nil when the rule has no
// branchName predicate) — see Engine.branchRE.
func matches(m MatchSpec, branchRE *regexp.Regexp, a Artifact) bool {
	kind := m.Kind
	if kind == "" {
		kind = MatchKindAll
	}
	require := []func() (have, want bool){}
	if len(m.Tag) > 0 {
		require = append(require, func() (have, want bool) {
			return anyTagMatches(a.Tags, m.Tag), true
		})
	}
	if len(m.Type) > 0 {
		require = append(require, func() (have, want bool) {
			return contains(m.Type, a.Type), true
		})
	}
	if len(m.ToolKind) > 0 {
		require = append(require, func() (have, want bool) {
			if a.Type != "tool" {
				return false, true
			}
			return contains(m.ToolKind, a.ToolKind), true
		})
	}
	if len(m.ToolCapability) > 0 {
		require = append(require, func() (have, want bool) {
			if a.Type != "tool" {
				return false, true
			}
			for _, c := range m.ToolCapability {
				if contains(a.ToolCapabilities, c) {
					return true, true
				}
			}
			return false, true
		})
	}
	if len(m.Scope.Kind) > 0 {
		require = append(require, func() (have, want bool) {
			return contains(m.Scope.Kind, a.ScopeKind), true
		})
	}
	if len(m.Scope.Project.ID) > 0 {
		require = append(require, func() (have, want bool) {
			for _, p := range m.Scope.Project.ID {
				if globMatch(p, a.ProjectID) {
					return true, true
				}
			}
			return false, true
		})
	}
	if m.Scope.Project.Ephemeral != nil {
		require = append(require, func() (have, want bool) {
			return *m.Scope.Project.Ephemeral == a.ProjectEphemeral, true
		})
	}
	if len(m.Scope.Namespace) > 0 {
		require = append(require, func() (have, want bool) {
			return contains(m.Scope.Namespace, a.Namespace), true
		})
	}
	if len(m.AgentSource) > 0 {
		require = append(require, func() (have, want bool) {
			return contains(m.AgentSource, a.OriginAgent), true
		})
	}
	if len(m.DeviceSource) > 0 {
		require = append(require, func() (have, want bool) {
			return contains(m.DeviceSource, a.OriginDevice), true
		})
	}
	if m.BranchName != "" && branchRE != nil {
		require = append(require, func() (have, want bool) {
			// BRD-05 §5.2: branchName is a regex over the artifact's head
			// branch (compiled once at rule load; see Engine.branchRE).
			return branchRE.MatchString(a.BranchName), true
		})
	}
	if m.Path != "" {
		require = append(require, func() (have, want bool) {
			return globMatch(m.Path, a.NativePath), true
		})
	}
	if len(require) == 0 {
		// Rule with no predicates matches every artifact.
		return true
	}
	if kind == MatchKindAny {
		for _, f := range require {
			have, _ := f()
			if have {
				return true
			}
		}
		return false
	}
	for _, f := range require {
		have, _ := f()
		if !have {
			return false
		}
	}
	return true
}

// anyTagMatches returns true if any tag in `have` matches any pattern
// in `want`. Patterns containing "*" use path.Match semantics.
func anyTagMatches(have, want []string) bool {
	for _, w := range want {
		for _, h := range have {
			if globMatch(w, h) {
				return true
			}
		}
	}
	return false
}

func globMatch(pattern, target string) bool {
	if pattern == target {
		return true
	}
	if !strings.ContainsAny(pattern, "*?[]") {
		return false
	}
	matched, err := path.Match(pattern, target)
	return err == nil && matched
}

func contains(slice []string, v string) bool {
	for _, s := range slice {
		if s == v {
			return true
		}
	}
	return false
}

func mapKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func setToSorted(m map[string]struct{}) []string {
	return mapKeys(m)
}

// DefaultRulesTOML returns the BRD-05 §6 "classic" rule set as a TOML
// string. These are NOT shipped as always-on defaults — per ADR-0042
// (rules safe-by-default) the running engine is built from the user's
// rules.toml only. They back the opt-in presets catalog: applying a preset
// writes the corresponding rule into the user's rules.toml.
func DefaultRulesTOML() string {
	return defaultRulesTOML
}

const defaultRulesTOML = `# BRD-05 §6 classic rules — opt-in presets catalog,
# NOT shipped as always-on defaults (see ADR-0042 rules-safe-by-default).
#
# These describe the "all artifacts to all installed agents" preset plus a
# handful of safety presets. They are NOT merged under the user's rules.toml;
# the running engine is built from the user file only. Applying a preset writes
# the corresponding rule into the user's rules.toml, after which
# ` + "`aplexica rules list`" + ` shows it (rules list returns user rules only).

# 1. Fan everything out to every installed and enabled agent.
[[sync.rules]]
name = "default-all-to-all"
match.kind = "any"
match.type = ["memory", "skill", "tool", "conversation"]
# no route.agents → all installed agents

# 2. Forks stay where they were forked.
[[sync.rules]]
name = "fork-respects-origin"
match.tag = ["fork-of:*"]
route.agents = ["__originatingAgent__"]

# 3. Reserved-tagged artifacts stay local — never propagated via any
#    remote transport plugin.
[[sync.rules]]
name = "private-stays-local"
match.tag = ["private", "secret"]
route.remote = "exclude"

# 4. Tool secret values stay local by default; the per-tool
#    syncSecrets flag is the actual gate (BRD-05 §6 / §9.4.4).
[[sync.rules]]
name = "tool-secrets-default-local"
match.type = ["tool"]
route.includeSecrets = false

# 5. Ephemeral projects stay on the originating agent by default.
[[sync.rules]]
name = "ephemeral-projects-stay-local"
match.scope.kind = ["project"]
match.scope.project.ephemeral = true
route.remote = "exclude"
`

// ParseDefault returns the parsed BRD-05 §6 default ruleset. Useful
// for tests + the orchestrator's bootstrap path.
func ParseDefault() (Config, error) {
	cfg, err := Parse([]byte(defaultRulesTOML))
	if err != nil {
		return cfg, err
	}
	return cfg, nil
}

// helper: defensive accessor to avoid lint warnings.
var _ = errors.New
