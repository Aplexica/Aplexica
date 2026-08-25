package keyrotation_test

import (
	"bytes"
	"encoding/base64"
	"testing"

	"github.com/aplexica/aplexica/internal/keyrotation"
	"github.com/aplexica/aplexica/internal/keys"
	"github.com/aplexica/aplexica/internal/secrets"
)

func newSecretsStore(t *testing.T) *secrets.Store {
	t.Helper()
	s := &secrets.Store{Root: t.TempDir()}
	if err := s.Init(); err != nil {
		t.Fatalf("secrets init: %v", err)
	}
	return s
}

func TestSecretsContentKeyStore_PutGetRoundTrip(t *testing.T) {
	st := keyrotation.NewSecretsContentKeyStore(newSecretsStore(t))
	ck, _ := keys.NewContentKey()

	if err := st.PutContentKey("ns-abc", 3, ck); err != nil {
		t.Fatalf("PutContentKey: %v", err)
	}
	got, ok, err := st.GetContentKey("ns-abc", 3)
	if err != nil {
		t.Fatalf("GetContentKey: %v", err)
	}
	if !ok {
		t.Fatal("expected ok=true after Put")
	}
	if !bytes.Equal(got, ck) {
		t.Fatalf("round-trip mismatch: got %x want %x", got, ck)
	}
}

func TestSecretsContentKeyStore_MissingIsNotOK(t *testing.T) {
	st := keyrotation.NewSecretsContentKeyStore(newSecretsStore(t))
	_, ok, err := st.GetContentKey("ns-none", 1)
	if err != nil {
		t.Fatalf("GetContentKey on missing should not error: %v", err)
	}
	if ok {
		t.Fatal("expected ok=false for a missing content key")
	}
}

func TestSecretsContentKeyStore_WrongLengthStoredKeyErrors(t *testing.T) {
	// A truncated/corrupt stored content key must NOT pass as a valid key:
	// GetContentKey must reject a decoded length != keys.ContentKeySize
	// (mirroring devicekey decode) rather than returning ok=true with a
	// wrong-length key that only fails far downstream in SealBody/WrapContentKey.
	s := newSecretsStore(t)
	// Persist a base64 value whose decoded length is wrong (16 bytes, not 32).
	short := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x01}, keys.ContentKeySize/2))
	if err := s.Put("ns-corrupt", "nskey_v1", short); err != nil {
		t.Fatalf("seed wrong-length key: %v", err)
	}

	st := keyrotation.NewSecretsContentKeyStore(s)
	got, ok, err := st.GetContentKey("ns-corrupt", 1)
	if err == nil {
		t.Fatalf("expected error for a wrong-length stored key, got ok=%v key=%x", ok, got)
	}
	if ok {
		t.Fatal("a wrong-length stored key must NOT return ok=true")
	}
	if got != nil {
		t.Fatalf("expected nil key on length-validation failure, got %x", got)
	}
}

func TestSecretsContentKeyStore_VersionsAreDistinct(t *testing.T) {
	st := keyrotation.NewSecretsContentKeyStore(newSecretsStore(t))
	v1, _ := keys.NewContentKey()
	v2, _ := keys.NewContentKey()
	if err := st.PutContentKey("ns-abc", 1, v1); err != nil {
		t.Fatalf("put v1: %v", err)
	}
	if err := st.PutContentKey("ns-abc", 2, v2); err != nil {
		t.Fatalf("put v2: %v", err)
	}
	got1, _, _ := st.GetContentKey("ns-abc", 1)
	got2, _, _ := st.GetContentKey("ns-abc", 2)
	if !bytes.Equal(got1, v1) || !bytes.Equal(got2, v2) {
		t.Fatal("versions collided in storage")
	}
}

func TestSecretsContentKeyStore_PersistsAcrossInstances(t *testing.T) {
	s := newSecretsStore(t)
	ck, _ := keys.NewContentKey()
	if err := keyrotation.NewSecretsContentKeyStore(s).PutContentKey("ns-abc", 5, ck); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, ok, err := keyrotation.NewSecretsContentKeyStore(s).GetContentKey("ns-abc", 5)
	if err != nil || !ok {
		t.Fatalf("recover from fresh instance: ok=%v err=%v", ok, err)
	}
	if !bytes.Equal(got, ck) {
		t.Fatal("content key not recovered from a fresh store instance")
	}
}
