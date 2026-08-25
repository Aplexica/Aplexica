package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	aplexicaupdate "github.com/aplexica/aplexica/internal/update"
	"github.com/aplexica/aplexica/internal/version"
	"github.com/spf13/cobra"
)

// The archive name this command prints is the fourth surface that has to agree
// on it, after .goreleaser.yaml, docs/install/verify.md and the packaging
// manifests. Filename disagreement is this project's #1 historical failure
// mode, and until now the updater's copy of the template was gated by nothing:
// switching the separator from a hyphen to an underscore left the whole
// cmd/aplexica suite green. The expected lines are therefore written out here
// rather than rebuilt from the helpers that produce them.
func TestRenderManualUpgradePrintsThePublishedAssetNames(t *testing.T) {
	extension := "tar.gz"
	if runtime.GOOS == "windows" {
		extension = "zip"
	}
	const base = "https://github.com/Aplexica/Aplexica/releases/download/v1.0.70"
	want := []string{
		base + "/aplexica-1.0.70-" + runtime.GOOS + "-" + runtime.GOARCH + "." + extension,
		base + "/SHA256SUMS",
		base + "/SHA256SUMS.sigstore.json",
		base + "/aplexica.provenance.sigstore.json",
	}

	var out bytes.Buffer
	renderManualUpgrade(&out, "1.0.70")
	printed := out.String()
	for _, line := range want {
		if !strings.Contains(printed, "  "+line+"\n") {
			t.Errorf("the manual upgrade recipe does not print %q\n---\n%s", line, printed)
		}
	}
	// The leading `v` survives in the tag path segment and nowhere else: the
	// archive carries the bare version. A helper that stripped or kept it in
	// both places would still satisfy a test that only looked for "1.0.70".
	if strings.Contains(printed, "aplexica-v1.0.70-") {
		t.Errorf("the archive name carries a leading v; assets use the bare version\n%s", printed)
	}
	if !strings.Contains(printed, "/releases/download/v1.0.70/") {
		t.Errorf("the download path dropped the tag's leading v\n%s", printed)
	}
}

// The loop closed: the template the updater prints is compared against the
// template GoReleaser builds from. Nothing else in the repository makes these
// two files disagree loudly, and they are edited by different people for
// different reasons.
func TestPlatformArchiveNameMatchesTheGoReleaserTemplate(t *testing.T) {
	config, err := os.ReadFile(filepath.Join("..", "..", ".goreleaser.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	// Git may materialize tracked text as CRLF on Windows. Normalize the bytes
	// before applying the deliberately small line-oriented config parser so the
	// same release filename contract is tested on every runner OS.
	configText := strings.ReplaceAll(string(config), "\r\n", "\n")
	block := goreleaserSection(t, configText, "archives:")
	template := goreleaserScalar(t, block, "name_template:")

	rendered := strings.NewReplacer(
		"{{ .Version }}", "1.0.70",
		"{{ .Os }}", runtime.GOOS,
		"{{ .Arch }}", runtime.GOARCH,
	).Replace(template)
	if strings.Contains(rendered, "{{") {
		t.Fatalf("archives name_template %q uses a variable this check does not model", template)
	}

	// GoReleaser appends the format; the updater has to append the same one.
	extension := ".tar.gz"
	if runtime.GOOS == "windows" {
		extension = ".zip"
	}
	if got, want := platformArchiveName("1.0.70"), rendered+extension; got != want {
		t.Fatalf("platformArchiveName = %q but .goreleaser.yaml publishes %q", got, want)
	}
	if !strings.Contains(block, "tar.gz") || !strings.Contains(block, "goos: windows") ||
		!strings.Contains(block, "zip") {
		t.Fatalf("the archives block no longer declares tar.gz with a windows zip override:\n%s", block)
	}
}

// goreleaserSection returns one top-level block of the config, ending at the
// next line that starts in column zero.
func goreleaserSection(t *testing.T, config, heading string) string {
	t.Helper()
	var block []string
	inside := false
	for _, line := range strings.Split(config, "\n") {
		if line == heading {
			inside = true
			continue
		}
		if !inside {
			continue
		}
		if line != "" && !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "#") {
			break
		}
		block = append(block, line)
	}
	if !inside {
		t.Fatalf(".goreleaser.yaml has no %q section", heading)
	}
	return strings.Join(block, "\n")
}

// goreleaserScalar reads the first `key: value` in a block, unquoting the
// single-quoted scalar GoReleaser templates are written as.
func goreleaserScalar(t *testing.T, block, key string) string {
	t.Helper()
	for _, line := range strings.Split(block, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, key) {
			continue
		}
		value := strings.TrimSpace(strings.TrimPrefix(trimmed, key))
		return strings.Trim(value, `'"`)
	}
	t.Fatalf("no %q in:\n%s", key, block)
	return ""
}

