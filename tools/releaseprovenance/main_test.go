package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGenerateEmitsOnlyReviewedPublicProvenance(t *testing.T) {
	root := t.TempDir()
	checksums := filepath.Join(root, "SHA256SUMS")
	portal := filepath.Join(root, "portal.json")
	writeTestFile(t, checksums, validChecksums("1.0.70"))
	writeTestFile(t, portal, `{"repository":"Aplexica/aplexica-portal","tag":"v0.2.0","asset":"aplexica-portal-v0.2.0-local.tar.gz","sha256":"`+strings.Repeat("b", 64)+`"}`)

	result, err := generate(validOptions(checksums, portal))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(result, &got); err != nil {
		t.Fatal(err)
	}
	if got["_type"] != "https://in-toto.io/Statement/v1" || got["predicateType"] != "https://slsa.dev/provenance/v1" {
		t.Fatalf("unexpected statement envelope: %s", result)
	}
	subjects, ok := got["subject"].([]any)
	if !ok || len(subjects) != 10 {
		t.Fatalf("subject count = %d, want 10", len(subjects))
	}

	text := string(result)
	for _, required := range []string{
		`"buildType": "https://slsa-framework.github.io/github-actions-buildtypes/workflow/v1"`,
		`"repository": "https://github.com/Aplexica/Aplexica"`,
		`"path": ".github/workflows/release.yml"`,
		`"uri": "git+https://github.com/Aplexica/Aplexica@refs/tags/v1.0.70"`,
		`"id": "https://github.com/Aplexica/Aplexica/.github/workflows/release.yml@refs/tags/v1.0.70"`,
		`"invocationId": "https://github.com/Aplexica/Aplexica/actions/runs/12345/attempts/2"`,
		`"uri": "https://github.com/Aplexica/aplexica-portal/releases/download/v0.2.0/aplexica-portal-v0.2.0-local.tar.gz"`,
	} {
		if !strings.Contains(text, required) {
			t.Errorf("statement is missing %s", required)
		}
	}
	for _, forbidden := range []string{
		"actor", "owner_id", "repository_id", "environment", "secret",
		"token", "arn:aws:", "accountId", "runner", "hostname", "/Users/",
		"private.example", "internalParameters",
	} {
		if strings.Contains(strings.ToLower(text), strings.ToLower(forbidden)) {
			t.Errorf("statement exposes forbidden field or value %q", forbidden)
		}
	}
	assertExactJSONKeys(t, got, []string{"_type", "predicate", "predicateType", "subject"})
	predicate := got["predicate"].(map[string]any)
	assertExactJSONKeys(t, predicate, []string{"buildDefinition", "runDetails"})
	definition := predicate["buildDefinition"].(map[string]any)
	assertExactJSONKeys(t, definition, []string{"buildType", "externalParameters", "resolvedDependencies"})
	external := definition["externalParameters"].(map[string]any)
	assertExactJSONKeys(t, external, []string{"workflow"})
	workflow := external["workflow"].(map[string]any)
	assertExactJSONKeys(t, workflow, []string{"path", "ref", "repository"})
	details := predicate["runDetails"].(map[string]any)
	assertExactJSONKeys(t, details, []string{"builder", "metadata"})
	assertExactJSONKeys(t, details["builder"].(map[string]any), []string{"id"})
	assertExactJSONKeys(t, details["metadata"].(map[string]any), []string{"invocationId"})
	for index, subject := range subjects {
		item := subject.(map[string]any)
		assertExactJSONKeys(t, item, []string{"digest", "name"})
		assertExactJSONKeys(t, item["digest"].(map[string]any), []string{"sha256"})
		if index > 0 {
			previous := subjects[index-1].(map[string]any)["name"].(string)
			if previous >= item["name"].(string) {
				t.Fatalf("subjects are not strictly sorted: %q then %q", previous, item["name"])
			}
		}
	}
	dependencies := definition["resolvedDependencies"].([]any)
	for index, dependency := range dependencies {
		item := dependency.(map[string]any)
		assertExactJSONKeys(t, item, []string{"digest", "uri"})
		digestKey := "gitCommit"
		if index == 1 {
			digestKey = "sha256"
		}
		assertExactJSONKeys(t, item["digest"].(map[string]any), []string{digestKey})
	}
}

func TestGenerateIsByteDeterministicAcrossManifestOrder(t *testing.T) {
	root := t.TempDir()
	portal := filepath.Join(root, "portal.json")
	writeTestFile(t, portal, `{"repository":"Aplexica/aplexica-portal","tag":"v0.2.0","asset":"aplexica-portal-v0.2.0-local.tar.gz","sha256":"`+strings.Repeat("b", 64)+`"}`)
	ordered := strings.Split(strings.TrimSpace(validChecksums("1.0.70")), "\n")
	reversed := append([]string(nil), ordered...)
	for left, right := 0, len(reversed)-1; left < right; left, right = left+1, right-1 {
		reversed[left], reversed[right] = reversed[right], reversed[left]
	}
	first := filepath.Join(root, "first")
	second := filepath.Join(root, "second")
	writeTestFile(t, first, strings.Join(ordered, "\n")+"\n")
	writeTestFile(t, second, strings.Join(reversed, "\n")+"\n")
	one, err := generate(validOptions(first, portal))
	if err != nil {
		t.Fatal(err)
	}
	two, err := generate(validOptions(second, portal))
	if err != nil {
		t.Fatal(err)
	}
	if string(one) != string(two) {
		t.Fatal("manifest input order changed the canonical statement bytes")
	}
}

