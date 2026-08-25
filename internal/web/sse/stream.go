package sse

import (
	"fmt"
	"net/http"
	"time"
)

// keepaliveInterval is the cadence of SSE comment frames sent to
// idle clients. Browsers reconnect after ~45-60s of silence on some
// platforms; 25s is conservative enough to stay well under that
// threshold while not flooding the wire.
const keepaliveInterval = 25 * time.Second

// Stream is the http.Handler that bridges a Bus to a Server-Sent
// Events connection. Register an instance on your server's
// authenticated mux (see Register, which mounts GET
// /api/events/stream); the handler holds the connection open until the
// client disconnects or the request context is cancelled.
type Stream struct {
	Bus         *Bus
	connections chan struct{}
}

// New returns a Stream backed by bus. The handler does not own the
// bus's lifecycle; callers can share a single bus across multiple
// streams (e.g. for testing) or wire it to one daemon-wide instance.
func New(bus *Bus) *Stream {
	return &Stream{Bus: bus, connections: make(chan struct{}, 8)}
}

// Register implements web.HandlerRegistrar so the daemon's startup
// wiring is one line:
//
//	webSrv.UseProtected(sse.New(bus))
//
// Mounts the stream at GET /api/events/stream on the protected mux. The
// design spec §6.4's nominal /events SSE path was relocated under /api/
// so the SPA's bare /events client route stays free (see embed.go).
func (s *Stream) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/events/stream", s.ServeHTTP)
}

// ServeHTTP upgrades the connection to SSE and forwards bus events
// until the client disconnects. Headers:
//
//	Content-Type: text/event-stream
//	Cache-Control: no-cache, no-transform
//	Connection: keep-alive
//	X-Accel-Buffering: no   (defeats nginx proxy buffering even
//	                          though we're loopback-only — defensive
//	                          for users who reverse-proxy locally)
//
// Frame shape per spec §6.4:
//
//	id: <seq>
//	event: <kind>
//	data: <json body>
//	\n
//
// A blank-line frame keeps the connection alive when idle.
func (s *Stream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	select {
	case s.connections <- struct{}{}:
		defer func() { <-s.connections }()
	default:
		w.Header().Set("Retry-After", "1")
		http.Error(w, "too many streams", http.StatusTooManyRequests)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	// Immediate flush so the client sees the headers and switches
	// EventSource state to OPEN without waiting for the first event.
	flusher.Flush()

	ch := make(chan Event, subscriberChanBuffer)
	id := s.Bus.Subscribe(ch)
	defer s.Bus.Unsubscribe(id)

	ctx := r.Context()
	keepalive := time.NewTicker(keepaliveInterval)
	defer keepalive.Stop()

	for {
		select {
		case <-ctx.Done():
			return

		case e, ok := <-ch:
			if !ok {
				return
			}
			if err := writeFrame(w, e); err != nil {
				return
			}
			flusher.Flush()

		case <-keepalive.C:
			// SSE comment frame — starts with a colon, ignored by
			// the client but keeps proxies and load balancers from
			// dropping the connection.
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// writeFrame serializes one Event as a complete SSE frame. The `data:` line
// is the event's BODY payload only — the client reads the kind from `event:`
// and the seq from `id:`, and parses `data` as the kind-specific body (e.g.
// {source, artifactId, name} for artifact.synced). Emitting the whole Event
// wrapper here buried those fields one level deep under "body", which made
// every live row in the web UI render blank.
func writeFrame(w http.ResponseWriter, e Event) error {
	body := []byte(e.Body)
	if len(body) == 0 {
		// Kinds with no payload (e.g. a bare heartbeat) still need valid
		// JSON on the data line so the client's JSON.parse succeeds.
		body = []byte("null")
	}
	_, err := fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", e.Seq, e.Kind, body)
	return err
}
