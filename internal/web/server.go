package web

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"html"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aplexica/aplexica/internal/privatefs"
	"github.com/aplexica/aplexica/internal/web/auth"
	webembed "github.com/aplexica/aplexica/internal/web/embed"
	"github.com/aplexica/aplexica/internal/web/middleware"
)

// portInfoFilename is the well-known basename of the on-disk port-info
// file inside the daemon's state directory. Kept as a package constant
// so callers (the tray, the `aplexica web port` CLI, etc.) can build
// the same path without depending on the Server type.
const portInfoFilename = "portinfo.json"

// httpReadHeaderTimeout caps the time the server will spend reading
// request headers before returning a timeout. The 5-second budget is
// the standard "Slowloris" mitigation in the Go HTTP stack; loopback
// callers complete header reads in microseconds, so the budget is
// effectively infinite for legitimate traffic.
const httpReadHeaderTimeout = 5 * time.Second

// shutdownGracePeriod bounds how long the HTTP server waits for in-
// flight requests to drain when Start's context is cancelled. The
// SSE /events stream (W5) holds open connections, so a short grace
// avoids stalling daemon shutdown indefinitely; in-flight REST
// handlers should complete well within the budget.
const shutdownGracePeriod = 5 * time.Second

// defaultTokenSweepInterval is the cadence at which the server's
// lifecycle goroutine evicts expired bootstrap tokens from the in-memory
// TokenStore. It mirrors auth.DefaultTokenTTL: an entry is dead within
// one TTL of issuance, so sweeping at the TTL keeps the table bounded
// without spending more wakeups than necessary. The tray issues a token
// on every "Open Aplexica" click but the browser consumes at most one,
// so without this sweep the surplus entries (and their per-entry argon2
// hash+salt) would accumulate for the daemon's whole lifetime and slow
// every subsequent Consume. See internal/web/auth/token.go SweepExpired.
const defaultTokenSweepInterval = auth.DefaultTokenTTL

// HandlerRegistrar is implemented by subpackages (auth, api, sse) that
// want to attach their routes to the server's multiplexer. Decoupling
// keeps `web` free of import cycles back to its own auth/api children;
// each child package exposes a single Register function that the
// daemon's startup path calls before Start.
//
// Server.Use registers a HandlerRegistrar before Start; Server.Start
// runs the registered functions in order against the embedded
// `*http.ServeMux`.
type HandlerRegistrar interface {
	Register(mux *http.ServeMux)
}

// Options configures the local web listener. Set by the daemon at
// startup from internal/daemon/config.WebConfig (with WebBind /
// WebEnabled applying their default fallbacks).
type Options struct {
	// Bind is the loopback address: "127.0.0.1" or "::1". The
	// listener constructor refuses anything else (LAN access is
	// explicitly deferred to V2).
	Bind string

	// Port is the TCP port to listen on. 0 selects a random
	// ephemeral port (49152-65535); the chosen port is written into
	// portinfo.json on successful bind.
	Port int

	// PortInfoDir is the directory that receives portinfo.json. The
	// daemon passes its state-dir (~/.aplexica/state by default).
	PortInfoDir string

	// Version is the daemon's version string written into portinfo
	// for upgrade-mismatch detection by tooling.
	Version string

	// SessionTTL overrides the session lifetime. Zero falls back to
	// auth.DefaultSessionTTL. The daemon passes auth.LocalSessionTTL so
	// the single-user local session is effectively long-lived.
	SessionTTL time.Duration

	// SessionStorePath, when non-empty, persists the session table to
	// this file so sessions survive a daemon restart (a page refresh then
	// re-authenticates without a fresh bootstrap token). The file holds
	// bearer-equivalent session IDs — pass a path inside a user-private
	// directory (the daemon uses its 0700 state-dir). Empty keeps the
	// store purely in-memory (cleared on restart).
	SessionStorePath string
}

// Server owns the loopback HTTP listener, the request multiplexer, and
// the lifecycle that wires them together. It is constructed by
// NewServer; routes are attached either directly (via Mux, ProtectedMux)
// or through HandlerRegistrar implementations passed to Use /
// UseProtected before Start.
type Server struct {
	opts          Options
	mux           *http.ServeMux // public root: /healthz, /api/auth/*, SPA
	protectedMux  *http.ServeMux // mounted under RequireSession+RequireCSRF at /api/
	regs          []HandlerRegistrar
	protectedRegs []HandlerRegistrar
	port          atomic.Int32
	httpSrv       *http.Server
	startOnce     sync.Once
	startCalled   bool

	// sweepInterval is the cadence of the expired-bootstrap-token
	// housekeeping goroutine started in run(). Defaulted in NewServer to
	// defaultTokenSweepInterval; tests override it for a fast, deterministic
	// sweep.
	sweepInterval time.Duration

	tokens   *auth.TokenStore
	sessions *auth.SessionStore
	instance Instance
}

