package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/plugin/proto"
)

func TestPaceRetainedInboundV2DelaysOnlyRetainedDeliveries(t *testing.T) {
	const interval = 30 * time.Millisecond
	handler := paceRetainedInboundV2(context.Background(), interval, func(delivery proto.RemoteInboundDeliveryV2) proto.RemoteInboundAckV2 {
		return proto.RemoteInboundAckV2{DeliveryID: delivery.DeliveryID}
	})

	live := proto.RemoteInboundDeliveryV2{DeliveryID: "live", Events: []proto.RemoteEvent{{Lane: "live"}}}
	started := time.Now()
	handler(live)
	handler(live)
	if elapsed := time.Since(started); elapsed >= interval {
		t.Fatalf("live deliveries were paced: elapsed=%s interval=%s", elapsed, interval)
	}

	retained := proto.RemoteInboundDeliveryV2{DeliveryID: "retained", Events: []proto.RemoteEvent{{Lane: "retained"}}}
	handler(retained)
	started = time.Now()
	handler(retained)
	if elapsed := time.Since(started); elapsed < interval-5*time.Millisecond {
		t.Fatalf("second retained delivery was not paced: elapsed=%s interval=%s", elapsed, interval)
	}
}

func TestPaceRetainedInboundV2CancellationReturnsRetryableAck(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	handler := paceRetainedInboundV2(ctx, time.Hour, func(delivery proto.RemoteInboundDeliveryV2) proto.RemoteInboundAckV2 {
		calls++
		return proto.RemoteInboundAckV2{DeliveryID: delivery.DeliveryID}
	})
	delivery := proto.RemoteInboundDeliveryV2{DeliveryID: "retained", Events: []proto.RemoteEvent{{Lane: "retained"}}}
	handler(delivery)
	cancel()
	ack := handler(delivery)

	if calls != 1 {
		t.Fatalf("handler calls = %d, want 1", calls)
	}
	if len(ack.Outcomes) != 1 || ack.Outcomes[0].Disposition != "retryable" || ack.Outcomes[0].ReasonCode != "daemon-shutdown" {
		t.Fatalf("canceled pacing ack = %+v", ack)
	}
}
