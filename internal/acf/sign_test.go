package acf

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSignAndVerify_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	privPath := filepath.Join(dir, "priv.key")
	pubPath := filepath.Join(dir, "pub.key")
	require.NoError(t, GenerateKeyPairFiles(privPath, pubPath))

	bundlePath := filepath.Join(dir, "bundle.tgz")
	require.NoError(t, os.WriteFile(bundlePath, []byte("hello bundle"), 0o644))

	sig, err := SignBundle(privPath, bundlePath)
	require.NoError(t, err)
	require.NotEmpty(t, sig)

	require.NoError(t, VerifyBundle(pubPath, bundlePath, sig))
}

func TestVerify_TamperedBundleFails(t *testing.T) {
	dir := t.TempDir()
	privPath := filepath.Join(dir, "priv.key")
	pubPath := filepath.Join(dir, "pub.key")
	require.NoError(t, GenerateKeyPairFiles(privPath, pubPath))

	bundlePath := filepath.Join(dir, "bundle.tgz")
	require.NoError(t, os.WriteFile(bundlePath, []byte("original"), 0o644))
	sig, err := SignBundle(privPath, bundlePath)
	require.NoError(t, err)

	require.NoError(t, os.WriteFile(bundlePath, []byte("tampered"), 0o644))
	err = VerifyBundle(pubPath, bundlePath, sig)
	require.Error(t, err)
}

func TestVerify_WrongPubKeyFails(t *testing.T) {
	dir := t.TempDir()
	privPath := filepath.Join(dir, "priv.key")
	pubPath := filepath.Join(dir, "pub.key")
	wrongPubPath := filepath.Join(dir, "wrong-pub.key")
	otherPrivPath := filepath.Join(dir, "other-priv.key")
	require.NoError(t, GenerateKeyPairFiles(privPath, pubPath))
	require.NoError(t, GenerateKeyPairFiles(otherPrivPath, wrongPubPath))

	bundlePath := filepath.Join(dir, "bundle.tgz")
	require.NoError(t, os.WriteFile(bundlePath, []byte("payload"), 0o644))
	sig, err := SignBundle(privPath, bundlePath)
	require.NoError(t, err)

	err = VerifyBundle(wrongPubPath, bundlePath, sig)
	require.Error(t, err)
}