// NewServer constructs a Server with a fresh multiplexer and a /healthz
// route already attached. It does NOT bind a listener — call Start.
//
// Returns an error when opts.PortInfoDir is empty; binding location is
// a structural requirement, not a runtime fallback, because tooling
// expects portinfo.json to exist at a predictable path.
func NewServer(opts Options) (*Server, error) {
	if opts.PortInfoDir == "" {
		return nil, errors.New("web: NewServer requires Options.PortInfoDir")
	}
	if opts.Bind == "" {
		// Defensive: callers should pass the resolved daemon.WebBind
		// value; defaulting here keeps NewServer usable from tests
		// that construct Options directly.
		opts.Bind = "127.0.0.1"
	}
	instance, err := NewInstance()
	if err != nil {
		return nil, err
	}

	sessionTTL := opts.SessionTTL
	if sessionTTL <= 0 {
		sessionTTL = auth.DefaultSessionTTL
	}
	// Sessions are deliberately runtime-local. A cookie captured by a process
	// that occupied the prior port cannot authenticate after daemon restart.
	sessions := auth.NewSessionStoreForInstance(sessionTTL, instance.ID)
	if opts.SessionStorePath != "" {
		_ = os.Remove(opts.SessionStorePath)
	}

	s := &Server{
		opts:          opts,
		mux:           http.NewServeMux(),
		protectedMux:  http.NewServeMux(),
		sweepInterval: defaultTokenSweepInterval,
		tokens:        auth.NewTokenStore(auth.DefaultTokenTTL),
		sessions:      sessions,
		instance:      instance,
	}

	// /healthz lives on the public mux so liveness probes (and the
	// `aplexica web port` smoke check) never require a session.
	s.mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write([]byte(`{"status":"ok"}` + "\n"))
	})

	// /api/auth/* — bootstrap, whoami, logout — mounted on the
	// public mux too. These are the only paths that can mint cookies,
	// so they have to be reachable without an existing session.
	authHandler := &auth.Handler{
		Tokens:   s.tokens,
		Sessions: s.sessions,
		Version:  opts.Version,
	}
	authHandler.Register(s.mux)

	// SPA fallback: any non-/api non-/events path falls through to
	// the embedded portal bundle. Registered on the public mux
	// because the SPA must load BEFORE bootstrap (its JS is what
	// drives the bootstrap exchange). Registered last so explicit
	// routes (auth, healthz) take precedence.
	s.mux.Handle("/", webembed.Handler())

	return s, nil
}

// Use registers a HandlerRegistrar whose Register will be called
// against the PUBLIC multiplexer at Start time, before the listener
// binds. Public-mux handlers are NOT wrapped by RequireSession /
// RequireCSRF — they are reachable to anyone who can hit the listener.
// Reserve this for endpoints that genuinely must be unauthenticated
// (auth bootstrap is the canonical example; the SPA's static asset
// serving is another).
//
// Panics if called after Start.
func (s *Server) Use(r HandlerRegistrar) {
	if s.startCalled {
		panic("web: Server.Use called after Start")
	}
	s.regs = append(s.regs, r)
}

// UseProtected registers a HandlerRegistrar that runs against the
// PROTECTED multiplexer, which is mounted under RequireSession +
// RequireCSRF middleware. Use this for /api/* handlers that mutate
// state or surface authenticated data. Idempotent GETs still flow
// through RequireSession (so we don't leak server state to
// unauthenticated callers) but pass through RequireCSRF unchecked.
//
// Panics if called after Start.
func (s *Server) UseProtected(r HandlerRegistrar) {
	if s.startCalled {
		panic("web: Server.UseProtected called after Start")
	}
	s.protectedRegs = append(s.protectedRegs, r)
}

// Mux returns the public multiplexer for callers that prefer to
// attach handlers directly rather than via HandlerRegistrar.
func (s *Server) Mux() *http.ServeMux {
	if s.startCalled {
		panic("web: Server.Mux called after Start")
	}
	return s.mux
}

// ProtectedMux returns the protected sub-multiplexer for direct
// handler attachment (same semantics as UseProtected but without the
// Registrar indirection).
func (s *Server) ProtectedMux() *http.ServeMux {
	if s.startCalled {
		panic("web: Server.ProtectedMux called after Start")
	}
	return s.protectedMux
}

// Tokens exposes the bootstrap-token store. Used by the daemon's UDS
// control-server adapter so `aplexica web issue-token` can mint a URL
// in-process.
func (s *Server) Tokens() *auth.TokenStore { return s.tokens }

