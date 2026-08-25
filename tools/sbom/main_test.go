package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"
)

var testIdentity = releaseIdentity{
	version: "v1.2.3",
	commit:  strings.Repeat("a", 40),
	epoch:   1_700_000_000,
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	// The persistent Windows runner may retain CRLF working-tree fixtures from
	// before their eol=lf attribute was introduced. Normalize only fixture
	// transport line endings; inline CR rejection tests still exercise the
	// production parser's strict canonical-evidence rule.
	return bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))
}

func TestGenerateBOMIsCanonicalAndDeterministic(t *testing.T) {
	first, err := generateBOM(fixture(t, "modules-order-a.json"), fixture(t, "graph-order-a.txt"), testIdentity)
	if err != nil {
		t.Fatal(err)
	}
	second, err := generateBOM(fixture(t, "modules-order-b.json"), fixture(t, "graph-order-b.txt"), testIdentity)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatalf("reordered Go evidence changed the SBOM:\nfirst:\n%s\nsecond:\n%s", first, second)
	}
	if !strings.HasSuffix(string(first), "\n") {
		t.Fatal("SBOM must end in exactly one newline")
	}
	if strings.Contains(string(first), "/Users/") || strings.Contains(string(first), "testdata") {
		t.Fatal("SBOM leaked a local filesystem path")
	}

	var parsed bom
	if err := json.Unmarshal(first, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed.Schema != cycloneDXSchema || parsed.BOMFormat != "CycloneDX" || parsed.SpecVersion != "1.6" || parsed.Version != 1 {
		t.Fatalf("invalid CycloneDX envelope: %+v", parsed)
	}
	if got, want := parsed.Metadata.Timestamp, time.Unix(testIdentity.epoch, 0).UTC(); !got.Equal(want) {
		t.Fatalf("timestamp = %s, want %s", got, want)
	}
	rootPURL := "pkg:golang/github.com/aplexica/aplexica@v1.2.3"
	if parsed.Metadata.Component.BOMRef != rootPURL || parsed.Metadata.Component.PURL != rootPURL {
		t.Fatalf("root component is not identified by canonical purl: %+v", parsed.Metadata.Component)
	}
	if got, want := parsed.Metadata.Component.Properties, []property{
		{Name: "aplexica:gomod:path", Value: "github.com/aplexica/aplexica"},
		{Name: "aplexica:source:commit", Value: strings.Repeat("a", 40)},
		{Name: "aplexica:source:epoch", Value: "1700000000"},
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("source properties = %+v, want %+v", got, want)
	}

	wantComponentRefs := []string{
		"pkg:golang/example.com/alpha/mod@v1.2.3%2Bincompatible",
		"pkg:golang/example.net/z@v2.0.0",
	}
	var gotComponentRefs []string
	for _, item := range parsed.Components {
		gotComponentRefs = append(gotComponentRefs, item.BOMRef)
		if item.BOMRef != item.PURL || item.Type != "library" {
			t.Fatalf("module component is malformed: %+v", item)
		}
	}
	if !reflect.DeepEqual(gotComponentRefs, wantComponentRefs) {
		t.Fatalf("component refs = %v, want %v", gotComponentRefs, wantComponentRefs)
	}
	if got := parsed.Components[0].Hashes[0]; got.Algorithm != "SHA-256" || got.Content != strings.Repeat("01", 32) {
		t.Fatalf("decoded module hash = %+v", got)
	}

	wantDependencies := []dependency{
		{Ref: wantComponentRefs[0], DependsOn: []string{wantComponentRefs[1]}},
		{Ref: wantComponentRefs[1], DependsOn: []string{}},
		{Ref: rootPURL, DependsOn: wantComponentRefs},
	}
	if !reflect.DeepEqual(parsed.Dependencies, wantDependencies) {
		t.Fatalf("dependencies = %+v, want %+v", parsed.Dependencies, wantDependencies)
	}
}

