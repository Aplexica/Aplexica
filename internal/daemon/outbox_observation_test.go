package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/plugin/proto"
	"github.com/stretchr/testify/require"
)

type capturedSyncObservation struct {
	metric         string
	value          float64
	unit           string
	sourceIdentity string
}

type observingPublishClient struct {
	observed chan capturedSyncObservation
}

func (*observingPublishClient) Publish(context.Context, []proto.RemoteEvent) (proto.RemotePublishResult, error) {
	return proto.RemotePublishResult{}, nil
}

func (c *observingPublishClient) ObserveSyncV1Async(metric string, value float64, unit, sourceIdentity string) bool {
	c.observed <- capturedSyncObservation{metric: metric, value: value, unit: unit, sourceIdentity: sourceIdentity}
	return true
}

func TestOutboxOldestPendingAgeUsesOldestValidLiveIntent(t *testing.T) {
	outbox := newTestOutbox(t)
	now := time.Now().UTC()
	require.NoError(t, outbox.Append(ev("oldest")))
	time.Sleep(2 * time.Millisecond)
	require.NoError(t, outbox.Append(ev("newer")))
	age, present, err := outbox.OldestPendingAge(now.Add(time.Second))
	require.NoError(t, err)
	require.True(t, present)
	require.Greater(t, age, 900*time.Millisecond)
	require.Less(t, age, 1100*time.Millisecond)

	require.NoError(t, outbox.Remove("oldest"))
	require.NoError(t, outbox.Remove("newer"))
	age, present, err = outbox.OldestPendingAge(now.Add(time.Second))
	require.NoError(t, err)
	require.False(t, present)
	require.Zero(t, age)
}

func TestOutboxOldestPendingAgeChargesMalformedEntriesToReadBudget(t *testing.T) {
	const malformed = "not-json-at-all!"
	outbox := newLimitedTestOutbox(t, outboxMaxEntryBytes, outboxMaxBytes, int64(len(malformed)))
	require.NoError(t, outbox.Append(ev("valid")))
	require.NoError(t, os.WriteFile(
		filepath.Join(outbox.Root, "0000000000000000-aaa.json"),
		[]byte(malformed),
		outboxFilePerm,
	))

	age, present, err := outbox.OldestPendingAge(time.Now().UTC().Add(time.Second))
	require.NoError(t, err)
	require.False(t, present, "the scan must stop when malformed FIFO entries exhaust its byte budget")
	require.Zero(t, age)

	outbox.listMaxBytes = outboxListMaxBytes
	age, present, err = outbox.OldestPendingAge(time.Now().UTC().Add(time.Second))
	require.NoError(t, err)
	require.True(t, present, "a later valid entry remains observable when it fits in the bounded prefix")
	require.Greater(t, age, 0*time.Second)
}

func TestOutboxPendingSnapshotRejectsNonzeroCountMismatch(t *testing.T) {
	outbox := newTestOutbox(t)
	require.NoError(t, outbox.Append(ev("tracked")))
	external := ev("external")
	require.NoError(t, os.WriteFile(
		filepath.Join(outbox.Root, outboxFileName(42, external.EventID)),
		marshalSeedOutboxEntry(t, 42, external),
		outboxFilePerm,
	))

	pending, age, present, err := outbox.PendingSnapshot(time.Now().UTC().Add(time.Second))
	require.Error(t, err)
	require.Zero(t, pending)
	require.Zero(t, age)
	require.False(t, present)
}

func TestOutboxPendingSnapshotRejectsUntrackedMalformedPendingFile(t *testing.T) {
	outbox := newTestOutbox(t)
	require.NoError(t, os.WriteFile(
		filepath.Join(outbox.Root, outboxFileName(42, "malformed")),
		[]byte("not-json"),
		outboxFilePerm,
	))

	pending, age, present, err := outbox.PendingSnapshot(time.Now().UTC())
	require.Error(t, err)
	require.Zero(t, pending)
	require.Zero(t, age)
	require.False(t, present)
}

func TestRemotePublisherReportsOldestOutboxGaugeAtBoundedCadenceIdentity(t *testing.T) {
	outbox := newTestOutbox(t)
	require.NoError(t, outbox.Append(ev("pending")))
	client := &observingPublishClient{observed: make(chan capturedSyncObservation, 1)}
	adapter := &RemotePublishAdapter{client: client, outbox: outbox}
	now := time.Now().UTC().Add(2 * time.Second)
	adapter.observeOldestOutboxOnce(now)

	observation := <-client.observed
	require.Equal(t, proto.RemoteSyncMetricOldestOutboxAgeSeconds, observation.metric)
	require.Equal(t, proto.RemoteSyncObservationUnitSeconds, observation.unit)
	require.GreaterOrEqual(t, observation.value, 1.5)
	require.Less(t, observation.value, 3.0)
	require.Contains(t, observation.sourceIdentity, now.Truncate(oldestOutboxObservationInterval).Format(time.RFC3339))
}