// Sessions exposes the session store. Used by `aplexica web
// revoke-sessions` via the control-server adapter.
func (s *Server) Sessions() *auth.SessionStore { return s.sessions }

// IssueTokenURL is a convenience that issues a fresh bootstrap token
// and returns the full URL clients should open. baseURL is derived
// from Options.Bind + Port at issue time (so the helper picks up the
// ephemeral port chosen by Start).
//
// Returns an error if Start has not yet bound the listener.
func (s *Server) IssueTokenURL() (string, error) {
	port := s.Port()
	if port == 0 {
		return "", errors.New("web: cannot issue token before listener binds")
	}
	urlOut, _, err := s.tokens.Issue(fmt.Sprintf("http://%s:%d", s.instance.Hostname, port))
	return urlOut, err
}

func (s *Server) IssueBootstrapFile() (string, error) {
	if s.Port() == 0 {
		return "", errors.New("web: cannot issue bootstrap before listener binds")
	}
	_, token, err := s.tokens.Issue(s.Origin())
	if err != nil {
		return "", err
	}
	dir := filepath.Join(s.opts.PortInfoDir, "web-bootstrap")
	if err := privatefs.EnsureDir(dir, privatefs.DirPolicy{Access: privatefs.AccessPrivate, RepairOwned: true, AllowExisting: true}); err != nil {
		return "", err
	}
	root, err := privatefs.OpenRoot(dir, privatefs.DirPolicy{Access: privatefs.AccessPrivate, RepairOwned: true, AllowExisting: true})
	if err != nil {
		return "", err
	}
	defer root.Close()
	s.sweepBootstrapFiles(root, time.Now())
	f, name, err := root.CreateTemp(".", "bootstrap-")
	if err != nil {
		return "", err
	}
	final := name + ".html"
	script := `document.getElementById('bootstrap').submit()`
	scriptSum := sha256.Sum256([]byte(script))
	scriptHash := base64.StdEncoding.EncodeToString(scriptSum[:])
	doc := fmt.Sprintf(`<!doctype html><meta name="referrer" content="no-referrer"><meta http-equiv="Content-Security-Policy" content="default-src 'none'; base-uri 'none'; frame-ancestors 'none'; script-src 'sha256-%s'; form-action %s"><form id="bootstrap" method="post" action="%s/api/auth/bootstrap-form"><input type="hidden" name="token" value="%s"><button type="submit">Open Aplexica</button></form><script>%s</script>`, scriptHash, html.EscapeString(s.Origin()), html.EscapeString(s.Origin()), html.EscapeString(token), script)
	if _, err = f.WriteString(doc); err == nil {
		err = f.Sync()
	}
	if e := f.Close(); err == nil {
		err = e
	}
	if err != nil {
		_ = root.RemoveRegular(name)
		return "", err
	}
	if err = root.InstallNoReplace(name, final); err != nil {
		return "", err
	}
	path := filepath.Join(dir, final)
	if err := s.tokens.SetCleanup(token, func() { removeBootstrapFile(path) }); err != nil {
		_ = os.Remove(path)
		return "", err
	}
	return path, nil
}

func (s *Server) sweepBootstrapFiles(root *privatefs.Root, now time.Time) {
	entries, err := root.ReadDir(".")
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "bootstrap-") || !strings.HasSuffix(entry.Name(), ".html") {
			continue
		}
		info, err := entry.Info()
		if err == nil && now.Sub(info.ModTime()) >= auth.DefaultTokenTTL {
			_ = root.RemoveRegular(entry.Name())
		}
	}
}

func removeBootstrapFile(path string) {
	if err := os.Remove(path); err == nil || errors.Is(err, os.ErrNotExist) {
		return
	}
	// Windows can transiently keep the file open in the default browser.
	// Retry asynchronously; expiration still makes any residual body inert.
	go func() {
		for i := 0; i < 4; i++ {
			time.Sleep(250 * time.Millisecond)
			if err := os.Remove(path); err == nil || errors.Is(err, os.ErrNotExist) {
				return
			}
		}
	}()
}

// Port returns the chosen listener port. 0 before Start has bound.
// Safe to call from any goroutine.
func (s *Server) Port() int {
	return int(s.port.Load())
}
func (s *Server) Origin() string {
	if s.Port() == 0 {
		return ""
	}
	return fmt.Sprintf("http://%s:%d", s.instance.Hostname, s.Port())
}