type staticUpdateDiscovery struct {
	target aplexicaupdate.Target
	err    error
}

func (discovery staticUpdateDiscovery) Refresh(context.Context, string) (aplexicaupdate.Target, error) {
	return discovery.target, discovery.err
}

// Rollback errors are rendered for people, so they name semantic versions and
// the waiver instead of exposing the packed integers used for comparison.
func TestRunUpdatePrintsRollbackVersions(t *testing.T) {
	restore := func(engine func() aplexicaupdate.Engine, executable func() (string, error),
		asJSON, check, yes, downgrade bool, currentVersion, gitCommit, releaseTrain string) {
		newUpdateEngine, updateExecutable = engine, executable
		updateFlagJSON, updateFlagCheck = asJSON, check
		updateFlagYes, updateFlagAllowDowngrade = yes, downgrade
		version.Version = currentVersion
		version.GitCommit = gitCommit
		version.ReleaseTrain = releaseTrain
	}
	oldEngine, oldExecutable := newUpdateEngine, updateExecutable
	oldJSON, oldCheck := updateFlagJSON, updateFlagCheck
	oldYes, oldDowngrade := updateFlagYes, updateFlagAllowDowngrade
	oldVersion, oldCommit, oldReleaseTrain := version.Version, version.GitCommit, version.ReleaseTrain
	t.Cleanup(func() {
		restore(oldEngine, oldExecutable, oldJSON, oldCheck, oldYes, oldDowngrade,
			oldVersion, oldCommit, oldReleaseTrain)
	})

	// A release build with no package-manager receipt is the only install that
	// reaches discovery at all.
	version.Version = "v1.2.2"
	version.GitCommit = strings.Repeat("a", 40)
	version.ReleaseTrain = "github-actions-aws-kms-v1"
	updateExecutable = func() (string, error) { return filepath.Join(t.TempDir(), "aplexica"), nil }
	updateFlagJSON, updateFlagCheck, updateFlagYes, updateFlagAllowDowngrade = false, true, false, false
	stateDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(stateDir, "update-floor"), []byte("1.2.3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	newUpdateEngine = func() aplexicaupdate.Engine {
		return aplexicaupdate.Engine{
			Classifier: aplexicaupdate.Classifier{Runner: updateCommandRunner{brewPrefix: ""}},
			DiscoveryFactory: func(string) (aplexicaupdate.Discovery, error) {
				return staticUpdateDiscovery{target: aplexicaupdate.Target{
					Schema:          "aplexica.channel-target/v1",
					Channel:         "stable",
					Repository:      "Aplexica/Aplexica",
					Version:         "1.2.2",
					Sequence:        1_002_002,
					ReleaseNotesURL: "https://github.com/Aplexica/Aplexica/releases/tag/v1.2.2",
				}}, nil
			},
			StateDir: stateDir,
		}
	}

	var stdout, stderr bytes.Buffer
	command := &cobra.Command{}
	command.SetContext(context.Background())
	command.SetOut(&stdout)
	command.SetErr(&stderr)
	err := runUpdate(command, nil)
	if aplexicaupdate.ExitCode(err) != aplexicaupdate.ExitSecurity {
		t.Fatalf("exit = %d, want %d (%v)", aplexicaupdate.ExitCode(err), aplexicaupdate.ExitSecurity, err)
	}
	printed := stdout.String()
	for _, want := range []string{"stable version 1.2.2", "accepted floor 1.2.3", "--allow-downgrade"} {
		if !strings.Contains(printed, want) {
			t.Fatalf("rollback output = %q, want %q", printed, want)
		}
	}
	if strings.Contains(printed, "1002002") || strings.Contains(printed, "1002003") {
		t.Fatalf("rollback output exposes packed sequences: %q", printed)
	}
}

