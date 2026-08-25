package middleware

import (
	"context"
	"net/http"

	"github.com/aplexica/aplexica/internal/web/auth"
)

type sessionCtxKey struct{}

// RequireSession returns middleware that enforces a valid session
// cookie on every request. Returns 401 to unauthenticated callers
// without leaking whether the session was missing, expired, or
// fabricated.
//
// Mount this AFTER the public routes (/healthz, /api/auth/*) so they
// remain reachable without a session. The typical pattern is to mount
// a sub-multiplexer at /api/ and wrap THAT with RequireSession +
// RequireCSRF, leaving the root mux unprotected.
func RequireSession(sessions *auth.SessionStore) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			sess, ok := auth.SessionFromCookie(r, sessions)
			if !ok {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			ctx := context.WithValue(r.Context(), sessionCtxKey{}, sess)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// SessionFromContext extracts the auth.Session previously stashed by
// RequireSession. Handlers downstream of the middleware use this to
// access the current session's metadata (CSRF token, user identity,
// etc.) without re-parsing the cookie.
//
// Returns (zero, false) when called outside a RequireSession-wrapped
// handler.
func SessionFromContext(ctx context.Context) (auth.Session, bool) {
	sess, ok := ctx.Value(sessionCtxKey{}).(auth.Session)
	return sess, ok
}
