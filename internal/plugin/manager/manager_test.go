// SPDX-License-Identifier: AGPL-3.0-or-later
package manager

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/plugin/host"
	"github.com/aplexica/aplexica/internal/plugin/proto"
)

// ---------------------------------------------------------------------------
// Subprocess helper-plugin scaffolding.
//
// The manager spawns real OS processes, so its tests need a real
// executable. We use the canonical Go subprocess-test idiom: the test
// binary re-execs itself. When the manager runs <test-binary> with
// APLEXICA_PLUGIN_TEST set in the environment, the binary's TestMain (or
// the TestHelperPlugin guard) runs host.Serve over os.Stdin/os.Stdout
// instead of the normal test suite, behaving as a memory(markdown) plugin.
// ---------------------------------------------------------------------------

const helperEnvVar = "APLEXICA_PLUGIN_TEST"

// helperMode selects which behaviour the re-exec'd binary performs.
const helperModeEnvVar = "APLEXICA_PLUGIN_MODE"

// helperPIDFileEnvVar, when set, names a file the re-exec'd child writes its
// PID to on startup. Used by the zombie-reap regression test to track a
// failed-spawn child the manager never retains.
const helperPIDFileEnvVar = "APLEXICA_PLUGIN_PIDFILE"

func TestMain(m *testing.M) {
	switch os.Getenv(helperEnvVar) {
	case "1":
		os.Exit(runHelperPlugin())
	default:
		os.Exit(m.Run())
	}
}

// runHelperPlugin implements the child-process side. Returns the process
// exit code.
func runHelperPlugin() int {
	// If a PID-record path is set, drop our PID there before doing anything
	// else so a test can probe this child's liveness after a failed spawn
	// (the manager never retains a failed plugin's cmd, so the test can't
	// reach the PID any other way).
	recordHelperPID()
	switch os.Getenv(helperModeEnvVar) {
	case "crash":
		// Exit immediately with a non-zero code before speaking the
		// protocol — simulates a broken plugin.
		return 3
	case "garbage":
		// Print non-protocol bytes to stdout, then exit. proxy.Open's
		// initialize read should fail to decode this.
		_, _ = io.WriteString(os.Stdout, "this is not a json-rpc frame\n")
		return 0
	case "hang":
		// Accept stdin but never reply. Block forever (until the manager
		// kills us on the initialize timeout). Read to keep the pipe open.
		_, _ = io.Copy(io.Discard, os.Stdin)
		return 0
	default:
		// Normal memory plugin.
		if err := host.Serve(context.Background(), &helperHandler{}, os.Stdin, os.Stdout); err != nil {
			_, _ = io.WriteString(os.Stderr, "helper serve error: "+err.Error()+"\n")
			return 1
		}
		return 0
	}
}

// recordHelperPID writes the child's PID to the file named by
// helperPIDFileEnvVar, if set. Best-effort: a write failure is ignored so it
// never perturbs the behaviour the test is exercising.
func recordHelperPID() {
	path := os.Getenv(helperPIDFileEnvVar)
	if path == "" {
		return
	}
	_ = os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())), 0o644)
}

// helperHandler is a pure-translator memory(markdown) handler used by the
// re-exec'd helper plugin.
type helperHandler struct {
	host.BaseCapabilitiesHandler
}

func (h *helperHandler) Initialize(_ context.Context, _ proto.InitializeParams) (proto.InitializeResult, error) {
	return proto.InitializeResult{
		PluginName:    "helper-mem",
		PluginVersion: "0.0.1",
		ABIVersion:    proto.ABIVersion,
		Kinds:         []acf.Kind{acf.KindMemory},
		Formats:       map[acf.Kind][]string{acf.KindMemory: {"markdown"}},
	}, nil
}

func (h *helperHandler) Import(_ context.Context, p proto.ImportParams) (proto.ImportResult, error) {
	base := filepath.Base(p.NativePath)
	if base != "MEMORY.md" {
		return proto.ImportResult{}, &proto.RPCError{Code: proto.CodeUnrecognizedNativeFile, Message: "not a memory file"}
	}
	content, err := os.ReadFile(p.NativePath)
	if err != nil {
		return proto.ImportResult{}, &proto.RPCError{Code: proto.CodeIOError, Message: err.Error()}
	}
	pld, _ := acf.EncodePayload(acf.MemoryPayload{Format: "markdown", Content: string(content)})
	return proto.ImportResult{
		Imports: []proto.ImportedItem{{
			Kind:       acf.KindMemory,
			Scope:      acf.ScopeGlobal,
			Name:       base,
			SourcePath: p.NativePath,
			Payload:    json.RawMessage(pld),
		}},
	}, nil
}

