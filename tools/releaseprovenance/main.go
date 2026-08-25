// Command releaseprovenance creates and verifies the deterministic in-toto
// SLSA v1 statement authorized by the release KMS key. It deliberately accepts a small,
// validated input surface: the statement is uploaded to a public transparency
// log, so serializing a GitHub context, environment, or build-tool object would
// risk publishing credentials or private infrastructure metadata forever.
package main

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const (
	canonicalRepository = "Aplexica/Aplexica"
	canonicalWorkflow   = ".github/workflows/release.yml"
	statementType       = "https://in-toto.io/Statement/v1"
	predicateType       = "https://slsa.dev/provenance/v1"
	githubBuildType     = "https://slsa-framework.github.io/github-actions-buildtypes/workflow/v1"
	portalRepository    = "Aplexica/aplexica-portal"
	maxInputSize        = 1 << 20
	maxBundleSize       = 8 << 20
)

var (
	versionRE      = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
	commitRE       = regexp.MustCompile(`^[0-9a-f]{40}$`)
	digestRE       = regexp.MustCompile(`^[0-9a-f]{64}$`)
	digitsRE       = regexp.MustCompile(`^(0|[1-9][0-9]*)$`)
	checksumLineRE = regexp.MustCompile(`^([0-9a-f]{64})  ([^[:space:]]+)$`)
)

type options struct {
	checksums    string
	portal       string
	output       string
	repository   string
	ref          string
	commit       string
	workflowRef  string
	runID        string
	runAttempt   string
	verifyBundle string
}

type portalRelease struct {
	Repository string `json:"repository"`
	Tag        string `json:"tag"`
	Asset      string `json:"asset"`
	SHA256     string `json:"sha256"`
}

type statement struct {
	Type          string        `json:"_type"`
	Subject       []resource    `json:"subject"`
	PredicateType string        `json:"predicateType"`
	Predicate     slsaPredicate `json:"predicate"`
}

type resource struct {
	Name   string            `json:"name,omitempty"`
	URI    string            `json:"uri,omitempty"`
	Digest map[string]string `json:"digest"`
}

type slsaPredicate struct {
	BuildDefinition buildDefinition `json:"buildDefinition"`
	RunDetails      runDetails      `json:"runDetails"`
}

type buildDefinition struct {
	BuildType            string             `json:"buildType"`
	ExternalParameters   externalParameters `json:"externalParameters"`
	ResolvedDependencies []resource         `json:"resolvedDependencies"`
}

type externalParameters struct {
	// Package-channel promotion runs only after the GitHub Release has been
	// built, signed, published, and verified. Its switch is deliberately
	// outside this build-provenance boundary.
	Workflow workflowParameters `json:"workflow"`
}

type workflowParameters struct {
	Ref        string `json:"ref"`
	Repository string `json:"repository"`
	Path       string `json:"path"`
}

type runDetails struct {
	Builder  builder  `json:"builder"`
	Metadata metadata `json:"metadata"`
}

type builder struct {
	ID string `json:"id"`
}

type metadata struct {
	InvocationID string `json:"invocationId"`
}

func main() {
	var opts options
	flag.StringVar(&opts.checksums, "checksums", "dist/SHA256SUMS", "GoReleaser SHA256SUMS path")
	flag.StringVar(&opts.portal, "portal-release", "packaging/portal-release.json", "pinned public Portal release descriptor")
	flag.StringVar(&opts.output, "output", "dist/aplexica.provenance.json", "unsigned statement output path")
	flag.StringVar(&opts.repository, "repository", "", "canonical GitHub repository slug")
	flag.StringVar(&opts.ref, "ref", "", "release tag ref")
	flag.StringVar(&opts.commit, "commit", "", "full source commit")
	flag.StringVar(&opts.workflowRef, "workflow-ref", "", "GitHub workflow ref")
	flag.StringVar(&opts.runID, "run-id", "", "GitHub Actions run ID")
	flag.StringVar(&opts.runAttempt, "run-attempt", "", "GitHub Actions run attempt")
	flag.StringVar(&opts.verifyBundle, "verify-bundle", "", "verify a signed provenance bundle against public release policy")
	flag.Parse()
	if flag.NArg() != 0 {
		fatalf("unexpected positional arguments")
	}

	if opts.verifyBundle != "" {
		if err := verifySignedProvenance(opts); err != nil {
			fatalf("verify statement: %v", err)
		}
		return
	}
	if err := validateOutputPath(opts); err != nil {
		fatalf("%v", err)
	}
	result, err := generate(opts)
	if err != nil {
		fatalf("%v", err)
	}
	if err := writeAtomically(opts.output, result); err != nil {
		fatalf("write statement: %v", err)
	}
}

