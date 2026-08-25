package syncd

import (
	"reflect"
	"testing"
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
