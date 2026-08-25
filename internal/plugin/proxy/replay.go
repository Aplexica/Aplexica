// Package proxy is the daemon-side implementation of the plugin
// protocol. A Proxy satisfies adapter.Adapter over an io.ReadWriter
// transport; in tests the transport is an io.Pipe wired to host.Serve,
// in production it uses a subprocess's stdin/stdout.
package proxy

import (
	"encoding/json"
	"fmt"

	"github.com/aplexica/aplexica/internal/acf"
)

// ReplayCurrentPayload walks the event log and returns the JSON payload
// of the most recent create/update event. Redaction events are ignored
// here — the caller (Proxy.Export) must check Artifact.Tombstoned
// BEFORE calling and refuse to export tombstoned artifacts (per the
// design, tombstoned artifacts are never passed to plugins).
//
// Returns an error if events is empty or if every event is a redaction
// (i.e., no create/update has ever been written).
func ReplayCurrentPayload(events []acf.Event) (json.RawMessage, error) {
	if len(events) == 0 {
		return nil, fmt.Errorf("plugin/proxy: no events to replay")
	}
	var current json.RawMessage
	for _, e := range events {
		switch e.Type {
		case acf.EventTypeCreate, acf.EventTypeUpdate, acf.EventTypeResolution:
			current = e.Payload
		}
	}
	if len(current) == 0 {
		return nil, fmt.Errorf("plugin/proxy: no create/update/resolution events in log")
	}
	// Defensive copy — events are owned by the store; we own the result.
	out := make(json.RawMessage, len(current))
	copy(out, current)
	return out, nil
}
