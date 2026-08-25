package releasetrust

import (
	"os"
	"strings"
	"testing"
)

func TestPublicReleaseConstantsAreExact(t *testing.T) {
	wants := map[string]struct{ got, want string }{
		"repository":        {Repository, "Aplexica/Aplexica"},
		"workflow path":     {ReleaseWorkflowPath, ".github/workflows/release.yml"},
		"checksums":         {ChecksumsAsset, "SHA256SUMS"},
		"checksums bundle":  {ChecksumsBundle, "SHA256SUMS.sigstore.json"},
		"provenance bundle": {ProvenanceBundle, "aplexica.provenance.sigstore.json"},
		"public key":        {PublicKey, "aplexica-release.pub"},
		"statement type":    {InTotoStatementType, "https://in-toto.io/Statement/v1"},
		"provenance type":   {SLSAProvenanceType, "https://slsa.dev/provenance/v1"},
	}
	for name, pair := range wants {
		if pair.got != pair.want {
			t.Errorf("%s = %q, want %q", name, pair.got, pair.want)
		}
	}
}

func TestKMSPolicyDoesNotRegressToCertificateIdentity(t *testing.T) {
	for _, path := range []string{
		"../../.github/workflows/release.yml",
		"../../docs/install/verify.md",
		"../../docs/RELEASING.md",
	} {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		text := string(body)
		for _, forbidden := range []string{
			"--certificate-identity", "--certificate-oidc-issuer",
			"actions/attest-build-provenance", "gh attestation verify",
		} {
			if strings.Contains(text, forbidden) {
				t.Errorf("%s contains obsolete release authority %q", path, forbidden)
			}
		}
		for _, required := range []string{ChecksumsAsset, ChecksumsBundle, ProvenanceBundle, PublicKey} {
			if !strings.Contains(text, required) {
				t.Errorf("%s omits public release object %q", path, required)
			}
		}
	}
}

func TestReleaseWorkflowPathAndRepositoryRemainPublicPolicy(t *testing.T) {
	body, err := os.ReadFile("../../.github/workflows/release.yml")
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	if !strings.Contains(text, Repository) {
		t.Fatalf("release workflow does not bind canonical repository %q", Repository)
	}
	if !strings.Contains(text, `--workflow-ref "$GITHUB_WORKFLOW_REF"`) {
		t.Fatalf("release workflow does not pass its runtime workflow identity to provenance")
	}
}
