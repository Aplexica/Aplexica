package daemon

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	syncd "github.com/aplexica/aplexica/internal/sync"
)

// oversizeEventForTest builds an OutboundEvent big enough (or not) to trip the
// oversized dead-letter path in PublishOutbound.
func oversizeEventForTest(artifactID string, body json.RawMessage) syncd.OutboundEvent {
	return syncd.OutboundEvent{
		ArtifactID: artifactID,
		EventID:    "evt-" + artifactID,
		Kind:       "conversation",
		Bytes:      body,
	}
}

// An event over remotePublishMaxEventBytes must surface a bus notification —
// a silently dead-lettered conversation otherwise just stops syncing forever.
func TestRemotePublishAdapter_OversizedEventNotifies(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := NewRemotePublishAdapter(ctx, &RemoteRunner{Executable: "/bin/true"}, t.TempDir(), nil)
	got := make(chan map[string]any, 4)
	a.SetEventNotifier(func(kind string, body map[string]any) {
		require.Equal(t, "remote.outbound_oversized", kind)
		got <- body
	})

	big := make(json.RawMessage, remotePublishMaxEventBytes+1)
	a.PublishOutbound(oversizeEventForTest("art-big", big))

	select {
	case body := <-got:
		require.Equal(t, "art-big", body["artifact_id"])
	case <-time.After(2 * time.Second):
		t.Fatal("oversized event did not notify")
	}

	// Throttle: a second oversized event for the SAME artifact within the
	// interval must not re-notify.
	a.PublishOutbound(oversizeEventForTest("art-big", big))
	select {
	case <-got:
		t.Fatal("oversized notification must be throttled per artifact")
	case <-time.After(300 * time.Millisecond):
	}
}

// The oversized notify body must state whether the RETAINED lane — the
// recovery baseline peers depend on — is the thing that is too large, so the
// status surface can distinguish "this conversation cannot be baselined for
// peers at all" (retained_too_large=true; no recovery path until the head
// shrinks) from a live-lane/legacy oversize the retained baseline still
// covers.
func TestNotifyOversized_FlagsRetainedTooLarge(t *testing.T) {
	a := &RemotePublishAdapter{retries: map[string]int{}}
	got := make(chan map[string]any, 2)
	a.SetEventNotifier(func(kind string, body map[string]any) {
		require.Equal(t, "remote.outbound_oversized", kind)
		got <- body
	})

	a.notifyOversized(toRemoteEvent(syncd.OutboundEvent{
		EventID: "evt-1-r-dev", ArtifactID: "art-retained", Lane: syncd.LaneRetained,
	}))
	require.Equal(t, true, (<-got)["retained_too_large"],
		"a retained-lane oversize must be flagged for the status surface")

	a.notifyOversized(toRemoteEvent(syncd.OutboundEvent{
		EventID: "evt-2", ArtifactID: "art-live", Lane: syncd.LaneLive,
	}))
	require.Equal(t, false, (<-got)["retained_too_large"],
		"a live-lane oversize is not a baseline-recovery gap")
}

// SetEventNotifier is called by serveCmd AFTER NewRemotePublishAdapter has
// already spawned the pump/resume/periodicDrain goroutines, which read the
// notifier inside notifyOversized — so the write and the reads must be
// synchronized. This test's value is under the race detector: an
// unsynchronized notify field makes -race report a write/read race here.
func TestSetEventNotifier_RaceSafe(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := NewRemotePublishAdapter(ctx, &RemoteRunner{Executable: "/bin/true"}, t.TempDir(), nil)

	ev := toRemoteEvent(oversizeEventForTest("art-race", nil))
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			if i%2 == 0 {
				a.SetEventNotifier(func(kind string, body map[string]any) {})
			} else {
				a.SetEventNotifier(nil)
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			a.notifyOversized(ev)
		}
	}()
	wg.Wait()
}
