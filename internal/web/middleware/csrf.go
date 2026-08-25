package middleware

import (
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/aplexica/aplexica/internal/web/auth"
)

// csrfHeaderName is the request header that callers must echo from
// the JS-readable CSRF cookie on mutating requests. Mirrors the
// portal's fetch client (sets X-CSRF-Token unconditionally on POST/
// PUT/PATCH/DELETE).
const csrfHeaderName = "X-CSRF-Token"

// RequireCSRF returns middleware that enforces the double-submit
// CSRF token on mutating verbs. GET, HEAD, and OPTIONS requests pass
// through without inspection — they are idempotent and safe from
// cross-site state mutation in this design.
//
// Validation is constant-time so that a known-cookie-value vs an
// attacker-supplied header doesn't leak timing information.
func RequireCSRF(requiredOrigin ...string) func(http.Handler) http.Handler {
	origin := ""
	if len(requiredOrigin) > 0 {
		origin = requiredOrigin[0]
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodGet, http.MethodHead, http.MethodOptions:
				next.ServeHTTP(w, r)
				return
			}
			if origin != "" && r.Header.Get("Origin") != origin {
				http.Error(w, "origin mismatch", http.StatusForbidden)
				return
			}
			if site := strings.ToLower(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site"))); site != "" && site != "same-origin" {
				http.Error(w, "cross-site request rejected", http.StatusForbidden)
				return
			}
			cookie, err := r.Cookie(auth.CookieCSRF)
			if err != nil || cookie.Value == "" {
				http.Error(w, "csrf cookie missing", http.StatusForbidden)
				return
			}
			header := r.Header.Get(csrfHeaderName)
			if header == "" {
				http.Error(w, "csrf header missing", http.StatusForbidden)
				return
			}
			// Equal-length constant-time compare. If lengths differ,
			// ConstantTimeCompare returns 0 in non-constant time, but
			// the actual values still don't leak because we never
			// branch on the cookie or header contents elsewhere.
			if subtle.ConstantTimeCompare([]byte(header), []byte(cookie.Value)) != 1 {
				http.Error(w, "csrf mismatch", http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
