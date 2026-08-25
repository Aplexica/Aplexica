package kilo

import (
	"strings"
	"testing"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/stretchr/testify/require"
)

// The deterministic kilo synced-session id must round-trip: kiloSyncedThreadMap
// maps each conversation artifact's synced session id back to the artifact id, so
// a continuation the user made in a materialized kilo session can be reconciled to
// its canonical thread. The seed is TRUNCATED (sessionIDSeedLen < a UUID's 32 hex
// chars), so this forward map — not a parse of the id — is the recovery path.
func TestKiloSyncedThreadMap_RecoversThread(t *testing.T) {
	store := &acf.Store{Root: t.TempDir()}
	require.NoError(t, store.Init())
	ids := []string{
		"019e0000-0000-7000-8000-000000000001",
		"019e1111-2222-7333-8444-555566667777",
	}
	for _, id := range ids {
		require.NoError(t, store.WriteArtifact(acf.Artifact{
			AcfSchemaVersion: acf.SchemaVersion, ArtifactID: id, Kind: acf.KindConversation, Scope: acf.ScopeGlobal, Name: "c",
			MaterializedBranchByAgent: map[string]string{"kilo": "review-branch"},
		}))
	}
	m := kiloSyncedThreadMap(store)
	refMap := kiloSyncedThreadRefMap(store)
	for _, id := range ids {
		sid := kiloSyncedSessionID(id)
		require.True(t, strings.HasPrefix(sid, syncedSessionIDPrefix), "synced id carries the prefix")
		require.Equal(t, id, m[sid], "forward map recovers the artifact id from the synced session id")
		branchSID := kiloSyncedSessionIDForBranch(id, "review-branch")
		require.Equal(t, id, refMap[branchSID].ArtifactID)
		require.Equal(t, "review-branch", refMap[branchSID].BranchID)
		require.NotEqual(t, sid, branchSID, "branch session id must not collide with main")
	}
	// A synced session id that no artifact produces is absent → the caller skips it.
	require.Equal(t, "", m["ses_aplxdeadbeefdeadbeefdeadbeef"])
}
