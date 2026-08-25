// SPDX-License-Identifier: AGPL-3.0-or-later

// Package manager owns the lifecycle of out-of-process adapter plugins.
//
// It discovers plugin.json manifests under a plugins root (one
// subdirectory per plugin), spawns each adapter plugin as a subprocess
// wiring the child's stdin/stdout as the JSON-RPC transport, calls
// proxy.Open to obtain an adapter.Adapter, supervises the process, and
// shuts each one down on Close.
//
// The manager is 100% additive: a missing/empty plugins dir, a name that
// collides with a built-in adapter, a disabled name, or ANY single plugin
// failing to spawn/initialize all degrade to the daemon's exact prior
// behavior — Load never returns an error for a bad plugin and never
// panics. The daemon owns store IO (a *proxy.Proxy returns pure
// translation results and the daemon-side reconciler persists them); the
// manager owns process teardown because *proxy.Proxy has no Close method.
package manager

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os/exec"
	"sync"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter"
	"github.com/aplexica/aplexica/internal/plugin/proto"
	"github.com/aplexica/aplexica/internal/plugin/proxy"
)

// initializeTimeout bounds the proxy.Open handshake. proxy.Open performs
// a blocking initialize round-trip with no timeout of its own, so a plugin
// that accepts stdin but never replies would hang Load forever. We wrap
// the spawn+Open in a context with this deadline and Kill on expiry.
const initializeTimeout = 5 * time.Second

// shutdownTimeout bounds the graceful teardown of each plugin in Close
// before the process is force-killed.
const shutdownTimeout = 3 * time.Second

// Manager owns the lifecycle of all loaded external adapter plugins.
type Manager struct {
	logger        *slog.Logger
	store         *acf.Store
	deviceID      string
	daemonVersion string
	dir           string

	mu     sync.Mutex
	loaded []*loadedPlugin
	closed bool
}

// loadedPlugin is the per-plugin handle the manager retains so it can
// report status and tear the process down on Close.
type loadedPlugin struct {
	name      string
	manifest  proto.InitializeResult
	cmd       *exec.Cmd
	transport *stdioTransport // closing this closes the child's stdin (EOF)
	proxy     *proxy.Proxy
}

// New constructs a Manager. dir is the plugins root (one subdirectory per
// plugin). A nil/zero store is allowed only in tests that don't drive an
// Import. daemonVersion is version.Version; deviceID may be "" (matches the
// daemon wiring, which can provide the device ID after initialization).
// logger may be nil, in which case slog.Default() is used.
func New(dir string, store *acf.Store, deviceID, daemonVersion string, logger *slog.Logger) *Manager {
	if logger == nil {
		logger = slog.Default()
	}
	return &Manager{
		logger:        logger,
		store:         store,
		deviceID:      deviceID,
		daemonVersion: daemonVersion,
		dir:           dir,
	}
}

// Load discovers, then spawns and initializes every adapter plugin not in
// skip, returning the ones that came up cleanly as adapter.Adapter (each a
// *proxy.Proxy). skip is the set of names already taken by built-ins or
// disabled by the user; a discovered plugin whose Manifest.Name is in skip
// is logged and omitted.
//
// Load is bulletproof against partial spawns: a missing executable, an
// instant exit, garbage on stdout, or a hang during initialize each cause
// that single plugin to be logged at WARN and skipped — Load returns a
// non-nil error ONLY for an unexpected internal failure (none today), so
// callers may ignore the error and proceed with whatever loaded. Loaded
// plugins are tracked for Close().
func (m *Manager) Load(ctx context.Context, skip map[string]struct{}) ([]adapter.Adapter, error) {
	discovered, err := m.Discover()
	if err != nil {
		// A discovery error (e.g. an unreadable dir) is non-fatal: log
		// and behave as if no plugins were found.
		m.logger.Warn("plugin/manager: discovery failed, no external plugins loaded", "error", err)
		return nil, nil
	}

	var adapters []adapter.Adapter
	for _, d := range discovered {
		name := d.Manifest.Name
		if skip != nil {
			if _, taken := skip[name]; taken {
				m.logger.Info("plugin/manager: skipping plugin (name reserved by a built-in or disabled)",
					"name", name, "dir", d.Dir)
				continue
			}
		}
		lp, lerr := m.spawn(ctx, d)
		if lerr != nil {
			m.logger.Warn("plugin/manager: plugin failed to load, skipping",
				"name", name, "dir", d.Dir, "error", lerr)
			continue
		}
		m.mu.Lock()
		m.loaded = append(m.loaded, lp)
		m.mu.Unlock()
		adapters = append(adapters, lp.proxy)
		m.logger.Info("plugin/manager: adapter plugin loaded",
			"name", lp.proxy.Name(), "version", lp.proxy.Version())
	}
	return adapters, nil
}