// `aplexica update --help` distinguishes metadata completeness from artifact
// authentication. Presence of the three verification assets is checked, but
// their bytes are neither downloaded nor verified by this command.
func TestUpdateHelpDescribesMetadataOnlyDiscoveryHonestly(t *testing.T) {
	long := updateCmd.Long
	const want = "Report which installer owns this build and print the command that upgrades it.\n\n" +
		"If a package manager owns the executable (Homebrew, apt/dpkg, WinGet) or it is\n" +
		"a source build, this command answers from local ownership or build provenance and\n" +
		"does not contact GitHub. For an installation no package manager claims, it reads\n" +
		"the newest stable release's GitHub metadata and requires that metadata to list\n" +
		"SHA256SUMS, SHA256SUMS.sigstore.json, and\n" +
		"aplexica.provenance.sigstore.json. It does not download them or\n" +
		"verify a signature; authenticate downloaded bytes with the documented verify\n" +
		"command before replacing anything.\n\n" +
		"This command is advisory. It never downloads a platform archive and never\n" +
		"replaces a file on disk; you run the steps it prints."
	if long != want {
		t.Fatalf("update --help drifted from the reviewed metadata-only contract:\n%s", long)
	}
	if updateCmd.Short != "Report which installer owns this build and how to upgrade it" {
		t.Fatalf("update Short drifted: %q", updateCmd.Short)
	}
	for _, asset := range []string{
		"SHA256SUMS", "SHA256SUMS.sigstore.json", "aplexica.provenance.sigstore.json",
	} {
		if !strings.Contains(long, asset) {
			t.Fatalf("update --help must name required release asset %s:\n%s", asset, long)
		}
	}
	for _, claim := range []string{"does not download them", "verify a signature"} {
		if !strings.Contains(long, claim) {
			t.Fatalf("update --help omits %q:\n%s", claim, long)
		}
	}
	for _, falseClaim := range []string{"verify the cosign", "authenticated update"} {
		if strings.Contains(long, falseClaim) {
			t.Fatalf("update --help still claims in-process authentication (%q):\n%s", falseClaim, long)
		}
	}
	// The Short is what `aplexica --help` lists, so it carries the same
	// obligation in one line.
	if strings.Contains(updateCmd.Short, "authenticated") {
		t.Fatalf("update Short still claims every install gets an authenticated answer: %q", updateCmd.Short)
	}
}

func TestRenderAvailableReleaseCallsItUnverifiedMetadata(t *testing.T) {
	oldJSON, oldCheck := updateFlagJSON, updateFlagCheck
	t.Cleanup(func() {
		updateFlagJSON, updateFlagCheck = oldJSON, oldCheck
	})
	updateFlagJSON, updateFlagCheck = false, true

	var out bytes.Buffer
	command := &cobra.Command{}
	command.SetOut(&out)
	renderUpdateResult(command, aplexicaupdate.Result{
		Status:          aplexicaupdate.StatusUpdateAvailable,
		InstallMethod:   aplexicaupdate.MethodUnknown,
		Channel:         "stable",
		CurrentVersion:  "1.0.69",
		TargetVersion:   "1.0.70",
		TargetSequence:  1_000_070,
		ReleaseNotesURL: "https://github.com/Aplexica/Aplexica/releases/tag/v1.0.70",
	})
	printed := out.String()
	for _, want := range []string{
		"Newest complete GitHub release: 1.0.70",
		"No release asset was downloaded or verified by this check.",
	} {
		if !strings.Contains(printed, want) {
			t.Fatalf("available-release output = %q, want %q", printed, want)
		}
	}
	if strings.Contains(printed, "authenticated") {
		t.Fatalf("available-release output claims authentication: %q", printed)
	}
}

