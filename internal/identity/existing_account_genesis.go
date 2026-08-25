package identity

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"runtime"
	"time"

	"github.com/aplexica/aplexica/internal/keys"
	"github.com/google/uuid"
	"golang.org/x/crypto/curve25519"
)

const (
	existingAccountGenesisCredentialLifetime = 365 * 24 * time.Hour
	existingAccountGenesisRosterLifetime     = 24 * time.Hour
)

var ErrInvalidExistingAccountGenesis = errors.New("identity: invalid existing-account genesis input")

// ExistingAccountGenesisInput contains only locally authenticated inputs. In
// particular it has no server-roster or server-device field: legacy cloud rows
// are deliberately incapable of becoming part of the new trust root.
//
// ConfirmedRecoveryMnemonic must be a dedicated mutable copy obtained only
// after user confirmation. BuildExistingAccountGenesis consumes and clears it
// on every success and failure path.
type ExistingAccountGenesisInput struct {
	ServiceOrigin             string
	AccountID                 string
	UserID                    string
	DeviceID                  string
	Confirmed                 bool
	ConfirmedRecoveryMnemonic []byte
	DeviceIdentity            keys.DeviceIdentity
	Random                    io.Reader
	Clock                     func() time.Time
}

// ExistingAccountGenesisChainInputs are the exact three values accepted by
// ChainStore.Initialize. ExpectedRecoveryRoot is public verification material,
// never the mnemonic or a recovery private key.
type ExistingAccountGenesisChainInputs struct {
	Anchor               AccountTrustAnchorV1
	ExpectedRecoveryRoot ed25519.PublicKey
	Roster               RosterManifestV1
}

// GenesisSecurityEpochV1 is the complete account security-epoch.json v1
// payload. It intentionally mirrors the strict read-only consumers without
// importing a higher layer back into identity.
type GenesisSecurityEpochV1 struct {
	Version               uint16   `json:"version"`
	ScopeType             string   `json:"scopeType"`
	ScopeID               string   `json:"scopeId"`
	RosterHash            [32]byte `json:"rosterHash"`
	AccessGeneration      uint64   `json:"accessGeneration"`
	AccessSetHash         [32]byte `json:"accessSetHash"`
	BarrierID             [32]byte `json:"barrierId"`
	TreeHeadDigest        [32]byte `json:"treeHeadDigest"`
	KeyMode               string   `json:"keyMode"`
	KeyVersion            uint64   `json:"keyVersion"`
	CoordinatorGeneration uint64   `json:"coordinatorGeneration"`
}

type ExistingAccountGenesisResult struct {
	Chain         ExistingAccountGenesisChainInputs
	Verified      VerifiedRoster
	SecurityEpoch GenesisSecurityEpochV1
}

