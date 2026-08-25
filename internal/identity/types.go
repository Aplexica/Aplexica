package identity

type DeviceKeyID [32]byte
type RosterHash [32]byte
type TrustAnchorHash [32]byte

type RosterAuthorityV1 struct {
	DeviceID         string   `cbor:"deviceId"`
	SigningKeyID     [32]byte `cbor:"signingKeyId"`
	SigningPublicKey [32]byte `cbor:"signingPublicKey"`
}
type AccountTrustAnchorUnsignedV1 struct {
	Version               uint16              `cbor:"version"`
	ServiceOrigin         string              `cbor:"serviceOrigin"`
	AccountID             string              `cbor:"accountId"`
	PersonalScopeID       string              `cbor:"personalScopeId"`
	RecoveryKDFProfileID  string              `cbor:"recoveryKdfProfileId"`
	RecoverySalt          [16]byte            `cbor:"recoverySalt"`
	RecoveryRootPublicKey [32]byte            `cbor:"recoveryRootPublicKey"`
	RecoveryWrapKeyID     [32]byte            `cbor:"recoveryWrapKeyId"`
	RecoveryWrapPublicKey [32]byte            `cbor:"recoveryWrapPublicKey"`
	AuthorityEpoch        uint64              `cbor:"authorityEpoch"`
	Authorities           []RosterAuthorityV1 `cbor:"authorities"`
	AuthorityThreshold    uint16              `cbor:"authorityThreshold"`
}
type AccountTrustAnchorV1 struct {
	Anchor            AccountTrustAnchorUnsignedV1 `cbor:"anchor"`
	RecoverySignature [64]byte                     `cbor:"recoverySignature"`
}
type AuthorityTransitionUnsignedV1 struct {
	Version                uint16              `cbor:"version"`
	AccountID              string              `cbor:"accountId"`
	TrustAnchorHash        [32]byte            `cbor:"trustAnchorHash"`
	PreviousStateHash      [32]byte            `cbor:"previousStateHash"`
	PreviousAuthorityEpoch uint64              `cbor:"previousAuthorityEpoch"`
	NewAuthorityEpoch      uint64              `cbor:"newAuthorityEpoch"`
	NewAuthorities         []RosterAuthorityV1 `cbor:"newAuthorities"`
	NewThreshold           uint16              `cbor:"newThreshold"`
	AuthorizationMode      string              `cbor:"authorizationMode"`
	IssuedAtUnix           int64               `cbor:"issuedAtUnix"`
	Nonce                  [32]byte            `cbor:"nonce"`
}
type AuthorityTransitionV1 struct {
	Transition   AuthorityTransitionUnsignedV1 `cbor:"transition"`
	SignerKeyIDs [][32]byte                    `cbor:"signerKeyIds"`
	Signatures   [][64]byte                    `cbor:"signatures"`
}

type DeviceCertificateUnsignedV1 struct {
	Version                   uint16   `cbor:"version"`
	AccountID                 string   `cbor:"accountId"`
	UserID                    string   `cbor:"userId"`
	DeviceID                  string   `cbor:"deviceId"`
	KeyEpoch                  uint64   `cbor:"keyEpoch"`
	SigningKeyID              [32]byte `cbor:"signingKeyId"`
	SigningPublicKey          [32]byte `cbor:"signingPublicKey"`
	WrapKeyID                 [32]byte `cbor:"wrapKeyId"`
	WrapPublicKey             [32]byte `cbor:"wrapPublicKey"`
	EnvelopeVersions          []uint16 `cbor:"envelopeVersions"`
	NotBeforeUnix             int64    `cbor:"notBeforeUnix"`
	NotAfterUnix              int64    `cbor:"notAfterUnix"`
	IssuanceMode              string   `cbor:"issuanceMode"`
	IssuedUnderAuthorityEpoch uint64   `cbor:"issuedUnderAuthorityEpoch"`
	JoinNonce                 [32]byte `cbor:"joinNonce"`
	EnrollmentContextHash     [32]byte `cbor:"enrollmentContextHash"`
	ApproverDeviceID          string   `cbor:"approverDeviceId"`
	ApproverSigningKeyID      [32]byte `cbor:"approverSigningKeyId"`
	CandidateProof            [64]byte `cbor:"candidateProof"`
	ApproverProof             [64]byte `cbor:"approverProof"`
	IssuingAuthorityStateHash [32]byte `cbor:"issuingAuthorityStateHash"`
}
type DeviceCertificateV1 struct {
	Certificate        DeviceCertificateUnsignedV1 `cbor:"certificate"`
	IssuerKeyIDs       [][32]byte                  `cbor:"issuerKeyIds"`
	IssuanceSignatures [][64]byte                  `cbor:"issuanceSignatures"`
}
type RecoveryEnrollmentUnsignedV1 struct {
	Version                 uint16   `cbor:"version"`
	AccountID               string   `cbor:"accountId"`
	TrustAnchorHash         [32]byte `cbor:"trustAnchorHash"`
	AuthorityTransitionHash [32]byte `cbor:"authorityTransitionHash"`
	CandidateDeviceID       string   `cbor:"candidateDeviceId"`
	CandidateSigningKeyID   [32]byte `cbor:"candidateSigningKeyId"`
	CandidateSigningPublic  [32]byte `cbor:"candidateSigningPublic"`
	CandidateWrapKeyID      [32]byte `cbor:"candidateWrapKeyId"`
	CandidateWrapPublic     [32]byte `cbor:"candidateWrapPublic"`
	EnvelopeVersions        []uint16 `cbor:"envelopeVersions"`
	JoinNonce               [32]byte `cbor:"joinNonce"`
	RecoveryNonce           [32]byte `cbor:"recoveryNonce"`
	NotBeforeUnix           int64    `cbor:"notBeforeUnix"`
	NotAfterUnix            int64    `cbor:"notAfterUnix"`
}
type RecoveryEnrollmentV1 struct {
	Enrollment        RecoveryEnrollmentUnsignedV1 `cbor:"enrollment"`
	RecoverySignature [64]byte                     `cbor:"recoverySignature"`
}

