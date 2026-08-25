package update

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/releasetrust"
)

func TestRefreshResolvesCompleteReleaseMetadataWithoutDownloadingAssets(t *testing.T) {
	document := releaseDocument("v1.0.70", false, false,
		releasetrust.ChecksumsAsset,
		releasetrust.ChecksumsBundle,
		releasetrust.ProvenanceBundle,
		"aplexica-1.0.70-darwin-arm64.tar.gz",
	)
	server, requests := newReleaseDocumentServer(t, document)
	defer server.Close()

	target, err := (&ReleaseDiscovery{
		apiBase: server.URL,
		client:  server.Client(),
	}).Refresh(t.Context(), "stable")
	if err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 1 {
		t.Fatalf("release discovery made %d requests, want exactly the metadata request", requests.Load())
	}
	if target.Schema != "aplexica.channel-target/v1" || target.Channel != "stable" ||
		target.Repository != releasetrust.Repository || target.Version != "1.0.70" ||
		target.Sequence != 1_000_070 {
		t.Fatalf("unexpected target: %+v", target)
	}
	// The response carries an attacker-controlled html_url and asset download
	// URLs. Discovery ignores both: the notes URL is derived locally, and the
	// single-request assertion above proves no listed asset was fetched.
	if want := "https://github.com/Aplexica/Aplexica/releases/tag/v1.0.70"; target.ReleaseNotesURL != want {
		t.Fatalf("release notes URL = %q, want locally derived %q", target.ReleaseNotesURL, want)
	}
}

func TestRefreshRequiresBothVerificationAssetsByExactName(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		assets  []string
		missing string
	}{
		{
			name:    "missing checksums",
			assets:  []string{releasetrust.ChecksumsBundle, releasetrust.ProvenanceBundle},
			missing: releasetrust.ChecksumsAsset,
		},
		{
			name:    "renamed checksums",
			assets:  []string{"SHA256SUMS.txt", releasetrust.ChecksumsBundle, releasetrust.ProvenanceBundle},
			missing: releasetrust.ChecksumsAsset,
		},
		{
			name:    "missing bundle",
			assets:  []string{releasetrust.ChecksumsAsset, releasetrust.ProvenanceBundle},
			missing: releasetrust.ChecksumsBundle,
		},
		{
			name:    "renamed bundle",
			assets:  []string{releasetrust.ChecksumsAsset, "SHA256SUMS.bundle", releasetrust.ProvenanceBundle},
			missing: releasetrust.ChecksumsBundle,
		},
		{
			name:    "missing provenance",
			assets:  []string{releasetrust.ChecksumsAsset, releasetrust.ChecksumsBundle},
			missing: releasetrust.ProvenanceBundle,
		},
		{
			name: "case and path lookalikes",
			assets: []string{
				"sha256sums", "./" + releasetrust.ChecksumsBundle,
				releasetrust.ProvenanceBundle + " ",
			},
			missing: releasetrust.ChecksumsAsset,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			server, requests := newReleaseDocumentServer(t,
				releaseDocument("v1.0.70", false, false, testCase.assets...))
			defer server.Close()

			target, err := (&ReleaseDiscovery{
				apiBase: server.URL,
				client:  server.Client(),
			}).Refresh(t.Context(), "stable")
			if err == nil || !strings.Contains(err.Error(), testCase.missing) {
				t.Fatalf("target=%+v err=%v, want missing %s", target, err, testCase.missing)
			}
			if target != (Target{}) {
				t.Fatalf("incomplete release produced target %+v", target)
			}
			if requests.Load() != 1 {
				t.Fatalf("incomplete release made %d requests, want metadata only", requests.Load())
			}
		})
	}
}