func TestMaliciousAndNonCanonicalEvidenceFailsClosed(t *testing.T) {
	validModules := fixture(t, "modules-order-a.json")
	validGraph := fixture(t, "graph-order-a.txt")
	tests := []struct {
		name    string
		modules []byte
		graph   []byte
	}{
		{name: "escaped newline in path", modules: fixture(t, "modules-malicious-newline.json"), graph: validGraph},
		{name: "path traversal graph fixture", modules: validModules, graph: fixture(t, "graph-malicious-path.txt")},
		{name: "backslash path", modules: []byte("{\"Path\":\"github.com/aplexica/aplexica\",\"Main\":true}\n{\"Path\":\"example.com/a\\\\b\",\"Version\":\"v1.0.0\"}"), graph: validGraph},
		{name: "local replacement", modules: []byte("{\"Path\":\"github.com/aplexica/aplexica\",\"Main\":true}\n{\"Path\":\"example.com/a\",\"Version\":\"v1.0.0\",\"Replace\":{\"Path\":\"../fork\"}}"), graph: []byte("github.com/aplexica/aplexica example.com/a@v1.0.0\n")},
		{name: "duplicate module", modules: append(append([]byte{}, validModules...), []byte("{\"Path\":\"example.net/z\",\"Version\":\"v2.0.0\"}\n")...), graph: validGraph},
		{name: "canonical purl collision", modules: []byte("{\"Path\":\"github.com/aplexica/aplexica\",\"Main\":true}\n{\"Path\":\"Example.com/a\",\"Version\":\"v1.0.0\"}\n{\"Path\":\"example.com/A\",\"Version\":\"v1.0.0\"}\n"), graph: []byte("github.com/aplexica/aplexica Example.com/a@v1.0.0\ngithub.com/aplexica/aplexica example.com/A@v1.0.0\n")},
		{name: "duplicate JSON field", modules: []byte("{\"Path\":\"github.com/aplexica/aplexica\",\"Main\":true,\"Main\":false}\n"), graph: nil},
		{name: "trailing JSON", modules: append(append([]byte{}, validModules...), []byte("not-json")...), graph: validGraph},
		{name: "truncated empty graph", modules: validModules, graph: nil},
		{name: "truncated selected graph", modules: validModules, graph: []byte("github.com/aplexica/aplexica example.net/z@v2.0.0\n")},
		{name: "unknown graph module", modules: validModules, graph: []byte("github.com/aplexica/aplexica example.org/unknown@v1.0.0\n")},
		{name: "tab separator", modules: validModules, graph: []byte("github.com/aplexica/aplexica\texample.net/z@v2.0.0\n")},
		{name: "embedded carriage return", modules: validModules, graph: []byte("github.com/aplexica/aplexica example.net/z@v2.0.0\r\n")},
		{name: "empty middle line", modules: validModules, graph: []byte("github.com/aplexica/aplexica example.net/z@v2.0.0\n\ngithub.com/aplexica/aplexica example.com/Alpha/mod@v1.2.3+incompatible\n")},
		{name: "malformed language node", modules: validModules, graph: []byte("github.com/aplexica/aplexica go@evil\n")},
		{name: "invalid h1 sum", modules: []byte("{\"Path\":\"github.com/aplexica/aplexica\",\"Main\":true}\n{\"Path\":\"example.com/a\",\"Version\":\"v1.0.0\",\"Sum\":\"h1:not-base64\"}"), graph: []byte("github.com/aplexica/aplexica example.com/a@v1.0.0\n")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := generateBOM(test.modules, test.graph, testIdentity); err == nil {
				t.Fatal("malicious or non-canonical evidence was accepted")
			}
		})
	}
}

func TestModuleWithoutDependenciesMayHaveEmptyGraph(t *testing.T) {
	modules := []byte("{\"Path\":\"github.com/aplexica/aplexica\",\"Main\":true}\n")
	result, err := generateBOM(modules, nil, testIdentity)
	if err != nil {
		t.Fatal(err)
	}
	var parsed bom
	if err := json.Unmarshal(result, &parsed); err != nil {
		t.Fatal(err)
	}
	if len(parsed.Components) != 0 || len(parsed.Dependencies) != 1 || parsed.Dependencies[0].Ref != parsed.Metadata.Component.BOMRef {
		t.Fatalf("unexpected empty-module SBOM: %+v", parsed)
	}
}

