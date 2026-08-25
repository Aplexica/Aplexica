package syncd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/syncrules"
	"github.com/stretchr/testify/require"
)

// newStoreOrchWithRules is newStoreOrch plus a rules engine, so the outbound
// route.remote gate (FR-05.9) is active.
func newStoreOrchWithRules(t *testing.T, pub RemoteEventPublisher, local testDevice, eng *syncrules.Engine) (*Orchestrator, *acf.Store) {
	t.Helper()
	root := realTempDir(t)
	watched := filepath.Join(root, "proj")
	require.NoError(t, os.MkdirAll(watched, 0o755))
	adapters, store, _ := buildAllThreeAdapters(t, root)

	o, err := NewOrchestrator(Config{
		Dir:                  watched,
		Adapters:             adapters,
		Store:                store,
		QuietPeriod:          50 * time.Millisecond,
		GuardWindow:          time.Second,
		RemoteEventPublisher: pub,
		LocalDeviceID:        local.id,
		RecipientResolver:    staticResolver{recipients: []Recipient{{DeviceID: local.id, PubKey: local.pub}}},
		DeviceKeyProvider:    fixedKeyProvider{priv: local.priv},
		RulesEngine:          eng,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = o.Close() })
	return o, store
}

// seedTaggedArtifact writes a memory artifact + genesis event with the given
// tags so a route rule can match on them.
func seedTaggedArtifact(t *testing.T, store *acf.Store, provenanceDevice string, tags []string) string {
	t.Helper()
	id := acf.NewID()
	now := time.Now().UTC()
	require.NoError(t, store.WriteArtifact(acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion,
		ArtifactID:       id,
		Kind:             acf.KindMemory,
		Scope:            acf.ScopeGlobal,
		Tags:             tags,
		CreatedAt:        now,
		UpdatedAt:        now,
	}))
	payload, _ := json.Marshal(acf.MemoryPayload{Format: "markdown", Content: "secret"})
	require.NoError(t, store.AppendEvent(acf.KindMemory, acf.Event{
		EventID:    acf.NewID(),
		ArtifactID: id,
		Type:       acf.EventTypeCreate,
		Timestamp:  now,
		Provenance: acf.Provenance{DeviceID: provenanceDevice, SourceAgent: "test"},
		Payload:    payload,
	}))
	return id
}

// TestForwardCommitted_RemoteExcludeDropsOutbound is the P1-3 fix: an artifact a
// rule marks route.remote="exclude" must NOT be published to the relay (FR-05.9),
// while a non-excluded artifact still forwards.
func TestForwardCommitted_RemoteExcludeDropsOutbound(t *testing.T) {
	local := newTestDevice(t, "this-device")
	eng, err := syncrules.New([]syncrules.Rule{{
		Name:  "private-local",
		Match: syncrules.MatchSpec{Kind: syncrules.MatchKindAny, Tag: []string{"private"}},
		Route: syncrules.RouteSpec{Remote: "exclude", Agents: []string{"*"}},
	}})
	require.NoError(t, err)

	pub := &stubRemotePublisher{}
	o, store := newStoreOrchWithRules(t, pub, local, eng)

	excluded := seedTaggedArtifact(t, store, "this-device", []string{"private"})
	o.forwardCommitted(excluded)
	require.Equal(t, 0, pub.Count(), "route.remote=exclude artifact must NOT be uploaded")

	allowed := seedTaggedArtifact(t, store, "this-device", nil)
	o.forwardCommitted(allowed)
	require.Equal(t, 1, pub.Count(), "non-excluded artifact must still forward")
}