func validateOutputPath(opts options) error {
	output, err := filepath.Abs(filepath.Clean(opts.output))
	if err != nil {
		return fmt.Errorf("resolve output: %w", err)
	}
	for label, input := range map[string]string{"checksums": opts.checksums, "portal release": opts.portal} {
		resolved, resolveErr := filepath.Abs(filepath.Clean(input))
		if resolveErr != nil {
			return fmt.Errorf("resolve %s: %w", label, resolveErr)
		}
		if output == resolved {
			return fmt.Errorf("output must not overwrite %s input", label)
		}
	}
	return nil
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "release provenance: "+format+"\n", args...)
	os.Exit(1)
}

func generate(opts options) ([]byte, error) {
	version, err := validateIdentity(opts)
	if err != nil {
		return nil, err
	}
	subjects, err := readChecksums(opts.checksums, version)
	if err != nil {
		return nil, fmt.Errorf("checksums: %w", err)
	}
	portal, err := readPortalRelease(opts.portal)
	if err != nil {
		return nil, fmt.Errorf("portal release: %w", err)
	}

	repositoryURL := "https://github.com/" + canonicalRepository
	s := statement{
		Type:          statementType,
		Subject:       subjects,
		PredicateType: predicateType,
		Predicate: slsaPredicate{
			BuildDefinition: buildDefinition{
				BuildType: githubBuildType,
				ExternalParameters: externalParameters{Workflow: workflowParameters{
					Ref:        opts.ref,
					Repository: repositoryURL,
					Path:       canonicalWorkflow,
				}},
				ResolvedDependencies: []resource{
					{
						URI:    "git+" + repositoryURL + "@" + opts.ref,
						Digest: map[string]string{"gitCommit": opts.commit},
					},
					{
						URI:    "https://github.com/" + portal.Repository + "/releases/download/" + portal.Tag + "/" + portal.Asset,
						Digest: map[string]string{"sha256": portal.SHA256},
					},
				},
			},
			RunDetails: runDetails{
				Builder:  builder{ID: "https://github.com/" + opts.workflowRef},
				Metadata: metadata{InvocationID: repositoryURL + "/actions/runs/" + opts.runID + "/attempts/" + opts.runAttempt},
			},
		},
	}

	encoded, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal statement: %w", err)
	}
	return append(encoded, '\n'), nil
}

// verifySignedProvenance enforces the semantic policy cosign intentionally
// does not: the exact subject set, builder, source/tag binding, public Portal
// dependency, and the absence of every unreviewed JSON field. Callers must run
// cosign verify-blob-attestation first; this function validates the signed
// payload, not its cryptography.
func verifySignedProvenance(opts options) error {
	version, err := validateVerificationIdentity(opts)
	if err != nil {
		return err
	}
	wantSubjects, err := readChecksums(opts.checksums, version)
	if err != nil {
		return fmt.Errorf("checksums: %w", err)
	}
	portal, err := readPortalRelease(opts.portal)
	if err != nil {
		return fmt.Errorf("portal release: %w", err)
	}
	payload, err := readBundlePayload(opts.verifyBundle)
	if err != nil {
		return err
	}
	got, err := decodeStrictStatement(payload)
	if err != nil {
		return err
	}
	if got.Type != statementType || got.PredicateType != predicateType {
		return errors.New("statement uses an unexpected envelope or predicate type")
	}
	if !resourcesEqual(got.Subject, wantSubjects) {
		return errors.New("statement subjects do not exactly match the canonical SHA256SUMS set")
	}

	repositoryURL := "https://github.com/" + canonicalRepository
	definition := got.Predicate.BuildDefinition
	if definition.BuildType != githubBuildType ||
		definition.ExternalParameters.Workflow != (workflowParameters{
			Ref: opts.ref, Repository: repositoryURL, Path: canonicalWorkflow,
		}) {
		return errors.New("statement build definition does not bind the canonical release workflow")
	}
	if len(definition.ResolvedDependencies) != 2 {
		return errors.New("statement must contain exactly the source and public Portal dependencies")
	}
	source := definition.ResolvedDependencies[0]
	if source.Name != "" || source.URI != "git+"+repositoryURL+"@"+opts.ref ||
		len(source.Digest) != 1 || source.Digest["gitCommit"] != opts.commit {
		return errors.New("statement source dependency is not the canonical tagged Git commit")
	}
	portalDependency := definition.ResolvedDependencies[1]
	wantPortalURI := "https://github.com/" + portal.Repository + "/releases/download/" + portal.Tag + "/" + portal.Asset
	if portalDependency.Name != "" || portalDependency.URI != wantPortalURI ||
		len(portalDependency.Digest) != 1 || portalDependency.Digest["sha256"] != portal.SHA256 {
		return errors.New("statement Portal dependency does not match the public pinned descriptor")
	}

	wantBuilder := "https://github.com/" + canonicalRepository + "/" + canonicalWorkflow + "@" + opts.ref
	if got.Predicate.RunDetails.Builder.ID != wantBuilder {
		return errors.New("statement builder does not bind the canonical release workflow")
	}
	runPrefix := repositoryURL + "/actions/runs/"
	run := strings.TrimPrefix(got.Predicate.RunDetails.Metadata.InvocationID, runPrefix)
	parts := strings.Split(run, "/attempts/")
	if len(parts) != 2 || run == got.Predicate.RunDetails.Metadata.InvocationID ||
		!positiveCanonicalDecimal(parts[0]) || !positiveCanonicalDecimal(parts[1]) {
		return errors.New("statement invocation ID is not a canonical public release run URL")
	}
	return nil
}

