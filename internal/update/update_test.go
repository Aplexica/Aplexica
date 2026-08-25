package update

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/aplexica/aplexica/internal/releasetrust"
)

type fakeRunner func(name string, args ...string) ([]byte, error)

func (runner fakeRunner) Output(_ context.Context, name string, args ...string) ([]byte, error) {
	return runner(name, args...)
}

type staticDiscovery struct {
	target Target
	err    error
	called bool
}

func (discovery *staticDiscovery) Refresh(context.Context, string) (Target, error) {
	discovery.called = true
	return discovery.target, discovery.err
}

func staticEngine(t *testing.T, discovery *staticDiscovery, stateDir string) Engine {
	t.Helper()
	return Engine{
		Classifier: Classifier{Runner: fakeRunner(func(string, ...string) ([]byte, error) {
			return nil, fmt.Errorf("no manager receipt")
		})},
		DiscoveryFactory: func(string) (Discovery, error) { return discovery, nil },
		StateDir:         stateDir,
	}
}

func releaseProvenance(version string) Provenance {
	return Provenance{
		Version: version, GitCommit: strings.Repeat("a", 40), ReleaseTrain: officialReleaseTrain,
	}
}

func TestEngineCheckReportsAvailableRelease(t *testing.T) {
	discovery := &staticDiscovery{target: testTarget("1.2.4")}
	engine := staticEngine(t, discovery, t.TempDir())
	result, _, _, err := engine.Check(t.Context(), filepath.Join(t.TempDir(), "aplexica"),
		releaseProvenance("v1.2.3"), CheckOptions{Channel: "stable"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != StatusUpdateAvailable || result.TargetVersion != "1.2.4" ||
		result.TargetSequence != 1_002_004 || result.CurrentSequence != 1_002_003 {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestEngineCheckReportsUpToDateWhenReleaseMatchesRunningBuild(t *testing.T) {
	discovery := &staticDiscovery{target: testTarget("1.2.3")}
	engine := staticEngine(t, discovery, t.TempDir())
	result, _, _, err := engine.Check(t.Context(), filepath.Join(t.TempDir(), "aplexica"),
		releaseProvenance("v1.2.3"), CheckOptions{Channel: "stable"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != StatusUpToDate || result.RestartRequired {
		t.Fatalf("unexpected result: %+v", result)
	}
}

// The floor is written from the version that is running, so a machine that has
// run 1.2.3 must warn about a discovery answer of 1.2.2. The message is a human
// contract and must render versions, not the packed comparison integers.
func TestEngineCheckRejectsReleaseBelowTheLocalFloor(t *testing.T) {
	stateDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(stateDir, updateFloorFile), []byte("1.2.3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	discovery := &staticDiscovery{target: testTarget("1.2.2")}
	engine := staticEngine(t, discovery, stateDir)
	result, _, _, err := engine.Check(t.Context(), filepath.Join(t.TempDir(), "aplexica"),
		releaseProvenance("v1.2.2"), CheckOptions{Channel: "stable"})
	if result.Status != StatusFailed || ExitCode(err) != ExitSecurity {
		t.Fatalf("rollback result=%+v err=%v code=%d", result, err, ExitCode(err))
	}
	for _, version := range []string{"stable version 1.2.2", "accepted floor 1.2.3"} {
		if !strings.Contains(result.Message, version) {
			t.Fatalf("rollback message = %q, want %q", result.Message, version)
		}
	}
	for _, packed := range []string{"1002002", "1002003"} {
		if strings.Contains(result.Message, packed) {
			t.Fatalf("rollback message exposes packed sequence %s instead of a version: %q", packed, result.Message)
		}
	}
}

func TestEngineCheckAllowsAnExplicitDowngrade(t *testing.T) {
	stateDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(stateDir, updateFloorFile), []byte("1.2.3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	discovery := &staticDiscovery{target: testTarget("1.2.2")}
	engine := staticEngine(t, discovery, stateDir)
	result, _, _, err := engine.Check(t.Context(), filepath.Join(t.TempDir(), "aplexica"),
		releaseProvenance("v1.2.1"), CheckOptions{Channel: "stable", AllowDowngrade: true})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != StatusUpdateAvailable || result.TargetVersion != "1.2.2" {
		t.Fatalf("explicit downgrade result=%+v", result)
	}
}

func TestEngineCheckRaisesTheFloorToTheRunningVersion(t *testing.T) {
	stateDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(stateDir, updateFloorFile), []byte("1.2.3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	discovery := &staticDiscovery{target: testTarget("1.3.0")}
	engine := staticEngine(t, discovery, stateDir)
	if _, _, _, err := engine.Check(t.Context(), filepath.Join(t.TempDir(), "aplexica"),
		releaseProvenance("v1.2.9"), CheckOptions{Channel: "stable"}); err != nil {
		t.Fatal(err)
	}
	if version, sequence := readUpdateFloor(stateDir); version != "1.2.9" || sequence != 1_002_009 {
		t.Fatalf("floor after check = %q/%d, want the running version", version, sequence)
	}
	// A later run of an older build must not lower the watermark.
	if _, _, _, err := engine.Check(t.Context(), filepath.Join(t.TempDir(), "aplexica"),
		releaseProvenance("v1.2.4"), CheckOptions{Channel: "stable"}); err != nil {
		t.Fatal(err)
	}
	if version, _ := readUpdateFloor(stateDir); version != "1.2.9" {
		t.Fatalf("floor after downgrade run = %q, want it to stay at 1.2.9", version)
	}
}

// A package-manager install must get its upgrade command without a network
// round trip: the answer does not depend on which release GitHub calls newest,
// and discovery needs a token those users have no reason to hold.
func TestEngineCheckDelegatesWithoutDiscovery(t *testing.T) {
	prefix := filepath.Join(t.TempDir(), "Cellar", "aplexica", "1.2.3")
	executable := filepath.Join(prefix, "bin", "aplexica")
	discovery := &staticDiscovery{target: testTarget("1.2.4")}
	engine := Engine{
		Classifier: Classifier{Runner: fakeRunner(func(name string, _ ...string) ([]byte, error) {
			if name == "brew" {
				return []byte(prefix + "\n"), nil
			}
			return nil, fmt.Errorf("no receipt")
		})},
		DiscoveryFactory: func(string) (Discovery, error) { return discovery, nil },
		StateDir:         t.TempDir(),
	}
	result, _, _, err := engine.Check(t.Context(), executable, releaseProvenance("v1.2.3"),
		CheckOptions{Channel: "stable"})
	if ExitCode(err) != ExitDelegated || result.Status != StatusManagerDelegated {
		t.Fatalf("delegated result=%+v err=%v code=%d", result, err, ExitCode(err))
	}
	if result.ManagerCommand != nil {
		t.Fatalf("delegated command = %v, want nil while the Homebrew channel is paused", result.ManagerCommand)
	}
	if result.Message != "Aplexica is managed by Homebrew; the Aplexica Homebrew tap has not been advanced yet." {
		t.Fatalf("delegated message = %q", result.Message)
	}
	if discovery.called {
		t.Fatal("a package-manager install reached the network")
	}
}

// A source build can carry a release-looking baseline newer than anything this
// machine has installed. It is delegated to the checkout before discovery and
// must not turn that development version into a persistent downgrade floor.
func TestEngineCheckSourceBuildNeverRaisesTheFloor(t *testing.T) {
	stateDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(stateDir, updateFloorFile), []byte("1.2.3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	discovery := &staticDiscovery{target: testTarget("9.9.10")}
	engine := staticEngine(t, discovery, stateDir)
	result, installation, target, err := engine.Check(
		t.Context(), filepath.Join(t.TempDir(), "aplexica"),
		Provenance{Version: "v9.9.9", GitCommit: "unknown", ReleaseTrain: ""},
		CheckOptions{Channel: "stable"},
	)
	if ExitCode(err) != ExitDelegated || result.Status != StatusManagerDelegated ||
		installation.Method != MethodSource || target != nil {
		t.Fatalf("source result=%+v installation=%+v target=%+v err=%v", result, installation, target, err)
	}
	if discovery.called {
		t.Fatal("a source build reached release discovery")
	}
	if version, sequence := readUpdateFloor(stateDir); version != "1.2.3" || sequence != 1_002_003 {
		t.Fatalf("source build changed floor to %q/%d", version, sequence)
	}
}

// Custom Discovery implementations are an extension point. Preserve their
// typed bootstrap status even though the built-in GitHub metadata discovery
// has no trust-root bootstrap phase of its own.
func TestEngineCheckPreservesBootstrapStatusFromCustomDiscovery(t *testing.T) {
	discovery := &staticDiscovery{err: &Error{
		Stage: "discover", Class: ClassBootstrap, Code: ExitBootstrapRequired,
		Err: fmt.Errorf("custom metadata source requires setup"),
	}}
	engine := staticEngine(t, discovery, t.TempDir())
	result, _, target, err := engine.Check(t.Context(), filepath.Join(t.TempDir(), "aplexica"),
		releaseProvenance("v1.2.3"), CheckOptions{Channel: "stable"})
	if result.Status != StatusBootstrapRequired {
		t.Fatalf("status = %q, want %q", result.Status, StatusBootstrapRequired)
	}
	if ExitCode(err) != ExitBootstrapRequired {
		t.Fatalf("exit code = %d, want %d (%v)", ExitCode(err), ExitBootstrapRequired, err)
	}
	if target != nil {
		t.Fatalf("a target escaped a failed discovery: %+v", target)
	}
	if result.Message == "" {
		t.Fatal("the bootstrap result carries no message for --json to report")
	}
}

// JSON callers receive the same actionable rollback text as terminal users.
func TestEngineCheckIncludesTheRollbackWaiverInTheResult(t *testing.T) {
	stateDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(stateDir, updateFloorFile), []byte("1.2.3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	discovery := &staticDiscovery{target: testTarget("1.2.2")}
	engine := staticEngine(t, discovery, stateDir)
	result, _, _, err := engine.Check(t.Context(), filepath.Join(t.TempDir(), "aplexica"),
		releaseProvenance("v1.2.2"), CheckOptions{Channel: "stable"})
	if ExitCode(err) != ExitSecurity {
		t.Fatalf("floor exit code = %d, want %d", ExitCode(err), ExitSecurity)
	}
	if !strings.Contains(result.Message, "floor") || !strings.Contains(result.Message, "--allow-downgrade") {
		t.Fatalf("floor message = %q, want the floor and its waiver named", result.Message)
	}
}

// The exit codes are a published machine contract — docs/install/update.md
// tabulates every one of them and tells wrapper authors to branch on them — so
// they are pinned to their literals rather than rebuilt from the constants
// they document. Renumbering ExitUpdateAvailable to 99 and ExitOwnership to 98
// used to leave both this package and cmd/aplexica green.
func TestExitCodesMatchThePublishedContract(t *testing.T) {
	for name, pair := range map[string]struct{ got, want int }{
		"ExitOperational":       {ExitOperational, 1},
		"ExitConfirmation":      {ExitConfirmation, 2},
		"ExitSecurity":          {ExitSecurity, 3},
		"ExitUpdateAvailable":   {ExitUpdateAvailable, 10},
		"ExitDelegated":         {ExitDelegated, 20},
		"ExitBootstrapRequired": {ExitBootstrapRequired, 21},
		"ExitOwnership":         {ExitOwnership, 22},
	} {
		if pair.got != pair.want {
			t.Errorf("%s = %d, want %d — docs/install/update.md publishes it as %d",
				name, pair.got, pair.want, pair.want)
		}
	}
	// An untyped error is exit 1, which is what the table's row 1 promises for
	// "operational failure" and what every unwrapped error in the CLI becomes.
	if code := ExitCode(fmt.Errorf("plain")); code != ExitOperational {
		t.Fatalf("ExitCode(untyped) = %d, want %d", code, ExitOperational)
	}
	if code := ExitCode(nil); code != 0 {
		t.Fatalf("ExitCode(nil) = %d, want 0", code)
	}
}

func TestEngineCheckRejectsUnsupportedChannel(t *testing.T) {
	discovery := &staticDiscovery{target: testTarget("1.2.4")}
	engine := staticEngine(t, discovery, t.TempDir())
	result, _, _, err := engine.Check(t.Context(), filepath.Join(t.TempDir(), "aplexica"),
		releaseProvenance("v1.2.3"), CheckOptions{Channel: "beta"})
	if err == nil || result.Status != StatusFailed || discovery.called {
		t.Fatalf("unsupported channel result=%+v err=%v", result, err)
	}
}

func TestVersionSequenceOrdersReleases(t *testing.T) {
	for _, testCase := range []struct {
		version  string
		sequence uint64
	}{
		{"1.0.70", 1_000_070},
		{"0.0.1", 1},
		{"1.0.0", 1_000_000},
		{"999.999.999", 999_999_999},
	} {
		sequence, err := VersionSequence(testCase.version)
		if err != nil || sequence != testCase.sequence {
			t.Fatalf("VersionSequence(%q) = %d, %v; want %d", testCase.version, sequence, err, testCase.sequence)
		}
	}
	for _, invalid := range []string{"", "v1.0.70", "1.0", "1.0.70-rc1", "dev", "0.0.0", "1.0.1000"} {
		if sequence, err := VersionSequence(invalid); err == nil {
			t.Fatalf("VersionSequence(%q) = %d, want an error", invalid, sequence)
		}
	}
}

func TestReadUpdateFloorIgnoresUnusableState(t *testing.T) {
	stateDir := t.TempDir()
	if version, sequence := readUpdateFloor(stateDir); version != "" || sequence != 0 {
		t.Fatalf("missing floor = %q/%d", version, sequence)
	}
	if err := os.WriteFile(filepath.Join(stateDir, updateFloorFile), []byte("not-a-version\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if version, sequence := readUpdateFloor(stateDir); version != "" || sequence != 0 {
		t.Fatalf("malformed floor = %q/%d", version, sequence)
	}
}

// A state directory that does not exist yet is not an error and is not created
// on our behalf: `aplexica update` reads, it does not provision daemon state.
func TestRaiseUpdateFloorDoesNotCreateTheStateDirectory(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "absent")
	raiseUpdateFloor(stateDir, "1.2.3")
	if _, err := os.Stat(stateDir); !os.IsNotExist(err) {
		t.Fatalf("state directory stat = %v, want it left absent", err)
	}
}

func TestRaiseUpdateFloorNeverRegressesUnderConcurrentWriters(t *testing.T) {
	stateDir := t.TempDir()
	versions := []string{"1.2.4", "1.9.9", "2.0.0", "1.3.8", "1.2.5"}
	var writers sync.WaitGroup
	for repetition := 0; repetition < 20; repetition++ {
		for _, version := range versions {
			writers.Add(1)
			go func() {
				defer writers.Done()
				raiseUpdateFloor(stateDir, version)
			}()
		}
	}
	writers.Wait()
	if version, sequence := readUpdateFloor(stateDir); version != "2.0.0" || sequence != 2_000_000 {
		t.Fatalf("concurrent floor = %q/%d, want 2.0.0/2000000", version, sequence)
	}
}

func TestEngineCheckIncludesEveryOperationalFailureInTheResult(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		engine Engine
	}{
		{
			name: "factory",
			engine: Engine{
				Classifier: Classifier{Runner: fakeRunner(func(string, ...string) ([]byte, error) {
					return nil, fmt.Errorf("no manager receipt")
				})},
				DiscoveryFactory: func(string) (Discovery, error) {
					return nil, fmt.Errorf("factory unavailable")
				},
				StateDir: t.TempDir(),
			},
		},
		{
			name:   "refresh",
			engine: staticEngine(t, &staticDiscovery{err: fmt.Errorf("metadata unavailable")}, t.TempDir()),
		},
		{
			name:   "invalid target",
			engine: staticEngine(t, &staticDiscovery{target: Target{}}, t.TempDir()),
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			result, _, _, err := testCase.engine.Check(
				t.Context(), filepath.Join(t.TempDir(), "aplexica"),
				releaseProvenance("v1.2.3"), CheckOptions{Channel: "stable"},
			)
			if err == nil || result.Status != StatusFailed || strings.TrimSpace(result.Message) == "" {
				t.Fatalf("result=%+v err=%v, want a visible operational failure", result, err)
			}
		})
	}
}

func testTarget(version string) Target {
	sequence, err := VersionSequence(version)
	if err != nil {
		panic(err)
	}
	return Target{
		Schema: "aplexica.channel-target/v1", Channel: "stable",
		Repository: releasetrust.Repository, Version: version, Sequence: sequence,
		ReleaseNotesURL: "https://github.com/Aplexica/Aplexica/releases/tag/v" + version,
	}
}
