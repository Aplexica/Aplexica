package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// secureLoopbackTransport lets the cookie jar exercise browser-equivalent
// Secure-cookie behavior for *.localhost while the in-process test server
// continues to speak plain HTTP. Browsers treat localhost as a secure context,
// but net/http/cookiejar intentionally requires an https URL before it stores
// or sends a Secure cookie.
type secureLoopbackTransport struct {
	base http.RoundTripper
}

func (t secureLoopbackTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	url := *req.URL
	url.Scheme = "http"
	clone.URL = &url
	return t.base.RoundTrip(clone)
}

func secureLoopbackClient() *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{
		Jar:       jar,
		Transport: secureLoopbackTransport{base: loopbackTransport()},
	}
}

func loopbackTransport() http.RoundTripper {
	return &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			_, port, err := net.SplitHostPort(address)
			if err != nil {
				return nil, err
			}
			return (&net.Dialer{}).DialContext(ctx, network, net.JoinHostPort("127.0.0.1", port))
		},
	}
}

func instanceLoopbackClient() *http.Client {
	return &http.Client{Transport: loopbackTransport()}
}

func secureLoopbackOrigin(origin string) string {
	return "https://" + strings.TrimPrefix(origin, "http://")
}

func TestNewServerRequiresPortInfoDir(t *testing.T) {
	_, err := NewServer(Options{Bind: "127.0.0.1"})
	if err == nil {
		t.Fatal("NewServer must error when PortInfoDir is empty")
	}
}

func TestServerStartHealthzAndShutdown(t *testing.T) {
	dir := t.TempDir()

	srv, err := NewServer(Options{
		Bind:        "127.0.0.1",
		Port:        0,
		PortInfoDir: dir,
		Version:     "v0.0.0-test",
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Start(ctx) }()

	port := waitForPort(t, srv)
	defer cancel()

	// /healthz round-trip with a recognised Host
	req, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/healthz", port), nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("/healthz status = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()

	// portinfo.json was written with the chosen port
	body, err := os.ReadFile(filepath.Join(dir, "portinfo.json"))
	if err != nil {
		t.Fatalf("read portinfo.json: %v", err)
	}
	var info PortInfo
	if err := json.Unmarshal(body, &info); err != nil {
		t.Fatalf("unmarshal portinfo: %v", err)
	}
	if info.Port != port {
		t.Errorf("portinfo.Port = %d, want %d", info.Port, port)
	}
	if info.Bind != "127.0.0.1" {
		t.Errorf("portinfo.Bind = %q, want 127.0.0.1", info.Bind)
	}
	if info.Version != "v0.0.0-test" {
		t.Errorf("portinfo.Version = %q, want v0.0.0-test", info.Version)
	}

	// Shutdown is clean (no error after context cancel)
	cancel()
	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("Start returned non-clean error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Start did not return within 3s of context cancel")
	}
}

func TestServerHostAllowlistRejectsForeignHost(t *testing.T) {
	dir := t.TempDir()

	srv, err := NewServer(Options{
		Bind:        "127.0.0.1",
		Port:        0,
		PortInfoDir: dir,
		Version:     "v0.0.0-test",
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.Start(ctx) }()
	port := waitForPort(t, srv)

	// Spoof a foreign Host header — DNS rebinding simulator.
	req, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/healthz", port), nil)
	req.Host = "evil.example.com"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET with foreign Host: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMisdirectedRequest {
		t.Errorf("foreign Host status = %d, want 421", resp.StatusCode)
	}
}

func TestServerSetsCSPHeader(t *testing.T) {
	dir := t.TempDir()

	srv, err := NewServer(Options{
		Bind:        "127.0.0.1",
		Port:        0,
		PortInfoDir: dir,
		Version:     "v0.0.0-test",
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.Start(ctx) }()
	port := waitForPort(t, srv)

	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/healthz", port))
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer resp.Body.Close()
	if csp := resp.Header.Get("Content-Security-Policy"); csp == "" {
		t.Error("Content-Security-Policy header missing")
	}
}

func TestUseRegistersHandlerBeforeStart(t *testing.T) {
	dir := t.TempDir()
	srv, _ := NewServer(Options{Bind: "127.0.0.1", PortInfoDir: dir})

	srv.Use(handlerRegistrarFunc(func(mux *http.ServeMux) {
		mux.HandleFunc("/api/probe", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusAccepted)
		})
	}))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.Start(ctx) }()
	waitForPort(t, srv)

	resp, err := instanceLoopbackClient().Get(srv.Origin() + "/api/probe")
	if err != nil {
		t.Fatalf("GET /api/probe: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("/api/probe status = %d, want 202", resp.StatusCode)
	}
}

