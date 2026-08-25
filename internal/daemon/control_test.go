package daemon

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// shortTempDir returns a temp directory with a path short enough to use
// as a Unix-domain-socket path. macOS caps AF_UNIX paths at 104 bytes
// and t.TempDir() resolves to /var/folders/... which routinely exceeds
// that; on macOS we use /tmp directly. Linux's AF_UNIX limit is 108 and
// /tmp/Test... fits comfortably.
//
// Windows is skipped: despite AF_UNIX existing on Windows 10 1803+, binding
// a unix socket under the GitHub windows-latest runner's temp path fails
// with "bind: invalid argument" (observed in CI). The control socket is the
// only thing these tests exercise, so they can't run on that runner; the
// daemon's control transport is covered on the Linux + macOS CI legs. Every
// control-socket test routes through this helper, so the skip lives here.
func shortTempDir(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("unix-domain-socket control tests unsupported on the Windows CI runner (AF_UNIX bind fails)")
	}
	if runtime.GOOS == "darwin" {
		dir, err := os.MkdirTemp("/tmp", "apdtest")
		require.NoError(t, err)
		t.Cleanup(func() { os.RemoveAll(dir) })
		return dir
	}
	return t.TempDir()
}

func TestControl_StatusRoundTrip(t *testing.T) {
	dir := shortTempDir(t)
	sockPath := filepath.Join(dir, "test.sock")

	srv := NewControlServer(sockPath, &StatusInfo{
		PID:        12345,
		StartedAt:  time.Now().Add(-time.Hour),
		WatchedDir: "/proj",
	}, nil)
	require.NoError(t, srv.Start())
	defer srv.Stop()

	// Give the server a moment to bind.
	time.Sleep(50 * time.Millisecond)

	resp, err := SendCommand(sockPath, Request{Command: "status"})
	require.NoError(t, err)
	require.True(t, resp.OK)
	require.NotNil(t, resp.Data)
	// resp.Data is a map[string]any from JSON unmarshaling.
	d, ok := resp.Data.(map[string]any)
	require.True(t, ok)
	require.Equal(t, float64(12345), d["pid"]) // JSON numbers decode as float64
	require.Equal(t, "/proj", d["watchedDir"])
}

// TestControl_StatusReportsRotatedLocalDeviceID covers the status leg: a
// (re-)pair rotates the cloud device id while the daemon runs, and CLI
// adapter construction adopts whatever the status response reports — so the
// response must carry the rotating override, never a construction-time seed
// frozen at boot (which would hand every `aplexica import` the RETIRED
// identity). An empty override must not erase the seed.
func TestControl_StatusReportsRotatedLocalDeviceID(t *testing.T) {
	dir := shortTempDir(t)
	sockPath := filepath.Join(dir, "test.sock")

	srv := NewControlServer(sockPath, &StatusInfo{
		PID:           1,
		LocalDeviceID: "11111111-1111-4111-8111-111111111111",
	}, nil)
	require.NoError(t, srv.Start())
	defer srv.Stop()
	time.Sleep(50 * time.Millisecond)

	status := func() string {
		resp, err := SendCommand(sockPath, Request{Command: "status"})
		require.NoError(t, err)
		require.True(t, resp.OK)
		d, ok := resp.Data.(map[string]any)
		require.True(t, ok)
		id, _ := d["localDeviceId"].(string)
		return id
	}

	require.Equal(t, "11111111-1111-4111-8111-111111111111", status(),
		"before any rotation the boot seed must be reported")

	srv.SetLocalDeviceID("22222222-2222-4222-8222-222222222222")
	require.Equal(t, "22222222-2222-4222-8222-222222222222", status(),
		"a rotated identity must replace the boot seed in the status response")

	srv.SetLocalDeviceID("")
	require.Equal(t, "22222222-2222-4222-8222-222222222222", status(),
		"an empty id must not erase a known identity")
}

func TestControl_GenerationActivationRequestIsContentFreeWakeup(t *testing.T) {
	dir := shortTempDir(t)
	sockPath := filepath.Join(dir, "test.sock")
	srv := NewControlServer(sockPath, &StatusInfo{PID: 1}, nil)
	requested := make(chan struct{}, 1)
	srv.SetGenerationActivationRequester(func() { requested <- struct{}{} })
	require.NoError(t, srv.Start())
	defer srv.Stop()

	resp, err := SendCommand(sockPath, Request{Command: "generation-activation-request"})
	require.NoError(t, err)
	require.True(t, resp.OK)
	select {
	case <-requested:
	case <-time.After(time.Second):
		t.Fatal("generation activation driver was not requested")
	}
}

