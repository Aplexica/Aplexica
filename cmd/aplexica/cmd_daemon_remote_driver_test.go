package main

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/keys"
	"github.com/aplexica/aplexica/internal/plugin/proto"
	syncd "github.com/aplexica/aplexica/internal/sync"
)

// fakeDriverClient satisfies remoteSyncDriverClient, capturing the wrap pubkey
// registered and the namespaces subscribed.
type fakeDriverClient struct {
	mu              sync.Mutex
	registerCalls   atomic.Int64
	enumerateCalls  atomic.Int64
	restartCount    atomic.Uint64
	registeredPub   []byte
	registerErr     error
	enumerateErr    error
	enumerateResult proto.RemoteEnumerateResult
	subscribed      []string
	fetchResults    []proto.RemoteFetchResult
	fetchIdx        int
	fetchSince      []string // RemoteFetchParams.Since captured per Fetch call
}

func (f *fakeDriverClient) RegisterWrapKey(_ context.Context, pub []byte) error {
	f.registerCalls.Add(1)
	if f.registerErr != nil {
		return f.registerErr
	}
	f.mu.Lock()
	f.registeredPub = append([]byte(nil), pub...)
	f.mu.Unlock()
	return nil
}

func (f *fakeDriverClient) Enumerate(_ context.Context, _ proto.RemoteEnumerateParams) (proto.RemoteEnumerateResult, error) {
	f.enumerateCalls.Add(1)
	return f.enumerateResult, f.enumerateErr
}

func (f *fakeDriverClient) Subscribe(_ context.Context, ns string) error {
	f.mu.Lock()
	f.subscribed = append(f.subscribed, ns)
	f.mu.Unlock()
	return nil
}

func (f *fakeDriverClient) Fetch(_ context.Context, params proto.RemoteFetchParams) (proto.RemoteFetchResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fetchSince = append(f.fetchSince, params.Since)
	if f.fetchIdx >= len(f.fetchResults) {
		return proto.RemoteFetchResult{}, nil
	}
	r := f.fetchResults[f.fetchIdx]
	f.fetchIdx++
	return r, nil
}

func (f *fakeDriverClient) RestartCount() uint64 { return f.restartCount.Load() }

// noopLogger satisfies the driver's structured-logger interface.
type noopLogger struct{}

func (noopLogger) Info(string, ...any)  {}
func (noopLogger) Warn(string, ...any)  {}
func (noopLogger) Error(string, ...any) {}

type recordingLogger struct {
	mu       sync.Mutex
	messages []string
}

func (l *recordingLogger) record(message string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.messages = append(l.messages, message)
}

func (l *recordingLogger) Info(message string, _ ...any)  { l.record(message) }
func (l *recordingLogger) Warn(message string, _ ...any)  { l.record(message) }
func (l *recordingLogger) Error(message string, _ ...any) { l.record(message) }

func (l *recordingLogger) count(message string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	count := 0
	for _, got := range l.messages {
		if got == message {
			count++
		}
	}
	return count
}