func TestGenerateRejectsUnreviewedInputs(t *testing.T) {
	root := t.TempDir()
	checksums := filepath.Join(root, "SHA256SUMS")
	portal := filepath.Join(root, "portal.json")
	writeTestFile(t, checksums, validChecksums("1.0.70"))
	writeTestFile(t, portal, `{"repository":"Aplexica/aplexica-portal","tag":"v0.2.0","asset":"aplexica-portal-v0.2.0-local.tar.gz","sha256":"`+strings.Repeat("b", 64)+`"}`)

	tests := []struct {
		name   string
		mutate func(*options)
	}{
		{"repository", func(o *options) { o.repository = "aplexica/aplexica" }},
		{"branch ref", func(o *options) { o.ref = "refs/heads/main" }},
		{"workflow", func(o *options) { o.workflowRef = "Aplexica/Aplexica/.github/workflows/test.yml@refs/tags/v1.0.70" }},
		{"commit", func(o *options) { o.commit = strings.Repeat("A", 40) }},
		{"run id", func(o *options) { o.runID = "01" }},
		{"zero run id", func(o *options) { o.runID = "0" }},
		{"overflow run id", func(o *options) { o.runID = "18446744073709551616" }},
		{"run attempt", func(o *options) { o.runAttempt = "0" }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			opts := validOptions(checksums, portal)
			tc.mutate(&opts)
			if _, err := generate(opts); err == nil {
				t.Fatal("generate accepted unreviewed input")
			}
		})
	}
}

func TestReadChecksumsRequiresExactUniqueReleaseSet(t *testing.T) {
	root := t.TempDir()
	valid := validChecksums("1.0.70")
	tests := []struct {
		name string
		body string
	}{
		{"missing", strings.Join(strings.Split(strings.TrimSpace(valid), "\n")[:9], "\n") + "\n"},
		{"extra", valid + strings.Repeat("c", 64) + "  extra.txt\n"},
		{"duplicate", valid + strings.Repeat("d", 64) + "  aplexica-1.0.70-darwin-amd64.tar.gz\n"},
		{"wrong version", strings.Replace(valid, "1.0.70", "1.0.71", 1)},
		{"path", strings.Replace(valid, "aplexica.sbom.cdx.json", "dist/aplexica.sbom.cdx.json", 1)},
		{"uppercase digest", strings.Repeat("A", 64) + valid[64:]},
		{"one separator space", strings.Replace(valid, "  ", " ", 1)},
		{"three separator spaces", strings.Replace(valid, "  ", "   ", 1)},
		{"tab separator", strings.Replace(valid, "  ", "\t", 1)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			filename := filepath.Join(root, strings.ReplaceAll(tc.name, " ", "-")+".txt")
			writeTestFile(t, filename, tc.body)
			if _, err := readChecksums(filename, "1.0.70"); err == nil {
				t.Fatal("readChecksums accepted malformed manifest")
			}
		})
	}
}

func TestVerifySignedProvenanceEnforcesEverySemanticField(t *testing.T) {
	root := t.TempDir()
	checksums := filepath.Join(root, "SHA256SUMS")
	portal := filepath.Join(root, "portal.json")
	writeTestFile(t, checksums, validChecksums("1.0.70"))
	writeTestFile(t, portal, `{"repository":"Aplexica/aplexica-portal","tag":"v0.2.0","asset":"aplexica-portal-v0.2.0-local.tar.gz","sha256":"`+strings.Repeat("b", 64)+`"}`)
	opts := validOptions(checksums, portal)
	payload, err := generate(opts)
	if err != nil {
		t.Fatal(err)
	}
	bundle := filepath.Join(root, "bundle.json")
	writeBundle := func(contents []byte) {
		t.Helper()
		writeTestFile(t, bundle, `{"dsseEnvelope":{"payload":"`+base64.StdEncoding.EncodeToString(contents)+`"}}`)
	}
	writeBundle(payload)
	verify := opts
	verify.verifyBundle = bundle
	verify.workflowRef, verify.runID, verify.runAttempt = "", "", ""
	if err := verifySignedProvenance(verify); err != nil {
		t.Fatalf("valid signed payload: %v", err)
	}

	mutations := map[string]func(map[string]any){
		"extra subject field": func(root map[string]any) {
			root["subject"].([]any)[0].(map[string]any)["buildHost"] = "/private/company/release-staging"
		},
		"wrong builder": func(root map[string]any) {
			root["predicate"].(map[string]any)["runDetails"].(map[string]any)["builder"].(map[string]any)["id"] = "https://example.invalid/builder"
		},
		"wrong source tag": func(root map[string]any) {
			root["predicate"].(map[string]any)["buildDefinition"].(map[string]any)["resolvedDependencies"].([]any)[0].(map[string]any)["uri"] = "git+https://github.com/Aplexica/Aplexica@refs/tags/v1.0.69"
		},
		"wrong source commit": func(root map[string]any) {
			root["predicate"].(map[string]any)["buildDefinition"].(map[string]any)["resolvedDependencies"].([]any)[0].(map[string]any)["digest"].(map[string]any)["gitCommit"] = strings.Repeat("c", 40)
		},
		"extra subject": func(root map[string]any) {
			root["subject"] = append(root["subject"].([]any), root["subject"].([]any)[0])
		},
		"wrong portal": func(root map[string]any) {
			root["predicate"].(map[string]any)["buildDefinition"].(map[string]any)["resolvedDependencies"].([]any)[1].(map[string]any)["uri"] = "https://example.invalid/portal.tar.gz"
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			var changed map[string]any
			if err := json.Unmarshal(payload, &changed); err != nil {
				t.Fatal(err)
			}
			mutate(changed)
			encoded, err := json.MarshalIndent(changed, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			writeBundle(append(encoded, '\n'))
			if err := verifySignedProvenance(verify); err == nil {
				t.Fatal("policy verifier accepted a mutated signed statement")
			}
		})
	}
}

