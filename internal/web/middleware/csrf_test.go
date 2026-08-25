package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aplexica/aplexica/internal/web/auth"
)

func TestRequireCSRFAllowsSafeMethods(t *testing.T) {
	mw := RequireCSRF()
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodOptions} {
		req := httptest.NewRequest(method, "/api/x", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("method=%s: status = %d, want 200", method, rec.Code)
		}
	}
}

func TestRequireCSRFRejectsMissingCookieOnPost(t *testing.T) {
	mw := RequireCSRF()
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodPost, "/api/x", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "csrf") {
		t.Errorf("body = %q, want csrf mention", rec.Body.String())
	}
}

func TestRequireCSRFRejectsMissingHeaderOnPost(t *testing.T) {
	mw := RequireCSRF()
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodPost, "/api/x", nil)
	req.AddCookie(&http.Cookie{Name: auth.CookieCSRF, Value: "the-csrf-value"})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
}

func TestRequireCSRFRejectsMismatch(t *testing.T) {
	mw := RequireCSRF()
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodPost, "/api/x", nil)
	req.AddCookie(&http.Cookie{Name: auth.CookieCSRF, Value: "abc"})
	req.Header.Set("X-CSRF-Token", "xyz") // different
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
}

func TestRequireCSRFAllowsMatchingHeaderAndCookie(t *testing.T) {
	mw := RequireCSRF()
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		req := httptest.NewRequest(method, "/api/x", nil)
		req.AddCookie(&http.Cookie{Name: auth.CookieCSRF, Value: "matching"})
		req.Header.Set("X-CSRF-Token", "matching")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("method=%s: status = %d, want 200", method, rec.Code)
		}
	}
}
