package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/plugin/proto"
	"github.com/stretchr/testify/require"
)

func TestRemotePublisherOutboxEvidenceStatusIsBoundedAndContentFree(t *testing.T) {
	outbox := newTestOutbox(t)
	require.NoError(t, outbox.Append(ev("event-secret-must-not-appear")))
	publisher := &RemotePublishAdapter{outbox: outbox}

	status := publisher.OutboxEvidenceStatus(time.Now().UTC().Add(2 * time.Second))
	require.True(t, status.Available)
	require.Equal(t, uint64(1), status.Pending)
	require.True(t, status.OldestPendingPresent)
	require.GreaterOrEqual(t, status.OldestPendingAgeSeconds, uint64(1))

	body, err := json.Marshal(status)
	require.NoError(t, err)
	require.NotContains(t, string(body), "event-secret-must-not-appear")
	require.NotContains(t, string(body), "namespace_id")
	require.NotContains(t, string(body), "artifact_id")

	require.NoError(t, outbox.Remove("event-secret-must-not-appear"))
	status = publisher.OutboxEvidenceStatus(time.Now().UTC())
	require.True(t, status.Available)
	require.Zero(t, status.Pending)
	require.False(t, status.OldestPendingPresent)
}

func TestRemotePublisherOutboxEvidenceStatusUnavailableBeforeInitialization(t *testing.T) {
	publisher := &RemotePublishAdapter{outbox: &Outbox{Root: filepath.Join(t.TempDir(), "missing")}}

	status := publisher.OutboxEvidenceStatus(time.Now().UTC())
	require.False(t, status.Available)
	require.Zero(t, status.Pending)
	require.False(t, status.OldestPendingPresent)
}

func TestRemotePublisherOutboxEvidenceStatusUnavailableWhenInitializedRootDisappears(t *testing.T) {
	outbox := newTestOutbox(t)
	require.NoError(t, os.RemoveAll(outbox.Root))
	publisher := &RemotePublishAdapter{outbox: outbox}

	status := publisher.OutboxEvidenceStatus(time.Now().UTC())
	require.False(t, status.Available)
	require.Zero(t, status.Pending)
	require.False(t, status.OldestPendingPresent)
}

func TestRemotePublisherOutboxEvidenceStatusUnavailableOnCountDiskMismatch(t *testing.T) {
	outbox := newTestOutbox(t)
	event := ev("externally-added")
	require.NoError(t, os.WriteFile(
		filepath.Join(outbox.Root, outboxFileName(42, event.EventID)),
		marshalSeedOutboxEntry(t, 42, event),
		outboxFilePerm,
	))
	publisher := &RemotePublishAdapter{outbox: outbox}

	status := publisher.OutboxEvidenceStatus(time.Now().UTC())
	require.False(t, status.Available, "a disk entry outside the locked accounting snapshot must fail closed")
	require.Zero(t, status.Pending)
	require.False(t, status.OldestPendingPresent)
}

func TestControlStatusSyncEvidenceIsExplicitAndBounded(t *testing.T) {
	dir := shortTempDir(t)
	sockPath := filepath.Join(dir, "test.sock")
	srv := NewControlServer(sockPath, &StatusInfo{PID: 123}, nil)
	deadlineObserved := make(chan bool, 1)
	var providerCalls atomic.Int32
	srv.SetSyncEvidenceProvider(func(ctx context.Context) SyncEvidenceStatus {
		providerCalls.Add(1)
		_, hasDeadline := ctx.Deadline()
		deadlineObserved <- hasDeadline
		return SyncEvidenceStatus{
			RemoteAvailable: true,
			Remote: &proto.RemoteStatusResult{SyncEvidence: &proto.RemoteSyncEvidenceV1{
				SchemaVersion: 1,
				SelectedMode:  "delta_preferred",
				Complete:      true,
				CollectedAt:   time.Date(2026, 7, 20, 18, 0, 0, 0, time.UTC),
				Streams: []proto.RemoteSyncStreamEvidenceV1{{
					StreamID: "scope-digest", StreamEpoch: "epoch-1",
					ServerTipPosition: 2, ServerDevicePosition: 2,
					LocalCursorPresent: true, LocalCursorPosition: 2,
					CursorAndHeadConverged: true,
				}},
				Outbound: proto.RemoteOutboundEvidenceV1{DeltaCommitted: 2},
			}},
			Outbox: OutboxEvidenceStatus{Available: true},
		}
	})
	require.NoError(t, srv.Start())
	defer srv.Stop()

	// The longstanding status wire remains local-only by default. This is the
	// path used by the tray, watch mode, and ordinary `aplexica status` calls.
	resp, err := SendCommand(sockPath, Request{Command: "status"})
	require.NoError(t, err)
	require.True(t, resp.OK)
	require.Zero(t, providerCalls.Load())
	data, ok := resp.Data.(map[string]any)
	require.True(t, ok)
	_, present := data["syncEvidence"]
	require.False(t, present)

	// Acceptance explicitly requests one bounded evidence snapshot.
	resp, err = SendCommand(sockPath, Request{Command: "status", IncludeSyncEvidence: true})
	require.NoError(t, err)
	require.True(t, resp.OK)
	require.Equal(t, int32(1), providerCalls.Load())
	require.True(t, <-deadlineObserved)

	data, ok = resp.Data.(map[string]any)
	require.True(t, ok)
	evidence, ok := data["syncEvidence"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, true, evidence["remoteAvailable"])
	remote, ok := evidence["remote"].(map[string]any)
	require.True(t, ok)
	syncEvidence, ok := remote["sync_evidence"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "delta_preferred", syncEvidence["selected_mode"])
	require.Equal(t, true, syncEvidence["complete"])

	body, err := json.Marshal(resp.Data)
	require.NoError(t, err)
	for _, forbidden := range []string{"namespace_id", "artifact_id", `"cursor"`, `"bytes"`} {
		require.NotContains(t, string(body), forbidden)
	}
}