func TestRenderBootstrapStatusDoesNotInventASignatureFailure(t *testing.T) {
	oldJSON := updateFlagJSON
	t.Cleanup(func() { updateFlagJSON = oldJSON })
	updateFlagJSON = false

	var out bytes.Buffer
	command := &cobra.Command{}
	command.SetOut(&out)
	renderUpdateResult(command, aplexicaupdate.Result{
		Status:  aplexicaupdate.StatusBootstrapRequired,
		Message: "custom metadata source requires setup",
	})
	printed := out.String()
	if !strings.Contains(printed, "custom metadata source requires setup") ||
		!strings.Contains(printed, "No release assets were downloaded") {
		t.Fatalf("bootstrap output omits its actual state: %q", printed)
	}
	for _, invented := range []string{"Sigstore", "signature could not be checked"} {
		if strings.Contains(printed, invented) {
			t.Fatalf("bootstrap output invents %q: %q", invented, printed)
		}
	}
}

type updateCommandRunner struct {
	brewPrefix string
}

func (runner updateCommandRunner) Output(_ context.Context, name string, _ ...string) ([]byte, error) {
	if name == "brew" {
		return []byte(runner.brewPrefix + "\n"), nil
	}
	return nil, fmt.Errorf("no receipt")
}

func TestRunUpdateJSONReportsManagerDelegationOnce(t *testing.T) {
	prefix := filepath.Join(t.TempDir(), "Cellar", "aplexica", "1.2.3")
	executable := filepath.Join(prefix, "bin", "aplexica")

	oldEngine := newUpdateEngine
	oldExecutable := updateExecutable
	oldJSON := updateFlagJSON
	oldCheck := updateFlagCheck
	oldYes := updateFlagYes
	oldAllowDowngrade := updateFlagAllowDowngrade
	t.Cleanup(func() {
		newUpdateEngine = oldEngine
		updateExecutable = oldExecutable
		updateFlagJSON = oldJSON
		updateFlagCheck = oldCheck
		updateFlagYes = oldYes
		updateFlagAllowDowngrade = oldAllowDowngrade
	})
	newUpdateEngine = func() aplexicaupdate.Engine {
		return aplexicaupdate.Engine{
			Classifier: aplexicaupdate.Classifier{Runner: updateCommandRunner{brewPrefix: prefix}},
			StateDir:   t.TempDir(),
		}
	}
	updateExecutable = func() (string, error) { return executable, nil }
	updateFlagJSON = true
	updateFlagCheck = true
	updateFlagYes = false
	updateFlagAllowDowngrade = false

	var stdout, stderr bytes.Buffer
	command := &cobra.Command{}
	command.SetContext(context.Background())
	command.SetOut(&stdout)
	command.SetErr(&stderr)
	err := runUpdate(command, nil)
	if aplexicaupdate.ExitCode(err) != aplexicaupdate.ExitDelegated {
		t.Fatalf("runUpdate error=%v exit=%d", err, aplexicaupdate.ExitCode(err))
	}
	decoder := json.NewDecoder(&stdout)
	var result aplexicaupdate.Result
	if err := decoder.Decode(&result); err != nil {
		t.Fatal(err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		t.Fatalf("JSON stdout contains more than one result: %q", stdout.String())
	}
	if result.Status != aplexicaupdate.StatusManagerDelegated ||
		result.InstallMethod != aplexicaupdate.MethodHomebrew {
		t.Fatalf("unexpected delegated result: %+v", result)
	}
	if result.ManagerCommand != nil {
		t.Fatalf("delegated manager command = %v, want nil while the Homebrew channel is paused", result.ManagerCommand)
	}
	if result.Message != "Aplexica is managed by Homebrew; the Aplexica Homebrew tap has not been advanced yet." {
		t.Fatalf("delegated message = %q", result.Message)
	}
	if stderr.Len() != 0 {
		t.Fatalf("JSON command wrote unexpected stderr progress: %q", stderr.String())
	}
}
