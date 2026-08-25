package syncd

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/syncrules"
	"github.com/stretchr/testify/require"
)

// TestOrchestrator_RulesGateFanout proves the v0.104.0 rule engine
// skips fan-out to adapters NOT in Decision.AllowedAgents. With a
// single rule that routes memory→["claude-code"] only, writing
// CLAUDE.md must produce zero AGENTS.md fan-out on codex / kilo.
func TestOrchestrator_RulesGateFanout(t *testing.T) {
	root := realTempDir(t)
	watched := filepath.Join(root, "proj")
	require.NoError(t, os.MkdirAll(watched, 0o755))

	adapters, store, _ := buildAllThreeAdapters(t, root)

	eng, err := syncrules.New([]syncrules.Rule{{
		Name:  "claude-only",
		Match: syncrules.MatchSpec{Kind: syncrules.MatchKindAny, Type: []string{"memory"}},
		Route: syncrules.RouteSpec{Agents: []string{"claude-code"}},
	}})
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	orch, err := NewOrchestrator(Config{
		Dir:         watched,
		Adapters:    adapters,
		Store:       store,
		QuietPeriod: 100 * time.Millisecond,
		GuardWindow: 2 * time.Second,
		RulesEngine: eng,
	})
	require.NoError(t, err)
	defer orch.Close()
	go orch.Run(ctx)
	time.Sleep(150 * time.Millisecond)

	require.NoError(t, os.WriteFile(filepath.Join(watched, "CLAUDE.md"),
		[]byte("# claude-only\n"), 0o644))

	time.Sleep(1500 * time.Millisecond)

	// claudecode's CLAUDE.md exists; codex/kilo's AGENTS.md must NOT.
	_, err = os.Stat(filepath.Join(watched, "AGENTS.md"))
	require.True(t, os.IsNotExist(err),
		"AGENTS.md should NOT exist because rule restricted to claude-code")

	memories, err := store.ListArtifacts(acf.KindMemory)
	require.NoError(t, err)
	require.Len(t, memories, 1, "inbound import still runs")
}

// TestOrchestrator_RulesAssignTags proves tag-assigning rules
// (BRD-05 §5.5) persist their tags onto the artifact at fan-out
// time.
func TestOrchestrator_RulesAssignTags(t *testing.T) {
	root := realTempDir(t)
	watched := filepath.Join(root, "proj")
	require.NoError(t, os.MkdirAll(watched, 0o755))

	adapters, store, _ := buildAllThreeAdapters(t, root)

	eng, err := syncrules.New([]syncrules.Rule{
		{
			Name:  "default-all",
			Match: syncrules.MatchSpec{Kind: syncrules.MatchKindAny, Type: []string{"memory"}},
		},
		{
			Name:   "auto-tag-work",
			Match:  syncrules.MatchSpec{AgentSource: []string{"claude-code"}, Type: []string{"memory"}},
			Assign: syncrules.AssignSpec{Tags: []string{"work"}},
		},
	})
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	orch, err := NewOrchestrator(Config{
		Dir:         watched,
		Adapters:    adapters,
		Store:       store,
		QuietPeriod: 100 * time.Millisecond,
		GuardWindow: 2 * time.Second,
		RulesEngine: eng,
	})
	require.NoError(t, err)
	defer orch.Close()
	go orch.Run(ctx)
	time.Sleep(150 * time.Millisecond)

	require.NoError(t, os.WriteFile(filepath.Join(watched, "CLAUDE.md"),
		[]byte("# work file\n"), 0o644))

	time.Sleep(1500 * time.Millisecond)

	memories, err := store.ListArtifacts(acf.KindMemory)
	require.NoError(t, err)
	require.Len(t, memories, 1)
	require.Contains(t, memories[0].Tags, "work",
		"tag-assigning rule should have stamped 'work' onto the artifact")
}

// TestRuleInputFor_PopulatesBranchName proves ruleInputFor threads the head
// branch into the syncrules projection's BranchName, normalized through
// acf.NormalizeBranchName (so a match.branchName regex predicate is live). An
// empty head branch maps to acf.MainBranch.
func TestRuleInputFor_PopulatesBranchName(t *testing.T) {
	art := acf.Artifact{
		ArtifactID: "conv-1",
		Kind:       acf.KindConversation,
		Scope:      acf.ScopeGlobal,
	}

	// A non-empty head branch is normalized: "feature/x" -> "feature-x".
	in := ruleInputFor(art, "claude-code", "feature/x")
	require.Equal(t, "feature-x", in.BranchName,
		"ruleInputFor must populate BranchName from the normalized head branch")

	// An empty head branch maps to main so a rule scoped to main can match.
	inMain := ruleInputFor(art, "claude-code", "")
	require.Equal(t, acf.MainBranch, inMain.BranchName,
		"empty head branch must normalize to main")
}

// TestRuleInputFor_BranchNameRegexGatesEvaluate is the end-to-end seam: a rule
// whose match.branchName is a regex, evaluated against the projection
// ruleInputFor builds, allows the head branch that matches and denies the one
// that doesn't.
func TestRuleInputFor_BranchNameRegexGatesEvaluate(t *testing.T) {
	// NormalizeBranchName turns "feature/x" into "feature-x" (slash -> hyphen),
	// so the rule's regex is expressed against that NORMALIZED form. This test
	// pins the wiring: the projection carries the normalized branch and the
	// regex is matched against that value.
	eng, err := syncrules.New([]syncrules.Rule{{
		Name:  "feature-to-codex",
		Match: syncrules.MatchSpec{BranchName: "^feature-.*$"},
		Route: syncrules.RouteSpec{Agents: []string{"codex"}},
	}})
	require.NoError(t, err)

	art := acf.Artifact{ArtifactID: "conv-1", Kind: acf.KindConversation, Scope: acf.ScopeGlobal}
	opts := syncrules.EvaluateOpts{InstalledAgents: []string{"claude-code", "codex"}}

	// Raw head branch "feature/x" normalizes to "feature-x", which the regex
	// matches -> codex allowed.
	allow := eng.Evaluate(ruleInputFor(art, "claude-code", "feature/x"), opts)
	require.Contains(t, allow.AllowedAgents, "codex")

	// "main" does not match ^feature-.*$ -> rule does not contribute.
	deny := eng.Evaluate(ruleInputFor(art, "claude-code", "main"), opts)
	require.NotContains(t, deny.MatchedRules, "feature-to-codex")
}
