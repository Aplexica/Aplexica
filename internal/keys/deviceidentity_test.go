package keys_test

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	"github.com/aplexica/aplexica/internal/keys"
	"github.com/aplexica/aplexica/internal/secrets"
	"github.com/stretchr/testify/require"
)

func TestDeviceIdentityConcurrentFirstUseConverges(t *testing.T) {
	root := filepath.Join(t.TempDir(), "secrets")
	s := &secrets.Store{Root: root}
	if err := s.Init(); err != nil {
		t.Fatal(err)
	}
	const n = 100
	out := make(chan keys.DeviceIdentity, n)
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, err := (&keys.DeviceIdentityStore{Secrets: s}).LoadOrCreate()
			if err != nil {
				errs <- err
				return
			}
			out <- id
		}()
	}
	wg.Wait()
	close(out)
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	var first keys.DeviceIdentity
	seen := false
	for id := range out {
		if !seen {
			first = id
			seen = true
			continue
		}
		if id.WrapPrivate != first.WrapPrivate || id.SigningKeyID != first.SigningKeyID {
			t.Fatal("concurrent creators returned different device identities")
		}
	}
	if len(first.SigningPrivate) != 64 || first.WrapKeyID == ([32]byte{}) {
		t.Fatal("incomplete identity")
	}
}

func TestDeviceIdentityRejectsCorruptOrLinkedFinal(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(string) error
	}{{"corrupt", func(p string) error { return os.WriteFile(p, []byte("not-a-key"), 0o600) }}, {"symlink", func(p string) error {
		target := p + ".target"
		if err := os.WriteFile(target, []byte("not-a-key"), 0o600); err != nil {
			return err
		}
		return os.Symlink(target, p)
	}}} {
		t.Run(tc.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "secrets")
			dir := filepath.Join(root, "_device")
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "x25519")
			if err := tc.setup(path); err != nil {
				if runtime.GOOS == "windows" && tc.name == "symlink" {
					t.Skipf("Windows test token has no symlink privilege: %v", err)
				}
				t.Fatal(err)
			}
			_, err := (&keys.DeviceIdentityStore{Secrets: &secrets.Store{Root: root}}).LoadOrCreate()
			if err == nil {
				t.Fatal("unsafe identity final was silently accepted or replaced")
			}
		})
	}
}

func TestDeviceIdentityLoadExistingNeverCreatesMissingSecrets(t *testing.T) {
	root := t.TempDir()
	store := &secrets.Store{Root: root}
	_, err := (&keys.DeviceIdentityStore{Secrets: store}).LoadExisting()
	require.Error(t, err)

	entries, readErr := os.ReadDir(root)
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		require.NoError(t, readErr)
	}
	require.Empty(t, entries)

	created, err := (&keys.DeviceIdentityStore{Secrets: store}).LoadOrCreate()
	require.NoError(t, err)
	loaded, err := (&keys.DeviceIdentityStore{Secrets: store}).LoadExisting()
	require.NoError(t, err)
	require.Equal(t, created.WrapKeyID, loaded.WrapKeyID)
	require.Equal(t, created.SigningKeyID, loaded.SigningKeyID)
	require.True(t, created.SigningPrivate.Equal(loaded.SigningPrivate))
}
