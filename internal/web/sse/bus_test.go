package sse

import (
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// badBody fails json.Marshal with an error message that itself contains
// a double-quote and a backslash, so a naive string-concatenated error
// body produces invalid JSON.
type badBody struct{}

func (badBody) MarshalJSON() ([]byte, error) {
	return nil, errors.New(`bad "quote" \ backslash`)
}

func TestBusPublishKindMarshalErrorEmitsValidJSON(t *testing.T) {
	bus := NewBus()
	ch := make(chan Event, 4)
	bus.Subscribe(ch)

	bus.PublishKind(KindArtifactSynced, badBody{})

	select {
	case e := <-ch:
		var payload map[string]string
		if err := json.Unmarshal(e.Body, &payload); err != nil {
			t.Fatalf("marshal-error fallback body is not valid JSON: %v\nraw body: %s", err, e.Body)
		}
		if _, ok := payload["error"]; !ok {
			t.Errorf("fallback body missing \"error\" key: %s", e.Body)
		}
	case <-time.After(time.Second):
		t.Fatal("subscriber timed out")
	}
}

func TestBusPublishFansOutToAllSubscribers(t *testing.T) {
	bus := NewBus()
	ch1 := make(chan Event, 4)
	ch2 := make(chan Event, 4)
	bus.Subscribe(ch1)
	bus.Subscribe(ch2)

	seq := bus.PublishKind(KindArtifactSynced, map[string]string{"artifactId": "abc"})
	if seq != 1 {
		t.Errorf("first publish seq = %d, want 1", seq)
	}

	for _, ch := range []chan Event{ch1, ch2} {
		select {
		case e := <-ch:
			if e.Kind != KindArtifactSynced {
				t.Errorf("kind = %q", e.Kind)
			}
			if e.Timestamp.IsZero() {
				t.Error("Timestamp must be set by Publish")
			}
			var body struct {
				ArtifactID string `json:"artifactId"`
			}
			if err := json.Unmarshal(e.Body, &body); err != nil {
				t.Fatalf("unmarshal body: %v", err)
			}
			if body.ArtifactID != "abc" {
				t.Errorf("body = %+v", body)
			}
		case <-time.After(time.Second):
			t.Fatal("subscriber timed out")
		}
	}
}

func TestBusSeqMonotonicIncreasing(t *testing.T) {
	bus := NewBus()
	ch := make(chan Event, 16)
	bus.Subscribe(ch)
	for i := 0; i < 5; i++ {
		bus.PublishKind(KindDaemonState, nil)
	}
	for i := uint64(1); i <= 5; i++ {
		e := <-ch
		if e.Seq != i {
			t.Errorf("Seq[%d] = %d, want %d", i, e.Seq, i)
		}
	}
}

func TestBusUnsubscribeStopsDelivery(t *testing.T) {
	bus := NewBus()
	ch := make(chan Event, 4)
	id := bus.Subscribe(ch)
	bus.PublishKind(KindRuleFired, nil)
	<-ch // drain
	bus.Unsubscribe(id)
	bus.PublishKind(KindRuleFired, nil)
	select {
	case <-ch:
		t.Error("event delivered after Unsubscribe")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestBusSlowSubscriberIsDropped(t *testing.T) {
	bus := NewBus()
	// Channel with capacity 1; fill it once then publish more
	ch := make(chan Event, 1)
	bus.Subscribe(ch)
	for i := 0; i < 5; i++ {
		bus.PublishKind(KindAgentActivity, nil)
	}
	if got := bus.DroppedCount(); got == 0 {
		t.Error("DroppedCount = 0; expected drops due to channel saturation")
	}
}

func TestBusConcurrentPublishAndSubscribe(t *testing.T) {
	bus := NewBus()
	const N = 50
	var wg sync.WaitGroup
	var got int64

	for i := 0; i < N; i++ {
		ch := make(chan Event, 1024)
		bus.Subscribe(ch)
		wg.Add(1)
		go func() {
			defer wg.Done()
			deadline := time.After(500 * time.Millisecond)
			for {
				select {
				case <-ch:
					atomic.AddInt64(&got, 1)
				case <-deadline:
					return
				}
			}
		}()
	}

	for i := 0; i < 10; i++ {
		bus.PublishKind(KindDaemonState, nil)
	}
	wg.Wait()
	if got < int64(N) { // each subscriber must have seen ≥ 1 event
		t.Errorf("got = %d, want at least %d", got, N)
	}
}

func TestBusSubscriberCount(t *testing.T) {
	bus := NewBus()
	if got := bus.SubscriberCount(); got != 0 {
		t.Errorf("empty bus SubscriberCount = %d, want 0", got)
	}
	ids := []uint64{}
	for i := 0; i < 3; i++ {
		ids = append(ids, bus.Subscribe(make(chan Event, 4)))
	}
	if got := bus.SubscriberCount(); got != 3 {
		t.Errorf("count = %d, want 3", got)
	}
	bus.Unsubscribe(ids[0])
	if got := bus.SubscriberCount(); got != 2 {
		t.Errorf("count = %d, want 2", got)
	}
}
