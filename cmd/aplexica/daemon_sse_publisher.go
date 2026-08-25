package main

import (
	"github.com/aplexica/aplexica/internal/web/sse"
)

// sseBusPublisher is the syncd.EventPublisher adapter that bridges
// the orchestrator's structured event notifications into the
// daemon's web-UI SSE bus. The adapter exists so internal/sync can
// stay agnostic to the web stack — it depends only on its own one-
// method EventPublisher interface, and the daemon plugs in this
// concrete wiring at startup.
//
// The bus's PublishKind already handles nil-body and marshal errors
// gracefully, so the adapter has nothing extra to do beyond the
// string-to-EventKind cast.
type sseBusPublisher struct {
	bus *sse.Bus
}

// Publish satisfies syncd.EventPublisher. Best-effort: if bus is
// nil the call no-ops, matching the publisher interface's "never
// blocks, never errors" contract.
func (p sseBusPublisher) Publish(kind string, body any) {
	if p.bus == nil {
		return
	}
	p.bus.PublishKind(sse.EventKind(kind), body)
}
