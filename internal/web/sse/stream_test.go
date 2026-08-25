package sse

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestStreamSetsSSEContentType(t *testing.T) {
	bus := NewBus()
	s := New(bus)

	srv := httptest.NewServer(http.HandlerFunc(s.ServeHTTP))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if got := resp.Header.Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", got)
	}
	if got := resp.Header.Get("Cache-Control"); !strings.Contains(got, "no-cache") {
		t.Errorf("Cache-Control = %q, want no-cache present", got)
	}
}

func TestStreamEmitsPublishedEvents(t *testing.T) {
	bus := NewBus()
	s := New(bus)

	srv := httptest.NewServer(http.HandlerFunc(s.ServeHTTP))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	// Publish after the connection is up — wait a beat so the
	// handler's Subscribe call has run.
	go func() {
		time.Sleep(50 * time.Millisecond)
		bus.PublishKind(KindArtifactSynced, map[string]string{"artifactId": "abc"})
	}()

	rd := bufio.NewReader(resp.Body)
	frame, err := readSSEFrame(rd, 2*time.Second)
	if err != nil {
		t.Fatalf("readSSEFrame: %v", err)
	}
	if !strings.Contains(frame, "event: artifact.synced") {
		t.Errorf("frame missing kind: %q", frame)
	}
	if !strings.Contains(frame, "artifactId") {
		t.Errorf("frame missing body: %q", frame)
	}
}

// The SSE `data:` line must be the event BODY itself, not the whole Event
// wrapper — the web UI parses data as the kind-specific payload (it reads
// kind from the `event:` line and seq from `id:`). Emitting the wrapper put
// the real fields one level too deep, so every live row rendered blank.
func TestStreamDataLineIsBodyNotWrapper(t *testing.T) {
	bus := NewBus()
	s := New(bus)
	srv := httptest.NewServer(http.HandlerFunc(s.ServeHTTP))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	go func() {
		time.Sleep(50 * time.Millisecond)
		bus.PublishKind(KindArtifactSynced, map[string]string{"source": "claude-code", "artifactId": "abc", "name": "CLAUDE.md"})
	}()

	frame, err := readSSEFrame(bufio.NewReader(resp.Body), 2*time.Second)
	if err != nil {
		t.Fatalf("readSSEFrame: %v", err)
	}

	// Pull the data: line and parse it.
	var dataLine string
	for _, ln := range strings.Split(frame, "\n") {
		if strings.HasPrefix(ln, "data: ") {
			dataLine = strings.TrimPrefix(ln, "data: ")
		}
	}
	if dataLine == "" {
		t.Fatalf("no data line in frame: %q", frame)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(dataLine), &payload); err != nil {
		t.Fatalf("data line is not JSON: %q (%v)", dataLine, err)
	}
	// The body's fields must be at the TOP level of the data payload...
	if payload["source"] != "claude-code" || payload["artifactId"] != "abc" || payload["name"] != "CLAUDE.md" {
		t.Errorf("data line is not the body payload: %v", payload)
	}
	// ...and the Event wrapper fields must NOT be present.
	for _, k := range []string{"seq", "kind", "ts", "body"} {
		if _, ok := payload[k]; ok {
			t.Errorf("data line leaked Event wrapper field %q: %v", k, payload)
		}
	}
}

func TestStreamUnsubscribesOnClose(t *testing.T) {
	bus := NewBus()
	s := New(bus)
	srv := httptest.NewServer(http.HandlerFunc(s.ServeHTTP))
	defer srv.Close()

	// Connect, then immediately cancel — the handler should
	// Unsubscribe on context cancel.
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}

	// Give the server a moment to Subscribe.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if bus.SubscriberCount() == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := bus.SubscriberCount(); got != 1 {
		t.Fatalf("server didn't Subscribe; SubscriberCount = %d", got)
	}

	cancel()
	resp.Body.Close()

	deadline = time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if bus.SubscriberCount() == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("SubscriberCount = %d after client disconnect; want 0", bus.SubscriberCount())
}

// readSSEFrame reads from rd until it has a complete SSE frame
// (terminated by a blank line) or the deadline elapses.
func readSSEFrame(rd *bufio.Reader, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	var sb strings.Builder
	for time.Now().Before(deadline) {
		line, err := rd.ReadString('\n')
		if err == io.EOF {
			return sb.String(), nil
		}
		if err != nil {
			return sb.String(), err
		}
		sb.WriteString(line)
		// Frame boundary: blank line
		if line == "\n" {
			return sb.String(), nil
		}
	}
	return sb.String(), nil
}