func TestValidateOptionsRejectsAmbiguityAndInjection(t *testing.T) {
	valid := options{
		output:          "aplexica.sbom.cdx.json",
		version:         "v1.2.3",
		commit:          strings.Repeat("b", 40),
		sourceDateEpoch: "1700000000",
	}
	identity, err := validateOptions(valid, "1700000000")
	if err != nil {
		t.Fatal(err)
	}
	if identity.epoch != 1_700_000_000 {
		t.Fatalf("epoch = %d", identity.epoch)
	}
	environmentOnly := valid
	environmentOnly.sourceDateEpoch = ""
	if identity, err := validateOptions(environmentOnly, "1700000000"); err != nil || identity.epoch != 1_700_000_000 {
		t.Fatalf("SOURCE_DATE_EPOCH fallback = %+v, %v", identity, err)
	}

	tests := []struct {
		name             string
		mutate           func(*options)
		environmentEpoch string
	}{
		{name: "output traversal", mutate: func(o *options) { o.output = "../sbom.json" }},
		{name: "output absolute", mutate: func(o *options) { o.output = "/tmp/sbom.json" }},
		{name: "output windows path", mutate: func(o *options) { o.output = `C:\\tmp\\sbom.json` }},
		{name: "version newline", mutate: func(o *options) { o.version = "v1.2.3\nevil" }},
		{name: "version path", mutate: func(o *options) { o.version = "v1.2.3/evil" }},
		{name: "version prerelease", mutate: func(o *options) { o.version = "v1.2.3-rc.1" }},
		{name: "version metadata", mutate: func(o *options) { o.version = "v1.2.3+build.1" }},
		{name: "short commit", mutate: func(o *options) { o.commit = "abcd" }},
		{name: "upper commit", mutate: func(o *options) { o.commit = strings.Repeat("A", 40) }},
		{name: "epoch sign", mutate: func(o *options) { o.sourceDateEpoch = "+1700000000" }},
		{name: "epoch newline", mutate: func(o *options) { o.sourceDateEpoch = "1700000000\n0" }},
		{name: "epoch disagreement", mutate: func(o *options) {}, environmentEpoch: "1700000001"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := valid
			test.mutate(&candidate)
			envEpoch := test.environmentEpoch
			if envEpoch == "" {
				envEpoch = candidate.sourceDateEpoch
			}
			if _, err := validateOptions(candidate, envEpoch); err == nil {
				t.Fatal("invalid option was accepted")
			}
		})
	}
}

func TestRejectParentGOROOT(t *testing.T) {
	if err := rejectParentGOROOT(func(string) (string, bool) { return "/attacker/go", true }); err == nil {
		t.Fatal("parent GOROOT override was accepted")
	}
	if err := rejectParentGOROOT(func(string) (string, bool) { return "", false }); err != nil {
		t.Fatalf("unset GOROOT was rejected: %v", err)
	}
	if err := rejectParentGOROOT(func(string) (string, bool) { return "", true }); err != nil {
		t.Fatalf("explicitly empty GOROOT was rejected: %v", err)
	}
}

type fakeCommandRunner struct {
	t           *testing.T
	executable  string
	directory   string
	home        string
	moduleCache string
	buildCache  string
	modules     []byte
	graph       []byte
	calls       int
}

func (runner *fakeCommandRunner) run(_ context.Context, executable string, args, env []string, directory string) ([]byte, []byte, error) {
	runner.t.Helper()
	if executable != runner.executable {
		runner.t.Fatalf("executable = %q", executable)
	}
	if directory != runner.directory {
		runner.t.Fatalf("unexpected module directory: %s", directory)
	}
	wantArgs := [][]string{
		{"list", "-mod=readonly", "-m", "-json", "all"},
		{"mod", "graph"},
	}
	if runner.calls >= len(wantArgs) || !reflect.DeepEqual(args, wantArgs[runner.calls]) {
		runner.t.Fatalf("call %d args = %v, want %v", runner.calls, args, wantArgs[runner.calls])
	}
	environment := make(map[string]string, len(env))
	for _, value := range env {
		key, value, found := strings.Cut(value, "=")
		if !found {
			runner.t.Fatalf("malformed environment entry %q", value)
		}
		if _, duplicate := environment[key]; duplicate {
			runner.t.Fatalf("duplicate environment key %q", key)
		}
		environment[key] = value
	}
	if environment["GOFLAGS"] != "-mod=readonly" || environment["GOWORK"] != "off" || environment["GOENV"] != "off" || environment["GOTOOLCHAIN"] != "local" || environment["GOPROXY"] != "off" || environment["GOSUMDB"] != "off" {
		runner.t.Fatalf("unsafe Go environment: %v", environment)
	}
	if environment["PATH"] != filepath.Dir(runner.executable) || environment["HOME"] != runner.home || environment["GOMODCACHE"] != runner.moduleCache || environment["GOCACHE"] != runner.buildCache {
		runner.t.Fatalf("unexpected path/home environment: %v", environment)
	}
	wantKeys := []string{
		"CGO_ENABLED", "GO111MODULE", "GOAUTH", "GOCACHE", "GOENV", "GOFLAGS", "GOMODCACHE", "GONOPROXY", "GONOSUMDB",
		"GOPRIVATE", "GOPROXY", "GOSUMDB", "GOTOOLCHAIN", "GOWORK", "HOME", "LANG", "LC_ALL", "PATH",
	}
	gotKeys := make([]string, 0, len(environment))
	for key := range environment {
		gotKeys = append(gotKeys, key)
	}
	sort.Strings(gotKeys)
	if !reflect.DeepEqual(gotKeys, wantKeys) {
		runner.t.Fatalf("environment keys = %v, want %v", gotKeys, wantKeys)
	}
	if _, inherited := environment["APLEXICA_TEST_SECRET"]; inherited {
		runner.t.Fatal("arbitrary parent environment reached the Go subprocess")
	}
	runner.calls++
	if runner.calls == 1 {
		return runner.modules, nil, nil
	}
	return runner.graph, nil, nil
}

