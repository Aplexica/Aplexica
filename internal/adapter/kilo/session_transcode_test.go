package kilo

import (
	"strings"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/stretchr/testify/require"
)

func TestBuildKiloExport_DeterministicAndTitled(t *testing.T) {
	art := acf.Artifact{
		ArtifactID: "019eb7c7-870f-75cc-8dc2-6a108812d7f1",
		CreatedAt:  time.Unix(1781100000, 0).UTC(),
	}
	turns := []acf.TextTurn{
		{Role: "user", Text: "# AGENTS.md instructions for /Users/testuser\ninjected"},
		{Role: "user", Text: "What is the capital of France?"},
		{Role: "assistant", Text: "Paris"},
	}
	a := buildKiloExport(art, turns, "codex", "/Users/testuser")
	b := buildKiloExport(art, turns, "codex", "/Users/testuser")
	require.Equal(t, a, b, "same inputs must produce identical documents (idempotent re-import)")
	branch := buildKiloExportForBranch(art, turns, "codex", "/Users/testuser", "review-branch")

	require.True(t, strings.HasPrefix(a.Info.ID, syncedSessionIDPrefix),
		"session id must carry the echo-guard marker")
	require.Equal(t, kiloSyncedSessionID(art.ArtifactID), a.Info.ID,
		"main branch keeps the legacy deterministic session id")
	require.Equal(t, kiloSyncedSessionIDForBranch(art.ArtifactID, "review-branch"), branch.Info.ID)
	require.NotEqual(t, a.Info.ID, branch.Info.ID,
		"non-main branch must materialize into a distinct Kilo session")
	require.Equal(t, "↪ Codex: What is the capital of France?", a.Info.Title,
		"title skips injected harness context")
	require.Equal(t, "[review-branch] ↪ Codex: What is the capital of France?", branch.Info.Title)
	require.Len(t, a.Messages, 3)
	require.Equal(t, a.Info.ID, a.Messages[0].Parts[0].SessionID)
	require.Equal(t, a.Messages[0].Info["id"], a.Messages[0].Parts[0].MessageID)
	require.NotEqual(t, a.Messages[0].Info["id"], a.Messages[1].Info["id"])

	// Role-specific schemas: kilo's import silently DROPS messages that
	// fail validation, so the shapes must mirror native exports exactly.
	user := a.Messages[1].Info // second user turn (first is injected)
	require.Equal(t, "user", user["role"])
	require.NotNil(t, user["model"], "user messages carry model{providerID,modelID}")
	require.NotNil(t, user["summary"], "user messages carry summary{diffs}")
	asst := a.Messages[2].Info
	require.Equal(t, "assistant", asst["role"])
	require.Equal(t, user["id"], asst["parentID"], "assistant chains to the preceding user message")
	for _, k := range []string{"mode", "path", "cost", "tokens", "modelID", "providerID", "finish"} {
		require.Contains(t, asst, k, "assistant message must carry %q", k)
	}
}