func waitForPort(t *testing.T, srv *Server) int {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if p := srv.Port(); p != 0 {
			return p
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("server never reported a port within 3s")
	return 0
}

type handlerRegistrarFunc func(*http.ServeMux)

func (f handlerRegistrarFunc) Register(mux *http.ServeMux) { f(mux) }

func TestProtectedRouteRequiresAuth(t *testing.T) {
	dir := t.TempDir()
	srv, _ := NewServer(Options{Bind: "127.0.0.1", PortInfoDir: dir, Version: "v0.0.0-test"})
	srv.UseProtected(handlerRegistrarFunc(func(mux *http.ServeMux) {
		mux.HandleFunc("/api/daemon", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		})
	}))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.Start(ctx) }()
	waitForPort(t, srv)

	resp, err := instanceLoopbackClient().Get(srv.Origin() + "/api/daemon")
	if err != nil {
		t.Fatalf("GET /api/daemon: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("unauthenticated GET /api/daemon status = %d, want 401", resp.StatusCode)
	}
}

func TestBootstrapFlowSetsCookiesAndUnlocksProtectedRoute(t *testing.T) {
	dir := t.TempDir()
	srv, _ := NewServer(Options{Bind: "127.0.0.1", PortInfoDir: dir, Version: "v0.0.0-test"})
	srv.UseProtected(handlerRegistrarFunc(func(mux *http.ServeMux) {
		mux.HandleFunc("/api/daemon", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
		})
	}))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.Start(ctx) }()
	port := waitForPort(t, srv)

	// Issue a token via the in-process accessor
	urlOut, err := srv.IssueTokenURL()
	if err != nil {
		t.Fatalf("IssueTokenURL: %v", err)
	}
	if !strings.Contains(urlOut, srv.Origin()+"/?bootstrap=") {
		t.Fatalf("URL = %q, want bootstrap form on :%d", urlOut, port)
	}
	rawToken := strings.SplitN(urlOut, "bootstrap=", 2)[1]

	// Use a cookie jar so the bootstrap response's cookies persist
	client := secureLoopbackClient()
	origin := secureLoopbackOrigin(srv.Origin())

	// POST /api/auth/bootstrap with the raw token
	body, _ := json.Marshal(map[string]string{"token": rawToken})
	bsURL := origin + "/api/auth/bootstrap"
	resp, err := client.Post(bsURL, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		buf, _ := io.ReadAll(resp.Body)
		t.Fatalf("bootstrap status = %d; body=%s", resp.StatusCode, buf)
	}
	resp.Body.Close()

	// GET /api/daemon should now succeed (GET doesn't need CSRF)
	dURL := origin + "/api/daemon"
	resp, err = client.Get(dURL)
	if err != nil {
		t.Fatalf("GET /api/daemon authenticated: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("authenticated GET status = %d, want 200", resp.StatusCode)
	}
}

func TestProtectedPostWithoutCSRFIs403(t *testing.T) {
	dir := t.TempDir()
	srv, _ := NewServer(Options{Bind: "127.0.0.1", PortInfoDir: dir, Version: "v0.0.0-test"})
	srv.UseProtected(handlerRegistrarFunc(func(mux *http.ServeMux) {
		mux.HandleFunc("/api/daemon/pause", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		})
	}))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.Start(ctx) }()
	waitForPort(t, srv)

	urlOut, _ := srv.IssueTokenURL()
	rawToken := strings.SplitN(urlOut, "bootstrap=", 2)[1]

	client := secureLoopbackClient()
	origin := secureLoopbackOrigin(srv.Origin())

	body, _ := json.Marshal(map[string]string{"token": rawToken})
	bsURL := origin + "/api/auth/bootstrap"
	if resp, err := client.Post(bsURL, "application/json", bytes.NewReader(body)); err != nil {
		t.Fatalf("bootstrap: %v", err)
	} else {
		resp.Body.Close()
	}

	// POST without X-CSRF-Token: 403
	pURL := origin + "/api/daemon/pause"
	resp, err := client.Post(pURL, "application/json", nil)
	if err != nil {
		t.Fatalf("POST without CSRF: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403", resp.StatusCode)
	}
}
