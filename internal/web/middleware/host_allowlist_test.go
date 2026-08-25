package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func passthroughHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
}

// TestHostAllowlistAccepts confirms every legal loopback Host header
// shape passes through to the inner handler.
func TestHostAllowlistAccepts(t *testing.T) {
	const listenerPort = 51234
	mw := HostAllowlist(listenerPort)
	handler := mw(passthroughHandler())

	cases := []string{
		"localhost",
		"localhost:51234",
		"127.0.0.1",
		"127.0.0.1:51234",
		"[::1]",
		"[::1]:51234",
		"::1",
		// Case-insensitivity: Host header is theoretically case-
		// insensitive (RFC 7230 §5.4 + RFC 7320). Treat "LOCALHOST"
		// the same as "localhost".
		"LOCALHOST",
		"LocalHost:51234",
	}
	for _, host := range cases {
		t.Run(host, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/anything", nil)
			req.Host = host
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Errorf("host=%q got %d, want 200", host, rec.Code)
			}
		})
	}
}

// TestHostAllowlistRejects confirms attacker-controlled and
// port-mismatched Host headers return 421 Misdirected Request.
func TestHostAllowlistRejects(t *testing.T) {
	const listenerPort = 51234
	mw := HostAllowlist(listenerPort)
	handler := mw(passthroughHandler())

	cases := []string{
		// Attacker-controlled domain (DNS rebinding scenario)
		"evil.example.com",
		"evil.example.com:51234",
		// Suffix attack: appending a legitimate-looking prefix to a
		// malicious domain
		"127.0.0.1.evil.com",
		"localhost.evil.com",
		// Port mismatch: same host name but wrong port — could be an
		// attacker probing for what other services run on standard ports
		"localhost:80",
		"localhost:443",
		"127.0.0.1:80",
		// LAN-IP shapes that should never reach a loopback-only listener
		"192.168.1.1",
		"10.0.0.1",
		"::",
		// Empty host
		"",
		// Mismatched bracketed v6
		"[::]:51234",
		"[2001:db8::1]:51234",
	}
	for _, host := range cases {
		t.Run(host, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/anything", nil)
			req.Host = host
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusMisdirectedRequest {
				t.Errorf("host=%q got %d, want 421 Misdirected Request", host, rec.Code)
			}
		})
	}
}

// TestHostAllowlistRebuildsPerInstance confirms that two middlewares
// constructed with different ports produce different allowlists — the
// port is "baked in" at construction time.
func TestHostAllowlistRebuildsPerInstance(t *testing.T) {
	a := HostAllowlist(7600)
	b := HostAllowlist(8800)
	handlerA := a(passthroughHandler())
	handlerB := b(passthroughHandler())

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "localhost:7600"
	recA := httptest.NewRecorder()
	handlerA.ServeHTTP(recA, req)
	if recA.Code != http.StatusOK {
		t.Errorf("port-7600 allowlist accepts localhost:7600: got %d", recA.Code)
	}
	recB := httptest.NewRecorder()
	handlerB.ServeHTTP(recB, req)
	if recB.Code != http.StatusMisdirectedRequest {
		t.Errorf("port-8800 allowlist rejects localhost:7600: got %d (want 421)", recB.Code)
	}
}
