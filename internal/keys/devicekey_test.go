package keys_test

import (
	"bytes"
	"testing"

	"github.com/aplexica/aplexica/internal/keys"
	"github.com/aplexica/aplexica/internal/secrets"
)

func newSecrets(t *testing.T) *secrets.Store {
	t.Helper()
	s := &secrets.Store{Root: t.TempDir()}
	if err := s.Init(); err != nil {
		t.Fatalf("secrets init: %v", err)
	}
	return s
}

func TestDeviceKeyStore_LoadOrCreate_IsStable(t *testing.T) {
	st := keys.NewDeviceKeyStore(newSecrets(t))

	priv1, pub1, err := st.LoadOrCreate()
	if err != nil {
		t.Fatalf("first LoadOrCreate: %v", err)
	}
	priv2, pub2, err := st.LoadOrCreate()
	if err != nil {
		t.Fatalf("second LoadOrCreate: %v", err)
	}
	if priv1 != priv2 {
		t.Fatal("private key changed across calls — not persisted")
	}
	if pub1 != pub2 {
		t.Fatal("public key changed across calls")
	}
}

func TestDeviceKeyStore_PersistsAcrossInstances(t *testing.T) {
	s := newSecrets(t)
	priv1, pub1, err := keys.NewDeviceKeyStore(s).LoadOrCreate()
	if err != nil {
		t.Fatalf("LoadOrCreate (instance 1): %v", err)
	}
	// A fresh store over the same secrets backend must recover the key.
	priv2, pub2, err := keys.NewDeviceKeyStore(s).LoadOrCreate()
	if err != nil {
		t.Fatalf("LoadOrCreate (instance 2): %v", err)
	}
	if priv1 != priv2 || pub1 != pub2 {
		t.Fatal("device key not recovered from a fresh store instance")
	}
}

func TestDeviceKeyStore_KeyIsUsableForUnwrap(t *testing.T) {
	priv, pub, err := keys.NewDeviceKeyStore(newSecrets(t)).LoadOrCreate()
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}
	ck, _ := keys.NewContentKey()
	wrapped, err := keys.WrapContentKey(ck, pub)
	if err != nil {
		t.Fatalf("WrapContentKey: %v", err)
	}
	got, err := keys.UnwrapContentKey(wrapped, priv)
	if err != nil {
		t.Fatalf("UnwrapContentKey: %v", err)
	}
	if !bytes.Equal(got, ck) {
		t.Fatal("stored device key failed to unwrap a key wrapped for its pub")
	}
}
