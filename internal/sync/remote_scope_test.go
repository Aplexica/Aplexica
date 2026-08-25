package syncd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/pending"
	"github.com/aplexica/aplexica/internal/plugin/proto"
	"github.com/aplexica/aplexica/internal/project"
	"github.com/stretchr/testify/require"
)

// newStoreOrchWithRegistry is newStoreOrch plus a project registry, so the
// stage-and-wait gate (BRD-02 §4.13) is active for inbound project artifacts.
func newStoreOrchWithRegistry(t *testing.T, pub RemoteEventPublisher, local testDevice, reg *project.Registry, extraRecipients ...Recipient) (*Orchestrator, *acf.Store) {
	t.Helper()
	root := realTempDir(t)
	watched := filepath.Join(root, "proj")
	require.NoError(t, os.MkdirAll(watched, 0o755))
	adapters, store, _ := buildAllThreeAdapters(t, root)

	recipients := append([]Recipient{{DeviceID: local.id, PubKey: local.pub}}, extraRecipients...)
	o, err := NewOrchestrator(Config{
		Dir:                  watched,
		Adapters:             adapters,
		Store:                store,
		QuietPeriod:          50 * time.Millisecond,
		GuardWindow:          time.Second,
		RemoteEventPublisher: pub,
		LocalDeviceID:        local.id,
		RecipientResolver:    staticResolver{recipients: recipients},
		DeviceKeyProvider:    fixedKeyProvider{priv: local.priv},
		ProjectRegistry:      reg,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = o.Close() })
	return o, store
}

// sealedScopedWire builds an inbound wire event sealed for `local`, carrying the
// given artifact scope/project INSIDE the encrypted envelope (the relay never
// sees them). Mirrors what a peer device's forwardCommitted produces.
func sealedScopedWire(t *testing.T, local testDevice, kind acf.Kind, scope acf.Scope, proj *project.ProjectInfo, origin string) proto.RemoteEvent {
	t.Helper()
	artID := acf.NewID()
	payload, _ := json.Marshal(acf.MemoryPayload{Format: "markdown", Content: "scoped"})
	ev := acf.Event{
		EventID:    acf.NewID(),
		ArtifactID: artID,
		Type:       acf.EventTypeCreate,
		Timestamp:  time.Now().UTC(),
		Provenance: acf.Provenance{DeviceID: origin, SourceAgent: "test"},
		Payload:    payload,
	}
	sealed, err := sealEnvelope(ev, scope, proj, []recipient{{deviceID: local.id, pub: local.pub}})
	require.NoError(t, err)
	return proto.RemoteEvent{
		ArtifactID: artID,
		EventID:    ev.EventID,
		Kind:       string(kind),
		Type:       string(acf.EventTypeCreate),
		Timestamp:  ev.Timestamp,
		Bytes:      sealed,
		Origin:     origin,
	}
}

// TestEnvelope_CarriesScopeAndProject verifies the artifact scope + project
// identity round-trip through the ENCRYPTED envelope (zero-knowledge: not on the
// plaintext wire). This is the carrier the inbound path needs to avoid
// materializing project-local memory as global.
func TestEnvelope_CarriesScopeAndProject(t *testing.T) {
	dev := newTestDevice(t, "d")
	payload, _ := json.Marshal(acf.MemoryPayload{Format: "markdown", Content: "x"})
	ev := acf.Event{EventID: acf.NewID(), ArtifactID: acf.NewID(), Type: acf.EventTypeCreate, Timestamp: time.Now().UTC(), Payload: payload}
	proj := &project.ProjectInfo{ID: "github.com/test/repo", VCS: "git", Path: "/sender/path"}

	sealed, err := sealEnvelope(ev, acf.ScopeProject, proj, []recipient{{deviceID: dev.id, pub: dev.pub}})
	require.NoError(t, err)

	gotEv, gotScope, gotProj, err := openEnvelope(sealed, dev.id, dev.priv)
	require.NoError(t, err)
	require.Equal(t, ev.EventID, gotEv.EventID)
	require.Equal(t, acf.ScopeProject, gotScope)
	require.NotNil(t, gotProj)
	require.Equal(t, "github.com/test/repo", gotProj.ID)
	require.Equal(t, "git", gotProj.VCS)
}