func TestControl_DeviceTransitionSubmitForwardsExactOpaqueBytes(t *testing.T) {
	dir := shortTempDir(t)
	sockPath := filepath.Join(dir, "test.sock")
	srv := NewControlServer(sockPath, &StatusInfo{PID: 1}, nil)
	want := []byte(`{"version":1,"signed":"opaque"}`)
	received := make(chan []byte, 1)
	srv.SetDeviceTransitionSubmitter(func(ctx context.Context, blob []byte) error {
		require.NoError(t, ctx.Err())
		received <- append([]byte(nil), blob...)
		return nil
	})
	require.NoError(t, srv.Start())
	defer srv.Stop()

	resp, err := SendCommandWithTimeout(sockPath, Request{Command: "device-transition-submit", PlanBlob: want}, time.Second)
	require.NoError(t, err)
	require.True(t, resp.OK)
	require.Equal(t, want, <-received)
}

func TestControl_DeviceTransitionSubmitFailsClosedWithoutServiceOrPlan(t *testing.T) {
	dir := shortTempDir(t)
	sockPath := filepath.Join(dir, "test.sock")
	srv := NewControlServer(sockPath, &StatusInfo{PID: 1}, nil)
	require.NoError(t, srv.Start())
	defer srv.Stop()

	resp, err := SendCommand(sockPath, Request{Command: "device-transition-submit", PlanBlob: []byte("opaque")})
	require.NoError(t, err)
	require.False(t, resp.OK)
	require.Contains(t, resp.Error, "unavailable")

	srv.SetDeviceTransitionSubmitter(func(context.Context, []byte) error { return nil })
	resp, err = SendCommand(sockPath, Request{Command: "device-transition-submit"})
	require.NoError(t, err)
	require.False(t, resp.OK)
	require.Contains(t, resp.Error, "required")
}

// fakeActivity is a minimal Activity provider for the live-overlay test.
type fakeActivity struct {
	at           time.Time
	pending      int
	states       map[string]string
	errs         map[string]string
	deferred     []map[string]any
	dropped      int
	suppressions []map[string]any
	syncDisabled bool
}

func (f fakeActivity) LastActivity() time.Time                 { return f.at }
func (f fakeActivity) PendingImports() int                     { return f.pending }
func (f fakeActivity) AdapterStates() map[string]string        { return f.states }
func (f fakeActivity) AdapterLastErrors() map[string]string    { return f.errs }
func (f fakeActivity) PendingProjects() []map[string]any       { return nil }
func (f fakeActivity) RefanOutByProject(_ string) (int, error) { return 0, nil }
func (f fakeActivity) MaterializeConversationBranch(_, _, _ string) (string, bool, error) {
	return "/tmp/materialized.jsonl", true, nil
}
func (f fakeActivity) DeferredMaterializations() []map[string]any { return f.deferred }
func (f fakeActivity) DropDeferredMaterializations(_, _ string) (int, error) {
	return f.dropped, nil
}
func (f fakeActivity) SyncSuppressions() []map[string]any { return f.suppressions }
func (f fakeActivity) SyncStructurallyDisabled() bool     { return f.syncDisabled }

func TestControl_StatusOverlaysLastActivity(t *testing.T) {
	dir := shortTempDir(t)
	sockPath := filepath.Join(dir, "test.sock")

	want := time.Date(2026, 5, 22, 14, 0, 0, 0, time.UTC)
	srv := NewControlServer(sockPath, &StatusInfo{
		PID:        12345,
		StartedAt:  time.Now().Add(-time.Hour),
		WatchedDir: "/proj",
	}, fakeActivity{
		at:      want,
		pending: 7,
		states:  map[string]string{"claudecode": "active", "hermes": "idle"},
		errs:    map[string]string{"openclaw": "permission denied: ~/foo"},
	})
	require.NoError(t, srv.Start())
	defer srv.Stop()
	time.Sleep(50 * time.Millisecond)

	resp, err := SendCommand(sockPath, Request{Command: "status"})
	require.NoError(t, err)
	require.True(t, resp.OK)
	d, ok := resp.Data.(map[string]any)
	require.True(t, ok)
	got, _ := d["lastActivity"].(string)
	require.Equal(t, want.Format(time.RFC3339Nano), got)
	require.Equal(t, float64(7), d["pendingImports"])
	// AdapterStates + AdapterLastErrors (v0.51.0).
	states, ok := d["adapterStates"].(map[string]any)
	require.True(t, ok, "adapterStates should be present and an object")
	require.Equal(t, "active", states["claudecode"])
	require.Equal(t, "idle", states["hermes"])
	errs, ok := d["adapterLastErrors"].(map[string]any)
	require.True(t, ok, "adapterLastErrors should be present and an object")
	require.Equal(t, "permission denied: ~/foo", errs["openclaw"])
}

