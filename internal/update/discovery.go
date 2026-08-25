package update

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/aplexica/aplexica/internal/releasetrust"
)

const (
	gitHubAPIBase = "https://api.github.com"

	acceptGitHubJSON = "application/vnd.github+json"

	// The release document is small and fixed-shape. One mebibyte leaves ample
	// headroom while bounding what an untrusted endpoint can make the updater
	// read into memory.
	maxReleaseDocumentBytes = 1 << 20

	updateRequestTimeout = 2 * time.Minute

	// Sequence packs a release version into one comparable integer. The radix
	// is deliberately generous rather than tight: it is not a version format,
	// only a total order, and a component at or above the radix would make an
	// older release compare as newer.
	sequenceRadix = 1000

	// versionFields is major, minor, patch -- the only version shape this train
	// publishes.
	versionFields = 3
)

// Discovery resolves the newest published release. It deliberately does not
// authenticate it. The update command is advisory: it never downloads an
// archive, installs a package, or replaces a binary. Authenticating a GitHub
// tag before printing a link would add a second verifier and a large trust-root
// dependency surface without protecting an execution path. Users authenticate
// downloaded bytes with the documented cosign command before installing them.
type Discovery interface {
	Refresh(ctx context.Context, channel string) (Target, error)
}

// ReleaseDiscovery resolves release metadata from GitHub. GitHub is an
// availability and discovery service here, not a release authority: every
// field in its response is treated as untrusted display metadata and reduced
// to a validated version plus URLs derived from compile-time constants.
type ReleaseDiscovery struct {
	// apiBase defaults to the public GitHub API and is unexported so callers
	// cannot repoint production discovery. Tests in this package serve the same
	// fixed endpoint from a local server.
	apiBase string
	client  *http.Client
}

// NewReleaseDiscovery satisfies DiscoveryFactory. Discovery keeps no local
// metadata of its own; the state directory is used only by the engine's local
// rollback floor.
func NewReleaseDiscovery(string) (Discovery, error) {
	return &ReleaseDiscovery{}, nil
}

// gitHubRelease is the deliberately small slice of the release document this
// updater reads. Free-text fields such as name, body, and html_url are not
// decoded, because no server-controlled string may be printed as trusted
// upgrade advice.
type gitHubRelease struct {
	TagName    string               `json:"tag_name"`
	Draft      bool                 `json:"draft"`
	Prerelease bool                 `json:"prerelease"`
	Assets     []gitHubReleaseAsset `json:"assets"`
}

type gitHubReleaseAsset struct {
	Name string `json:"name"`
}

func (release gitHubRelease) hasAsset(name string) bool {
	for _, asset := range release.Assets {
		if asset.Name == name {
			return true
		}
	}
	return false
}

func (discovery *ReleaseDiscovery) Refresh(ctx context.Context, channel string) (Target, error) {
	if channel != "stable" {
		return Target{}, fmt.Errorf("unsupported update channel %q", channel)
	}
	client := discovery.client
	if client == nil {
		client = secureUpdateHTTPClient()
	}
	apiBase := defaultString(discovery.apiBase, gitHubAPIBase)

	release, err := latestRelease(ctx, client, apiBase)
	if err != nil {
		return Target{}, err
	}
	version, err := releaseTagVersion(release)
	if err != nil {
		return Target{}, err
	}
	sequence, err := VersionSequence(version)
	if err != nil {
		return Target{}, err
	}

	// The updater never downloads these files, but refusing an incomplete
	// release prevents it from sending users to a version that cannot be
	// verified by the public installation procedure.
	for _, name := range []string{
		releasetrust.ChecksumsAsset,
		releasetrust.ChecksumsBundle,
		releasetrust.ProvenanceBundle,
	} {
		if !release.hasAsset(name) {
			return Target{}, fmt.Errorf("release %s does not publish %s", release.TagName, name)
		}
	}

	target := Target{
		Schema:          "aplexica.channel-target/v1",
		Channel:         "stable",
		Repository:      releasetrust.Repository,
		Version:         version,
		Sequence:        sequence,
		ReleaseNotesURL: releaseNotesURL(release.TagName),
	}
	if err := target.Validate(); err != nil {
		return Target{}, err
	}
	return target, nil
}

