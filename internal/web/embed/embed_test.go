package embed

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandlerServesSPAIndexAtRoot(t *testing.T) {
	h := Handler()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	// Two valid outcomes:
	//   - bundle present (release / dev with `make fetch-portal`):
	//     200 + HTML body, ETag set
	//   - bundle absent (fresh checkout, .gitkeep only):
	//     503 + placeholder body containing "local UI"
	if rec.Code == http.StatusOK {
		if !strings.Contains(rec.Body.String(), "<!doctype html") &&
			!strings.Contains(rec.Body.String(), "<!DOCTYPE html") {
			t.Errorf("populated bundle: body lacks HTML doctype: %q", rec.Body.String()[:200])
		}
		if rec.Header().Get("ETag") == "" {
			t.Error("populated bundle: ETag header missing")
		}
	} else if rec.Code == http.StatusServiceUnavailable {
		if !strings.Contains(rec.Body.String(), "local UI") {
			t.Errorf("placeholder body missing expected hint: %q", rec.Body.String())
		}
	} else {
		t.Errorf("status = %d, want 200 (bundle) or 503 (placeholder)", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
}

func TestHandlerRejectsAPIPathsAs404(t *testing.T) {
	// Only /api/* paths get the defensive 404 — every real API + SSE
	// endpoint lives under /api/ (/api/events backfill,
	// /api/events/stream SSE).
	h := Handler()
	cases := []string{"/api/daemon", "/api/auth/session", "/api/events", "/api/events/stream"}
	for _, path := range cases {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("path=%q status = %d, want 404", path, rec.Code)
		}
	}
}

// TestSPADeepLinksServeIndex is the regression test for the live-test
// finding: hard navigation / refresh / bookmark of a client-side SPA
// route (especially /events, which collided with the old /events
// guard) must serve index.html so React Router can take over — NOT
// 404. The bare /events path is a legit SPA route; only /api/* is an
// API path.
func TestSPADeepLinksServeIndex(t *testing.T) {
	h := Handler()
	spaRoutes := []string{
		"/events", "/events/stream", // the previously-shadowed route + a sub-path
		"/agents", "/agents/claude-code",
		"/rules", "/conflicts", "/pending",
		"/settings", "/settings/transport", "/help",
		"/onboarding/welcome",
	}
	for _, path := range spaRoutes {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		// Must NOT be 404. Either 200 (bundle present -> index.html) or
		// 503 (placeholder when bundle absent) is acceptable; both mean
		// "the SPA shell was served, client routing will handle it."
		if rec.Code == http.StatusNotFound {
			t.Errorf("SPA route %q returned 404; must serve the SPA shell so client routing works", path)
			continue
		}
		if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
			t.Errorf("SPA route %q Content-Type = %q, want text/html", path, ct)
		}
	}
}

func TestBundlePresentReflectsActualState(t *testing.T) {
	// Whether BundlePresent returns true depends on whether dist-local/
	// has been populated by `make fetch-portal` / the release pipeline
	// / a developer's local pnpm build. Both states are valid; we only
	// confirm the predicate is deterministic per invocation.
	first := BundlePresent()
	second := BundlePresent()
	if first != second {
		t.Errorf("BundlePresent() not deterministic: %v then %v", first, second)
	}
}

func TestIsHashedAssetHeuristic(t *testing.T) {
	cases := []struct {
		path   string
		hashed bool
	}{
		{"assets/index-abcdef12.js", true},
		{"assets/main.css", true},
		{"dist-local/assets/foo.js", true},
		{"index.html", false},
		{"robots.txt", false},
		{"manifest.webmanifest", false},
	}
	for _, c := range cases {
		if got := isHashedAsset(c.path); got != c.hashed {
			t.Errorf("isHashedAsset(%q) = %v, want %v", c.path, got, c.hashed)
		}
	}
}

func TestConflictDetailChunkRevalidates(t *testing.T) {
	got := cacheControlForAsset("assets/ConflictDetailPage-C1zFMnBj.js")
	if got != indexCacheControl {
		t.Fatalf("cacheControlForAsset(conflict detail) = %q, want %q", got, indexCacheControl)
	}
}
