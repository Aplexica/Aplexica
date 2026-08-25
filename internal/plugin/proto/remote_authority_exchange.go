package proto

const MethodRemoteExchangeAuthorityEndorsementsV1 = "remote.exchange_authority_endorsements_v1"

const (
	RemoteAuthorityPurposeGenerationActivation = "generation-activation-v1"
	RemoteAuthorityPurposeRosterFreshness      = "roster-freshness-v1"
)

// RemoteAuthorityEndorsementV1 carries one public signature. The proposal is
// independently validated against the locally pinned roster before signing;
// private authority keys never cross this protocol.
type RemoteAuthorityEndorsementV1 struct {
	SignerKeyID [32]byte `json:"signer_key_id"`
	Signature   [64]byte `json:"signature"`
}

// RemoteExchangeAuthorityEndorsementsV1Params proposes or joins the one
// first-writer-wins proposal for an exact signed-authority round. RoundDigest
// excludes only proposal freshness/nonce and is recomputed at both trust
// boundaries. Proposal is canonical CBOR and contains public metadata only.
type RemoteExchangeAuthorityEndorsementsV1Params struct {
	Purpose       string                         `json:"purpose"`
	ScopeType     string                         `json:"scope_type"`
	ScopeID       string                         `json:"scope_id"`
	RoundDigest   [32]byte                       `json:"round_digest"`
	Proposal      []byte                         `json:"proposal"`
	ExpiresAtUnix int64                          `json:"expires_at_unix"`
	Endorsements  []RemoteAuthorityEndorsementV1 `json:"endorsements"`
}

// RemoteExchangeAuthorityEndorsementsV1Result returns the elected proposal and
// the immutable, distinct signatures collected for it so far.
type RemoteExchangeAuthorityEndorsementsV1Result struct {
	ProposalDigest [32]byte                       `json:"proposal_digest"`
	Proposal       []byte                         `json:"proposal"`
	ExpiresAtUnix  int64                          `json:"expires_at_unix"`
	Endorsements   []RemoteAuthorityEndorsementV1 `json:"endorsements"`
}