func validateVerificationIdentity(opts options) (string, error) {
	if opts.repository != canonicalRepository {
		return "", fmt.Errorf("repository must be %q", canonicalRepository)
	}
	if !strings.HasPrefix(opts.ref, "refs/tags/") {
		return "", errors.New("ref must be a release tag ref")
	}
	version := strings.TrimPrefix(opts.ref, "refs/tags/")
	if !versionRE.MatchString(version) {
		return "", errors.New("ref must end in a canonical v-prefixed semantic version")
	}
	if !commitRE.MatchString(opts.commit) {
		return "", errors.New("commit must be the expected full lowercase 40-character Git commit")
	}
	return strings.TrimPrefix(version, "v"), nil
}

func positiveCanonicalDecimal(value string) bool {
	if !digitsRE.MatchString(value) {
		return false
	}
	number, err := strconv.ParseUint(value, 10, 64)
	return err == nil && number > 0
}

func resourcesEqual(got, want []resource) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if got[i].Name != want[i].Name || got[i].URI != want[i].URI ||
			len(got[i].Digest) != 1 || got[i].Digest["sha256"] != want[i].Digest["sha256"] {
			return false
		}
	}
	return true
}

func readBundlePayload(filename string) ([]byte, error) {
	data, err := readBoundedFile(filename, maxBundleSize, "bundle")
	if err != nil {
		return nil, err
	}
	var bundle struct {
		DSSEEnvelope *struct {
			Payload string `json:"payload"`
		} `json:"dsseEnvelope"`
	}
	if err := json.Unmarshal(data, &bundle); err != nil {
		return nil, fmt.Errorf("parse bundle: %w", err)
	}
	if bundle.DSSEEnvelope == nil || bundle.DSSEEnvelope.Payload == "" {
		return nil, errors.New("bundle has no DSSE payload")
	}
	payload, err := base64.StdEncoding.DecodeString(bundle.DSSEEnvelope.Payload)
	if err != nil {
		return nil, fmt.Errorf("decode DSSE payload: %w", err)
	}
	if len(payload) == 0 || len(payload) > maxInputSize {
		return nil, errors.New("DSSE payload size is outside policy")
	}
	return payload, nil
}

func readBoundedFile(filename string, limit int64, label string) ([]byte, error) {
	f, err := os.Open(filepath.Clean(filename))
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", label, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() > limit {
		return nil, fmt.Errorf("%s exceeds %d bytes", label, limit)
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%s exceeds %d bytes", label, limit)
	}
	return data, nil
}

func decodeStrictStatement(payload []byte) (statement, error) {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var result statement
	if err := decoder.Decode(&result); err != nil {
		return statement{}, fmt.Errorf("parse statement: %w", err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return statement{}, fmt.Errorf("parse statement: %w", err)
	}
	if err := validateExactStatementShape(payload); err != nil {
		return statement{}, err
	}
	canonical, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return statement{}, err
	}
	canonical = append(canonical, '\n')
	if !bytes.Equal(payload, canonical) {
		return statement{}, errors.New("statement is not the canonical deterministic encoding")
	}
	return result, nil
}

