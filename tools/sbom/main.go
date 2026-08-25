// Command sbom emits the deterministic, vendor-neutral CycloneDX SBOM
// published as aplexica.sbom.cdx.json with every release. It runs from
// .goreleaser.yaml's before.hooks, at the module root, and its output is
// folded into SHA256SUMS via checksum.extra_files — so the SBOM a user
// downloads is covered by the same cosign signature as the binaries.
//
// It deliberately uses only the Go standard library: an SBOM generator is a
// build-time dependency of every release, and pulling in a third-party one
// would widen the trusted build surface the SBOM exists to describe.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	cycloneDXSchema  = "http://cyclonedx.org/schema/bom-1.6.schema.json"
	maxCommandOutput = 32 << 20
	maxCommandError  = 1 << 20
	maxModules       = 100_000
	maxGraphEdges    = 1_000_000
)

var (
	releaseVersionRE = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
	commitRE         = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)
	moduleVersionRE  = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z]+(?:[.-][0-9A-Za-z]+)*)?(\+[0-9A-Za-z]+(?:[.-][0-9A-Za-z]+)*)?$`)
	goVersionNodeRE  = regexp.MustCompile(`^go@[0-9]+(?:\.[0-9]+){0,2}$`)
	toolchainNodeRE  = regexp.MustCompile(`^toolchain@go[0-9]+(?:\.[0-9]+){0,2}$`)
)

type options struct {
	output          string
	version         string
	commit          string
	sourceDateEpoch string
	home            string
	goModCache      string
	goBuildCache    string
	moduleRoot      string
}

type releaseIdentity struct {
	version string
	commit  string
	epoch   int64
}

type goModule struct {
	Path      string    `json:"Path"`
	Version   string    `json:"Version"`
	Main      bool      `json:"Main"`
	Indirect  bool      `json:"Indirect"`
	GoVersion string    `json:"GoVersion"`
	Sum       string    `json:"Sum"`
	Replace   *goModule `json:"Replace"`
}

type bom struct {
	Schema       string       `json:"$schema"`
	BOMFormat    string       `json:"bomFormat"`
	SpecVersion  string       `json:"specVersion"`
	Version      int          `json:"version"`
	Metadata     metadata     `json:"metadata"`
	Components   []component  `json:"components"`
	Dependencies []dependency `json:"dependencies"`
}

type metadata struct {
	Timestamp time.Time `json:"timestamp"`
	Component component `json:"component"`
}

type component struct {
	Type       string     `json:"type"`
	BOMRef     string     `json:"bom-ref"`
	Group      string     `json:"group,omitempty"`
	Name       string     `json:"name"`
	Version    string     `json:"version"`
	Hashes     []hash     `json:"hashes,omitempty"`
	PURL       string     `json:"purl"`
	Properties []property `json:"properties,omitempty"`
}

type hash struct {
	Algorithm string `json:"alg"`
	Content   string `json:"content"`
}

type property struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type dependency struct {
	Ref       string   `json:"ref"`
	DependsOn []string `json:"dependsOn"`
}

type commandRunner interface {
	run(context.Context, string, []string, []string, string) ([]byte, []byte, error)
}

type execRunner struct{}

func (execRunner) run(ctx context.Context, executable string, args, env []string, directory string) ([]byte, []byte, error) {
	var stdout, stderr limitedBuffer
	stdout.limit = maxCommandOutput
	stderr.limit = maxCommandError
	cmd := exec.CommandContext(ctx, executable, args...)
	cmd.Dir = directory
	cmd.Env = env
	cmd.Stdin = nil
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}

type limitedBuffer struct {
	bytes.Buffer
	limit int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if len(p) > b.limit-b.Len() {
		return 0, fmt.Errorf("command output exceeds %d bytes", b.limit)
	}
	return b.Buffer.Write(p)
}

func main() {
	var opts options
	flag.StringVar(&opts.output, "output", "aplexica.sbom.cdx.json", "output file (current-directory basename only)")
	flag.StringVar(&opts.version, "version", "", "source release version, including the leading v")
	flag.StringVar(&opts.commit, "commit", "", "full lowercase source commit identifier")
	flag.StringVar(&opts.sourceDateEpoch, "source-date-epoch", "", "tagged source commit time as Unix seconds")
	flag.StringVar(&opts.home, "home", "", "private absolute HOME for child Go commands (requires both cache flags)")
	flag.StringVar(&opts.goModCache, "go-mod-cache", "", "authenticated absolute offline module cache")
	flag.StringVar(&opts.goBuildCache, "go-build-cache", "", "private absolute writable Go build cache")
	flag.StringVar(&opts.moduleRoot, "module-root", "", "authenticated absolute module root for child Go commands")
	flag.Parse()
	if flag.NArg() != 0 {
		fatalf("unexpected positional arguments")
	}
	if err := rejectParentGOROOT(os.LookupEnv); err != nil {
		fatalf("unsafe release environment: %v", err)
	}

	identity, err := validateOptions(opts, os.Getenv("SOURCE_DATE_EPOCH"))
	if err != nil {
		fatalf("invalid release identity: %v", err)
	}
	goBinary, err := resolveGoBinary()
	if err != nil {
		fatalf("resolve release Go toolchain: %v", err)
	}
	home, moduleCache, buildCache, err := releaseGoPaths(opts)
	if err != nil {
		fatalf("resolve release Go paths: %v", err)
	}
	moduleRoot, err := releaseModuleRoot(opts.moduleRoot)
	if err != nil {
		fatalf("resolve release module root: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	modulesJSON, graphText, err := collectGoModuleEvidence(ctx, execRunner{}, goBinary, home, moduleCache, buildCache, moduleRoot)
	if err != nil {
		fatalf("collect locked Go module evidence: %v", err)
	}
	result, err := generateBOM(modulesJSON, graphText, identity)
	if err != nil {
		fatalf("generate CycloneDX SBOM: %v", err)
	}
	if err := writeAtomically(opts.output, result); err != nil {
		fatalf("write SBOM: %v", err)
	}
}

func rejectParentGOROOT(lookup func(string) (string, bool)) error {
	if value, present := lookup("GOROOT"); present && value != "" {
		return errors.New("GOROOT must be unset so the generator uses the Go root compiled into its trusted toolchain")
	}
	return nil
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "sbom: "+format+"\n", args...)
	os.Exit(1)
}

func validateOptions(opts options, environmentEpoch string) (releaseIdentity, error) {
	if !releaseVersionRE.MatchString(opts.version) {
		return releaseIdentity{}, errors.New("version must be a canonical v-prefixed semantic version")
	}
	if !commitRE.MatchString(opts.commit) {
		return releaseIdentity{}, errors.New("commit must be a full 40- or 64-character lowercase hexadecimal identifier")
	}
	if err := validateOutputName(opts.output); err != nil {
		return releaseIdentity{}, err
	}
	epochText := opts.sourceDateEpoch
	if epochText == "" {
		epochText = environmentEpoch
	} else if environmentEpoch != "" && environmentEpoch != epochText {
		return releaseIdentity{}, errors.New("--source-date-epoch disagrees with SOURCE_DATE_EPOCH")
	}
	if epochText == "" {
		return releaseIdentity{}, errors.New("source date epoch is required")
	}
	if len(epochText) > 20 || strings.HasPrefix(epochText, "+") || strings.TrimLeft(epochText, "0123456789") != "" {
		return releaseIdentity{}, errors.New("source date epoch must contain only unsigned decimal seconds")
	}
	epoch, err := strconv.ParseInt(epochText, 10, 64)
	if err != nil || epoch < 0 {
		return releaseIdentity{}, errors.New("source date epoch is outside the supported range")
	}
	timestamp := time.Unix(epoch, 0).UTC()
	if timestamp.Year() < 1 || timestamp.Year() > 9999 {
		return releaseIdentity{}, errors.New("source date epoch cannot be represented by CycloneDX RFC 3339 time")
	}
	return releaseIdentity{version: opts.version, commit: opts.commit, epoch: epoch}, nil
}

func validateOutputName(name string) error {
	if name == "" || name == "." || name == ".." || filepath.Base(name) != name || strings.ContainsAny(name, "/\\\x00\r\n") {
		return errors.New("output must be a plain filename in the current directory")
	}
	return nil
}

func resolveGoBinary() (string, error) {
	name := "go"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	candidate := filepath.Join(runtime.GOROOT(), "bin", name)
	resolved, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return "", err
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return "", errors.New("release Go executable is not an executable regular file")
	}
	return resolved, nil
}

func operatingSystemHome() (string, error) {
	current, err := user.Current()
	if err != nil {
		return "", err
	}
	home, err := filepath.Abs(current.HomeDir)
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(home) || strings.ContainsAny(home, "\x00\r\n") {
		return "", errors.New("operating-system home is not an absolute clean path")
	}
	return home, nil
}

func releaseGoPaths(opts options) (home, moduleCache, buildCache string, err error) {
	provided := 0
	for _, value := range []string{opts.home, opts.goModCache, opts.goBuildCache} {
		if value != "" {
			provided++
		}
	}
	if provided == 0 {
		home, err = operatingSystemHome()
		return home, "", "", err
	}
	if provided != 3 {
		return "", "", "", errors.New("--home, --go-mod-cache, and --go-build-cache must be supplied together")
	}
	for label, value := range map[string]string{"home": opts.home, "module cache": opts.goModCache, "build cache": opts.goBuildCache} {
		if !filepath.IsAbs(value) || filepath.Clean(value) != value || strings.ContainsAny(value, "\x00\r\n") {
			return "", "", "", fmt.Errorf("%s must be an absolute clean path", label)
		}
		info, statErr := os.Stat(value)
		if statErr != nil || !info.IsDir() {
			return "", "", "", fmt.Errorf("%s must be an existing directory", label)
		}
	}
	return opts.home, opts.goModCache, opts.goBuildCache, nil
}

func releaseModuleRoot(configured string) (string, error) {
	root := configured
	if root == "" {
		var err error
		root, err = os.Getwd()
		if err != nil {
			return "", err
		}
	}
	if !filepath.IsAbs(root) || filepath.Clean(root) != root || strings.ContainsAny(root, "\x00\r\n") {
		return "", errors.New("module root must be an absolute clean path")
	}
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil || resolved != root {
		return "", errors.New("module root or an ancestor is a symlink")
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("module root must be a direct directory")
	}
	return root, nil
}

func collectGoModuleEvidence(ctx context.Context, runner commandRunner, goBinary, home, moduleCache, buildCache, moduleRoot string) ([]byte, []byte, error) {
	if !filepath.IsAbs(goBinary) || !filepath.IsAbs(home) || strings.ContainsAny(goBinary+home, "\x00\r\n") {
		return nil, nil, errors.New("Go executable and home must be absolute clean paths")
	}
	if (moduleCache == "") != (buildCache == "") || (moduleCache != "" && (!filepath.IsAbs(moduleCache) || !filepath.IsAbs(buildCache))) {
		return nil, nil, errors.New("go module/build caches must be absent together or absolute together")
	}
	if !filepath.IsAbs(moduleRoot) || filepath.Clean(moduleRoot) != moduleRoot {
		return nil, nil, errors.New("module root must be absolute and clean")
	}
	env := sanitizedGoEnvironment(goBinary, home, moduleCache, buildCache)
	modules, stderr, err := runner.run(ctx, goBinary, []string{"list", "-mod=readonly", "-m", "-json", "all"}, env, moduleRoot)
	if err != nil {
		return nil, nil, commandError("go list -mod=readonly -m -json all", stderr, err)
	}
	graph, stderr, err := runner.run(ctx, goBinary, []string{"mod", "graph"}, env, moduleRoot)
	if err != nil {
		return nil, nil, commandError("go mod graph", stderr, err)
	}
	return modules, graph, nil
}

func sanitizedGoEnvironment(goBinary, home, moduleCache, buildCache string) []string {
	environment := []string{
		"PATH=" + filepath.Dir(goBinary),
		"HOME=" + home,
		"CGO_ENABLED=0",
		"GO111MODULE=on",
		"GOAUTH=off",
		"GOENV=off",
		"GOFLAGS=-mod=readonly",
		"GONOPROXY=",
		"GONOSUMDB=",
		"GOPRIVATE=",
		"GOPROXY=off",
		"GOSUMDB=off",
		"GOTOOLCHAIN=local",
		"GOWORK=off",
		"LANG=C",
		"LC_ALL=C",
	}
	if moduleCache != "" {
		environment = append(environment, "GOMODCACHE="+moduleCache, "GOCACHE="+buildCache)
	}
	return environment
}

func commandError(name string, stderr []byte, err error) error {
	message := strings.TrimSpace(string(stderr))
	if len(message) > 1024 {
		message = message[:1024] + "..."
	}
	if message == "" {
		return fmt.Errorf("%s: %w", name, err)
	}
	return fmt.Errorf("%s: %w: %s", name, err, message)
}

func generateBOM(moduleData, graphData []byte, identity releaseIdentity) ([]byte, error) {
	modules, root, err := parseModules(moduleData)
	if err != nil {
		return nil, err
	}
	rootRef := modulePURL(root.Path, identity.version)
	components := make([]component, 0, len(modules)-1)
	componentRefs := make(map[string]string, len(modules)-1)
	selectedVersions := make(map[string]string, len(modules)-1)
	purlOwners := map[string]string{rootRef: root.Path}
	for _, module := range modules {
		if module.Main {
			continue
		}
		ref := modulePURL(module.Path, module.Version)
		if owner, exists := purlOwners[ref]; exists {
			return nil, fmt.Errorf("module paths %q and %q collide after canonical purl normalization", owner, module.Path)
		}
		purlOwners[ref] = module.Path
		if prior, exists := componentRefs[module.Path]; exists && prior != ref {
			return nil, fmt.Errorf("module %q has more than one selected version", module.Path)
		}
		componentRefs[module.Path] = ref
		selectedVersions[module.Path] = module.Version
		item, err := moduleComponent(module, ref)
		if err != nil {
			return nil, err
		}
		components = append(components, item)
	}
	sort.Slice(components, func(i, j int) bool { return components[i].BOMRef < components[j].BOMRef })

	dependencies, err := parseGraph(graphData, root.Path, rootRef, componentRefs, selectedVersions)
	if err != nil {
		return nil, err
	}
	group, name := splitModuleName(root.Path)
	rootComponent := component{
		Type:    "application",
		BOMRef:  rootRef,
		Group:   group,
		Name:    name,
		Version: identity.version,
		PURL:    rootRef,
		Properties: []property{
			{Name: "aplexica:gomod:path", Value: root.Path},
			{Name: "aplexica:source:commit", Value: identity.commit},
			{Name: "aplexica:source:epoch", Value: strconv.FormatInt(identity.epoch, 10)},
		},
	}
	result := bom{
		Schema:      cycloneDXSchema,
		BOMFormat:   "CycloneDX",
		SpecVersion: "1.6",
		Version:     1,
		Metadata: metadata{
			Timestamp: time.Unix(identity.epoch, 0).UTC(),
			Component: rootComponent,
		},
		Components:   components,
		Dependencies: dependencies,
	}
	encoded, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(encoded, '\n'), nil
}

func parseModules(data []byte) ([]goModule, goModule, error) {
	if len(data) > maxCommandOutput {
		return nil, goModule{}, fmt.Errorf("module list exceeds %d bytes", maxCommandOutput)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	modules := make([]goModule, 0, 128)
	seen := make(map[string]struct{})
	var root goModule
	for {
		var raw json.RawMessage
		err := decoder.Decode(&raw)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, goModule{}, fmt.Errorf("decode go list module stream: %w", err)
		}
		if err := rejectDuplicateObjectFields(raw); err != nil {
			return nil, goModule{}, fmt.Errorf("decode go list module object: %w", err)
		}
		var module goModule
		if err := json.Unmarshal(raw, &module); err != nil {
			return nil, goModule{}, fmt.Errorf("decode go list module stream: %w", err)
		}
		if len(modules) >= maxModules {
			return nil, goModule{}, fmt.Errorf("module count exceeds %d", maxModules)
		}
		if err := validateModule(module); err != nil {
			return nil, goModule{}, err
		}
		if _, exists := seen[module.Path]; exists {
			return nil, goModule{}, fmt.Errorf("duplicate module path %q", module.Path)
		}
		seen[module.Path] = struct{}{}
		if module.Main {
			if root.Path != "" {
				return nil, goModule{}, errors.New("go list identified more than one main module")
			}
			root = module
		}
		modules = append(modules, module)
	}
	if root.Path == "" {
		return nil, goModule{}, errors.New("go list did not identify a main module")
	}
	return modules, root, nil
}

func rejectDuplicateObjectFields(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	opening, err := decoder.Token()
	if err != nil {
		return err
	}
	if delimiter, ok := opening.(json.Delim); !ok || delimiter != '{' {
		return errors.New("module entry is not a JSON object")
	}
	seen := make(map[string]struct{})
	for decoder.More() {
		fieldToken, err := decoder.Token()
		if err != nil {
			return err
		}
		field, ok := fieldToken.(string)
		if !ok {
			return errors.New("module object field name is not a string")
		}
		if _, exists := seen[field]; exists {
			return fmt.Errorf("duplicate module object field %q", field)
		}
		seen[field] = struct{}{}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return err
		}
	}
	closing, err := decoder.Token()
	if err != nil {
		return err
	}
	if delimiter, ok := closing.(json.Delim); !ok || delimiter != '}' {
		return errors.New("module object is not properly closed")
	}
	if decoder.More() {
		return errors.New("module object has trailing JSON values")
	}
	return nil
}

func validateModule(module goModule) error {
	if err := validateModulePath(module.Path); err != nil {
		return fmt.Errorf("invalid module path %q: %w", module.Path, err)
	}
	if module.Replace != nil {
		return fmt.Errorf("module %q uses a replacement; release SBOM requires an immutable version-only graph", module.Path)
	}
	if module.Main {
		if module.Version != "" {
			return errors.New("main module unexpectedly has a selected dependency version")
		}
		return nil
	}
	if !moduleVersionRE.MatchString(module.Version) {
		return fmt.Errorf("module %q has non-canonical selected version %q", module.Path, module.Version)
	}
	if module.GoVersion != "" && (!cleanASCII(module.GoVersion, 64) || strings.ContainsAny(module.GoVersion, `/\\@`)) {
		return fmt.Errorf("module %q has invalid Go version metadata", module.Path)
	}
	if module.Sum != "" {
		if _, err := decodeGoSum(module.Sum); err != nil {
			return fmt.Errorf("module %q has invalid content sum: %w", module.Path, err)
		}
	}
	return nil
}

func validateModulePath(path string) error {
	if !cleanASCII(path, 1024) || strings.ContainsAny(path, `\\@%?#`) {
		return errors.New("path contains a control, delimiter, non-ASCII, or forbidden URL character")
	}
	parts := strings.Split(path, "/")
	if len(parts) < 1 {
		return errors.New("path is empty")
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." || len(part) > 255 {
			return errors.New("path has an empty, traversal, or oversized segment")
		}
		for _, character := range []byte(part) {
			if !isModulePathCharacter(character) {
				return fmt.Errorf("path segment contains forbidden byte 0x%02x", character)
			}
		}
	}
	return nil
}

