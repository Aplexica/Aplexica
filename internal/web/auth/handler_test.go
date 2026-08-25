package auth

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestHandler() *Handler {
	return &Handler{
		Tokens:   NewTokenStore(DefaultTokenTTL),
		Sessions: NewSessionStore(DefaultSessionTTL),
		Version:  "v0.0.0-test",
	}
}

func TestBootstrapValidTokenSetsCookies(t *testing.T) {
	h := newTestHandler()
	_, raw, _ := h.Tokens.Issue("http://127.0.0.1:7600")

	body, _ := json.Marshal(bootstrapReq{Token: raw})
	req := httptest.NewRequest(http.MethodPost, "/api/auth/bootstrap", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.bootstrap(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	cookies := rec.Result().Cookies()
	var seenSession, seenCSRF bool
	for _, c := range cookies {
		if c.Name == CookieSession {
			seenSession = true
			if !c.HttpOnly {
				t.Error("session cookie must be HttpOnly")
			}
			if c.SameSite != http.SameSiteStrictMode {
				t.Error("session cookie must be SameSite=Strict")
			}
			if c.Value == "" {
				t.Error("session cookie value empty")
			}
		}
		if c.Name == CookieCSRF {
			seenCSRF = true
			if c.HttpOnly {
				t.Error("csrf cookie must NOT be HttpOnly (JS reads it)")
			}
			if c.Value == "" {
				t.Error("csrf cookie value empty")
			}
		}
	}
	if !seenSession {
		t.Error("session cookie missing")
	}
	if !seenCSRF {
		t.Error("csrf cookie missing")
	}

	var resp whoami
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if resp.User != "local" || resp.Mode != "local" {
		t.Errorf("whoami = %+v, want user/mode=local", resp)
	}
}

func TestBootstrapBadTokenReturns401AndNoCookies(t *testing.T) {
	h := newTestHandler()
	body, _ := json.Marshal(bootstrapReq{Token: "not-a-real-token"})
	req := httptest.NewRequest(http.MethodPost, "/api/auth/bootstrap", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.bootstrap(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
	if cs := rec.Result().Cookies(); len(cs) != 0 {
		t.Errorf("bad bootstrap should not set cookies; got %d", len(cs))
	}
}

func TestBootstrapEmptyTokenReturns400(t *testing.T) {
	h := newTestHandler()
	body, _ := json.Marshal(bootstrapReq{Token: ""})
	req := httptest.NewRequest(http.MethodPost, "/api/auth/bootstrap", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.bootstrap(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("empty token: status = %d, want 400", rec.Code)
	}
}

func TestSessionWithoutCookieReturns401(t *testing.T) {
	h := newTestHandler()
	req := httptest.NewRequest(http.MethodGet, "/api/auth/session", nil)
	rec := httptest.NewRecorder()
	h.whoami(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestSessionWithValidCookieReturnsWhoami(t *testing.T) {
	h := newTestHandler()
	sid, _, _ := h.Sessions.Create("local")

	req := httptest.NewRequest(http.MethodGet, "/api/auth/session", nil)
	req.AddCookie(&http.Cookie{Name: CookieSession, Value: sid})
	rec := httptest.NewRecorder()
	h.whoami(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"user":"local"`) {
		t.Errorf("body missing user field: %s", rec.Body.String())
	}
}

func TestLogoutClearsCookiesAndRevokesSession(t *testing.T) {
	h := newTestHandler()
	sid, _, _ := h.Sessions.Create("local")

	req := httptest.NewRequest(http.MethodPost, "/api/auth/logout", nil)
	req.AddCookie(&http.Cookie{Name: CookieSession, Value: sid})
	rec := httptest.NewRecorder()
	h.logout(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204", rec.Code)
	}
	if _, ok := h.Sessions.Get(sid); ok {
		t.Error("session must be revoked after logout")
	}

	// Both cookies must be set to MaxAge=-1
	cs := rec.Result().Cookies()
	for _, c := range cs {
		if c.MaxAge >= 0 {
			t.Errorf("cookie %s MaxAge = %d, want < 0 (expire)", c.Name, c.MaxAge)
		}
	}
}

func TestLogoutWithoutCookieReturns204(t *testing.T) {
	h := newTestHandler()
	req := httptest.NewRequest(http.MethodPost, "/api/auth/logout", nil)
	rec := httptest.NewRecorder()
	h.logout(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204", rec.Code)
	}
}

func TestSecondBootstrapWithSameTokenReturns401(t *testing.T) {
	h := newTestHandler()
	_, raw, _ := h.Tokens.Issue("http://127.0.0.1:7600")

	body, _ := json.Marshal(bootstrapReq{Token: raw})

	// First consumption: ok
	req1 := httptest.NewRequest(http.MethodPost, "/api/auth/bootstrap", bytes.NewReader(body))
	req1.Header.Set("Content-Type", "application/json")
	rec1 := httptest.NewRecorder()
	h.bootstrap(rec1, req1)
	if rec1.Code != http.StatusOK {
		t.Fatalf("first bootstrap status = %d, want 200", rec1.Code)
	}

	// Second consumption with the same token: 401 (replay)
	req2 := httptest.NewRequest(http.MethodPost, "/api/auth/bootstrap", bytes.NewReader(body))
	req2.Header.Set("Content-Type", "application/json")
	rec2 := httptest.NewRecorder()
	h.bootstrap(rec2, req2)
	if rec2.Code != http.StatusUnauthorized {
		t.Errorf("replay status = %d, want 401", rec2.Code)
	}
}
