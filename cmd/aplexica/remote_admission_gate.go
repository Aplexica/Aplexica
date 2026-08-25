package main

import (
	"sync"
	"time"
)

const remoteAdmissionRetryBackoff = 30 * time.Second

// remoteAdmissionGate prevents a missing or temporarily unreadable local
// security epoch from turning a retained MQTT replay into thousands of
// identical filesystem probes. It never accepts or discards a delivery: while
// the gate is closed callers return the same retryable acknowledgement and the
// plugin keeps the delivery eligible for a later attempt.
type remoteAdmissionGate struct {
	mu       sync.Mutex
	inFlight bool
	retryAt  time.Time
}

func (g *remoteAdmissionGate) begin(now time.Time) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.inFlight || (!g.retryAt.IsZero() && now.Before(g.retryAt)) {
		return false
	}
	g.inFlight = true
	return true
}

func (g *remoteAdmissionGate) finish(now time.Time, succeeded bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.inFlight = false
	if succeeded {
		g.retryAt = time.Time{}
		return
	}
	g.retryAt = now.Add(remoteAdmissionRetryBackoff)
}