func isModulePathCharacter(character byte) bool {
	return character >= 'a' && character <= 'z' ||
		character >= 'A' && character <= 'Z' ||
		character >= '0' && character <= '9' ||
		strings.ContainsRune("-._~+", rune(character))
}

func cleanASCII(value string, limit int) bool {
	if value == "" || len(value) > limit {
		return false
	}
	for _, character := range []byte(value) {
		if character < 0x21 || character > 0x7e {
			return false
		}
	}
	return true
}

func moduleComponent(module goModule, ref string) (component, error) {
	group, name := splitModuleName(module.Path)
	properties := []property{
		{Name: "aplexica:gomod:indirect", Value: strconv.FormatBool(module.Indirect)},
		{Name: "aplexica:gomod:path", Value: module.Path},
	}
	if module.GoVersion != "" {
		properties = append(properties, property{Name: "aplexica:gomod:go-version", Value: module.GoVersion})
	}
	var hashes []hash
	if module.Sum != "" {
		digest, err := decodeGoSum(module.Sum)
		if err != nil {
			return component{}, err
		}
		hashes = []hash{{Algorithm: "SHA-256", Content: strings.ToUpper(hex.EncodeToString(digest))}}
		properties = append(properties, property{Name: "aplexica:gomod:sum", Value: module.Sum})
	}
	sort.Slice(properties, func(i, j int) bool { return properties[i].Name < properties[j].Name })
	return component{
		Type:       "library",
		BOMRef:     ref,
		Group:      group,
		Name:       name,
		Version:    module.Version,
		Hashes:     hashes,
		PURL:       ref,
		Properties: properties,
	}, nil
}