func TestRefreshRejectsReleaseShapesTheTrainDoesNotPublish(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		tag        string
		draft      bool
		prerelease bool
	}{
		{name: "draft", tag: "v1.0.70", draft: true},
		{name: "prerelease flag", tag: "v1.0.70", prerelease: true},
		{name: "missing v", tag: "1.0.70"},
		{name: "prerelease tag", tag: "v1.0.70-rc1"},
		{name: "leading zero", tag: "v01.0.70"},
		{name: "zero version", tag: "v0.0.0"},
		{name: "unorderable component", tag: "v1.0.1000"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			server, requests := newReleaseDocumentServer(t, releaseDocument(
				testCase.tag, testCase.draft, testCase.prerelease,
				releasetrust.ChecksumsAsset, releasetrust.ChecksumsBundle,
				releasetrust.ProvenanceBundle,
			))
			defer server.Close()

			target, err := (&ReleaseDiscovery{
				apiBase: server.URL,
				client:  server.Client(),
			}).Refresh(t.Context(), "stable")
			if err == nil || target != (Target{}) {
				t.Fatalf("target=%+v err=%v, want rejection", target, err)
			}
			if requests.Load() != 1 {
				t.Fatalf("invalid release made %d requests, want metadata only", requests.Load())
			}
		})
	}
}

func TestRefreshRefusesUnsupportedChannelBeforeNetwork(t *testing.T) {
	server, requests := newReleaseDocumentServer(t, releaseDocument(
		"v1.0.70", false, false,
		releasetrust.ChecksumsAsset, releasetrust.ChecksumsBundle,
		releasetrust.ProvenanceBundle,
	))
	defer server.Close()

	_, err := (&ReleaseDiscovery{
		apiBase: server.URL,
		client:  server.Client(),
	}).Refresh(t.Context(), "beta")
	if err == nil || !strings.Contains(err.Error(), "unsupported update channel") {
		t.Fatalf("Refresh(beta) error = %v", err)
	}
	if requests.Load() != 0 {
		t.Fatalf("unsupported channel made %d network requests", requests.Load())
	}
}

func TestTargetValidatePinsDerivedFieldsAndSequence(t *testing.T) {
	target := Target{
		Schema:          "aplexica.channel-target/v1",
		Channel:         "stable",
		Repository:      releasetrust.Repository,
		Version:         "1.0.70",
		Sequence:        1_000_070,
		ReleaseNotesURL: "https://github.com/Aplexica/Aplexica/releases/tag/v1.0.70",
	}
	if err := target.Validate(); err != nil {
		t.Fatalf("valid target: %v", err)
	}

	for _, mutation := range []struct {
		name   string
		change func(*Target)
	}{
		{name: "foreign repository", change: func(target *Target) { target.Repository = "aplexica/aplexica" }},
		{name: "mismatched sequence", change: func(target *Target) { target.Sequence++ }},
		{name: "server URL", change: func(target *Target) {
			target.ReleaseNotesURL = "https://attacker.invalid/install-now"
		}},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			changed := target
			mutation.change(&changed)
			if err := changed.Validate(); err == nil {
				t.Fatalf("Validate accepted %+v", changed)
			}
		})
	}
}

func TestRefreshReportsHTTPAndJSONFailures(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{name: "http", status: http.StatusInternalServerError, body: "unavailable", want: "500 Internal Server Error"},
		{name: "json", status: http.StatusOK, body: "{", want: "parse latest release"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.WriteHeader(testCase.status)
				_, _ = writer.Write([]byte(testCase.body))
			}))
			defer server.Close()

			_, err := (&ReleaseDiscovery{apiBase: server.URL, client: server.Client()}).Refresh(t.Context(), "stable")
			if err == nil || !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("Refresh error = %v, want %q", err, testCase.want)
			}
		})
	}
}

func TestFetchBoundedRejectsOversizedReleaseDocument(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(strings.Repeat("x", maxReleaseDocumentBytes+1)))
	}))
	defer server.Close()

	_, err := fetchBounded(t.Context(), server.Client(), server.URL, acceptGitHubJSON)
	if err == nil || !strings.Contains(err.Error(), "exceeded") {
		t.Fatalf("oversized document error = %v", err)
	}
}