func validateExactStatementShape(payload []byte) error {
	var root map[string]any
	if err := json.Unmarshal(payload, &root); err != nil {
		return err
	}
	if err := exactKeys(root, "statement", "_type", "subject", "predicateType", "predicate"); err != nil {
		return err
	}
	subjects, ok := root["subject"].([]any)
	if !ok {
		return errors.New("statement subject is not an array")
	}
	for i, value := range subjects {
		item, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("subject %d is not an object", i)
		}
		if err := exactKeys(item, fmt.Sprintf("subject %d", i), "name", "digest"); err != nil {
			return err
		}
		if err := exactDigestKeys(item["digest"], fmt.Sprintf("subject %d digest", i), "sha256"); err != nil {
			return err
		}
	}
	predicate, ok := root["predicate"].(map[string]any)
	if !ok {
		return errors.New("predicate is not an object")
	}
	if err := exactKeys(predicate, "predicate", "buildDefinition", "runDetails"); err != nil {
		return err
	}
	definition, ok := predicate["buildDefinition"].(map[string]any)
	if !ok {
		return errors.New("buildDefinition is not an object")
	}
	if err := exactKeys(definition, "buildDefinition", "buildType", "externalParameters", "resolvedDependencies"); err != nil {
		return err
	}
	external, ok := definition["externalParameters"].(map[string]any)
	if !ok {
		return errors.New("externalParameters is not an object")
	}
	if err := exactKeys(external, "externalParameters", "workflow"); err != nil {
		return err
	}
	workflow, ok := external["workflow"].(map[string]any)
	if !ok {
		return errors.New("workflow parameters are not an object")
	}
	if err := exactKeys(workflow, "workflow parameters", "ref", "repository", "path"); err != nil {
		return err
	}
	dependencies, ok := definition["resolvedDependencies"].([]any)
	if !ok || len(dependencies) != 2 {
		return errors.New("resolvedDependencies must contain exactly two objects")
	}
	for i, value := range dependencies {
		item, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("dependency %d is not an object", i)
		}
		if err := exactKeys(item, fmt.Sprintf("dependency %d", i), "uri", "digest"); err != nil {
			return err
		}
		key := "gitCommit"
		if i == 1 {
			key = "sha256"
		}
		if err := exactDigestKeys(item["digest"], fmt.Sprintf("dependency %d digest", i), key); err != nil {
			return err
		}
	}
	details, ok := predicate["runDetails"].(map[string]any)
	if !ok {
		return errors.New("runDetails is not an object")
	}
	if err := exactKeys(details, "runDetails", "builder", "metadata"); err != nil {
		return err
	}
	for key, childKey := range map[string]string{"builder": "id", "metadata": "invocationId"} {
		object, ok := details[key].(map[string]any)
		if !ok {
			return fmt.Errorf("%s is not an object", key)
		}
		if err := exactKeys(object, key, childKey); err != nil {
			return err
		}
	}
	return nil
}

func exactDigestKeys(value any, label, key string) error {
	digest, ok := value.(map[string]any)
	if !ok {
		return fmt.Errorf("%s is not an object", label)
	}
	return exactKeys(digest, label, key)
}

func exactKeys(object map[string]any, label string, want ...string) error {
	if len(object) != len(want) {
		return fmt.Errorf("%s has unreviewed or missing fields", label)
	}
	for _, key := range want {
		if _, ok := object[key]; !ok {
			return fmt.Errorf("%s is missing %q", label, key)
		}
	}
	return nil
}

func validateIdentity(opts options) (string, error) {
	if opts.repository != canonicalRepository {
		return "", fmt.Errorf("repository must be %q", canonicalRepository)
	}
	if !strings.HasPrefix(opts.ref, "refs/tags/") {
		return "", errors.New("ref must be a release tag ref")
	}
	version := strings.TrimPrefix(opts.ref, "refs/tags/")
	if !versionRE.MatchString(version) {
		return "", errors.New("ref must end in a canonical v-prefixed semantic version")
	}
	if !commitRE.MatchString(opts.commit) {
		return "", errors.New("commit must be a full lowercase 40-character hexadecimal Git commit")
	}
	wantWorkflowRef := canonicalRepository + "/" + canonicalWorkflow + "@" + opts.ref
	if opts.workflowRef != wantWorkflowRef {
		return "", fmt.Errorf("workflow-ref must be %q", wantWorkflowRef)
	}
	if !digitsRE.MatchString(opts.runID) {
		return "", errors.New("run-id must be canonical unsigned decimal")
	}
	runID, err := strconv.ParseUint(opts.runID, 10, 64)
	if err != nil || runID == 0 {
		return "", errors.New("run-id must be positive canonical unsigned decimal")
	}
	attempt, err := strconv.ParseUint(opts.runAttempt, 10, 64)
	if err != nil || attempt == 0 || !digitsRE.MatchString(opts.runAttempt) {
		return "", errors.New("run-attempt must be positive canonical unsigned decimal")
	}
	return strings.TrimPrefix(version, "v"), nil
}