// spawn starts one plugin subprocess and performs the initialize
// handshake, returning a ready loadedPlugin or an error (which Load logs
// and swallows). On any failure the process is killed and its pipes
// released so nothing leaks.
func (m *Manager) spawn(ctx context.Context, d Discovered) (*loadedPlugin, error) {
	cmd := exec.CommandContext(ctx, d.Executable)
	cmd.Dir = d.Dir
	// Route plugin diagnostics to a writer that forwards each line to the
	// daemon log. The plugin MUST keep stdout reserved for protocol frames;
	// anything it prints to stderr surfaces here.
	cmd.Stderr = &stderrLogger{logger: m.logger, name: d.Manifest.Name}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, err
	}

	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, err
	}

	transport := &stdioTransport{r: stdout, w: stdin}

	// proxy.Open blocks on the initialize round-trip with no internal
	// timeout. Bound it: a plugin that accepts stdin but never replies
	// would otherwise hang Load. Run Open in a goroutine and race it
	// against a deadline; on expiry, kill the process so the blocking
	// Read unblocks and the goroutine drains.
	openCtx, cancel := context.WithTimeout(ctx, initializeTimeout)
	defer cancel()

	type openResult struct {
		p   *proxy.Proxy
		err error
	}
	resCh := make(chan openResult, 1)
	go func() {
		p, oerr := proxy.Open(openCtx, transport, m.store, m.deviceID, m.daemonVersion)
		resCh <- openResult{p: p, err: oerr}
	}()

	select {
	case res := <-resCh:
		if res.err != nil {
			// The open-goroutine has already returned (it sent res), so no
			// read races the Wait. Kill (the child may have produced garbage
			// then lived on, e.g. the hang-after-Start case) and reap so the
			// failed child is not left as a zombie.
			killProcess(cmd)
			reap(cmd)
			return nil, res.err
		}
		return &loadedPlugin{
			name:      res.p.Name(),
			manifest:  proto.InitializeResult{PluginName: res.p.Name(), PluginVersion: res.p.Version(), ABIVersion: proto.ABIVersion},
			cmd:       cmd,
			transport: transport,
			proxy:     res.p,
		}, nil
	case <-openCtx.Done():
		// Timed out (or parent ctx cancelled). Kill the child so the
		// open-goroutine's blocked Read returns; drain it (resCh is
		// buffered, so this never blocks) so no read races the Wait on the
		// stdout pipe; then reap. A Killed child of a still-running parent
		// is NOT collected by the OS until we Wait on it, so skipping the
		// Wait leaves a zombie for the daemon's whole lifetime (mirrors
		// closeOne's reap).
		killProcess(cmd)
		<-resCh
		reap(cmd)
		return nil, openCtx.Err()
	}
}

// Loaded returns the identity (name + version) of every plugin currently
// live, for status reporting and logging. Safe for concurrent use.
func (m *Manager) Loaded() []proto.InitializeResult {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]proto.InitializeResult, 0, len(m.loaded))
	for _, lp := range m.loaded {
		out = append(out, lp.manifest)
	}
	return out
}

// Close shuts every loaded plugin down gracefully: it closes the child's
// stdin (the transport's write half), which the plugin's host.Serve reads
// as EOF and returns; then it waits for the process with a timeout and
// Kills any plugin that does not exit in time. Idempotent and safe to
// defer in the daemon.
func (m *Manager) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	loaded := m.loaded
	m.loaded = nil
	m.mu.Unlock()

	for _, lp := range loaded {
		m.closeOne(lp)
	}
	return nil
}

// closeOne tears down a single plugin: close stdin to signal EOF, wait up
// to shutdownTimeout, then Kill on expiry. Best-effort; errors are logged.
func (m *Manager) closeOne(lp *loadedPlugin) {
	// Closing the transport closes the child's stdin -> host.Serve sees
	// EOF and returns -> the process exits on its own.
	if lp.transport != nil {
		_ = lp.transport.Close()
	}

	done := make(chan error, 1)
	go func() { done <- lp.cmd.Wait() }()

	select {
	case err := <-done:
		if err != nil && !isExpectedExit(err) {
			m.logger.Warn("plugin/manager: plugin exited with error",
				"name", lp.name, "error", err)
		}
	case <-time.After(shutdownTimeout):
		m.logger.Warn("plugin/manager: plugin did not exit, killing",
			"name", lp.name)
		killProcess(lp.cmd)
		// Reap so the OS does not leave a zombie; ignore the error (it
		// will report "signal: killed").
		<-done
	}
}

// killProcess force-terminates cmd's process if it is running. Safe to
// call when the process has already exited.
func killProcess(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	_ = cmd.Process.Kill()
}

// reap waits on a killed/exited child so the OS collects it. On Unix a
// child of a still-running parent stays a zombie until the parent Waits;
// the spawn failure paths call this after killProcess so a plugin that
// fails initialize does not leak a defunct process for the daemon's
// lifetime. Caller must ensure no goroutine is still reading the child's
// stdout pipe (Wait closes it). The Wait error is ignored — a Killed
// process reports "signal: killed", which is expected here.
func reap(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	_ = cmd.Wait()
}

// isExpectedExit reports whether a cmd.Wait error is a benign termination
// (e.g. the process was Killed during a timed-out shutdown) that should not
// be logged as a real fault.
func isExpectedExit(err error) bool {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return false
	}
	// A process we Killed surfaces as a non-zero exit; treat any signal-
	// induced termination as expected during teardown.
	return !exitErr.Exited()
}

// stderrLogger forwards a plugin's stderr to the daemon logger, one log
// line per write chunk. It implements io.Writer so it can be assigned to
// cmd.Stderr.
type stderrLogger struct {
	logger *slog.Logger
	name   string
}

func (s *stderrLogger) Write(p []byte) (int, error) {
	msg := string(p)
	// Trim a single trailing newline for tidier log lines.
	if n := len(msg); n > 0 && msg[n-1] == '\n' {
		msg = msg[:n-1]
	}
	if msg != "" {
		s.logger.Info("plugin stderr", "name", s.name, "line", msg)
	}
	return len(p), nil
}

// compile-time assertions.
var (
	_ io.ReadWriteCloser = (*stdioTransport)(nil)
	_ io.Writer          = (*stderrLogger)(nil)
)