// TestRemoteSyncDriver_RegistersWrapKeyAtBoot verifies the driver registers
// this device's wrap pubkey on its first tick and subscribes to enumerated
// namespaces.
func TestRemoteSyncDriver_RegistersWrapKeyAtBoot(t *testing.T) {
	old := remoteSyncDriverWarmup
	remoteSyncDriverWarmup = 5 * time.Millisecond
	defer func() { remoteSyncDriverWarmup = old }()

	_, pub, err := keys.NewDeviceKey()
	if err != nil {
		t.Fatal(err)
	}
	client := &fakeDriverClient{
		enumerateResult: proto.RemoteEnumerateResult{
			Namespaces: []proto.RemoteNamespaceManifest{{NamespaceID: "ns-1"}},
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	// Ensure the driver goroutine fully exits before the deferred var-restore
	// runs (LIFO defers: this registers AFTER the restore defer, so it executes
	// FIRST) — otherwise the still-running goroutine races the next test's write
	// to the package-level warmup/interval vars.
	defer func() { cancel(); <-done }()
	go func() {
		defer close(done)
		runRemoteSyncDriver(ctx, client, func() ([keys.X25519KeySize]byte, error) { return pub, nil }, nil, nil, noopLogger{}, nil)
	}()

	// Wait for the first tick to complete BOTH register and subscribe (subscribe
	// runs after register within the same tick).
	deadline := time.After(2 * time.Second)
	for {
		client.mu.Lock()
		tickDone := client.registerCalls.Load() >= 1 && len(client.subscribed) > 0
		client.mu.Unlock()
		if tickDone {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("driver did not complete first tick: registerCalls=%d subscribed=%v",
				client.registerCalls.Load(), client.subscribed)
		case <-time.After(5 * time.Millisecond):
		}
	}

	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.registeredPub) != keys.X25519KeySize {
		t.Fatalf("registered pub length = %d, want %d", len(client.registeredPub), keys.X25519KeySize)
	}
	for i := range pub {
		if client.registeredPub[i] != pub[i] {
			t.Fatalf("registered pub does not match device pub at byte %d", i)
		}
	}
	// Subscribe to the enumerated namespace also happened on the same tick.
	if len(client.subscribed) == 0 || client.subscribed[0] != "ns-1" {
		t.Fatalf("expected subscribe to ns-1, got %v", client.subscribed)
	}
}

// TestRemoteSyncDriver_RegisterRetriesUntilSuccess verifies a failing
// registration is retried (not marked done), so a reconnecting plugin
// eventually registers.
func TestRemoteSyncDriver_RegisterRetriesUntilSuccess(t *testing.T) {
	oldWarm, oldInt := remoteSyncDriverWarmup, remoteSyncDriverInterval
	remoteSyncDriverWarmup = 2 * time.Millisecond
	remoteSyncDriverInterval = 10 * time.Millisecond
	defer func() { remoteSyncDriverWarmup = oldWarm; remoteSyncDriverInterval = oldInt }()

	_, pub, _ := keys.NewDeviceKey()
	client := &fakeDriverClient{registerErr: errors.New("reconnecting")}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	// Drain the goroutine before the deferred var-restore (see the sibling test).
	defer func() { cancel(); <-done }()
	go func() {
		defer close(done)
		runRemoteSyncDriver(ctx, client, func() ([keys.X25519KeySize]byte, error) { return pub, nil }, nil, nil, noopLogger{}, nil)
	}()

	// The failing register must be retried on subsequent ticks (>= 2 calls).
	deadline := time.After(2 * time.Second)
	for {
		if client.registerCalls.Load() >= 2 {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("register not retried; calls=%d", client.registerCalls.Load())
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func TestRemoteSyncDriver_CachesWrapKeyAndSuppressesRepeatedExpectedErrors(t *testing.T) {
	oldWarm, oldInt := remoteSyncDriverWarmup, remoteSyncDriverInterval
	remoteSyncDriverWarmup = time.Millisecond
	remoteSyncDriverInterval = 5 * time.Millisecond
	t.Cleanup(func() {
		remoteSyncDriverWarmup = oldWarm
		remoteSyncDriverInterval = oldInt
	})

	_, pub, err := keys.NewDeviceKey()
	if err != nil {
		t.Fatal(err)
	}
	client := &fakeDriverClient{
		registerErr:  errors.New("device not paired"),
		enumerateErr: errors.New("device not paired"),
	}
	logger := &recordingLogger{}
	var keyLoads atomic.Int64
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	defer func() { cancel(); <-done }()
	go func() {
		defer close(done)
		runRemoteSyncDriver(ctx, client, func() ([keys.X25519KeySize]byte, error) {
			keyLoads.Add(1)
			return pub, nil
		}, nil, nil, logger, nil)
	}()

	deadline := time.After(2 * time.Second)
	for client.registerCalls.Load() < 5 || client.enumerateCalls.Load() < 5 {
		select {
		case <-deadline:
			t.Fatalf("driver did not retry: register=%d enumerate=%d", client.registerCalls.Load(), client.enumerateCalls.Load())
		case <-time.After(5 * time.Millisecond):
		}
	}
	if got := keyLoads.Load(); got != 1 {
		t.Fatalf("wrap key loaded %d times during repeated registration failure; want 1", got)
	}
	if got := logger.count("remote: register wrap key not ready (will retry)"); got != 1 {
		t.Fatalf("register failure logged %d times; want 1 per failure episode", got)
	}
	if got := logger.count("remote: enumerate not ready (will retry)"); got != 1 {
		t.Fatalf("enumerate failure logged %d times; want 1 per failure episode", got)
	}
}

func TestRemoteSyncDriver_SubscribesEachStableNamespaceOnce(t *testing.T) {
	oldWarm, oldInt := remoteSyncDriverWarmup, remoteSyncDriverInterval
	remoteSyncDriverWarmup = time.Millisecond
	remoteSyncDriverInterval = 5 * time.Millisecond
	t.Cleanup(func() {
		remoteSyncDriverWarmup = oldWarm
		remoteSyncDriverInterval = oldInt
	})

	client := &fakeDriverClient{enumerateResult: proto.RemoteEnumerateResult{
		Namespaces: []proto.RemoteNamespaceManifest{{NamespaceID: "ns-1"}},
	}}
	logger := &recordingLogger{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	defer func() { cancel(); <-done }()
	go func() {
		defer close(done)
		runRemoteSyncDriver(ctx, client, nil, nil, nil, logger, nil)
	}()

	deadline := time.After(2 * time.Second)
	for client.enumerateCalls.Load() < 5 {
		select {
		case <-deadline:
			t.Fatalf("driver did not enumerate repeatedly: calls=%d", client.enumerateCalls.Load())
		case <-time.After(5 * time.Millisecond):
		}
	}
	client.mu.Lock()
	subscribed := append([]string(nil), client.subscribed...)
	client.mu.Unlock()
	if len(subscribed) != 1 || subscribed[0] != "ns-1" {
		t.Fatalf("stable namespace subscriptions = %v; want one ns-1 subscription", subscribed)
	}
	if got := logger.count("remote: subscribed to namespaces"); got != 1 {
		t.Fatalf("subscription success logged %d times; want 1", got)
	}
}

func TestRemoteSyncDriver_ReplaysPluginOwnedBindingsAfterRunnerRestart(t *testing.T) {
	oldWarm, oldInt := remoteSyncDriverWarmup, remoteSyncDriverInterval
	remoteSyncDriverWarmup = time.Millisecond
	remoteSyncDriverInterval = 5 * time.Millisecond
	t.Cleanup(func() {
		remoteSyncDriverWarmup = oldWarm
		remoteSyncDriverInterval = oldInt
	})

	_, pub, err := keys.NewDeviceKey()
	if err != nil {
		t.Fatal(err)
	}
	client := &fakeDriverClient{enumerateResult: proto.RemoteEnumerateResult{
		Namespaces: []proto.RemoteNamespaceManifest{{NamespaceID: "ns-1"}},
	}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	defer func() { cancel(); <-done }()
	go func() {
		defer close(done)
		runRemoteSyncDriver(ctx, client, func() ([keys.X25519KeySize]byte, error) { return pub, nil }, nil, nil, noopLogger{}, nil)
	}()

	waitForBindings := func(want int64) {
		t.Helper()
		deadline := time.After(2 * time.Second)
		for {
			client.mu.Lock()
			subscriptions := int64(len(client.subscribed))
			client.mu.Unlock()
			if client.registerCalls.Load() >= want && subscriptions >= want {
				return
			}
			select {
			case <-deadline:
				t.Fatalf("bindings did not reach %d: register=%d subscribe=%d", want, client.registerCalls.Load(), subscriptions)
			case <-time.After(5 * time.Millisecond):
			}
		}
	}

	waitForBindings(1)
	client.restartCount.Add(1)
	waitForBindings(2)
	time.Sleep(30 * time.Millisecond)
	client.mu.Lock()
	subscriptions := len(client.subscribed)
	client.mu.Unlock()
	if got := client.registerCalls.Load(); got != 2 {
		t.Fatalf("register calls after one runner restart = %d; want 2", got)
	}
	if subscriptions != 2 {
		t.Fatalf("subscriptions after one runner restart = %d; want 2", subscriptions)
	}
}

// TestRemoteSyncDriver_FetchBackfillImportsMissedEvents verifies the P1-2 fetch
// loop: the driver enumerates branch tips, pages remote.fetch forward from its
// local EventID cursor, feeds every fetched event to the import sink, and stops
// re-fetching once caught up to the branch tip (idempotent).
func TestRemoteSyncDriver_FetchBackfillImportsMissedEvents(t *testing.T) {
	oldWarm, oldInt := remoteSyncDriverWarmup, remoteSyncDriverInterval
	remoteSyncDriverWarmup = 2 * time.Millisecond
	remoteSyncDriverInterval = 10 * time.Millisecond
	defer func() { remoteSyncDriverWarmup = oldWarm; remoteSyncDriverInterval = oldInt }()

	_, pub, _ := keys.NewDeviceKey()
	client := &fakeDriverClient{
		enumerateResult: proto.RemoteEnumerateResult{
			Namespaces: []proto.RemoteNamespaceManifest{{
				NamespaceID: "ns-1",
				Branches:    []proto.RemoteBranchManifest{{BranchID: "main", TipEventID: "e3"}},
			}},
		},
		// Two pages: page 1 has a NextCursor, page 2 ends the walk.
		fetchResults: []proto.RemoteFetchResult{
			{Events: []proto.RemoteEvent{{EventID: "e1"}, {EventID: "e2"}}, NextCursor: "c1"},
			{Events: []proto.RemoteEvent{{EventID: "e3"}}, NextCursor: ""},
		},
	}

	var mu sync.Mutex
	var got []proto.RemoteEvent
	sink := func(evs []proto.RemoteEvent) []syncd.ImportOutcome {
		mu.Lock()
		got = append(got, evs...)
		mu.Unlock()
		out := make([]syncd.ImportOutcome, len(evs))
		return out // all ImportApplied (zero value)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	defer func() { cancel(); <-done }()
	go func() {
		defer close(done)
		runRemoteSyncDriver(ctx, client, func() ([keys.X25519KeySize]byte, error) { return pub, nil }, sink, nil, noopLogger{}, nil)
	}()

	deadline := time.After(2 * time.Second)
	for {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n >= 3 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("backfill imported %d events, want 3", n)
		case <-time.After(5 * time.Millisecond):
		}
	}

	mu.Lock()
	ids := []string{got[0].EventID, got[1].EventID, got[2].EventID}
	mu.Unlock()
	if ids[0] != "e1" || ids[1] != "e2" || ids[2] != "e3" {
		t.Fatalf("imported events out of order/missing: %v", ids)
	}

	// Caught up to the tip (e3): further ticks must NOT re-import.
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	n := len(got)
	mu.Unlock()
	if n != 3 {
		t.Fatalf("caught-up branch re-fetched: got %d events, want 3", n)
	}
}

func TestRemoteSyncDriver_ThrottlesRetainedHeadRepublish(t *testing.T) {
	oldWarm, oldInt, oldRepublish := remoteSyncDriverWarmup, remoteSyncDriverInterval, remoteSyncDriverRepublishInterval
	remoteSyncDriverWarmup = 2 * time.Millisecond
	remoteSyncDriverInterval = 10 * time.Millisecond
	remoteSyncDriverRepublishInterval = 80 * time.Millisecond
	defer func() {
		remoteSyncDriverWarmup = oldWarm
		remoteSyncDriverInterval = oldInt
		remoteSyncDriverRepublishInterval = oldRepublish
	}()

	_, pub, _ := keys.NewDeviceKey()
	client := &fakeDriverClient{
		enumerateResult: proto.RemoteEnumerateResult{
			Namespaces: []proto.RemoteNamespaceManifest{{NamespaceID: "ns-1"}},
		},
	}
	var republishCalls atomic.Int64
	republish := func(context.Context) (int, error) {
		republishCalls.Add(1)
		return 7, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	defer func() { cancel(); <-done }()
	go func() {
		defer close(done)
		runRemoteSyncDriver(ctx, client, func() ([keys.X25519KeySize]byte, error) { return pub, nil }, nil, republish, noopLogger{}, nil)
	}()

	deadline := time.After(2 * time.Second)
	for {
		if republishCalls.Load() >= 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("driver did not republish local heads on the first successful tick")
		case <-time.After(5 * time.Millisecond):
		}
	}

	time.Sleep(remoteSyncDriverRepublishInterval / 2)
	if got := republishCalls.Load(); got != 1 {
		t.Fatalf("driver republished before throttle elapsed: got %d calls, want 1", got)
	}

	deadline = time.After(2 * time.Second)
	for {
		if republishCalls.Load() >= 2 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("driver did not republish after throttle elapsed")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// TestRemoteSyncDriver_AdvancesCursorPastDeferredNeedsBaseline: unlike
// ImportRetryable, ImportDeferredNeedsBaseline is a cursor-ADVANCING outcome —
// a lane=live conversation delta whose parent is unknown can never succeed by
// refetching (recovery arrives via the retained-lane baseline instead), so the
// driver must walk past it rather than wedging the branch on it.
func TestRemoteSyncDriver_AdvancesCursorPastDeferredNeedsBaseline(t *testing.T) {
	oldWarm, oldInt := remoteSyncDriverWarmup, remoteSyncDriverInterval
	remoteSyncDriverWarmup = 2 * time.Millisecond
	remoteSyncDriverInterval = 10 * time.Millisecond
	defer func() { remoteSyncDriverWarmup, remoteSyncDriverInterval = oldWarm, oldInt }()

	_, pub, _ := keys.NewDeviceKey()
	client := &fakeDriverClient{
		enumerateResult: proto.RemoteEnumerateResult{
			Namespaces: []proto.RemoteNamespaceManifest{{
				NamespaceID: "ns-1",
				Branches:    []proto.RemoteBranchManifest{{BranchID: "main", TipEventID: "e3"}},
			}},
		},
		// One page [e1, e2, e3]; e2 is deferred (needs baseline) by the sink.
		fetchResults: []proto.RemoteFetchResult{
			{Events: []proto.RemoteEvent{{EventID: "e1"}, {EventID: "e2"}, {EventID: "e3"}}, NextCursor: ""},
		},
	}
	sink := func(evs []proto.RemoteEvent) []syncd.ImportOutcome {
		out := make([]syncd.ImportOutcome, len(evs))
		for i, ev := range evs {
			if ev.EventID == "e2" {
				out[i] = syncd.ImportDeferredNeedsBaseline
			} else {
				out[i] = syncd.ImportApplied
			}
		}
		return out
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	defer func() { cancel(); <-done }()
	go func() {
		defer close(done)
		runRemoteSyncDriver(ctx, client, func() ([keys.X25519KeySize]byte, error) { return pub, nil }, sink, nil, noopLogger{}, nil)
	}()

	// The first tick fetches from "" and must advance the cursor THROUGH the
	// deferred e2 up to the tip (e3): every later tick then sees cursor == tip
	// and fetches nothing. If the deferral wrongly pinned the cursor, the next
	// ticks would keep refetching (Since == "e1").
	deadline := time.After(2 * time.Second)
	for {
		client.mu.Lock()
		n := len(client.fetchSince)
		client.mu.Unlock()
		if n >= 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("driver never fetched")
		case <-time.After(5 * time.Millisecond):
		}
	}
	time.Sleep(100 * time.Millisecond) // ~10 further ticks at the shrunken interval
	client.mu.Lock()
	since := append([]string(nil), client.fetchSince...)
	client.mu.Unlock()
	if len(since) != 1 || since[0] != "" {
		t.Fatalf("cursor did not advance past the deferred event: Fetch Since calls = %v, want exactly one fetch from %q", since, "")
	}
}

// TestRemoteSyncDriver_DoesNotAdvanceCursorPastFailedImport is the B2 fix: a
// per-event transient import failure (ImportRetryable) must NOT advance the
// resume cursor past it — otherwise the failed event is never refetched and is
// lost permanently (FR-03.13, lossless replication). The driver must stop at the
// first Retryable, leave the cursor at the last durably-consumed event, and
// refetch FROM there on the next tick.
func TestRemoteSyncDriver_DoesNotAdvanceCursorPastFailedImport(t *testing.T) {
	oldWarm, oldInt := remoteSyncDriverWarmup, remoteSyncDriverInterval
	remoteSyncDriverWarmup = 2 * time.Millisecond
	remoteSyncDriverInterval = 10 * time.Millisecond
	// Shrink the failed-import backoff (package vars for exactly this reason)
	// so the refetch retry lands well inside the test's 2s window.
	oldBase, oldMax := remoteFetchBackoffBase, remoteFetchBackoffMax
	remoteFetchBackoffBase, remoteFetchBackoffMax = 20*time.Millisecond, 50*time.Millisecond
	defer func() {
		remoteSyncDriverWarmup, remoteSyncDriverInterval = oldWarm, oldInt
		remoteFetchBackoffBase, remoteFetchBackoffMax = oldBase, oldMax
	}()

	_, pub, _ := keys.NewDeviceKey()
	client := &fakeDriverClient{
		enumerateResult: proto.RemoteEnumerateResult{
			Namespaces: []proto.RemoteNamespaceManifest{{
				NamespaceID: "ns-1",
				Branches:    []proto.RemoteBranchManifest{{BranchID: "main", TipEventID: "e3"}},
			}},
		},
		// One page [e1, e2, e3], NextCursor empty (single page).
		fetchResults: []proto.RemoteFetchResult{
			{Events: []proto.RemoteEvent{{EventID: "e1"}, {EventID: "e2"}, {EventID: "e3"}}, NextCursor: ""},
		},
	}

	// importInbound classifies e2 as a transient failure; e1 and e3 applied.
	sink := func(evs []proto.RemoteEvent) []syncd.ImportOutcome {
		out := make([]syncd.ImportOutcome, len(evs))
		for i, ev := range evs {
			if ev.EventID == "e2" {
				out[i] = syncd.ImportRetryable
			} else {
				out[i] = syncd.ImportApplied
			}
		}
		return out
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	defer func() { cancel(); <-done }()
	go func() {
		defer close(done)
		runRemoteSyncDriver(ctx, client, func() ([keys.X25519KeySize]byte, error) { return pub, nil }, sink, nil, noopLogger{}, nil)
	}()

	// After the first tick the cursor must be e1 (stopped at e2), so the NEXT
	// tick's Fetch resumes from Since == e1 (refetching the failed e2). Wait for
	// a Fetch whose Since is "e1".
	deadline := time.After(2 * time.Second)
	for {
		client.mu.Lock()
		refetched := false
		for _, s := range client.fetchSince {
			if s == "e1" {
				refetched = true
				break
			}
		}
		client.mu.Unlock()
		if refetched {
			return
		}
		select {
		case <-deadline:
			client.mu.Lock()
			since := append([]string(nil), client.fetchSince...)
			client.mu.Unlock()
			t.Fatalf("cursor advanced past failed import: Fetch Since values were %v, expected a refetch from e1", since)
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// TestRemoteRetainedBackfillTicker_ConnectedGated pins the slow retained-
// baseline backfill trickle's daemon wiring: the ticker must NOT run the
// orchestrator's backfill pass while the plugin is not connected (publishing
// would only pile events into the outbox against a dead transport), and must
// start running it once ConnState reports "connected".
func TestRemoteRetainedBackfillTicker_ConnectedGated(t *testing.T) {
	old := remoteRetainedBackfillInterval
	remoteRetainedBackfillInterval = 10 * time.Millisecond
	defer func() { remoteRetainedBackfillInterval = old }()

	var connState atomic.Value
	connState.Store("reconnecting")
	var calls atomic.Int64
	backfill := func(context.Context) (int, error) {
		calls.Add(1)
		return 3, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	defer func() { cancel(); <-done }()
	go func() {
		defer close(done)
		runRemoteRetainedBackfillTicker(ctx, func() string {
			s, _ := connState.Load().(string)
			return s
		}, backfill, noopLogger{})
	}()

	time.Sleep(80 * time.Millisecond)
	if got := calls.Load(); got != 0 {
		t.Fatalf("backfill ran while not connected: got %d calls, want 0", got)
	}

	connState.Store("connected")
	deadline := time.After(2 * time.Second)
	for calls.Load() < 1 {
		select {
		case <-deadline:
			t.Fatal("backfill ticker did not run after the plugin connected")
		case <-time.After(5 * time.Millisecond):
		}
	}
}
