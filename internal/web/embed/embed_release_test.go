//go:build release

package embed

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestReleaseBundleServesPortal makes the release build contract observable:
// a release-tagged test graph must contain and serve the real Portal entry
// point. The compile-time directives in embed_release.go reject absent files;
// this test additionally rejects a staged placeholder or an unusable handler.
func TestReleaseBundleServesPortal(t *testing.T) {
	if !BundlePresent() {
		t.Fatal("release build does not contain the local Portal bundle")
	}

	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("release Portal status = %d, want 200", rec.Code)
	}

	body := rec.Body.String()
	if strings.Contains(body, "local UI not bundled") {
		t.Fatal("release handler served the missing-bundle placeholder")
	}
	if !strings.Contains(body, `<div id="root"></div>`) {
		t.Fatal("release Portal entry point lacks the React root element")
	}
	if !strings.Contains(body, `/assets/`) {
		t.Fatal("release Portal entry point does not reference bundled assets")
	}
}
