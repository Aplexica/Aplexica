package openclaw

import (
	"strings"
	"testing"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/stretchr/testify/require"
)

// The deterministic OpenClaw synced sessionId must round-trip via the forward
// map so a continuation the user made in a materialized OpenClaw session can be
// reconciled to its canonical thread. The seed is TRUNCATED (uses seed[:idCut5],
// 28 < 32 hex chars), so this forward map — not a parse of the id — is the
// recovery path. A nil store yields no map (the import then skips).
func TestOpenclawSyncedThreadMap_RecoversThread(t *testing.T) {
	store := &acf.Store{Root: t.TempDir()}
	require.NoError(t, store.Init())
	ids := []string{
		"019e0000-0000-7000-8000-000000000001",
		"019e1111-2222-7333-8444-555566667777",
	}
	for _, id := range ids {
		require.NoError(t, store.WriteArtifact(acf.Artifact{
			AcfSchemaVersion: acf.SchemaVersion, ArtifactID: id, Kind: acf.KindConversation, Scope: acf.ScopeGlobal, Name: "c",
			MaterializedBranchByAgent: map[string]string{"openclaw": "review-branch"},
		}))
	}
	m := openclawSyncedThreadMap(store)
	refMap := openclawSyncedThreadRefMap(store)
	for _, id := range ids {
		sid := openclawSyncedSessionID(id)
		require.True(t, strings.HasPrefix(sid, syncedSessionIDPrefix), "synced id carries the prefix")
		require.Equal(t, id, m[sid], "forward map recovers the artifact id from the synced sessionId")
		branchSID := openclawSyncedSessionIDForBranch(id, "review-branch")
		require.Equal(t, id, refMap[branchSID].ArtifactID)
		require.Equal(t, "review-branch", refMap[branchSID].BranchID)
		require.NotEqual(t, sid, branchSID, "branch session id must not collide with main")
	}
	require.Nil(t, openclawSyncedThreadMap(nil), "nil store → nil map (import skips)")
}