// BuildExistingAccountGenesis creates a strictly local first-device root for
// an existing account. It derives authority only from the confirmed recovery
// phrase and the already-provisioned device identity; it performs no network
// read and accepts no server-generated trust material.
func BuildExistingAccountGenesis(input ExistingAccountGenesisInput) (ExistingAccountGenesisResult, error) {
	defer clearBytes(input.ConfirmedRecoveryMnemonic)
	if !input.Confirmed || !validText(input.ServiceOrigin, 512) || !validText(input.AccountID, 256) ||
		!validText(input.UserID, 256) || !validText(input.DeviceID, 256) ||
		validateExistingAccountDeviceIdentity(input.DeviceIdentity) != nil {
		return ExistingAccountGenesisResult{}, ErrInvalidExistingAccountGenesis
	}

	clock := input.Clock
	if clock == nil {
		clock = time.Now
	}
	now := clock().UTC().Truncate(time.Second)
	issuedAt := now.Unix()
	credentialNotAfter := now.Add(existingAccountGenesisCredentialLifetime).Unix()
	rosterNotAfter := now.Add(existingAccountGenesisRosterLifetime).Unix()
	if now.IsZero() || issuedAt <= 0 || credentialNotAfter <= issuedAt || rosterNotAfter <= issuedAt || credentialNotAfter < rosterNotAfter {
		return ExistingAccountGenesisResult{}, ErrInvalidExistingAccountGenesis
	}

	random := input.Random
	if random == nil {
		random = rand.Reader
	}
	var recoverySalt [16]byte
	if err := readGenesisRandom(random, recoverySalt[:], "recovery salt"); err != nil {
		return ExistingAccountGenesisResult{}, err
	}
	personalScopeID, err := randomUUIDv7(random, now)
	if err != nil {
		return ExistingAccountGenesisResult{}, err
	}
	var barrierID, treeHeadDigest, joinNonce [32]byte
	if err := readGenesisRandom(random, barrierID[:], "security barrier"); err != nil {
		return ExistingAccountGenesisResult{}, err
	}
	if err := readGenesisRandom(random, treeHeadDigest[:], "tree head"); err != nil {
		return ExistingAccountGenesisResult{}, err
	}
	if err := readGenesisRandom(random, joinNonce[:], "device join nonce"); err != nil {
		return ExistingAccountGenesisResult{}, err
	}

	recovery, err := DeriveRecoveryKeys(input.ConfirmedRecoveryMnemonic, recoverySalt, RecoveryKDFProfileArgon2idV1)
	if err != nil {
		return ExistingAccountGenesisResult{}, err
	}
	defer func() {
		recovery.Clear()
		runtime.KeepAlive(&recovery)
	}()

	var signingPublic [32]byte
	copy(signingPublic[:], input.DeviceIdentity.SigningPublic)
	authority := RosterAuthorityV1{
		DeviceID: input.DeviceID, SigningKeyID: input.DeviceIdentity.SigningKeyID, SigningPublicKey: signingPublic,
	}
	unsignedAnchor := AccountTrustAnchorUnsignedV1{
		Version: 1, ServiceOrigin: input.ServiceOrigin, AccountID: input.AccountID, PersonalScopeID: personalScopeID,
		RecoveryKDFProfileID: RecoveryKDFProfileArgon2idV1, RecoverySalt: recoverySalt,
		RecoveryRootPublicKey: recovery.SigningPublic, RecoveryWrapKeyID: recovery.WrapKeyID, RecoveryWrapPublicKey: recovery.WrapPublic,
		AuthorityEpoch: 1, Authorities: []RosterAuthorityV1{authority}, AuthorityThreshold: 1,
	}
	anchor, err := signExistingAccountTrustAnchor(&recovery, unsignedAnchor)
	if err != nil {
		return ExistingAccountGenesisResult{}, err
	}
	expectedRecoveryRoot := append(ed25519.PublicKey(nil), recovery.SigningPublic[:]...)
	verifiedAuthority, err := VerifyTrustAnchor(anchor, expectedRecoveryRoot)
	if err != nil {
		return ExistingAccountGenesisResult{}, err
	}

	credentialUnsigned := DeviceCertificateUnsignedV1{
		Version: 1, AccountID: input.AccountID, UserID: input.UserID, DeviceID: input.DeviceID, KeyEpoch: 1,
		SigningKeyID: input.DeviceIdentity.SigningKeyID, SigningPublicKey: signingPublic,
		WrapKeyID: input.DeviceIdentity.WrapKeyID, WrapPublicKey: input.DeviceIdentity.WrapPublic,
		// EnvelopeVersions is the decode-capability statement other devices'
		// seal paths trust (2026-07-29 envelope wire-efficiency ADR D2):
		// advertising 3 is truthful because this release ships the v3 decoder
		// (internal/sync/envelope_v3.go) alongside it. The list must stay
		// sorted and unique (sortedUniqueVersions).
		EnvelopeVersions: supportedEnvelopeVersions(), NotBeforeUnix: issuedAt, NotAfterUnix: credentialNotAfter,
		IssuanceMode: "recovery", IssuedUnderAuthorityEpoch: 1, JoinNonce: joinNonce,
		EnrollmentContextHash:     [32]byte(verifiedAuthority.AnchorHash),
		IssuingAuthorityStateHash: verifiedAuthority.StateHash,
	}
	// Genesis has no predecessor authority transition to invent. The recovery
	// signature on the anchor directly authorizes this sole device authority;
	// binding the recovery-mode possession proof to that verified anchor hash
	// provides the corresponding local enrollment context with all approver
	// fields remaining zero.
	candidateProof, err := signExistingAccountValue(input.DeviceIdentity.SigningPrivate, "aplexica/device-possession/v1", devicePossessionPreimage(credentialUnsigned))
	if err != nil {
		return ExistingAccountGenesisResult{}, err
	}
	credentialUnsigned.CandidateProof = candidateProof
	if err := verifyExistingAccountGenesisCredential(credentialUnsigned, verifiedAuthority); err != nil {
		return ExistingAccountGenesisResult{}, err
	}
	issuanceSignature, err := signExistingAccountValue(input.DeviceIdentity.SigningPrivate, "aplexica/device-credential/v1", credentialUnsigned)
	if err != nil {
		return ExistingAccountGenesisResult{}, err
	}
	credential := DeviceCertificateV1{
		Certificate: credentialUnsigned, IssuerKeyIDs: [][32]byte{input.DeviceIdentity.SigningKeyID},
		IssuanceSignatures: [][64]byte{issuanceSignature},
	}

	manifestUnsigned := RosterManifestUnsignedV1{
		Version: 1, ScopeType: "account", ScopeID: personalScopeID, Epoch: 1,
		TrustAnchorHash: [32]byte(verifiedAuthority.AnchorHash), AuthorityStateHash: verifiedAuthority.StateHash, AuthorityEpoch: 1,
		AccessGeneration: 1, IssuedAtUnix: issuedAt, NotAfterUnix: rosterNotAfter, MinEnvelopeVersion: 2,
		Devices: []DeviceCertificateV1{credential},
	}
	manifestUnsigned.AccessSetHash, err = AccessSetHash(manifestUnsigned)
	if err != nil {
		return ExistingAccountGenesisResult{}, err
	}
	rosterSignature, err := signExistingAccountValue(input.DeviceIdentity.SigningPrivate, "aplexica/roster-manifest/v1", manifestUnsigned)
	if err != nil {
		return ExistingAccountGenesisResult{}, err
	}
	roster := RosterManifestV1{
		Manifest: manifestUnsigned, SignerKeyIDs: [][32]byte{input.DeviceIdentity.SigningKeyID}, Signatures: [][64]byte{rosterSignature},
	}
	verifiedRoster, err := VerifyGenesis(verifiedAuthority, roster)
	if err != nil {
		return ExistingAccountGenesisResult{}, err
	}

	securityEpoch := GenesisSecurityEpochV1{
		Version: 1, ScopeType: "account", ScopeID: personalScopeID, RosterHash: [32]byte(verifiedRoster.Hash),
		AccessGeneration: manifestUnsigned.AccessGeneration, AccessSetHash: manifestUnsigned.AccessSetHash,
		BarrierID: barrierID, TreeHeadDigest: treeHeadDigest, KeyMode: "recipient-wrap-v2", KeyVersion: 0, CoordinatorGeneration: 1,
	}
	return ExistingAccountGenesisResult{
		Chain:    ExistingAccountGenesisChainInputs{Anchor: anchor, ExpectedRecoveryRoot: expectedRecoveryRoot, Roster: roster},
		Verified: verifiedRoster, SecurityEpoch: securityEpoch,
	}, nil
}