func TestCollectGoModuleEvidenceUsesExactCommandsAndSanitizedEnvironment(t *testing.T) {
	t.Setenv("GOWORK", "/tmp/attacker.work")
	t.Setenv("GOPROXY", "https://attacker.invalid")
	t.Setenv("APLEXICA_TEST_SECRET", "must-not-leak")
	root := filepath.Clean(t.TempDir())
	runner := &fakeCommandRunner{
		t:           t,
		executable:  filepath.Join(root, "bin", "go"),
		directory:   filepath.Join(root, "module"),
		home:        filepath.Join(root, "home"),
		moduleCache: filepath.Join(root, "modules"),
		buildCache:  filepath.Join(root, "cache"),
		modules:     fixture(t, "modules-order-a.json"),
		graph:       fixture(t, "graph-order-a.txt"),
	}
	modules, graph, err := collectGoModuleEvidence(context.Background(), runner, runner.executable, runner.home, runner.moduleCache, runner.buildCache, runner.directory)
	if err != nil {
		t.Fatal(err)
	}
	if runner.calls != 2 || !reflect.DeepEqual(modules, runner.modules) || !reflect.DeepEqual(graph, runner.graph) {
		t.Fatalf("unexpected collected evidence or call count: calls=%d", runner.calls)
	}
}

type errorRunner struct{}

func (errorRunner) run(_ context.Context, _ string, _ []string, _ []string, _ string) ([]byte, []byte, error) {
	return nil, []byte("controlled failure"), errors.New("exit status 1")
}

func TestCollectGoModuleEvidenceIncludesBoundedDiagnostic(t *testing.T) {
	root := filepath.Clean(t.TempDir())
	_, _, err := collectGoModuleEvidence(
		context.Background(),
		errorRunner{},
		filepath.Join(root, "bin", "go"),
		filepath.Join(root, "home"),
		filepath.Join(root, "modules"),
		filepath.Join(root, "cache"),
		filepath.Join(root, "module"),
	)
	if err == nil || !strings.Contains(err.Error(), "controlled failure") || !strings.Contains(err.Error(), "go list -mod=readonly") {
		t.Fatalf("error = %v", err)
	}
}

func TestLimitedBufferRejectsOversizeOutput(t *testing.T) {
	buffer := limitedBuffer{limit: 3}
	if _, err := buffer.Write([]byte("abc")); err != nil {
		t.Fatal(err)
	}
	if _, err := buffer.Write([]byte("d")); err == nil {
		t.Fatal("oversized output was accepted")
	}
}

func TestWriteAtomicallyRestrictsNameAndPermissions(t *testing.T) {
	original, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	if err := os.Chdir(directory); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(original) })
	if err := writeAtomically("sbom.json", []byte("evidence\n")); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile("sbom.json")
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "evidence\n" {
		t.Fatalf("content = %q", data)
	}
	info, err := os.Stat("sbom.json")
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		t.Fatalf("mode = %o, want 600", info.Mode().Perm())
	}
	if err := writeAtomically("../escape", []byte("bad")); err == nil {
		t.Fatal("path traversal output was accepted")
	}
}

func TestPercentEncodeUsesCanonicalPURLAlphabet(t *testing.T) {
	if got, want := modulePURL("example.com/A+B/mod", "v1.0.0+incompatible"), "pkg:golang/example.com/a%2Bb/mod@v1.0.0%2Bincompatible"; got != want {
		t.Fatalf("purl = %q, want %q", got, want)
	}
}
