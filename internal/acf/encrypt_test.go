package acf

import (
	"bytes"
	"os"
	"runtime"
	"testing"

	"filippo.io/age"
	"github.com/stretchr/testify/require"
)

func TestEncryptDecrypt_X25519RoundTrip(t *testing.T) {
	id, err := GenerateAgeIdentity()
	require.NoError(t, err)
	plain := []byte("hello bundle")

	var cipher bytes.Buffer
	require.NoError(t, EncryptBundle(bytes.NewReader(plain), &cipher, []age.Recipient{id.Recipient()}))
	require.NotEqual(t, plain, cipher.Bytes())

	var out bytes.Buffer
	require.NoError(t, DecryptBundle(bytes.NewReader(cipher.Bytes()), &out, []age.Identity{id}))
	require.Equal(t, plain, out.Bytes())
}

func TestEncryptDecrypt_WrongIdentityFails(t *testing.T) {
	id1, err := GenerateAgeIdentity()
	require.NoError(t, err)
	id2, err := GenerateAgeIdentity()
	require.NoError(t, err)
	var cipher bytes.Buffer
	require.NoError(t, EncryptBundle(bytes.NewReader([]byte("secret")), &cipher, []age.Recipient{id1.Recipient()}))
	var out bytes.Buffer
	err = DecryptBundle(bytes.NewReader(cipher.Bytes()), &out, []age.Identity{id2})
	require.Error(t, err)
}

func TestEncryptDecrypt_ScryptPassphraseRoundTrip(t *testing.T) {
	rec, err := age.NewScryptRecipient("correct-horse-battery-staple")
	require.NoError(t, err)
	rec.SetWorkFactor(10) // keep test fast
	var cipher bytes.Buffer
	require.NoError(t, EncryptBundle(bytes.NewReader([]byte("pwd-protected")), &cipher, []age.Recipient{rec}))

	id, err := age.NewScryptIdentity("correct-horse-battery-staple")
	require.NoError(t, err)
	var out bytes.Buffer
	require.NoError(t, DecryptBundle(bytes.NewReader(cipher.Bytes()), &out, []age.Identity{id}))
	require.Equal(t, "pwd-protected", out.String())
}

func TestSaveLoadAgeIdentity_RoundTrip(t *testing.T) {
	path := t.TempDir() + "/id.key"
	id, err := GenerateAgeIdentity()
	require.NoError(t, err)
	require.NoError(t, SaveAgeIdentity(path, id))
	info, err := os.Stat(path)
	require.NoError(t, err)
	if runtime.GOOS != "windows" {
		// Windows does not honor POSIX mode bits — stat returns 0o666
		// for any writable file. The mode-bit guarantee that matters
		// (the key file isn't world-readable) is a Unix-only concern.
		require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	}

	loaded, err := LoadAgeIdentity(path)
	require.NoError(t, err)
	require.Equal(t, id.Recipient().String(), loaded.Recipient().String())
}
