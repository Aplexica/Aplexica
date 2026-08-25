package daemon

import (
	"bytes"
	"context"
	"crypto/sha256"

	"github.com/aplexica/aplexica/internal/generationactivation"
	"github.com/aplexica/aplexica/internal/identity"
	"github.com/aplexica/aplexica/internal/plugin/proto"
)

type authorityEndorsementExchanger interface {
	ExchangeAuthorityEndorsementsV1(context.Context, proto.RemoteExchangeAuthorityEndorsementsV1Params) (proto.RemoteExchangeAuthorityEndorsementsV1Result, error)
}

// GenerationActivationEndorsementCollector exchanges only canonical public
// proposals/signatures. Every proposal selected by the relay is revalidated
// and signed locally with this device's already-provisioned authority key.
type GenerationActivationEndorsementCollector struct {
	Exchange authorityEndorsementExchanger
	Input    generationactivation.BuildInput
}

func (c *GenerationActivationEndorsementCollector) CollectActivationEndorsements(ctx context.Context, proposed generationactivation.GenerationActivationUnsignedV1, existing []generationactivation.ActivationEndorsementV1) (generationactivation.GenerationActivationUnsignedV1, []generationactivation.ActivationEndorsementV1, error) {
	if c == nil || c.Exchange == nil {
		return generationactivation.GenerationActivationUnsignedV1{}, nil, generationactivation.ErrSigningAuthorityUnavailable
	}
	input := c.Input
	input.PreviousAuthorityDigest = proposed.PreviousAuthorityDigest
	local, err := generationactivation.Endorse(input, proposed)
	if err != nil {
		return generationactivation.GenerationActivationUnsignedV1{}, nil, err
	}
	existing = mergeActivationEndorsement(existing, local)
	proposal, err := generationactivation.EncodeUnsignedCanonical(proposed)
	if err != nil {
		return generationactivation.GenerationActivationUnsignedV1{}, nil, err
	}
	round, err := generationactivation.EndorsementRoundDigest(proposed)
	if err != nil {
		return generationactivation.GenerationActivationUnsignedV1{}, nil, err
	}
	first, err := c.Exchange.ExchangeAuthorityEndorsementsV1(ctx, proto.RemoteExchangeAuthorityEndorsementsV1Params{
		Purpose: proto.RemoteAuthorityPurposeGenerationActivation, ScopeType: proposed.RosterScopeType, ScopeID: proposed.RosterScopeID,
		RoundDigest: round, Proposal: proposal, ExpiresAtUnix: proposed.NotAfterUnix, Endorsements: activationEndorsementsToProto(existing),
	})
	if err != nil {
		return generationactivation.GenerationActivationUnsignedV1{}, nil, err
	}
	selected, err := decodeActivationExchange(first)
	if err != nil {
		return generationactivation.GenerationActivationUnsignedV1{}, nil, err
	}
	input.PreviousAuthorityDigest = selected.PreviousAuthorityDigest
	selectedLocal, err := generationactivation.Endorse(input, selected)
	if err != nil {
		return generationactivation.GenerationActivationUnsignedV1{}, nil, err
	}
	second, err := c.Exchange.ExchangeAuthorityEndorsementsV1(ctx, proto.RemoteExchangeAuthorityEndorsementsV1Params{
		Purpose: proto.RemoteAuthorityPurposeGenerationActivation, ScopeType: selected.RosterScopeType, ScopeID: selected.RosterScopeID,
		RoundDigest: round, Proposal: first.Proposal, ExpiresAtUnix: first.ExpiresAtUnix,
		Endorsements: []proto.RemoteAuthorityEndorsementV1{{SignerKeyID: selectedLocal.SignerKeyID, Signature: selectedLocal.Signature}},
	})
	if err != nil {
		return generationactivation.GenerationActivationUnsignedV1{}, nil, err
	}
	confirmed, err := decodeActivationExchange(second)
	if err != nil || confirmed != selected {
		return generationactivation.GenerationActivationUnsignedV1{}, nil, generationactivation.ErrPendingActivation
	}
	return confirmed, activationEndorsementsFromProto(second.Endorsements), nil
}

