package syncd

import (
	"sync"
	"testing"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/stretchr/testify/require"
)

// TestSetLocalDeviceID_RotationStampsNewOriginOnNextOutbound covers the
// re-pair rotation: the cloud device id changes while the daemon is
// running, and the very next outbound event must carry the NEW identity —
// a daemon that keeps stamping the retired id has every durable append
// rejected as publisher_identity_conflict until restart.
func TestSetLocalDeviceID_RotationStampsNewOriginOnNextOutbound(t *testing.T) {
	pub := &stubRemotePublisher{}
	local := newTestDevice(t, "old-device")
	o, store := newStoreOrch(t, pub, local)

	before, _ := seedArtifact(t, store, acf.KindMemory, local.id)
	o.forwardCommitted(before)
	require.Equal(t, 1, pub.Count())

	o.SetLocalDeviceID("new-device")
	require.Equal(t, "new-device", o.LocalDeviceID(),
		"the public getter must observe the runtime rotation")

	after, _ := seedArtifact(t, store, acf.KindMemory, local.id)
	o.forwardCommitted(after)
	require.Equal(t, 2, pub.Count())

	pub.mu.Lock()
	defer pub.mu.Unlock()
	require.Equal(t, "old-device", pub.events[0].Origin)
	require.Equal(t, "new-device", pub.events[1].Origin,
		"the first outbound event after a rotation must carry the NEW cloud device id")
}

// TestSetLocalDeviceID_EmptyRefusedAndConcurrentRotationSafe: the identity is
// sticky-until-replaced — blanking it mid-flight would tear the
// non-empty-then-compare identity reads through the import path — and the
// setter must be safe against concurrent callers plus lock-free readers (the
// daemon's RefreshIdentity hook runs on its own goroutine per plugin
// reconnection).
func TestSetLocalDeviceID_EmptyRefusedAndConcurrentRotationSafe(t *testing.T) {
	const seed = "11111111-1111-4111-8111-111111111111"
	o := &Orchestrator{}
	o.SetLocalDeviceID(seed)
	o.SetLocalDeviceID("")
	require.Equal(t, seed, o.LocalDeviceID(), "an empty id must not erase a known identity")

	ids := []string{
		"22222222-2222-4222-8222-222222222222",
		"33333333-3333-4333-8333-333333333333",
		"44444444-4444-4444-8444-444444444444",
	}
	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			o.SetLocalDeviceID(id)
			_ = o.LocalDeviceID()
		}()
	}
	wg.Wait()
	require.Contains(t, ids, o.LocalDeviceID())
}

// TestForwardCommitted_UnpairedHostnameNeverRidesAsOrigin verifies that an
// event authored while unpaired falls back to native adapter provenance for
// its Origin, and that provenance defaults to os.Hostname()
// (for example, "test-host.localdomain").
// The wire invariant is a real cloud device id (UUID-shaped) or EMPTY — the
// plugin fills its current identity when empty.
func TestForwardCommitted_UnpairedHostnameNeverRidesAsOrigin(t *testing.T) {
	pub := &stubRemotePublisher{}
	local := newTestDevice(t, "") // unpaired: no cloud identity
	peer := newTestDevice(t, "8ba6bf0a-58db-4a05-9a26-fabe192f8b17")
	o, store := newStoreOrch(t, pub, local, Recipient{DeviceID: peer.id, PubKey: peer.pub})

	id, _ := seedArtifact(t, store, acf.KindMemory, "test-host.localdomain")
	o.forwardCommitted(id)

	require.Equal(t, 1, pub.Count())
	pub.mu.Lock()
	defer pub.mu.Unlock()
	require.Empty(t, pub.events[0].Origin,
		"a hostname must never reach the relay as a device origin")
}

// TestForwardCommitted_UnpairedUUIDProvenancePassesThrough: the unpaired
// fallback keeps a UUID-shaped provenance device id (a real cloud identity,
// e.g. from an artifact that round-tripped through a paired peer) so
// attribution survives where it is genuine.
func TestForwardCommitted_UnpairedUUIDProvenancePassesThrough(t *testing.T) {
	pub := &stubRemotePublisher{}
	local := newTestDevice(t, "")
	peer := newTestDevice(t, "8ba6bf0a-58db-4a05-9a26-fabe192f8b17")
	o, store := newStoreOrch(t, pub, local, Recipient{DeviceID: peer.id, PubKey: peer.pub})

	id, _ := seedArtifact(t, store, acf.KindMemory, "3f2c8a1e-9d4b-4c6f-8e21-7ab5d90c4e12")
	o.forwardCommitted(id)

	require.Equal(t, 1, pub.Count())
	pub.mu.Lock()
	defer pub.mu.Unlock()
	require.Equal(t, "3f2c8a1e-9d4b-4c6f-8e21-7ab5d90c4e12", pub.events[0].Origin)
}

func TestIsUUIDDeviceID(t *testing.T) {
	require.True(t, isUUIDDeviceID("8ba6bf0a-58db-4a05-9a26-fabe192f8b17"))
	require.False(t, isUUIDDeviceID(""))
	require.False(t, isUUIDDeviceID("test-host.localdomain"))
	require.False(t, isUUIDDeviceID("this-device"))
	require.False(t, isUUIDDeviceID("8BA6BF0A-58DB-4A05-9A26-FABE192F8B17"),
		"cloud ids are canonical lowercase; uppercase is not a round-trip match")
	require.False(t, isUUIDDeviceID("8ba6bf0a58db4a059a26fabe192f8b17"),
		"un-hyphenated hex parses as a UUID but is not the canonical wire shape")
	require.False(t, isUUIDDeviceID("urn:uuid:8ba6bf0a-58db-4a05-9a26-fabe192f8b17"))
}