func validateExistingAccountDeviceIdentity(device keys.DeviceIdentity) error {
	if len(device.SigningPrivate) != ed25519.PrivateKeySize || len(device.SigningPublic) != ed25519.PublicKeySize ||
		sha256.Sum256(device.SigningPublic) != device.SigningKeyID ||
		!device.SigningPrivate.Public().(ed25519.PublicKey).Equal(device.SigningPublic) {
		return ErrInvalidExistingAccountGenesis
	}
	reconstructed := ed25519.NewKeyFromSeed(device.SigningPrivate.Seed())
	consistent := device.SigningPrivate.Equal(reconstructed)
	clearBytes(reconstructed)
	if !consistent || device.WrapPrivate[0]&7 != 0 || device.WrapPrivate[31]&0x80 != 0 || device.WrapPrivate[31]&0x40 == 0 {
		return ErrInvalidExistingAccountGenesis
	}
	wrapPublic, err := curve25519.X25519(device.WrapPrivate[:], curve25519.Basepoint)
	if err != nil || zeroBytes(wrapPublic) || device.WrapPublic != [32]byte(wrapPublic) || sha256.Sum256(device.WrapPublic[:]) != device.WrapKeyID {
		clearBytes(wrapPublic)
		return ErrInvalidExistingAccountGenesis
	}
	clearBytes(wrapPublic)
	return nil
}

