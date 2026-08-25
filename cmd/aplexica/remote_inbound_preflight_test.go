package main

import (
	"encoding/json"
	"testing"

	"github.com/aplexica/aplexica/internal/plugin/proto"
	"github.com/stretchr/testify/require"
)

func TestTerminalPreAdmissionAckQuarantinesOnlyLegacyOrMalformedEnvelopes(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		terminal bool
		reason   string
	}{
		{name: "legacy v1", body: `{"v":1,"ct":"opaque"}`, terminal: true, reason: "envelope-version-below-minimum"},
		{name: "explicit old version", body: `{"version":1,"bodyCiphertext":"opaque"}`, terminal: true, reason: "envelope-version-below-minimum"},
		{name: "malformed", body: `{`, terminal: true, reason: "malformed-envelope"},
		{name: "current v2", body: `{"version":2,"bodyCiphertext":"opaque"}`},
		{name: "future version", body: `{"version":3,"bodyCiphertext":"opaque"}`},
		{name: "unknown first member", body: `{"algorithm":"x","version":1}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			delivery := proto.RemoteInboundDeliveryV2{DeliveryID: "delivery-1", Cursor: "cursor-1", Events: []proto.RemoteEvent{{Bytes: json.RawMessage(tc.body)}}}
			ack, ok := terminalPreAdmissionAck(delivery)
			require.Equal(t, tc.terminal, ok)
			if tc.terminal {
				require.Equal(t, delivery.DeliveryID, ack.DeliveryID)
				require.Equal(t, delivery.Cursor, ack.NextCursor)
				require.Equal(t, "quarantined", ack.Outcomes[0].Disposition)
				require.Equal(t, tc.reason, ack.Outcomes[0].ReasonCode)
			}
		})
	}
}

func TestTerminalPreAdmissionAckDoesNotPartiallyAcknowledgeMixedBatch(t *testing.T) {
	delivery := proto.RemoteInboundDeliveryV2{DeliveryID: "delivery-1", Cursor: "cursor-1", Events: []proto.RemoteEvent{
		{Bytes: json.RawMessage(`{"v":1}`)},
		{Bytes: json.RawMessage(`{"version":2}`)},
	}}
	_, ok := terminalPreAdmissionAck(delivery)
	require.False(t, ok)
}