func decodeGoSum(value string) ([]byte, error) {
	if !strings.HasPrefix(value, "h1:") {
		return nil, errors.New("sum does not use the h1 SHA-256 format")
	}
	digest, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(value, "h1:"))
	if err != nil || len(digest) != sha256.Size {
		return nil, errors.New("sum is not a 32-byte base64 SHA-256 digest")
	}
	return digest, nil
}

func parseGraph(data []byte, rootPath, rootRef string, refs, selectedVersions map[string]string) ([]dependency, error) {
	if len(data) > maxCommandOutput {
		return nil, fmt.Errorf("module graph exceeds %d bytes", maxCommandOutput)
	}
	edges := make(map[string]map[string]struct{}, len(refs)+1)
	seenRefs := make(map[string]struct{}, len(refs)+1)
	edges[rootRef] = make(map[string]struct{})
	for _, ref := range refs {
		edges[ref] = make(map[string]struct{})
	}
	lineNumber := 0
	for len(data) > 0 {
		lineNumber++
		if lineNumber > maxGraphEdges {
			return nil, fmt.Errorf("module graph exceeds %d edges", maxGraphEdges)
		}
		var line []byte
		if index := bytes.IndexByte(data, '\n'); index >= 0 {
			line, data = data[:index], data[index+1:]
		} else {
			line, data = data, nil
		}
		if len(line) == 0 {
			if len(data) == 0 {
				continue
			}
			return nil, fmt.Errorf("module graph line %d is empty", lineNumber)
		}
		if len(line) > 4096 || bytes.ContainsAny(line, "\r\t\x00") || bytes.Count(line, []byte{' '}) != 1 {
			return nil, fmt.Errorf("module graph line %d is not canonical", lineNumber)
		}
		separator := bytes.IndexByte(line, ' ')
		parentToken, childToken := string(line[:separator]), string(line[separator+1:])
		parentRef, parentSelected, parentSpecial, err := graphNode(parentToken, rootPath, rootRef, refs, selectedVersions)
		if err != nil {
			return nil, fmt.Errorf("module graph line %d parent: %w", lineNumber, err)
		}
		childRef, childSelected, childSpecial, err := graphNode(childToken, rootPath, rootRef, refs, selectedVersions)
		if err != nil {
			return nil, fmt.Errorf("module graph line %d child: %w", lineNumber, err)
		}
		if parentSelected && !parentSpecial {
			seenRefs[parentRef] = struct{}{}
		}
		if childSelected && !childSpecial {
			seenRefs[childRef] = struct{}{}
		}
		if parentSpecial || childSpecial || !parentSelected {
			continue
		}
		if parentRef == childRef {
			continue
		}
		edges[parentRef][childRef] = struct{}{}
	}
	if len(refs) > 0 {
		if _, found := seenRefs[rootRef]; !found {
			return nil, errors.New("module graph does not contain the main module")
		}
		for _, ref := range refs {
			if _, found := seenRefs[ref]; !found {
				return nil, fmt.Errorf("selected module %q is absent from the module graph", ref)
			}
		}
	}

	dependencies := make([]dependency, 0, len(edges))
	for ref, targets := range edges {
		dependsOn := make([]string, 0, len(targets))
		for target := range targets {
			dependsOn = append(dependsOn, target)
		}
		sort.Strings(dependsOn)
		dependencies = append(dependencies, dependency{Ref: ref, DependsOn: dependsOn})
	}
	sort.Slice(dependencies, func(i, j int) bool { return dependencies[i].Ref < dependencies[j].Ref })
	return dependencies, nil
}

