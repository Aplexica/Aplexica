package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestCSPHeaderSetsRequiredDirectives confirms every CSP directive the
// V1 threat model relies on is present in the Content-Security-Policy
// header. The strict CSP locks the SPA to same-origin only and blocks
// framing, embeds, base-URL hijacks, and form-action redirects.
func TestCSPHeaderSetsRequiredDirectives(t *testing.T) {
	mw := CSP()
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/anything", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	got := rec.Header().Get("Content-Security-Policy")
	if got == "" {
		t.Fatal("Content-Security-Policy header is empty")
	}
	required := []string{
		"default-src 'self'",
		"script-src 'self'",
		"style-src 'self' 'unsafe-inline'",
		"connect-src 'self'",
		"img-src 'self' data:",
		"font-src 'self'",
		"object-src 'none'",
		"frame-ancestors 'none'",
		"base-uri 'self'",
		"form-action 'self'",
	}
	for _, want := range required {
		if !strings.Contains(got, want) {
			t.Errorf("CSP missing %q;\n got: %q", want, got)
		}
	}
}

// TestCSPSetsCompanionHeaders confirms each defense-in-depth header
// alongside CSP is set on every response.
func TestCSPSetsCompanionHeaders(t *testing.T) {
	mw := CSP()
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/anything", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	cases := map[string]string{
		"X-Content-Type-Options":       "nosniff",
		"Referrer-Policy":              "no-referrer",
		"Cross-Origin-Resource-Policy": "same-origin",
	}
	for header, want := range cases {
		if got := rec.Header().Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
}

// TestCSPHeaderSetEvenOnErrorPaths confirms the middleware sets headers
// regardless of the wrapped handler's status code — security headers
// must apply to error responses (e.g., a 421 from the Host allowlist
// downstream of CSP, OR a 500 from a handler panic).
func TestCSPHeaderSetEvenOnErrorPaths(t *testing.T) {
	mw := CSP()
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	req := httptest.NewRequest(http.MethodGet, "/anything", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	if got := rec.Header().Get("Content-Security-Policy"); got == "" {
		t.Error("CSP must be set even when handler returns 500")
	}
}

// TestCSPHeaderPolicyIsConstant — the policy string is stable across
// invocations. (Regression guard against future per-request mutations
// that could let a downstream handler weaken the policy.)
func TestCSPHeaderPolicyIsConstant(t *testing.T) {
	mw := CSP()
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	var first string
	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodGet, "/anything", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		got := rec.Header().Get("Content-Security-Policy")
		if i == 0 {
			first = got
		} else if got != first {
			t.Errorf("policy drift on call %d:\n first: %q\n now:   %q", i, first, got)
		}
	}
}