func (h *helperHandler) Export(_ context.Context, p proto.ExportParams) (proto.ExportResult, error) {
	var mp acf.MemoryPayload
	if err := json.Unmarshal(p.Payload, &mp); err != nil {
		return proto.ExportResult{}, &proto.RPCError{Code: proto.CodeFormatUnsupported, Message: err.Error()}
	}
	if err := os.WriteFile(p.DestPath, []byte(mp.Content), 0o644); err != nil {
		return proto.ExportResult{}, &proto.RPCError{Code: proto.CodeIOError, Message: err.Error()}
	}
	return proto.ExportResult{Written: true}, nil
}

func (h *helperHandler) NativePath(_ context.Context, p proto.NativePathParams) (proto.NativePathResult, error) {
	if p.Artifact.Kind != acf.KindMemory {
		return proto.NativePathResult{}, nil
	}
	return proto.NativePathResult{Path: filepath.Join(p.ContextDir, p.Artifact.Name), Supports: true}, nil
}

func (h *helperHandler) HandlesFormat(_ context.Context, p proto.HandlesFormatParams) (proto.HandlesFormatResult, error) {
	return proto.HandlesFormatResult{Handles: p.Kind == acf.KindMemory && p.Format == "markdown"}, nil
}

func (h *helperHandler) Shutdown(_ context.Context, _ proto.ShutdownParams) (proto.ShutdownResult, error) {
	return proto.ShutdownResult{}, nil
}

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