func readGenesisRandom(reader io.Reader, destination []byte, purpose string) error {
	if reader == nil || len(destination) == 0 {
		return ErrInvalidExistingAccountGenesis
	}
	if _, err := io.ReadFull(reader, destination); err != nil {
		clearBytes(destination)
		return fmt.Errorf("identity: generate %s: %w", purpose, err)
	}
	if zeroBytes(destination) {
		clearBytes(destination)
		return fmt.Errorf("%w: zero %s", ErrInvalidExistingAccountGenesis, purpose)
	}
	return nil
}

func randomUUIDv7(reader io.Reader, now time.Time) (string, error) {
	millis := now.UnixMilli()
	if millis < 0 || uint64(millis) >= uint64(1)<<48 {
		return "", ErrInvalidExistingAccountGenesis
	}
	var raw [16]byte
	defer clearBytes(raw[:])
	if err := readGenesisRandom(reader, raw[:], "personal scope UUID"); err != nil {
		return "", err
	}
	raw[0] = byte(uint64(millis) >> 40)
	raw[1] = byte(uint64(millis) >> 32)
	raw[2] = byte(uint64(millis) >> 24)
	raw[3] = byte(uint64(millis) >> 16)
	raw[4] = byte(uint64(millis) >> 8)
	raw[5] = byte(uint64(millis))
	raw[6] = raw[6]&0x0f | 0x70
	raw[8] = raw[8]&0x3f | 0x80
	id := uuid.UUID(raw)
	if id.Version() != 7 || id.Variant() != uuid.RFC4122 {
		return "", ErrInvalidExistingAccountGenesis
	}
	return id.String(), nil
}

func signExistingAccountTrustAnchor(recovery *RecoveryKeys, unsigned AccountTrustAnchorUnsignedV1) (AccountTrustAnchorV1, error) {
	if recovery == nil || unsigned.RecoveryKDFProfileID != RecoveryKDFProfileArgon2idV1 ||
		unsigned.RecoveryRootPublicKey != recovery.SigningPublic || unsigned.RecoveryWrapPublicKey != recovery.WrapPublic ||
		unsigned.RecoveryWrapKeyID != recovery.WrapKeyID {
		return AccountTrustAnchorV1{}, ErrInvalidExistingAccountGenesis
	}
	preimage, err := canonical("aplexica/account-trust-anchor/v1", unsigned)
	if err != nil {
		return AccountTrustAnchorV1{}, err
	}
	private := ed25519.NewKeyFromSeed(recovery.SigningSeed[:])
	defer clearBytes(private)
	signatureBytes := ed25519.Sign(private, preimage)
	defer clearBytes(signatureBytes)
	anchor := AccountTrustAnchorV1{Anchor: unsigned}
	copy(anchor.RecoverySignature[:], signatureBytes)
	return anchor, nil
}

func signExistingAccountValue(private ed25519.PrivateKey, domain string, value any) ([64]byte, error) {
	preimage, err := canonical(domain, value)
	if err != nil {
		return [64]byte{}, err
	}
	signatureBytes := ed25519.Sign(private, preimage)
	defer clearBytes(signatureBytes)
	var signature [64]byte
	copy(signature[:], signatureBytes)
	return signature, nil
}

func verifyExistingAccountGenesisCredential(credential DeviceCertificateUnsignedV1, authority VerifiedAuthorityState) error {
	if credential.IssuanceMode != "recovery" || credential.JoinNonce == ([32]byte{}) ||
		credential.EnrollmentContextHash != [32]byte(authority.AnchorHash) || credential.ApproverDeviceID != "" ||
		credential.ApproverSigningKeyID != ([32]byte{}) || credential.ApproverProof != ([64]byte{}) {
		return ErrInvalidExistingAccountGenesis
	}
	return verifySig(credential.SigningPublicKey[:], "aplexica/device-possession/v1", devicePossessionPreimage(credential), credential.CandidateProof[:])
}

// supportedEnvelopeVersions is the ordered, unique set of envelope wire
// versions this build can DECODE, and therefore the set it is honest to
// advertise in a signed device certificate. Sealing a version additionally
// requires every peer in the verified roster to advertise it (see
// internal/sync/envelope_v3.go), so advertising is a capability statement,
// never a commitment to emit.
func supportedEnvelopeVersions() []uint16 {
	return []uint16{envelopeVersionV2, envelopeVersionV3}
}

const (
	envelopeVersionV2 = 2
	envelopeVersionV3 = 3
)