func TestControl_MaterializeConversationBranch(t *testing.T) {
	dir := shortTempDir(t)
	sockPath := filepath.Join(dir, "test.sock")

	srv := NewControlServer(sockPath, &StatusInfo{}, fakeActivity{})
	require.NoError(t, srv.Start())
	defer srv.Stop()
	time.Sleep(50 * time.Millisecond)

	resp, err := SendCommand(sockPath, Request{
		Command:    "materialize",
		ArtifactID: "conv-1",
		Agent:      "codex",
		Branch:     "review",
	})
	require.NoError(t, err)
	require.True(t, resp.OK)
	d, ok := resp.Data.(map[string]any)
	require.True(t, ok)
	require.Equal(t, "/tmp/materialized.jsonl", d["path"])
	require.Equal(t, true, d["materialized"])
}

func TestControl_StopShutsDownServer(t *testing.T) {
	dir := shortTempDir(t)
	sockPath := filepath.Join(dir, "test.sock")

	srv := NewControlServer(sockPath, &StatusInfo{
		PID:        0,
		StartedAt:  time.Now(),
		WatchedDir: "/x",
	}, nil)
	require.NoError(t, srv.Start())

	time.Sleep(50 * time.Millisecond)

	resp, err := SendCommand(sockPath, Request{Command: "stop"})
	require.NoError(t, err)
	require.True(t, resp.OK)

	// Wait for server to actually shut down.
	select {
	case <-srv.Done():
		// good
	case <-time.After(time.Second):
		t.Fatal("server did not shut down within 1s after stop command")
	}
}

func TestControl_StatusOverlaysStorePressure(t *testing.T) {
	dir := shortTempDir(t)
	sockPath := filepath.Join(dir, "test.sock")

	srv := NewControlServer(sockPath, &StatusInfo{PID: 1, WatchedDir: "/proj"}, nil)
	// Wire a provider reporting the store over the high watermark but under
	// the emergency ceiling, with the honest split showing the
	// watermark unreachable (pinned bytes alone exceed it).
	srv.SetPressureProvider(func() StorePressure {
		return StorePressure{
			StoreBytes:              9_000_000_000,
			StoreMaxBytes:           10_000_000_000,
			StoreHighWatermarkBytes: 8_000_000_000,
			StoreReclaimableBytes:   200_000_000,
			StorePinnedBytes:        8_800_000_000,
			StoreEventLogBytes:      8_500_000_000,
			OverHighWatermark:       true,
			WatermarkUnreachable:    true,
		}
	})
	require.NoError(t, srv.Start())
	defer srv.Stop()
	time.Sleep(50 * time.Millisecond)

	resp, err := SendCommand(sockPath, Request{Command: "status"})
	require.NoError(t, err)
	require.True(t, resp.OK)
	d, ok := resp.Data.(map[string]any)
	require.True(t, ok)
	require.Equal(t, float64(9_000_000_000), d["storeBytes"])
	require.Equal(t, float64(10_000_000_000), d["storeMaxBytes"])
	require.Equal(t, float64(8_000_000_000), d["storeHighWatermarkBytes"])
	require.Equal(t, float64(200_000_000), d["storeReclaimableBytes"])
	require.Equal(t, float64(8_800_000_000), d["storePinnedBytes"])
	require.Equal(t, float64(8_500_000_000), d["storeEventLogBytes"])
	require.Equal(t, true, d["overHighWatermark"])
	require.Equal(t, true, d["storeWatermarkUnreachable"])
	// overEmergency is false → omitempty drops it from the wire.
	_, hasEmergency := d["overEmergency"]
	require.False(t, hasEmergency)
}