func TestOutputCannotOverwriteAnInput(t *testing.T) {
	for name, mutate := range map[string]func(*options){
		"checksums": func(opts *options) { opts.output = opts.checksums },
		"portal":    func(opts *options) { opts.output = opts.portal },
	} {
		t.Run(name, func(t *testing.T) {
			opts := validOptions("SHA256SUMS", "portal.json")
			mutate(&opts)
			if err := validateOutputPath(opts); err == nil {
				t.Fatal("output collision was accepted")
			}
		})
	}
}

func TestReadPortalReleaseRejectsPrivateOrExpandedDescriptor(t *testing.T) {
	root := t.TempDir()
	for name, body := range map[string]string{
		"private repository": `{"repository":"Internal/private-portal","tag":"v0.2.0","asset":"aplexica-portal-v0.2.0-local.tar.gz","sha256":"` + strings.Repeat("b", 64) + `"}`,
		"unexpected field":   `{"repository":"Aplexica/aplexica-portal","tag":"v0.2.0","asset":"aplexica-portal-v0.2.0-local.tar.gz","sha256":"` + strings.Repeat("b", 64) + `","actor":"person"}`,
		"wrong asset":        `{"repository":"Aplexica/aplexica-portal","tag":"v0.2.0","asset":"dist-cloud.tar.gz","sha256":"` + strings.Repeat("b", 64) + `"}`,
	} {
		t.Run(name, func(t *testing.T) {
			filename := filepath.Join(root, strings.ReplaceAll(name, " ", "-")+".json")
			writeTestFile(t, filename, body)
			if _, err := readPortalRelease(filename); err == nil {
				t.Fatal("readPortalRelease accepted unreviewed descriptor")
			}
		})
	}
}

func TestInputSizeCapsAreEnforced(t *testing.T) {
	root := t.TempDir()
	oversized := strings.Repeat(" ", maxInputSize+1)
	checksums := filepath.Join(root, "checksums")
	portal := filepath.Join(root, "portal")
	writeTestFile(t, checksums, oversized)
	writeTestFile(t, portal, oversized)
	if _, err := readChecksums(checksums, "1.0.70"); err == nil {
		t.Fatal("readChecksums accepted an oversized manifest")
	}
	if _, err := readPortalRelease(portal); err == nil {
		t.Fatal("readPortalRelease accepted an oversized descriptor")
	}
}

func validOptions(checksums, portal string) options {
	return options{
		checksums:   checksums,
		portal:      portal,
		repository:  canonicalRepository,
		ref:         "refs/tags/v1.0.70",
		commit:      strings.Repeat("a", 40),
		workflowRef: "Aplexica/Aplexica/.github/workflows/release.yml@refs/tags/v1.0.70",
		runID:       "12345",
		runAttempt:  "2",
	}
}

func validChecksums(version string) string {
	expected := expectedAssets(version)
	names := make([]string, 0, len(expected))
	for name := range expected {
		names = append(names, name)
	}
	sortStrings(names)
	var b strings.Builder
	for i, name := range names {
		fmt.Fprintf(&b, "%064x  %s\n", i+1, name)
	}
	return b.String()
}

func sortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}

func writeTestFile(t *testing.T, filename, contents string) {
	t.Helper()
	if err := os.WriteFile(filename, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertExactJSONKeys(t *testing.T, object map[string]any, want []string) {
	t.Helper()
	got := make([]string, 0, len(object))
	for key := range object {
		got = append(got, key)
	}
	sortStrings(got)
	sortStrings(want)
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("JSON keys = %v, want %v", got, want)
	}
}
