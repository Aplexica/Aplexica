package syncd

import (
	"testing"
	"time"
)

// TestOrchestrator_LastActivity_GetterSetter exercises the mu-protected
// activity-stamp getter/setter without spinning up a real watcher.
// handleEvent's "stamp on success" path is exercised indirectly by the
// existing orchestrator_test.go fixture tests; this test isolates the
// thread-safe field.
func TestOrchestrator_LastActivity_GetterSetter(t *testing.T) {
	o := &Orchestrator{}

	if got := o.LastActivity(); !got.IsZero() {
		t.Errorf("fresh orchestrator should report zero activity, got %v", got)
	}

	want := time.Date(2026, 5, 22, 14, 0, 0, 0, time.UTC)
	o.setLastActivity(want)
	if got := o.LastActivity(); !got.Equal(want) {
		t.Errorf("got %v want %v", got, want)
	}

	// Second update overwrites the first.
	later := want.Add(time.Minute)
	o.setLastActivity(later)
	if got := o.LastActivity(); !got.Equal(later) {
		t.Errorf("got %v want %v", got, later)
	}
}
