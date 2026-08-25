package main

import (
	"bytes"
	"encoding/json"

	"github.com/aplexica/aplexica/internal/plugin/proto"
)

// terminalPreAdmissionAck recognizes only failures that are permanently safe
// to quarantine before consulting mutable roster/security-epoch state. This is
// deliberately narrow: current v2 and future-version envelopes continue into
// normal fail-closed admission. Legacy v1 envelopes can never become valid
// under the mandatory-v2 policy, so retaining them in the broker retry queue
// only creates an availability/CPU loop.
func terminalPreAdmissionAck(delivery proto.RemoteInboundDeliveryV2) (proto.RemoteInboundAckV2, bool) {
	if len(delivery.Events) == 0 {
		return proto.RemoteInboundAckV2{}, false
	}
	ack := proto.RemoteInboundAckV2{
		DeliveryID: delivery.DeliveryID,
		NextCursor: delivery.Cursor,
		Outcomes:   make([]proto.RemoteInboundEventOutcomeV2, len(delivery.Events)),
	}
	for index, event := range delivery.Events {
		reason, terminal := terminalEnvelopeReason(event.Bytes)
		if !terminal {
			return proto.RemoteInboundAckV2{}, false
		}
		ack.Outcomes[index] = proto.RemoteInboundEventOutcomeV2{
			Index:       uint32(index),
			Disposition: "quarantined",
			ReasonCode:  reason,
		}
	}
	return ack, true
}

// terminalEnvelopeReason parses only the first top-level member. Both Aplexica
// envelope encoders use a struct, so legacy v1 begins with {"v":1,...} and
// mandatory v2 begins with {"version":2,...}. Reading no body/ciphertext field
// keeps this preflight bounded even for a large retained conversation.
func terminalEnvelopeReason(input []byte) (string, bool) {
	decoder := json.NewDecoder(bytes.NewReader(input))
	token, err := decoder.Token()
	if err != nil {
		return "malformed-envelope", true
	}
	delimiter, ok := token.(json.Delim)
	if !ok || delimiter != '{' || !decoder.More() {
		return "malformed-envelope", true
	}
	nameToken, err := decoder.Token()
	if err != nil {
		return "malformed-envelope", true
	}
	name, ok := nameToken.(string)
	if !ok {
		return "malformed-envelope", true
	}
	switch name {
	case "v":
		var version uint64
		if err := decoder.Decode(&version); err != nil {
			return "malformed-envelope", true
		}
		if version < 2 {
			return "envelope-version-below-minimum", true
		}
	case "version":
		var version uint64
		if err := decoder.Decode(&version); err != nil {
			return "malformed-envelope", true
		}
		if version < 2 {
			return "envelope-version-below-minimum", true
		}
	default:
		// Unknown ordering/format is not enough evidence for a terminal ACK.
		// Let the normal verifier classify it under durable epoch admission.
		return "", false
	}
	return "", false
}
