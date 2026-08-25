package keys

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"runtime"

	"golang.org/x/crypto/curve25519"
)

// LoadExisting reads an already-provisioned device identity without creating
// missing key material. Security-transition publication must use this method:
// silently generating a replacement key would never satisfy the signed roster
// authority and would obscure the actual provisioning failure.
func (s *DeviceIdentityStore) LoadExisting() (DeviceIdentity, error) {
	if s == nil || s.Secrets == nil {
		return DeviceIdentity{}, fmt.Errorf("keys: nil atomic secrets store")
	}
	wrap, err := s.Secrets.Get(deviceKeyArtifactID, deviceWrapSecretName)
	if err != nil {
		return DeviceIdentity{}, fmt.Errorf("keys: load existing wrap identity: %w", err)
	}
	wrap, err = validateX25519Private(wrap)
	if err != nil {
		return DeviceIdentity{}, err
	}
	signing, err := s.Secrets.Get(deviceKeyArtifactID, deviceSigningSecretName)
	if err != nil {
		return DeviceIdentity{}, fmt.Errorf("keys: load existing signing identity: %w", err)
	}
	signing, err = validateEd25519Private(signing)
	if err != nil {
		return DeviceIdentity{}, err
	}

	var out DeviceIdentity
	wrapBytes, err := base64.StdEncoding.Strict().DecodeString(wrap)
	if err != nil {
		return DeviceIdentity{}, err
	}
	defer clearExistingIdentityBytes(wrapBytes)
	copy(out.WrapPrivate[:], wrapBytes)
	wrapPublic, err := curve25519.X25519(out.WrapPrivate[:], curve25519.Basepoint)
	if err != nil {
		return DeviceIdentity{}, err
	}
	copy(out.WrapPublic[:], wrapPublic)
	out.WrapKeyID = sha256.Sum256(out.WrapPublic[:])
	signingBytes, err := base64.StdEncoding.Strict().DecodeString(signing)
	if err != nil {
		return DeviceIdentity{}, err
	}
	defer clearExistingIdentityBytes(signingBytes)
	out.SigningPrivate = append(ed25519.PrivateKey(nil), signingBytes...)
	out.SigningPublic = append(ed25519.PublicKey(nil), out.SigningPrivate.Public().(ed25519.PublicKey)...)
	out.SigningKeyID = sha256.Sum256(out.SigningPublic)
	return out, nil
}

func clearExistingIdentityBytes(value []byte) {
	for i := range value {
		value[i] = 0
	}
	runtime.KeepAlive(value)
}