func decodeActivationExchange(result proto.RemoteExchangeAuthorityEndorsementsV1Result) (generationactivation.GenerationActivationUnsignedV1, error) {
	if len(result.Proposal) == 0 || sha256.Sum256(result.Proposal) != result.ProposalDigest || result.ExpiresAtUnix <= 0 {
		return generationactivation.GenerationActivationUnsignedV1{}, generationactivation.ErrPendingActivation
	}
	unsigned, err := generationactivation.DecodeUnsignedCanonical(result.Proposal)
	if err != nil || unsigned.NotAfterUnix != result.ExpiresAtUnix {
		return generationactivation.GenerationActivationUnsignedV1{}, generationactivation.ErrPendingActivation
	}
	return unsigned, nil
}

func activationEndorsementsToProto(values []generationactivation.ActivationEndorsementV1) []proto.RemoteAuthorityEndorsementV1 {
	result := make([]proto.RemoteAuthorityEndorsementV1, 0, len(values))
	for _, value := range values {
		result = append(result, proto.RemoteAuthorityEndorsementV1{SignerKeyID: value.SignerKeyID, Signature: value.Signature})
	}
	return result
}

func activationEndorsementsFromProto(values []proto.RemoteAuthorityEndorsementV1) []generationactivation.ActivationEndorsementV1 {
	result := make([]generationactivation.ActivationEndorsementV1, 0, len(values))
	for _, value := range values {
		result = append(result, generationactivation.ActivationEndorsementV1{SignerKeyID: value.SignerKeyID, Signature: value.Signature})
	}
	return result
}

func mergeActivationEndorsement(values []generationactivation.ActivationEndorsementV1, local generationactivation.ActivationEndorsementV1) []generationactivation.ActivationEndorsementV1 {
	result := append([]generationactivation.ActivationEndorsementV1(nil), values...)
	for index := range result {
		if result[index].SignerKeyID == local.SignerKeyID {
			result[index] = local
			return result
		}
	}
	return append(result, local)
}

// FreshnessEndorsementCollector elects one valid freshness successor for an
// exact roster head and contributes only this device's local authority
// signature. The cloud remains an opaque mailbox and cannot manufacture a
// renewal.
type FreshnessEndorsementCollector struct {
	Exchange authorityEndorsementExchanger
	Identity generationactivation.ExistingIdentitySource
}