func graphNode(token, rootPath, rootRef string, refs, selectedVersions map[string]string) (ref string, selected, special bool, err error) {
	if token == rootPath {
		return rootRef, true, false, nil
	}
	if goVersionNodeRE.MatchString(token) || toolchainNodeRE.MatchString(token) {
		return "", false, true, nil
	}
	separator := strings.LastIndexByte(token, '@')
	if separator <= 0 || separator == len(token)-1 {
		return "", false, false, fmt.Errorf("invalid module node %q", token)
	}
	path, version := token[:separator], token[separator+1:]
	if err := validateModulePath(path); err != nil {
		return "", false, false, fmt.Errorf("invalid module node path %q: %w", path, err)
	}
	if !moduleVersionRE.MatchString(version) {
		return "", false, false, fmt.Errorf("invalid module node version %q", version)
	}
	ref, exists := refs[path]
	if !exists {
		return "", false, false, fmt.Errorf("graph references module %q absent from the selected module list", path)
	}
	return ref, version == selectedVersions[path], false, nil
}

func splitModuleName(path string) (string, string) {
	separator := strings.LastIndexByte(path, '/')
	if separator < 0 {
		return "", path
	}
	return path[:separator], path[separator+1:]
}

func modulePURL(path, version string) string {
	// The registered Package URL definition for the golang type requires the
	// namespace and name to be lowercase, even though Go module paths may use
	// uppercase letters. Preserve the exact module path in the component fields
	// and aplexica:gomod:path property while normalizing its purl identity.
	path = strings.ToLower(path)
	parts := strings.Split(path, "/")
	for index := range parts {
		parts[index] = percentEncode(parts[index])
	}
	return "pkg:golang/" + strings.Join(parts, "/") + "@" + percentEncode(version)
}

func percentEncode(value string) string {
	const hexDigits = "0123456789ABCDEF"
	var result strings.Builder
	for _, character := range []byte(value) {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' || strings.ContainsRune("-._~", rune(character)) {
			result.WriteByte(character)
			continue
		}
		result.WriteByte('%')
		result.WriteByte(hexDigits[character>>4])
		result.WriteByte(hexDigits[character&15])
	}
	return result.String()
}

func writeAtomically(output string, data []byte) (returnErr error) {
	if err := validateOutputName(output); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(".", ".aplexica-sbom-*")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	defer func() {
		if err := os.Remove(temporaryName); err != nil && !os.IsNotExist(err) && returnErr == nil {
			returnErr = err
		}
	}()
	if err := temporary.Chmod(0600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryName, output); err != nil {
		return err
	}
	return nil
}