func testLogger() *slog.Logger {
	// Discard plugin diagnostics so test output stays clean; bump the
	// level if you need to see warnings while debugging.
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newTempStore(t *testing.T) *acf.Store {
	t.Helper()
	s := &acf.Store{Root: filepath.Join(t.TempDir(), "store")}
	if err := s.Init(); err != nil {
		t.Fatalf("init store: %v", err)
	}
	return s
}

// installHelperPlugin writes a plugin subdirectory under root whose
// manifest's executable points at the current test binary, optionally in
// a special mode. It sets APLEXICA_PLUGIN_TEST=1 in the test process env so
// the spawned child (which inherits the env) runs as the helper plugin.
func installHelperPlugin(t *testing.T, root, sub, name, mode string) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	manifest := map[string]any{
		"manifest_version": 1,
		"name":             name,
		"version":          "0.0.1",
		"abi_version":      proto.ABIVersion,
		"executable":       self,
		"kind":             "adapter",
		"kinds":            []string{"memory"},
	}
	body, _ := json.MarshalIndent(manifest, "", "  ")
	dir := filepath.Join(root, sub)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, manifestFilename), body, 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	// The child inherits this process's environment (the manager does not
	// override cmd.Env), so set the helper markers here.
	t.Setenv(helperEnvVar, "1")
	if mode != "" {
		t.Setenv(helperModeEnvVar, mode)
	} else {
		// Ensure a previous test's mode does not leak.
		t.Setenv(helperModeEnvVar, "")
	}
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestLoad_SpawnsAndImports(t *testing.T) {
	root := t.TempDir()
	installHelperPlugin(t, root, "helper-mem", "helper-mem", "")

	store := newTempStore(t)
	m := New(root, store, "device-1", "0.26.0", testLogger())
	t.Cleanup(func() { _ = m.Close() })

	adapters, err := m.Load(context.Background(), nil)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(adapters) != 1 {
		t.Fatalf("Load() returned %d adapters, want 1", len(adapters))
	}
	a := adapters[0]
	if a.Name() != "helper-mem" {
		t.Errorf("adapter Name() = %q, want helper-mem", a.Name())
	}
	if a.Version() != "0.0.1" {
		t.Errorf("adapter Version() = %q, want 0.0.1", a.Version())
	}

	// Loaded() reflects the live plugin.
	loaded := m.Loaded()
	if len(loaded) != 1 || loaded[0].PluginName != "helper-mem" {
		t.Errorf("Loaded() = %+v, want one helper-mem entry", loaded)
	}

	// Import a MEMORY.md and confirm the daemon-side reconciler wrote an
	// artifact + a create event (proving the proxy/manager round-trip).
	src := filepath.Join(t.TempDir(), "MEMORY.md")
	if err := os.WriteFile(src, []byte("hello from plugin\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ids, err := a.Import(context.Background(), store, src)
	if err != nil {
		t.Fatalf("Import() error = %v", err)
	}
	if len(ids) != 1 || ids[0] == "" {
		t.Fatalf("Import() ids = %v, want exactly one non-empty id", ids)
	}
	art, err := store.ReadArtifact(acf.KindMemory, ids[0])
	if err != nil {
		t.Fatalf("ReadArtifact: %v", err)
	}
	if art.SourcePath != src {
		t.Errorf("artifact SourcePath = %q, want %q", art.SourcePath, src)
	}
	events, err := store.ReadEvents(acf.KindMemory, ids[0])
	if err != nil {
		t.Fatalf("ReadEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("want 1 event, got %d", len(events))
	}
	if events[0].Type != acf.EventTypeCreate {
		t.Errorf("event type = %s, want create", events[0].Type)
	}
	if events[0].Provenance.SourceAgent != "helper-mem" {
		t.Errorf("event SourceAgent = %q, want helper-mem", events[0].Provenance.SourceAgent)
	}
	if events[0].Provenance.DeviceID != "device-1" {
		t.Errorf("event DeviceID = %q, want device-1", events[0].Provenance.DeviceID)
	}
}

func TestLoad_NameInSkipIsOmitted(t *testing.T) {
	root := t.TempDir()
	installHelperPlugin(t, root, "helper-mem", "helper-mem", "")

	store := newTempStore(t)
	m := New(root, store, "", "0.0.0", testLogger())
	t.Cleanup(func() { _ = m.Close() })

	skip := map[string]struct{}{"helper-mem": {}}
	adapters, err := m.Load(context.Background(), skip)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(adapters) != 0 {
		t.Fatalf("Load() returned %d adapters, want 0 (name was in skip)", len(adapters))
	}
	if len(m.Loaded()) != 0 {
		t.Errorf("Loaded() = %v, want empty (nothing spawned)", m.Loaded())
	}
}

func TestLoad_CrashingPluginIsSkipped(t *testing.T) {
	root := t.TempDir()
	installHelperPlugin(t, root, "crasher", "crasher", "crash")

	m := New(root, newTempStore(t), "", "0.0.0", testLogger())
	t.Cleanup(func() { _ = m.Close() })

	adapters, err := m.Load(context.Background(), nil)
	if err != nil {
		t.Fatalf("Load() error = %v, want nil (bad plugin must not error Load)", err)
	}
	if len(adapters) != 0 {
		t.Fatalf("Load() returned %d adapters, want 0 (plugin crashed)", len(adapters))
	}
}

func TestLoad_GarbagePluginIsSkipped(t *testing.T) {
	root := t.TempDir()
	installHelperPlugin(t, root, "garbage", "garbage-plugin", "garbage")

	m := New(root, newTempStore(t), "", "0.0.0", testLogger())
	t.Cleanup(func() { _ = m.Close() })

	adapters, err := m.Load(context.Background(), nil)
	if err != nil {
		t.Fatalf("Load() error = %v, want nil (garbage plugin must not error Load)", err)
	}
	if len(adapters) != 0 {
		t.Fatalf("Load() returned %d adapters, want 0 (plugin emitted garbage)", len(adapters))
	}
}

func TestLoad_HangingPluginTimesOutAndIsSkipped(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping hang test in -short (waits for initialize timeout)")
	}
	root := t.TempDir()
	installHelperPlugin(t, root, "hanger", "hanger", "hang")

	m := New(root, newTempStore(t), "", "0.0.0", testLogger())
	t.Cleanup(func() { _ = m.Close() })

	start := time.Now()
	adapters, err := m.Load(context.Background(), nil)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("Load() error = %v, want nil (hang must be tolerated)", err)
	}
	if len(adapters) != 0 {
		t.Fatalf("Load() returned %d adapters, want 0 (plugin hung on initialize)", len(adapters))
	}
	// It must return roughly within the initialize timeout, not block
	// indefinitely. Allow generous slack for CI.
	if elapsed > initializeTimeout+5*time.Second {
		t.Errorf("Load() took %v, expected to bail near the %v initialize timeout", elapsed, initializeTimeout)
	}
}

// readHelperPID waits for the child to drop its PID file (it writes it on
// startup) and returns the recorded PID.
func readHelperPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(path)
		if err == nil && len(b) > 0 {
			pid, perr := strconv.Atoi(string(b))
			if perr == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("child never recorded its PID at %s", path)
	return 0
}

// assertReaped fails unless the process is no longer signalable within a short
// window. A child that was Killed but never Wait()ed stays a zombie (defunct)
// on Unix and remains signalable; reaping it via cmd.Wait() removes it from the
// process table so Signal(0) errors. This is the exact difference the
// zombie-reap fix introduces.
func assertReaped(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if processGone(pid) {
			return // reaped — good
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("child pid %d still alive after Load returned (left as a zombie — not reaped)", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// processGone reports whether pid is no longer a live process, portably.
//
// On Unix os.FindProcess always succeeds, so liveness is probed with the
// signal-0 trick (an error means the process is gone). On Windows
// os.FindProcess calls OpenProcess, which fails once the process has fully
// exited and been reaped ("OpenProcess: The parameter is incorrect") — that
// failure IS the reaped signal — and Signal(0) is not a supported liveness
// probe there, so a still-openable handle is treated as not-yet-gone and the
// caller keeps polling until the deadline.
func processGone(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return true
	}
	if runtime.GOOS == "windows" {
		return false
	}
	return proc.Signal(syscall.Signal(0)) != nil
}

// TestLoad_FailedSpawnReapsChild asserts that a plugin which fails the
// initialize handshake AFTER cmd.Start() succeeds does not leak a defunct
// (zombie) process: spawn must Wait() on the killed child. Covers both spawn
// failure branches — garbage stdout (the res.err path) and an initialize hang
// (the openCtx.Done path).
func TestLoad_FailedSpawnReapsChild(t *testing.T) {
	cases := []struct {
		name string
		mode string
		long bool // waits for the 5s initialize timeout
	}{
		{name: "garbage", mode: "garbage"},
		{name: "hang", mode: "hang", long: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.long && testing.Short() {
				t.Skip("skipping hang case in -short (waits for initialize timeout)")
			}
			root := t.TempDir()
			installHelperPlugin(t, root, tc.name, tc.name, tc.mode)
			pidFile := filepath.Join(t.TempDir(), "child.pid")
			t.Setenv(helperPIDFileEnvVar, pidFile)

			m := New(root, newTempStore(t), "", "0.0.0", testLogger())
			t.Cleanup(func() { _ = m.Close() })

			adapters, err := m.Load(context.Background(), nil)
			if err != nil {
				t.Fatalf("Load() error = %v, want nil", err)
			}
			if len(adapters) != 0 {
				t.Fatalf("Load() returned %d adapters, want 0 (plugin failed initialize)", len(adapters))
			}

			pid := readHelperPID(t, pidFile)
			assertReaped(t, pid)
		})
	}
}

func TestClose_IsIdempotentAndTerminatesChild(t *testing.T) {
	root := t.TempDir()
	installHelperPlugin(t, root, "helper-mem", "helper-mem", "")

	m := New(root, newTempStore(t), "", "0.0.0", testLogger())
	adapters, err := m.Load(context.Background(), nil)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(adapters) != 1 {
		t.Fatalf("Load() returned %d adapters, want 1", len(adapters))
	}

	// Grab the child PID before close so we can confirm it goes away.
	m.mu.Lock()
	if len(m.loaded) != 1 {
		m.mu.Unlock()
		t.Fatalf("manager tracked %d plugins, want 1", len(m.loaded))
	}
	proc := m.loaded[0].cmd.Process
	m.mu.Unlock()
	if proc == nil {
		t.Fatal("loaded plugin has no os.Process")
	}

	if err := m.Close(); err != nil {
		t.Fatalf("first Close() error = %v", err)
	}
	// Second Close must be a no-op, never panic.
	if err := m.Close(); err != nil {
		t.Fatalf("second Close() error = %v", err)
	}

	// After Close the manager forgets its plugins.
	if got := m.Loaded(); len(got) != 0 {
		t.Errorf("Loaded() after Close = %v, want empty", got)
	}

	// The child process must have terminated. Signal 0 probes liveness;
	// once reaped, Signal returns an error.
	deadline := time.Now().Add(2 * time.Second)
	for {
		if err := proc.Signal(os.Signal(nil)); err != nil {
			break // process gone — good
		}
		if time.Now().After(deadline) {
			t.Fatalf("child process %d still alive after Close", proc.Pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestLoad_MissingDirYieldsNoAdaptersNoError(t *testing.T) {
	m := New(filepath.Join(t.TempDir(), "absent"), newTempStore(t), "", "0.0.0", testLogger())
	t.Cleanup(func() { _ = m.Close() })

	adapters, err := m.Load(context.Background(), nil)
	if err != nil {
		t.Fatalf("Load() error = %v, want nil for absent dir", err)
	}
	if len(adapters) != 0 {
		t.Fatalf("Load() = %d adapters, want 0 for absent dir", len(adapters))
	}
}
