package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aplexica/aplexica/internal/web/auth"
)

func TestRequireSessionRejectsMissingCookie(t *testing.T) {
	sessions := auth.NewSessionStore(auth.DefaultSessionTTL)
	handler := RequireSession(sessions)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/something", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestRequireSessionRejectsForgedCookie(t *testing.T) {
	sessions := auth.NewSessionStore(auth.DefaultSessionTTL)
	handler := RequireSession(sessions)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/something", nil)
	req.AddCookie(&http.Cookie{Name: auth.CookieSession, Value: "i-made-this-up"})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestRequireSessionAllowsValidCookieAndStashesInContext(t *testing.T) {
	sessions := auth.NewSessionStore(auth.DefaultSessionTTL)
	sid, _, _ := sessions.Create("local")

	var seen auth.Session
	handler := RequireSession(sessions)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s, ok := SessionFromContext(r.Context())
		if !ok {
			t.Error("SessionFromContext returned !ok inside protected handler")
		}
		seen = s
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/something", nil)
	req.AddCookie(&http.Cookie{Name: auth.CookieSession, Value: sid})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	// The middleware resolved the raw cookie value to a session and stashed
	// it in context. The stashed session carries the hashed ID, never the
	// raw cookie value (the raw sid is not stored anywhere server-side).
	if seen.User != "local" {
		t.Errorf("SessionFromContext.User = %q, want local", seen.User)
	}
	if seen.ID == "" {
		t.Error("SessionFromContext.ID is empty; expected the hashed session ID")
	}
	if seen.ID == sid {
		t.Errorf("SessionFromContext.ID is the raw sid %q; must be the hash", sid)
	}
}

func TestSessionFromContextReturnsFalseWithoutMiddleware(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	if _, ok := SessionFromContext(req.Context()); ok {
		t.Error("SessionFromContext outside middleware should return !ok")
	}
}