func (c *FreshnessEndorsementCollector) CollectFreshnessEndorsements(ctx context.Context, previous identity.VerifiedRoster, proposed identity.RosterManifestUnsignedV1) (identity.RosterManifestUnsignedV1, []identity.RosterFreshnessEndorsementV1, error) {
	if c == nil || c.Exchange == nil || c.Identity == nil {
		return identity.RosterManifestUnsignedV1{}, nil, identity.ErrFreshnessAuthorityUnavailable
	}
	device, err := c.Identity.LoadExisting()
	if err != nil {
		return identity.RosterManifestUnsignedV1{}, nil, identity.ErrFreshnessAuthorityUnavailable
	}
	local, err := identity.SignFreshnessRenewal(previous, proposed, device.SigningKeyID, device.SigningPrivate)
	if err != nil {
		return identity.RosterManifestUnsignedV1{}, nil, err
	}
	proposal, err := identity.CanonicalRosterBytes(proposed)
	if err != nil {
		return identity.RosterManifestUnsignedV1{}, nil, err
	}
	round, err := identity.FreshnessEndorsementRoundDigest(previous)
	if err != nil {
		return identity.RosterManifestUnsignedV1{}, nil, err
	}
	first, err := c.Exchange.ExchangeAuthorityEndorsementsV1(ctx, proto.RemoteExchangeAuthorityEndorsementsV1Params{
		Purpose: proto.RemoteAuthorityPurposeRosterFreshness, ScopeType: proposed.ScopeType, ScopeID: proposed.ScopeID,
		// The mailbox authorization cannot outlive the authority roster that
		// authorizes these signers.  The proposed successor itself is valid for
		// another 24 hours, but it is not authoritative until threshold-finalized
		// and appended.
		RoundDigest: round, Proposal: proposal, ExpiresAtUnix: previous.Manifest.Manifest.NotAfterUnix,
		Endorsements: []proto.RemoteAuthorityEndorsementV1{{SignerKeyID: local.SignerKeyID, Signature: local.Signature}},
	})
	if err != nil {
		return identity.RosterManifestUnsignedV1{}, nil, err
	}
	selected, err := decodeFreshnessExchange(first)
	if err != nil {
		return identity.RosterManifestUnsignedV1{}, nil, err
	}
	selectedLocal, err := identity.SignFreshnessRenewal(previous, selected, device.SigningKeyID, device.SigningPrivate)
	if err != nil {
		return identity.RosterManifestUnsignedV1{}, nil, err
	}
	second, err := c.Exchange.ExchangeAuthorityEndorsementsV1(ctx, proto.RemoteExchangeAuthorityEndorsementsV1Params{
		Purpose: proto.RemoteAuthorityPurposeRosterFreshness, ScopeType: selected.ScopeType, ScopeID: selected.ScopeID,
		RoundDigest: round, Proposal: first.Proposal, ExpiresAtUnix: first.ExpiresAtUnix,
		Endorsements: []proto.RemoteAuthorityEndorsementV1{{SignerKeyID: selectedLocal.SignerKeyID, Signature: selectedLocal.Signature}},
	})
	if err != nil {
		return identity.RosterManifestUnsignedV1{}, nil, err
	}
	confirmed, err := decodeFreshnessExchange(second)
	if err != nil || !sameRosterProposal(confirmed, selected) {
		return identity.RosterManifestUnsignedV1{}, nil, identity.ErrInvalidFreshnessRenewal
	}
	endorsements := make([]identity.RosterFreshnessEndorsementV1, 0, len(second.Endorsements))
	for _, value := range second.Endorsements {
		endorsements = append(endorsements, identity.RosterFreshnessEndorsementV1{SignerKeyID: value.SignerKeyID, Signature: value.Signature})
	}
	return confirmed, endorsements, nil
}

func decodeFreshnessExchange(result proto.RemoteExchangeAuthorityEndorsementsV1Result) (identity.RosterManifestUnsignedV1, error) {
	if len(result.Proposal) == 0 || sha256.Sum256(result.Proposal) != result.ProposalDigest || result.ExpiresAtUnix <= 0 {
		return identity.RosterManifestUnsignedV1{}, identity.ErrInvalidFreshnessRenewal
	}
	unsigned, err := identity.DecodeCanonicalRosterUnsigned(result.Proposal)
	if err != nil {
		return identity.RosterManifestUnsignedV1{}, identity.ErrInvalidFreshnessRenewal
	}
	return unsigned, nil
}

func sameRosterProposal(left, right identity.RosterManifestUnsignedV1) bool {
	leftBytes, leftErr := identity.CanonicalRosterBytes(left)
	rightBytes, rightErr := identity.CanonicalRosterBytes(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftBytes, rightBytes)
}

var (
	_ generationactivation.ActivationEndorsementCollector = (*GenerationActivationEndorsementCollector)(nil)
	_ interface {
		CollectFreshnessEndorsements(context.Context, identity.VerifiedRoster, identity.RosterManifestUnsignedV1) (identity.RosterManifestUnsignedV1, []identity.RosterFreshnessEndorsementV1, error)
	} = (*FreshnessEndorsementCollector)(nil)
)