// TestEnvelope_ScopeAbsentDecodesGlobal verifies an envelope sealed with no
// scope (back-compat / global) opens with an empty scope and nil project.
func TestEnvelope_ScopeAbsentDecodesGlobal(t *testing.T) {
	dev := newTestDevice(t, "d")
	payload, _ := json.Marshal(acf.MemoryPayload{Format: "markdown", Content: "x"})
	ev := acf.Event{EventID: acf.NewID(), ArtifactID: acf.NewID(), Type: acf.EventTypeCreate, Timestamp: time.Now().UTC(), Payload: payload}

	sealed, err := sealEnvelope(ev, acf.ScopeGlobal, nil, []recipient{{deviceID: dev.id, pub: dev.pub}})
	require.NoError(t, err)

	gotEv, gotScope, gotProj, err := openEnvelope(sealed, dev.id, dev.priv)
	require.NoError(t, err)
	require.Equal(t, ev.EventID, gotEv.EventID)
	require.Equal(t, acf.ScopeGlobal, gotScope)
	require.Nil(t, gotProj)
}

// TestImportInbound_ProjectScopeStagedWhenUnregistered is the core P1-4 fix: an
// inbound project-scoped artifact for a project NOT registered locally must keep
// ScopeProject (not become global) and be STAGED (pending), not materialized.
func TestImportInbound_ProjectScopeStagedWhenUnregistered(t *testing.T) {
	local := newTestDevice(t, "this-device")
	reg, err := project.NewRegistry(filepath.Join(realTempDir(t), "registry.json"))
	require.NoError(t, err)
	pub := &stubRemotePublisher{}
	o, store := newStoreOrchWithRegistry(t, pub, local, reg)

	proj := &project.ProjectInfo{ID: "github.com/test/scoped-repo", VCS: "git", Path: "/peer/path/repo"}
	wire := sealedScopedWire(t, local, acf.KindMemory, acf.ScopeProject, proj, "peer")

	o.ImportInbound([]proto.RemoteEvent{wire})

	art, found := o.findArtifact(wire.ArtifactID)
	require.True(t, found)
	require.Equal(t, acf.ScopeProject, art.Scope, "inbound project artifact must NOT materialize as global")
	require.NotNil(t, art.Project)
	require.Equal(t, proj.ID, art.Project.ID)
	require.Empty(t, art.Project.Path, "unregistered project: path must not be trusted from the wire")

	list, err := pending.List(store, reg)
	require.NoError(t, err)
	require.Len(t, list, 1, "unregistered project artifact must be staged as pending")
	require.Equal(t, proj.ID, list[0].ID)
}

// TestImportInbound_ProjectScopeResolvesPathWhenRegistered verifies a registered
// project's inbound artifact resolves its Path from the LOCAL registry (not the
// sender's wire path) and is not pending.
func TestImportInbound_ProjectScopeResolvesPathWhenRegistered(t *testing.T) {
	local := newTestDevice(t, "this-device")
	reg, err := project.NewRegistry(filepath.Join(realTempDir(t), "registry.json"))
	require.NoError(t, err)
	localPath := filepath.Join(realTempDir(t), "myrepo")
	require.NoError(t, os.MkdirAll(localPath, 0o755))
	require.NoError(t, reg.Add(project.Entry{ID: "github.com/test/scoped-repo", Path: localPath, VCS: "git"}))

	pub := &stubRemotePublisher{}
	o, store := newStoreOrchWithRegistry(t, pub, local, reg)

	proj := &project.ProjectInfo{ID: "github.com/test/scoped-repo", VCS: "git", Path: "/peer/DIFFERENT/path"}
	wire := sealedScopedWire(t, local, acf.KindMemory, acf.ScopeProject, proj, "peer")

	o.ImportInbound([]proto.RemoteEvent{wire})

	art, found := o.findArtifact(wire.ArtifactID)
	require.True(t, found)
	require.Equal(t, acf.ScopeProject, art.Scope)
	require.NotNil(t, art.Project)
	require.Equal(t, localPath, art.Project.Path, "path must resolve from the local registry, not the wire")

	list, err := pending.List(store, reg)
	require.NoError(t, err)
	require.Len(t, list, 0, "registered project artifact must not be pending")
}

// TestImportInbound_GlobalScopeUnchanged verifies a global inbound artifact still
// imports as ScopeGlobal with no project (the working 2-device conversation path).
func TestImportInbound_GlobalScopeUnchanged(t *testing.T) {
	local := newTestDevice(t, "this-device")
	pub := &stubRemotePublisher{}
	o, _ := newStoreOrch(t, pub, local)

	wire := sealedScopedWire(t, local, acf.KindMemory, acf.ScopeGlobal, nil, "peer")
	o.ImportInbound([]proto.RemoteEvent{wire})

	art, found := o.findArtifact(wire.ArtifactID)
	require.True(t, found)
	require.Equal(t, acf.ScopeGlobal, art.Scope)
	require.Nil(t, art.Project)
}
