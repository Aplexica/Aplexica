package syncd

import (
	"reflect"
	"testing"
	"time"
)

func TestAdapterBlocker_SetClearSnapshot(t *testing.T) {
	b := NewAdapterBlocker(map[string]string{"kilo": "backup failed"})
	if reason, ok := b.Blocked("kilo"); !ok || reason != "backup failed" {
		t.Fatalf("Blocked(kilo) = %q, %v", reason, ok)
	}

	b.Set("codex", "backup required")
	snap := b.Snapshot()
	if snap["codex"] != "backup required" {
		t.Fatalf("Snapshot = %+v", snap)
	}

	b.Clear("kilo")
	if _, ok := b.Blocked("kilo"); ok {
		t.Fatalf("kilo should be unblocked")
	}
}

func TestAdapterBlocker_ClearListenersObserveTransitionsOnly(t *testing.T) {
	b := NewAdapterBlocker(map[string]string{"hermes": "backup pending"})
	var cleared []string
	unsubscribe := b.SubscribeClears(func(name string) {
		cleared = append(cleared, name)
	})

	b.Clear("hermes")
	b.Clear("hermes")
	b.Set("kilo", "backup pending")
	b.Clear("kilo")
	unsubscribe()
	b.Set("openclaw", "backup pending")
	b.Clear("openclaw")
	unsubscribe()

	if want := []string{"hermes", "kilo"}; !reflect.DeepEqual(cleared, want) {
		t.Fatalf("clear transitions = %v, want %v", cleared, want)
	}
}

// An agent blocked before its first import is never touched, so the
// adapterTouched loop cannot see it. Reporting only touched adapters is what
// let a device import nothing while `aplexica status` showed no adapter
// state at all: every agent was blocked by a failed startup safety snapshot,
// and therefore every agent was missing from the map rather than present and
// marked blocked.
func TestAdapterStatesReportsAgentsBlockedBeforeTheyWereEverTouched(t *testing.T) {
	blocker := NewAdapterBlocker(map[string]string{
		"claude-code": "nativebackup: privatefs: unsafe directory permissions 0775",
		"codex":       "nativebackup: privatefs: unsafe directory permissions 0775",
	})
	orch := &Orchestrator{cfg: Config{AdapterBlocker: blocker}}

	states := orch.AdapterStates()
	for _, name := range []string{"claude-code", "codex"} {
		if states[name] != "blocked" {
			t.Errorf("AdapterStates()[%q] = %q, want \"blocked\" even though the agent was never touched",
				name, states[name])
		}
	}
}

// A touched adapter keeps whatever state the activity clock gives it; the
// blocked union must add entries, never overwrite them.
func TestAdapterStatesDoesNotLetTheBlockedUnionOverwriteTouchedState(t *testing.T) {
	blocker := NewAdapterBlocker(map[string]string{"kilo": "backup pending"})
	orch := &Orchestrator{
		cfg:            Config{AdapterBlocker: blocker},
		adapterTouched: map[string]time.Time{"codex": time.Now()},
	}
	states := orch.AdapterStates()
	if states["codex"] != "active" {
		t.Errorf("a touched, unblocked adapter must keep its activity state, got %q", states["codex"])
	}
	if states["kilo"] != "blocked" {
		t.Errorf("an untouched blocked adapter must be reported, got %q", states["kilo"])
	}
}

// AdapterBlocks must answer even when AdapterStates cannot: that method
// returns nil outright if it fails to take the orchestrator lock, which is
// tolerable for an activity hint and not for a hard block.
func TestAdapterBlocksAnswersWithoutTheOrchestratorLock(t *testing.T) {
	blocker := NewAdapterBlocker(map[string]string{"hermes": "backup pending"})
	orch := &Orchestrator{cfg: Config{AdapterBlocker: blocker}}

	orch.mu.Lock()
	defer orch.mu.Unlock()

	if got := orch.AdapterStates(); got != nil {
		t.Fatalf("precondition: AdapterStates must yield nil while the lock is held, got %v", got)
	}
	blocks := orch.AdapterBlocks()
	if blocks["hermes"] != "backup pending" {
		t.Errorf("AdapterBlocks must report the block while the orchestrator lock is held, got %v", blocks)
	}
}

// No blocker wired, or nothing blocked, must be silent rather than an empty
// non-nil map that renders an empty warning block.
func TestAdapterBlocksIsNilWhenNothingIsBlocked(t *testing.T) {
	if got := (&Orchestrator{}).AdapterBlocks(); got != nil {
		t.Errorf("no blocker must report nil, got %v", got)
	}
}