type AccessDeviceV1 struct {
	DeviceID         string   `cbor:"deviceId"`
	KeyEpoch         uint64   `cbor:"keyEpoch"`
	SigningKeyID     [32]byte `cbor:"signingKeyId"`
	SigningPublicKey [32]byte `cbor:"signingPublicKey"`
	WrapKeyID        [32]byte `cbor:"wrapKeyId"`
	WrapPublicKey    [32]byte `cbor:"wrapPublicKey"`
}
type RosterManifestUnsignedV1 struct {
	Version            uint16                `cbor:"version"`
	ScopeType          string                `cbor:"scopeType"`
	ScopeID            string                `cbor:"scopeId"`
	Epoch              uint64                `cbor:"epoch"`
	PreviousHash       [32]byte              `cbor:"previousHash"`
	TrustAnchorHash    [32]byte              `cbor:"trustAnchorHash"`
	AuthorityStateHash [32]byte              `cbor:"authorityStateHash"`
	AuthorityEpoch     uint64                `cbor:"authorityEpoch"`
	AccessGeneration   uint64                `cbor:"accessGeneration"`
	AccessSetHash      [32]byte              `cbor:"accessSetHash"`
	IssuedAtUnix       int64                 `cbor:"issuedAtUnix"`
	NotAfterUnix       int64                 `cbor:"notAfterUnix"`
	MinEnvelopeVersion uint16                `cbor:"minEnvelopeVersion"`
	Devices            []DeviceCertificateV1 `cbor:"devices"`
}
type RosterManifestV1 struct {
	Manifest     RosterManifestUnsignedV1 `cbor:"manifest"`
	SignerKeyIDs [][32]byte               `cbor:"signerKeyIds"`
	Signatures   [][64]byte               `cbor:"signatures"`
}
type AtomicAuthorityRosterTransitionV1 struct {
	AuthorityTransition AuthorityTransitionV1  `cbor:"authorityTransition"`
	RecoveryEnrollments []RecoveryEnrollmentV1 `cbor:"recoveryEnrollments"`
	NextRoster          RosterManifestV1       `cbor:"nextRoster"`
}

type PairingTranscriptV1 struct {
	Version                   uint16   `cbor:"version"`
	ServiceOrigin             string   `cbor:"serviceOrigin"`
	AccountID                 string   `cbor:"accountId"`
	PendingID                 string   `cbor:"pendingId"`
	PairingNonce              [32]byte `cbor:"pairingNonce"`
	CandidateDeviceID         string   `cbor:"candidateDeviceId"`
	CandidateEphemeralPublic  [32]byte `cbor:"candidateEphemeralPublic"`
	CandidateSigningPublic    [32]byte `cbor:"candidateSigningPublic"`
	CandidateWrapPublic       [32]byte `cbor:"candidateWrapPublic"`
	CandidateEnvelopeVersions []uint16 `cbor:"candidateEnvelopeVersions"`
	ApproverDeviceID          string   `cbor:"approverDeviceId"`
	ApproverEphemeralPublic   [32]byte `cbor:"approverEphemeralPublic"`
	TrustAnchorHash           [32]byte `cbor:"trustAnchorHash"`
	CurrentRosterHash         [32]byte `cbor:"currentRosterHash"`
}

type VerifiedAuthorityState struct {
	Anchor         AccountTrustAnchorV1
	AnchorHash     TrustAnchorHash
	StateHash      [32]byte
	AuthorityEpoch uint64
	Authorities    map[DeviceKeyID]RosterAuthorityV1
	Threshold      uint16
}
type VerifiedRoster struct {
	Manifest  RosterManifestV1
	Hash      RosterHash
	Authority VerifiedAuthorityState
}