func readChecksums(filename, version string) ([]resource, error) {
	f, err := os.Open(filepath.Clean(filename))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() > maxInputSize {
		return nil, fmt.Errorf("manifest exceeds %d bytes", maxInputSize)
	}

	expected := expectedAssets(version)
	seen := make(map[string]bool, len(expected))
	var subjects []resource
	scanner := bufio.NewScanner(io.LimitReader(f, maxInputSize+1))
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		fields := checksumLineRE.FindStringSubmatch(scanner.Text())
		if len(fields) != 3 {
			return nil, fmt.Errorf("line %d is not a lowercase SHA-256, two spaces, and one filename", lineNumber)
		}
		name := fields[2]
		if filepath.Base(name) != name || strings.ContainsAny(name, "/\\") {
			return nil, fmt.Errorf("line %d has a non-canonical asset name", lineNumber)
		}
		if _, ok := expected[name]; !ok {
			return nil, fmt.Errorf("unexpected asset %q", name)
		}
		if seen[name] {
			return nil, fmt.Errorf("duplicate asset %q", name)
		}
		seen[name] = true
		subjects = append(subjects, resource{Name: name, Digest: map[string]string{"sha256": fields[1]}})
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(subjects) != len(expected) {
		var missing []string
		for name := range expected {
			if !seen[name] {
				missing = append(missing, name)
			}
		}
		sort.Strings(missing)
		return nil, fmt.Errorf("found %d assets, want %d; missing %s", len(subjects), len(expected), strings.Join(missing, ", "))
	}
	sort.Slice(subjects, func(i, j int) bool { return subjects[i].Name < subjects[j].Name })
	return subjects, nil
}

func expectedAssets(version string) map[string]struct{} {
	names := []string{
		"aplexica-" + version + "-darwin-amd64.tar.gz",
		"aplexica-" + version + "-darwin-arm64.tar.gz",
		"aplexica-" + version + "-linux-amd64.tar.gz",
		"aplexica-" + version + "-linux-arm64.tar.gz",
		"aplexica-" + version + "-windows-amd64.zip",
		"aplexica-" + version + "-windows-arm64.zip",
		"aplexica_" + version + "_amd64.deb",
		"aplexica_" + version + "_arm64.deb",
		"aplexica-" + version + "-source.tar.gz",
		"aplexica.sbom.cdx.json",
	}
	result := make(map[string]struct{}, len(names))
	for _, name := range names {
		result[name] = struct{}{}
	}
	return result
}

func readPortalRelease(filename string) (portalRelease, error) {
	f, err := os.Open(filepath.Clean(filename))
	if err != nil {
		return portalRelease{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return portalRelease{}, err
	}
	if info.Size() > maxInputSize {
		return portalRelease{}, fmt.Errorf("descriptor exceeds %d bytes", maxInputSize)
	}
	decoder := json.NewDecoder(io.LimitReader(f, maxInputSize+1))
	decoder.DisallowUnknownFields()
	var portal portalRelease
	if err := decoder.Decode(&portal); err != nil {
		return portalRelease{}, err
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return portalRelease{}, err
	}
	if portal.Repository != portalRepository {
		return portalRelease{}, fmt.Errorf("repository must be %q", portalRepository)
	}
	if !versionRE.MatchString(portal.Tag) {
		return portalRelease{}, errors.New("tag must be a canonical v-prefixed semantic version")
	}
	wantAsset := "aplexica-portal-" + portal.Tag + "-local.tar.gz"
	if portal.Asset != wantAsset {
		return portalRelease{}, fmt.Errorf("asset must be %q", wantAsset)
	}
	if !digestRE.MatchString(portal.SHA256) {
		return portalRelease{}, errors.New("sha256 must be a lowercase SHA-256")
	}
	return portal, nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	err := decoder.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return errors.New("multiple JSON values are forbidden")
	}
	return err
}

func writeAtomically(filename string, contents []byte) error {
	directory := filepath.Dir(filename)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".aplexica-provenance-*")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(temporaryName)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := io.Copy(temporary, bytes.NewReader(contents)); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryName, filename); err != nil {
		return err
	}
	committed = true
	return nil
}
