package keys

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"

	"golang.org/x/crypto/curve25519"
)

const (
	deviceKeyArtifactID     = "_device"
	deviceWrapSecretName    = "x25519"
	deviceSigningSecretName = "ed25519"
)

type DeviceIdentity struct {
	WrapPrivate    [32]byte
	WrapPublic     [32]byte
	WrapKeyID      [32]byte
	SigningPrivate ed25519.PrivateKey
	SigningPublic  ed25519.PublicKey
	SigningKeyID   [32]byte
}

type AtomicSecretsStore interface {
	Get(artifactID, key string) (string, error)
	GetOrCreate(artifactID, key string, generate func() (string, error), validate func(string) (string, error)) (string, bool, error)
}

type DeviceIdentityStore struct{ Secrets AtomicSecretsStore }

func validateX25519Private(raw string) (string, error) {
	b, err := base64.StdEncoding.Strict().DecodeString(raw)
	if err != nil || len(b) != 32 {
		return "", fmt.Errorf("keys: invalid X25519 private key encoding")
	}
	if b[0]&7 != 0 || b[31]&0x80 != 0 || b[31]&0x40 == 0 {
		return "", fmt.Errorf("keys: X25519 private key is not canonically clamped")
	}
	pub, err := curve25519.X25519(b, curve25519.Basepoint)
	if err != nil || allZero(pub) {
		return "", fmt.Errorf("keys: invalid X25519 private key")
	}
	return base64.StdEncoding.EncodeToString(b), nil
}
func validateEd25519Private(raw string) (string, error) {
	b, err := base64.StdEncoding.Strict().DecodeString(raw)
	if err != nil || len(b) != ed25519.PrivateKeySize {
		return "", fmt.Errorf("keys: invalid Ed25519 private key encoding")
	}
	priv := ed25519.PrivateKey(b)
	pub := priv.Public().(ed25519.PublicKey)
	derived := ed25519.NewKeyFromSeed(priv.Seed())
	if !priv.Equal(derived) || !pub.Equal(derived.Public()) {
		return "", fmt.Errorf("keys: inconsistent Ed25519 private key")
	}
	return base64.StdEncoding.EncodeToString(b), nil
}
func allZero(b []byte) bool {
	var v byte
	for _, x := range b {
		v |= x
	}
	return v == 0
}

func (s *DeviceIdentityStore) LoadOrCreate() (DeviceIdentity, error) {
	if s == nil || s.Secrets == nil {
		return DeviceIdentity{}, fmt.Errorf("keys: nil atomic secrets store")
	}
	wrap, _, err := s.Secrets.GetOrCreate(deviceKeyArtifactID, deviceWrapSecretName, func() (string, error) {
		priv, _, err := NewDeviceKey()
		if err != nil {
			return "", err
		}
		return base64.StdEncoding.EncodeToString(priv[:]), nil
	}, validateX25519Private)
	if err != nil {
		return DeviceIdentity{}, fmt.Errorf("keys: load or create wrap identity: %w", err)
	}
	signing, _, err := s.Secrets.GetOrCreate(deviceKeyArtifactID, deviceSigningSecretName, func() (string, error) {
		_, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return "", err
		}
		return base64.StdEncoding.EncodeToString(priv), nil
	}, validateEd25519Private)
	if err != nil {
		return DeviceIdentity{}, fmt.Errorf("keys: load or create signing identity: %w", err)
	}
	var out DeviceIdentity
	wb, _ := base64.StdEncoding.DecodeString(wrap)
	copy(out.WrapPrivate[:], wb)
	wp, err := curve25519.X25519(out.WrapPrivate[:], curve25519.Basepoint)
	if err != nil {
		return DeviceIdentity{}, err
	}
	copy(out.WrapPublic[:], wp)
	out.WrapKeyID = sha256.Sum256(out.WrapPublic[:])
	sb, _ := base64.StdEncoding.DecodeString(signing)
	out.SigningPrivate = append(ed25519.PrivateKey(nil), sb...)
	out.SigningPublic = append(ed25519.PublicKey(nil), out.SigningPrivate.Public().(ed25519.PublicKey)...)
	out.SigningKeyID = sha256.Sum256(out.SigningPublic)
	return out, nil
}