func TestFetchBoundedUsesOptionalGitHubTokenPrecedence(t *testing.T) {
	for _, testCase := range []struct {
		name        string
		ghToken     string
		githubToken string
		want        string
	}{
		{name: "gh token", ghToken: "gh-test-token", githubToken: "fallback-test-token", want: "Bearer gh-test-token"},
		{name: "github fallback", ghToken: "  ", githubToken: "fallback-test-token", want: "Bearer fallback-test-token"},
		{name: "anonymous", ghToken: "", githubToken: "", want: ""},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Setenv("GH_TOKEN", testCase.ghToken)
			t.Setenv("GITHUB_TOKEN", testCase.githubToken)
			observed := make(chan string, 1)
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				observed <- request.Header.Get("Authorization")
				_, _ = writer.Write([]byte("ok"))
			}))
			defer server.Close()

			if _, err := fetchBounded(t.Context(), server.Client(), server.URL, acceptGitHubJSON); err != nil {
				t.Fatal(err)
			}
			if got := <-observed; got != testCase.want {
				t.Fatalf("Authorization = %q, want %q", got, testCase.want)
			}
		})
	}
}

func TestSecureUpdateHTTPClientRefusesRedirectBeforeForwardingCredentials(t *testing.T) {
	t.Setenv("GH_TOKEN", "redirect-test-token")
	var redirectedRequests atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		redirectedRequests.Add(1)
	}))
	defer destination.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer redirect-test-token" {
			t.Errorf("origin did not receive configured token")
		}
		http.Redirect(writer, request, destination.URL, http.StatusFound)
	}))
	defer origin.Close()

	_, err := fetchBounded(t.Context(), secureUpdateHTTPClient(), origin.URL, acceptGitHubJSON)
	if err == nil || !strings.Contains(err.Error(), "does not follow redirects") {
		t.Fatalf("redirect error = %v", err)
	}
	if redirectedRequests.Load() != 0 {
		t.Fatalf("redirect destination received %d requests", redirectedRequests.Load())
	}
}

func TestSecureUpdateHTTPClientHasABoundedRequestLifetime(t *testing.T) {
	client := secureUpdateHTTPClient()
	if client.Timeout != 2*time.Minute || client.CheckRedirect == nil {
		t.Fatalf("client timeout=%s redirect-policy-present=%v", client.Timeout, client.CheckRedirect != nil)
	}
}

func TestNewReleaseDiscoveryDoesNotCreateUpdaterState(t *testing.T) {
	stateDir := t.TempDir() + "/not-created"
	discovery, err := NewReleaseDiscovery(stateDir)
	if err != nil || discovery == nil {
		t.Fatalf("NewReleaseDiscovery = %T, %v", discovery, err)
	}
	if _, err := os.Stat(stateDir); !os.IsNotExist(err) {
		t.Fatalf("discovery created state path: %v", err)
	}
}

func newReleaseDocumentServer(t *testing.T, document []byte) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	requests := &atomic.Int32{}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		wantPath := "/repos/" + releasetrust.Repository + "/releases/latest"
		if request.Method != http.MethodGet || request.URL.Path != wantPath {
			t.Errorf("unexpected release request %s %s; asset downloads are forbidden", request.Method, request.URL.Path)
			http.NotFound(writer, request)
			return
		}
		if got := request.Header.Get("Accept"); got != acceptGitHubJSON {
			t.Errorf("Accept = %q, want %q", got, acceptGitHubJSON)
		}
		if got := request.Header.Get("X-GitHub-Api-Version"); got != "2022-11-28" {
			t.Errorf("X-GitHub-Api-Version = %q", got)
		}
		if got := request.Header.Get("User-Agent"); got != "aplexica-updater/1" {
			t.Errorf("User-Agent = %q", got)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write(document)
	}))
	return server, requests
}

func releaseDocument(tag string, draft, prerelease bool, assetNames ...string) []byte {
	assets := make([]map[string]any, 0, len(assetNames))
	for index, name := range assetNames {
		assets = append(assets, map[string]any{
			"id":                   index + 1,
			"name":                 name,
			"url":                  fmt.Sprintf("https://attacker.invalid/api/assets/%d", index+1),
			"browser_download_url": "https://attacker.invalid/download/" + name,
		})
	}
	document, err := json.Marshal(map[string]any{
		"tag_name":   tag,
		"draft":      draft,
		"prerelease": prerelease,
		"html_url":   "https://attacker.invalid/\x1b[31mreplace-now",
		"assets":     assets,
	})
	if err != nil {
		panic(err)
	}
	return document
}