// Start binds the loopback listener and serves requests until ctx is
// cancelled. Writes portinfo.json after a successful bind; tears the
// file down on shutdown.
//
// Returns nil on graceful shutdown (context cancellation or
// http.ErrServerClosed); propagates any other error from the listener
// constructor or the http server.
//
// Start is single-shot: subsequent calls return an error. This keeps
// the lifecycle simple — a fresh Server is the unit of restart.
func (s *Server) Start(ctx context.Context) error {
	var startErr error
	s.startOnce.Do(func() {
		s.startCalled = true
		startErr = s.run(ctx)
	})
	if startErr == nil && !s.startCalled {
		// Should never happen — startOnce ran exactly once and
		// returned, so this is just a defensive marker.
		return errors.New("web: Server.Start called more than once")
	}
	return startErr
}

func (s *Server) run(ctx context.Context) error {
	for _, r := range s.regs {
		r.Register(s.mux)
	}
	for _, r := range s.protectedRegs {
		r.Register(s.protectedMux)
	}
	// Mount the protected mux under /api/ wrapped by Session + CSRF
	// enforcement. Order matters: RequireSession runs first so we
	// reject unauthenticated callers before doing CSRF token work.
	protected := middleware.RequireSession(s.sessions)(middleware.RequireCSRF(s.Origin())(s.protectedMux))
	s.mux.Handle("/api/", protected)

	v4, v6, err := NewListener(s.opts.Bind, s.opts.Port)
	if err != nil {
		return err
	}
	chosen := pickPort(v4, v6)
	if chosen == 0 {
		closeListeners(v4, v6)
		return errors.New("web: neither v4 nor v6 listener reported a port")
	}
	s.port.Store(int32(chosen))

	if err := WritePortInfo(filepath.Join(s.opts.PortInfoDir, portInfoFilename), PortInfo{
		Port:       chosen,
		Bind:       s.opts.Bind,
		Version:    s.opts.Version,
		InstanceID: s.instance.ID,
		Origin:     fmt.Sprintf("http://%s:%d", s.instance.Hostname, chosen),
	}); err != nil {
		closeListeners(v4, v6)
		return fmt.Errorf("web: write portinfo: %w", err)
	}

	handler := middleware.InstanceHostAllowlist(s.instance.Hostname, chosen)(limitUnauthenticated(middleware.CSP()(s.mux), s.sessions))
	s.httpSrv = &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: httpReadHeaderTimeout,
		ReadTimeout:       10 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}

	// Concurrently serve v4 and v6 (when available). Either returning
	// a non-ErrServerClosed error means an unrecoverable listener
	// failure; we propagate it after tearing the peer down.
	errs := make(chan error, 2)
	var serving sync.WaitGroup

	// Periodic housekeeping: evict expired bootstrap tokens so the
	// in-memory store stays bounded over the daemon's lifetime. Tied to
	// ctx so it stops on shutdown (no goroutine leak); serving.Wait below
	// ensures it has returned before run() does.
	serving.Add(1)
	go func() {
		defer serving.Done()
		s.sweepExpiredTokens(ctx)
	}()

	if v4 != nil {
		serving.Add(1)
		go func() {
			defer serving.Done()
			errs <- s.httpSrv.Serve(limitListener(v4, maxListenerConnections))
		}()
	}
	if v6 != nil {
		serving.Add(1)
		go func() {
			defer serving.Done()
			errs <- s.httpSrv.Serve(limitListener(v6, maxListenerConnections))
		}()
	}

	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), shutdownGracePeriod)
		defer cancel()
		_ = s.httpSrv.Shutdown(shutCtx)
		serving.Wait()
		return nil
	case err := <-errs:
		// One listener failed; tell the http server to shut down so
		// the other serve goroutine returns cleanly, then propagate.
		shutCtx, cancel := context.WithTimeout(context.Background(), shutdownGracePeriod)
		defer cancel()
		_ = s.httpSrv.Shutdown(shutCtx)
		serving.Wait()
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// sweepExpiredTokens runs SweepExpired on a ticker until ctx is
// cancelled. The cadence is s.sweepInterval (defaultTokenSweepInterval
// in production; overridden by tests). Returns promptly on ctx.Done()
// so daemon shutdown is not delayed.
func (s *Server) sweepExpiredTokens(ctx context.Context) {
	interval := s.sweepInterval
	if interval <= 0 {
		interval = defaultTokenSweepInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			s.tokens.SweepExpired(now)
		}
	}
}

func pickPort(v4, v6 net.Listener) int {
	for _, l := range []net.Listener{v4, v6} {
		if l == nil {
			continue
		}
		if addr, ok := l.Addr().(*net.TCPAddr); ok && addr.Port > 0 {
			return addr.Port
		}
	}
	return 0
}

func closeListeners(ls ...net.Listener) {
	for _, l := range ls {
		if l != nil {
			_ = l.Close()
		}
	}
}