func TestControl_StatusNoPressureProvider_OmitsStoreFields(t *testing.T) {
	dir := shortTempDir(t)
	sockPath := filepath.Join(dir, "test.sock")

	// No provider wired (the disabled / nil case).
	srv := NewControlServer(sockPath, &StatusInfo{PID: 1, WatchedDir: "/proj"}, nil)
	require.NoError(t, srv.Start())
	defer srv.Stop()
	time.Sleep(50 * time.Millisecond)

	resp, err := SendCommand(sockPath, Request{Command: "status"})
	require.NoError(t, err)
	require.True(t, resp.OK)
	d, ok := resp.Data.(map[string]any)
	require.True(t, ok)
	_, hasBytes := d["storeBytes"]
	require.False(t, hasBytes, "nil pressure provider must leave store fields zero (omitted)")
	_, hasMax := d["storeMaxBytes"]
	require.False(t, hasMax)
}

func TestControl_UnknownCommand_ReturnsError(t *testing.T) {
	dir := shortTempDir(t)
	sockPath := filepath.Join(dir, "test.sock")

	srv := NewControlServer(sockPath, &StatusInfo{}, nil)
	require.NoError(t, srv.Start())
	defer srv.Stop()

	time.Sleep(50 * time.Millisecond)

	resp, err := SendCommand(sockPath, Request{Command: "unknown-command"})
	require.NoError(t, err)
	require.False(t, resp.OK)
	require.Contains(t, resp.Error, "unknown-command")
}

func TestControl_SendCommand_NoSocket_Errors(t *testing.T) {
	_, err := SendCommand("/nonexistent/socket", Request{Command: "status"})
	require.Error(t, err)
	// Should be a connect error, not a server-side error.
	var nerr *net.OpError
	if !errors.As(err, &nerr) {
		// Some platforms wrap differently — just assert error mentions
		// the path or connection.
		require.Contains(t, err.Error(), "/nonexistent/socket")
	}
}

func TestValidateBackfillScope(t *testing.T) {
	require.NoError(t, ValidateBackfillScope("", false))
	require.NoError(t, ValidateBackfillScope("local", false))
	require.NoError(t, ValidateBackfillScope("local", true))

	// Cloud is refused REGARDLESS of the reserved gate — only the error's
	// naming differs — so a future implementation cannot be enabled by
	// config alone against a daemon that doesn't support it.
	err := ValidateBackfillScope("cloud", false)
	require.ErrorContains(t, err, "sync.cloudBackfill")
	require.ErrorContains(t, err, "reserved")
	err = ValidateBackfillScope("cloud", true)
	require.ErrorContains(t, err, "not implemented")

	require.ErrorContains(t, ValidateBackfillScope("global", false), "unknown backfill scope")
}

func TestControl_BackfillDispatch(t *testing.T) {
	dir := shortTempDir(t)
	sockPath := filepath.Join(dir, "test.sock")
	srv := NewControlServer(sockPath, &StatusInfo{PID: 1, StartedAt: time.Now()}, nil)
	require.NoError(t, srv.Start())
	defer srv.Stop()
	time.Sleep(50 * time.Millisecond)

	// Unwired: the command must fail closed, not panic.
	resp, err := SendCommand(sockPath, Request{Command: "backfill", Depth: -1})
	require.NoError(t, err)
	require.False(t, resp.OK)
	require.Contains(t, resp.Error, "not wired")

	// Wired: every wire field must reach the runner intact.
	type call struct {
		agents []string
		depth  int
		scope  string
		dryRun bool
	}
	var got call
	srv.SetBackfillRunner(func(agents []string, depth int, scope string, dryRun bool) (any, error) {
		got = call{agents: agents, depth: depth, scope: scope, dryRun: dryRun}
		return map[string]any{"conversations": 7}, nil
	})
	resp, err = SendCommand(sockPath, Request{
		Command: "backfill",
		Agents:  []string{"claude-code", "codex"},
		Depth:   -1,
		Scope:   "local",
		DryRun:  true,
	})
	require.NoError(t, err)
	require.True(t, resp.OK)
	require.Equal(t, call{agents: []string{"claude-code", "codex"}, depth: -1, scope: "local", dryRun: true}, got)
	data, ok := resp.Data.(map[string]any)
	require.True(t, ok)
	require.EqualValues(t, 7, data["conversations"])

	// A runner error surfaces as OK:false with the message.
	srv.SetBackfillRunner(func([]string, int, string, bool) (any, error) {
		return nil, ValidateBackfillScope("cloud", false)
	})
	resp, err = SendCommand(sockPath, Request{Command: "backfill", Scope: "cloud", Depth: -1})
	require.NoError(t, err)
	require.False(t, resp.OK)
	require.Contains(t, resp.Error, "sync.cloudBackfill")
}