// VersionSequence packs MAJOR.MINOR.PATCH into a single increasing integer so
// the rollback floor is one comparison instead of a three-field dance:
// v1.0.70 -> 1_000_070. It is an ordering, not an identity -- nothing
// reconstructs a version from it -- so the only property that matters is that
// a newer release always produces a larger number. Components are therefore
// rejected at the radix rather than silently wrapping into the next field.
func VersionSequence(version string) (uint64, error) {
	if !versionPattern.MatchString(version) {
		return 0, fmt.Errorf("%q is not a release version", version)
	}
	var sequence uint64
	for _, field := range strings.SplitN(version, ".", versionFields) {
		component, err := strconv.ParseUint(field, decimalRadix, unsignedIntegerBits)
		if err != nil || component >= sequenceRadix {
			return 0, fmt.Errorf("release version %q has a component this updater cannot order", version)
		}
		sequence = sequence*sequenceRadix + component
	}
	if sequence == 0 {
		return 0, fmt.Errorf("release version %q has no ordinal position", version)
	}
	return sequence, nil
}

func latestRelease(ctx context.Context, client *http.Client, apiBase string) (gitHubRelease, error) {
	data, err := fetchBounded(ctx, client, strings.TrimRight(apiBase, "/")+"/repos/"+
		releasetrust.Repository+"/releases/latest", acceptGitHubJSON)
	if err != nil {
		return gitHubRelease{}, fmt.Errorf("fetch latest release: %w", err)
	}
	var release gitHubRelease
	if err := json.Unmarshal(data, &release); err != nil {
		return gitHubRelease{}, fmt.Errorf("parse latest release: %w", err)
	}
	return release, nil
}

func releaseTagVersion(release gitHubRelease) (string, error) {
	if release.Draft || release.Prerelease {
		return "", fmt.Errorf("latest release is draft or prerelease")
	}
	if !strings.HasPrefix(release.TagName, "v") {
		return "", fmt.Errorf("release tag %q is not vMAJOR.MINOR.PATCH", release.TagName)
	}
	version := strings.TrimPrefix(release.TagName, "v")
	if !versionPattern.MatchString(version) {
		return "", fmt.Errorf("release tag %q is not vMAJOR.MINOR.PATCH", release.TagName)
	}
	return version, nil
}

func fetchBounded(
	ctx context.Context, client *http.Client, url, accept string,
) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", accept)
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	request.Header.Set("User-Agent", "aplexica-updater/1")
	if token := gitHubToken(); token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4<<10))
		return nil, fmt.Errorf("GET %s: %s", url, response.Status)
	}
	limited := io.LimitReader(response.Body, maxReleaseDocumentBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if len(data) > maxReleaseDocumentBytes {
		return nil, fmt.Errorf("GET %s exceeded %d bytes", url, maxReleaseDocumentBytes)
	}
	return data, nil
}

func gitHubToken() string {
	if token := strings.TrimSpace(os.Getenv("GH_TOKEN")); token != "" {
		return token
	}
	return strings.TrimSpace(os.Getenv("GITHUB_TOKEN"))
}

func secureUpdateHTTPClient() *http.Client {
	return &http.Client{
		Timeout: updateRequestTimeout,
		// Release discovery needs exactly one response from the configured
		// GitHub API origin. A redirect is outside that fixed endpoint contract,
		// so refuse every redirect rather than reason about credential forwarding
		// or which alternate origin may supply the metadata.
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return fmt.Errorf("release discovery does not follow redirects")
		},
	}
}

func defaultString(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
