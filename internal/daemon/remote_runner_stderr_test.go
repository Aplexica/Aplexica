package daemon

import (
	"sync"
	"testing"
	"time"
)

// stderrCaptureLogger captures log calls (including args) so tests can
// assert what the stderr writer forwarded. It satisfies the
// RemoteRunner.Logger interface. (The package's other test loggers discard
// the variadic args, which this test needs to inspect.)
type stderrCaptureLogger struct {
	mu   sync.Mutex
	msgs []stderrLogEntry
}

type stderrLogEntry struct {
	level string
	msg   string
	args  []any
}

func (c *stderrCaptureLogger) Info(msg string, args ...any)  { c.record("info", msg, args) }
func (c *stderrCaptureLogger) Warn(msg string, args ...any)  { c.record("warn", msg, args) }
func (c *stderrCaptureLogger) Error(msg string, args ...any) { c.record("error", msg, args) }

func (c *stderrCaptureLogger) record(level, msg string, args []any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.msgs = append(c.msgs, stderrLogEntry{level: level, msg: msg, args: args})
}

func (c *stderrCaptureLogger) lines() []stderrLogEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]stderrLogEntry, len(c.msgs))
	copy(out, c.msgs)
	return out
}

// TestStderrLogWriter_ForwardsLinesToLogger asserts the remote-plugin
// stderr writer forwards each written chunk into the daemon logger (so the
// plugin's diagnostics reach `aplexica daemon logs`), trimming a single
// trailing newline. Before the fix the runner set cmd.Stderr = nil, routing
// the plugin's stderr to the null device, so nothing was ever forwarded.
func TestStderrLogWriter_ForwardsLinesToLogger(t *testing.T) {
	cl := &stderrCaptureLogger{}
	w := &stderrLogWriter{logger: cl}

	const payload = "auth failed: token expired\n"
	n, err := w.Write([]byte(payload))
	if err != nil {
		t.Fatalf("Write returned error: %v", err)
	}
	if n != len(payload) {
		t.Fatalf("Write returned n=%d, want full byte count %d", n, len(payload))
	}

	lines := cl.lines()
	if len(lines) != 1 {
		t.Fatalf("expected exactly one forwarded log line, got %d", len(lines))
	}
	got := lines[0]
	if got.msg != "remote plugin stderr" {
		t.Fatalf("forwarded message = %q, want %q", got.msg, "remote plugin stderr")
	}
	// The trailing newline must be trimmed and the line carried as a "line" arg.
	foundLine := false
	for i := 0; i+1 < len(got.args); i += 2 {
		if got.args[i] == "line" {
			foundLine = true
			if got.args[i+1] != "auth failed: token expired" {
				t.Fatalf("forwarded line = %q, want %q (trailing newline trimmed)", got.args[i+1], "auth failed: token expired")
			}
		}
	}
	if !foundLine {
		t.Fatalf("forwarded log line missing a \"line\" arg: %+v", got.args)
	}
}

// TestStderrLogWriter_EmptyWriteNoLog asserts a blank stderr write (e.g. a
// lone newline) does not emit a noise log line, mirroring the plugin
// manager's stderrLogger behaviour.
func TestStderrLogWriter_EmptyWriteNoLog(t *testing.T) {
	cl := &stderrCaptureLogger{}
	w := &stderrLogWriter{logger: cl}

	n, err := w.Write([]byte("\n"))
	if err != nil {
		t.Fatalf("Write returned error: %v", err)
	}
	if n != 1 {
		t.Fatalf("Write returned n=%d, want 1", n)
	}
	if got := len(cl.lines()); got != 0 {
		t.Fatalf("expected no forwarded log lines for blank write, got %d", got)
	}
}

// TestStderrLogWriter_NilLoggerSafe asserts the writer tolerates a nil
// logger (the runner's Logger is documented as nil-tolerated in tests).
func TestStderrLogWriter_NilLoggerSafe(t *testing.T) {
	w := &stderrLogWriter{logger: nil}
	if _, err := w.Write([]byte("diagnostic\n")); err != nil {
		t.Fatalf("Write with nil logger returned error: %v", err)
	}
}

func TestStderrLogWriter_RateLimitsInboundRetryFlood(t *testing.T) {
	cl := &stderrCaptureLogger{}
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	w := &stderrLogWriter{
		logger:      cl,
		now:         func() time.Time { return now },
		retryWindow: time.Minute,
	}
	line := "eventsync: mqtt inbound push failed event=e1 err=remote: daemon requested delivery retry\n"
	for range 100 {
		if _, err := w.Write([]byte(line)); err != nil {
			t.Fatalf("Write returned error: %v", err)
		}
	}
	if got := len(cl.lines()); got != 1 {
		t.Fatalf("retry flood emitted %d log entries in one window, want 1", got)
	}

	now = now.Add(time.Minute)
	if _, err := w.Write([]byte(line)); err != nil {
		t.Fatalf("Write after window returned error: %v", err)
	}
	lines := cl.lines()
	if len(lines) != 3 {
		t.Fatalf("window rollover emitted %d total entries, want first + summary + current", len(lines))
	}
	if lines[1].msg != "remote plugin stderr messages suppressed" {
		t.Fatalf("rollover entry = %q, want suppression summary", lines[1].msg)
	}
	foundCount := false
	for i := 0; i+1 < len(lines[1].args); i += 2 {
		if lines[1].args[i] == "count" {
			foundCount = true
			if lines[1].args[i+1] != uint64(99) {
				t.Fatalf("suppressed count = %v, want 99", lines[1].args[i+1])
			}
		}
	}
	if !foundCount {
		t.Fatalf("suppression summary missing count: %+v", lines[1].args)
	}
}

func TestStderrLogWriter_DoesNotRateLimitOtherDiagnostics(t *testing.T) {
	cl := &stderrCaptureLogger{}
	w := &stderrLogWriter{logger: cl}
	for range 3 {
		if _, err := w.Write([]byte("certificate renewal failed\n")); err != nil {
			t.Fatalf("Write returned error: %v", err)
		}
	}
	if got := len(cl.lines()); got != 3 {
		t.Fatalf("ordinary diagnostics emitted %d entries, want 3", got)
	}
}

func TestStderrLogWriter_RateLimitsInboundEventFlood(t *testing.T) {
	cl := &stderrCaptureLogger{}
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	w := &stderrLogWriter{
		logger:      cl,
		now:         func() time.Time { return now },
		retryWindow: time.Minute,
	}
	for range 100 {
		if _, err := w.Write([]byte("eventsync: scheduler: realtime inbound ns= branch=main event=e1\n")); err != nil {
			t.Fatalf("Write returned error: %v", err)
		}
	}
	if got := len(cl.lines()); got != 1 {
		t.Fatalf("inbound event flood emitted %d log entries in one window, want 1", got)
	}
}
