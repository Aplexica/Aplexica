package syncd

import "sync"

// AdapterBlocker is a live, user-visible adapter policy gate. It is used for
// safety conditions that should skip one adapter without stopping the daemon,
// such as "native backup required before import/sync".
type AdapterBlocker struct {
	mu             sync.RWMutex
	reasons        map[string]string
	clearListeners map[uint64]func(string)
	nextListenerID uint64
}

// NewAdapterBlocker returns a blocker seeded with initial reasons.
func NewAdapterBlocker(initial map[string]string) *AdapterBlocker {
	b := &AdapterBlocker{reasons: map[string]string{}}
	for name, reason := range initial {
		if name == "" {
			continue
		}
		b.reasons[name] = reason
	}
	return b
}

// Blocked reports whether name is blocked and why.
func (b *AdapterBlocker) Blocked(name string) (string, bool) {
	if b == nil {
		return "", false
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	reason, ok := b.reasons[name]
	return reason, ok
}

// Set blocks name with reason.
func (b *AdapterBlocker) Set(name, reason string) {
	if b == nil || name == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.reasons == nil {
		b.reasons = map[string]string{}
	}
	b.reasons[name] = reason
}

// Clear removes any block for name.
func (b *AdapterBlocker) Clear(name string) {
	if b == nil || name == "" {
		return
	}
	b.mu.Lock()
	if _, blocked := b.reasons[name]; !blocked {
		b.mu.Unlock()
		return
	}
	delete(b.reasons, name)
	listeners := make([]func(string), 0, len(b.clearListeners))
	for _, listener := range b.clearListeners {
		listeners = append(listeners, listener)
	}
	b.mu.Unlock()

	// Listener code may inspect the blocker again or schedule asynchronous
	// work, so it must never run under b.mu. Clear is intentionally synchronous:
	// once it returns every registered observer has seen the transition, while
	// the observer remains responsible for keeping any heavyweight work async.
	for _, listener := range listeners {
		listener(name)
	}
}

// SubscribeClears registers a callback for blocked -> unblocked transitions.
// Calling Clear for an already-unblocked adapter emits nothing. The returned
// function unregisters the listener and is safe to call repeatedly.
func (b *AdapterBlocker) SubscribeClears(listener func(string)) func() {
	if b == nil || listener == nil {
		return func() {}
	}
	b.mu.Lock()
	if b.clearListeners == nil {
		b.clearListeners = map[uint64]func(string){}
	}
	b.nextListenerID++
	id := b.nextListenerID
	b.clearListeners[id] = listener
	b.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			b.mu.Lock()
			delete(b.clearListeners, id)
			b.mu.Unlock()
		})
	}
}

// Snapshot returns a copy of the current block map.
func (b *AdapterBlocker) Snapshot() map[string]string {
	if b == nil {
		return nil
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make(map[string]string, len(b.reasons))
	for name, reason := range b.reasons {
		out[name] = reason
	}
	return out
}
