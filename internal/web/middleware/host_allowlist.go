// Package middleware contains the HTTP middleware stack for the local
// web listener. Each middleware addresses one threat-model item from
// the V1 design spec (§7):
//
//   - HostAllowlist: DNS rebinding mitigation (this file)
//   - CSP:           cross-origin script injection + framing (csp.go)
//   - RequireSession: session cookie auth (session.go; added in W3.4)
//   - RequireCSRF:    double-submit token enforcement (csrf.go; added in W3.4)
//
// Composition order matters: the recommended stack from outermost to
// innermost is HostAllowlist → CSP → RequireSession → RequireCSRF →
// handler.  HostAllowlist comes first so we reject rebinding attempts
// before any cookie/state lookup.
package middleware

import (
	"fmt"
	"net/http"
	"strings"
)

// HostAllowlist returns middleware that rejects requests with Host
// headers outside the loopback set. Mitigates DNS rebinding: an
// attacker who resolves their own hostname to 127.0.0.1 still presents
// the attacker hostname in the Host header, which we reject with 421
// Misdirected Request (the HTTP semantic for "this request was routed
// to the wrong server").
//
// The listener's bound port is required so we can also accept the
// exact "host:port" forms that clients normally send when connecting
// to a non-default-port listener. Port-less variants are kept in the
// allowlist (some clients/proxies strip the port) and are safe under
// the DNS-rebinding threat model because the protected names
// ("localhost", "127.0.0.1", "::1") aren't controllable by an
// external attacker.
//
// Host header comparison is case-insensitive (RFC 7230 §5.4).
func HostAllowlist(port int) func(http.Handler) http.Handler {
	allowed := map[string]struct{}{
		// Port-less forms (default-port + tolerance for clients/proxies
		// that strip the port).
		"localhost": {},
		"127.0.0.1": {},
		"::1":       {},
		"[::1]":     {},
		// Exact port-bound forms — the typical browser-sent shapes for
		// a listener on a non-default port.
		fmt.Sprintf("localhost:%d", port): {},
		fmt.Sprintf("127.0.0.1:%d", port): {},
		fmt.Sprintf("[::1]:%d", port):     {},
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			host := strings.ToLower(strings.TrimSpace(r.Host))
			if _, ok := allowed[host]; !ok {
				http.Error(w, "421 Misdirected Request", http.StatusMisdirectedRequest)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func InstanceHostAllowlist(hostname string, port int) func(http.Handler) http.Handler {
	exact := strings.ToLower(fmt.Sprintf("%s:%d", hostname, port))
	health := HostAllowlist(port)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/healthz" {
				health(next).ServeHTTP(w, r)
				return
			}
			if strings.ToLower(strings.TrimSpace(r.Host)) != exact {
				http.Error(w, "421 Misdirected Request", http.StatusMisdirectedRequest)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
