package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/stretchr/testify/require"
)

// runVerifyCmd invokes `aplexica verify …` via rootCmd. Returns combined
// stdout+stderr and the error.
func runVerifyCmd(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&out)
	rootCmd.SetArgs(append([]string{"verify"}, args...))
	t.Cleanup(func() {
		verifyPubKey = ""
		verifyExpectedKeyID = ""
		verifyUnsignedOK = false
		verifyVerbose = false
	})
	err := rootCmd.Execute()
	return out.String(), err
}

func TestVerify_HappyPath(t *testing.T) {
	tmp := t.TempDir()
	storeRoot := filepath.Join(tmp, "src-store")
	bundlePath := filepath.Join(tmp, "bundle.tar.gz")

	seedStoreWithMemory(t, storeRoot, "# happy path\n")
	bundleStore(t, storeRoot, bundlePath)

	out, err := runVerifyCmd(t, bundlePath, "--unsigned-ok", "--verbose")
	require.NoError(t, err, "verify of a known-good bundle must pass; output:\n%s", out)
	require.Contains(t, out, "1 ok, 0 failed")
	// tabwriter aligns columns with spaces, so the literal substring is
	// "events:  1" with 2 spaces after the colon (depending on the longest
	// label width). Just assert the event count is present somewhere.
	require.Regexp(t, `events:\s+1\b`, out)
}

func TestVerify_RejectsCorruptedBundle(t *testing.T) {
	// BRD-01 §10 acceptance: `aplexica verify` rejects a deliberately
	// corrupted bundle with a clear error.
	tmp := t.TempDir()
	bundlePath := filepath.Join(tmp, "garbage.tar.gz")

	// A file that is decidedly not a gzip stream.
	require.NoError(t, os.WriteFile(bundlePath,
		[]byte("not even gzip"), 0o644))

	_, err := runVerifyCmd(t, bundlePath, "--unsigned-ok")
	require.Error(t, err, "verify must reject a corrupted bundle")
}

func TestVerify_RejectsTamperedArtifact(t *testing.T) {
	// Build a good bundle, tamper an event-log entry inside the
	// extracted tar so the chain check fails when verify re-restores.
	tmp := t.TempDir()
	storeRoot := filepath.Join(tmp, "src-store")
	bundlePath := filepath.Join(tmp, "bundle.tar.gz")

	id := seedStoreWithMemory(t, storeRoot, "# original\n")
	bundleStore(t, storeRoot, bundlePath)

	// Tamper directly in the source store BEFORE re-bundling: rewrite the
	// .jsonl event payload but leave the hash field untouched. The chain
	// check will then catch the hash mismatch on restore.
	eventsPath := filepath.Join(storeRoot, "events", "memories", id+".jsonl")
	data, err := os.ReadFile(eventsPath)
	require.NoError(t, err)
	tampered := bytes.Replace(data, []byte("original"), []byte("tampered"), 1)
	require.NotEqual(t, string(data), string(tampered),
		"sanity: tamper must change at least one byte")
	require.NoError(t, os.WriteFile(eventsPath, tampered, 0o644))

	// Re-bundle the tampered store. Restore will succeed (it doesn't
	// re-hash on the way in), but verify's per-event VerifyChain re-pass
	// must catch the mismatch.
	bundleStore(t, storeRoot, bundlePath)

	out, err := runVerifyCmd(t, bundlePath, "--unsigned-ok")
	require.Error(t, err, "verify must reject a tampered event log; output:\n%s", out)
	require.Contains(t, err.Error(), "event chain")
}

func TestVerify_RejectsUnacknowledgedUnsignedBundle(t *testing.T) {
	tmp := t.TempDir()
	storeRoot := filepath.Join(tmp, "src-store")
	bundlePath := filepath.Join(tmp, "bundle.tar.gz")

	seedStoreWithMemory(t, storeRoot, "# unsigned\n")
	bundleStore(t, storeRoot, bundlePath)

	_, err := runVerifyCmd(t, bundlePath)
	require.Error(t, err)
	require.Contains(t, err.Error(), "unsigned")
}

func TestVerify_AcksWithUnsignedOK(t *testing.T) {
	tmp := t.TempDir()
	storeRoot := filepath.Join(tmp, "src-store")
	bundlePath := filepath.Join(tmp, "bundle.tar.gz")

	seedStoreWithMemory(t, storeRoot, "# acknowledged\n")
	bundleStore(t, storeRoot, bundlePath)

	out, err := runVerifyCmd(t, bundlePath, "--unsigned-ok")
	require.NoError(t, err)
	require.NotContains(t, out, "WARN", "with --unsigned-ok, no warning expected")
	require.Contains(t, out, "acknowledged")
}

func TestVerify_VerifiesGoodSignature(t *testing.T) {
	tmp := t.TempDir()
	storeRoot := filepath.Join(tmp, "src-store")
	bundlePath := filepath.Join(tmp, "bundle.tar.gz")

	seedStoreWithMemory(t, storeRoot, "# signed\n")
	bundleStore(t, storeRoot, bundlePath)

	// Generate a keypair and sign the bundle.
	privPath := filepath.Join(tmp, "key.priv")
	pubPath := filepath.Join(tmp, "key.pub")
	require.NoError(t, acf.GenerateKeyPairFiles(privPath, pubPath))
	sig, err := acf.SignBundle(privPath, bundlePath)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(bundlePath+".sig", sig, 0o644))

	out, err := runVerifyCmd(t, bundlePath, "--pubkey", pubPath, "--key-id", publicKeyID(t, pubPath))
	require.NoError(t, err, "verify of signed bundle must pass; output:\n%s", out)
	require.Contains(t, out, "verified")
}

func TestVerify_RejectsBadSignature(t *testing.T) {
	tmp := t.TempDir()
	storeRoot := filepath.Join(tmp, "src-store")
	bundlePath := filepath.Join(tmp, "bundle.tar.gz")

	seedStoreWithMemory(t, storeRoot, "# will tamper after sign\n")
	bundleStore(t, storeRoot, bundlePath)

	privPath := filepath.Join(tmp, "key.priv")
	pubPath := filepath.Join(tmp, "key.pub")
	require.NoError(t, acf.GenerateKeyPairFiles(privPath, pubPath))
	sig, err := acf.SignBundle(privPath, bundlePath)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(bundlePath+".sig", sig, 0o644))

	// Tamper the bundle AFTER signing so the SHA-256 no longer matches.
	tampered := append([]byte{}, mustRead(t, bundlePath)...)
	// Flip one byte well past the gzip header to keep the stream still
	// decodable enough that restore proceeds (so we exercise both the
	// signature check AND that the chain check would otherwise pass).
	// If restore fails first, that's also a valid "verify rejects bad
	// bundle" outcome — the test asserts only that verify errors out.
	if len(tampered) > 50 {
		tampered[len(tampered)-10] ^= 0xff
	}
	require.NoError(t, os.WriteFile(bundlePath, tampered, 0o644))

	_, err = runVerifyCmd(t, bundlePath, "--pubkey", pubPath, "--key-id", publicKeyID(t, pubPath))
	require.Error(t, err, "verify must reject a tampered signed bundle")
}

func publicKeyID(t *testing.T, path string) string {
	t.Helper()
	line := strings.TrimSpace(string(mustRead(t, path)))
	raw, err := hex.DecodeString(strings.TrimPrefix(line, "acf-pub  v1 "))
	require.NoError(t, err)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	return b
}
