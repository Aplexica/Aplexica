// Package generationactivation publishes an already verified local identity
// chain and creates the authority-signed statement that activates one durable
// sync security generation. It never initializes trust or creates key material.
package generationactivation

import (
	"errors"
	"time"
)

const (
	AttestationVersion  uint16 = 1
	attestationDomain          = "aplexica/durable-sync-generation-activation/v1"
	bindingDigestDomain        = "aplexica/durable-sync-generation-activation-binding/v1"
)

var (
	ErrInvalidState                = errors.New("generation activation: invalid local state")
	ErrSigningAuthorityUnavailable = errors.New("generation activation: signing authority unavailable")
	ErrPendingActivation           = errors.New("generation activation: unresolved pending activation")
)

// GenerationActivationUnsignedV1 must remain byte-for-byte compatible with
// control-plane/internal/syncactivation.GenerationActivationUnsignedV1.
type GenerationActivationUnsignedV1 struct {
	Version                           uint16   `cbor:"version"`
	AccountID                         string   `cbor:"accountId"`
	NamespaceID                       string   `cbor:"namespaceId"`
	RosterScopeType                   string   `cbor:"rosterScopeType"`
	RosterScopeID                     string   `cbor:"rosterScopeId"`
	StreamEpoch                       string   `cbor:"streamEpoch"`
	RosterEpoch                       uint64   `cbor:"rosterEpoch"`
	RosterHash                        [32]byte `cbor:"rosterHash"`
	AuthorityEpoch                    uint64   `cbor:"authorityEpoch"`
	AuthorityStateHash                [32]byte `cbor:"authorityStateHash"`
	AccessGeneration                  uint64   `cbor:"accessGeneration"`
	AccessSetHash                     [32]byte `cbor:"accessSetHash"`
	SecurityGeneration                uint64   `cbor:"securityGeneration"`
	SecurityBarrier                   [32]byte `cbor:"securityBarrier"`
	KeyMode                           string   `cbor:"keyMode"`
	KeyVersion                        uint64   `cbor:"keyVersion"`
	ServerObservedNamespaceKeyVersion uint64   `cbor:"serverObservedNamespaceKeyVersion"`
	PreviousAuthorityDigest           [32]byte `cbor:"previousAuthorityDigest"`
	IssuedAtUnix                      int64    `cbor:"issuedAtUnix"`
	NotAfterUnix                      int64    `cbor:"notAfterUnix"`
	Nonce                             [32]byte `cbor:"nonce"`
}

type GenerationActivationAttestationV1 struct {
	Attestation  GenerationActivationUnsignedV1 `cbor:"attestation"`
	SignerKeyIDs [][32]byte                     `cbor:"signerKeyIds"`
	Signatures   [][64]byte                     `cbor:"signatures"`
}

// ActivationEndorsementV1 is one independently collected signature from an
// active roster authority. Threshold finalization accepts only sorted,
// distinct, cryptographically valid endorsements; it never fabricates or
// assumes access to another authority's private key.
type ActivationEndorsementV1 struct {
	SignerKeyID [32]byte
	Signature   [64]byte
}

// SecurityEpochState is the complete locally committed security barrier. It
// mirrors identity/<scope>/security-epoch.json and is independently rebound to
// the verified chain before any network write.
type SecurityEpochState struct {
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

type SignedObject struct {
	ScopeType    string
	ScopeID      string
	Kind         string
	Sequence     uint64
	PreviousHash [32]byte
	Hash         [32]byte
	Blob         []byte
	ProofBlob    []byte
}

type CredentialRegistration struct {
	CredentialBlob   []byte
	SigningKeyID     [32]byte
	WrapKeyID        [32]byte
	EnvelopeVersions []uint16
	RosterEpoch      uint64
}

type ActivationReceipt struct {
	AuthorityDigest string
	Revision        uint64
	Duplicate       bool
}

// ActivationStatus is an authenticated recovery answer for one exact durable
// activation statement. Exactly one of Committed and Absent must be true. A
// committed answer carries the server's durable authority receipt; an absent
// answer proves that the statement's predecessor is still current and permits
// replacing an expired statement without ever opening the local traffic gate.
type ActivationStatus struct {
	Committed bool
	Absent    bool
	Receipt   ActivationReceipt
}

type Result struct {
	AlreadyActivated bool
	Receipt          ActivationReceipt
	BindingDigest    [32]byte
	AttestationBlob  []byte
	ActivatedAt      time.Time
}
