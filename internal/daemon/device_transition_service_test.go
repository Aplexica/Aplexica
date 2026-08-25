package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"testing"

	"github.com/aplexica/aplexica/internal/devicetransition"
	"github.com/aplexica/aplexica/internal/identity"
	"github.com/aplexica/aplexica/internal/keys"
	"github.com/aplexica/aplexica/internal/plugin/proto"
	"github.com/aplexica/aplexica/internal/securityepoch"
	"github.com/stretchr/testify/require"
)

type transitionServiceRemoteStub struct{ submitted int }

func (*transitionServiceRemoteStub) CurrentDeviceID() string { return "device-a" }
func (stub *transitionServiceRemoteStub) SubmitDeviceTransitionPlan(context.Context, proto.RemoteSubmitDeviceTransitionPlanParams) error {
	stub.submitted++
	return nil
}
func (*transitionServiceRemoteStub) GetDeviceTransitionPlans(context.Context, proto.RemoteGetDeviceTransitionPlansParams) (proto.RemoteGetDeviceTransitionPlansResult, error) {
	return proto.RemoteGetDeviceTransitionPlansResult{}, nil
}
func (*transitionServiceRemoteStub) SecurityEpochPrepare(context.Context, proto.RemoteSecurityEpochPrepareParams) (proto.RemoteSecurityEpochStatusResult, error) {
	return proto.RemoteSecurityEpochStatusResult{}, nil
}
func (*transitionServiceRemoteStub) SecurityEpochCommit(context.Context, proto.RemoteSecurityEpochCommandParams) (proto.RemoteSecurityEpochStatusResult, error) {
	return proto.RemoteSecurityEpochStatusResult{}, nil
}
func (*transitionServiceRemoteStub) SecurityEpochActivate(context.Context, proto.RemoteSecurityEpochCommandParams) (proto.RemoteSecurityEpochStatusResult, error) {
	return proto.RemoteSecurityEpochStatusResult{}, nil
}
func (*transitionServiceRemoteStub) SecurityEpochStatus(context.Context, proto.RemoteSecurityEpochCommandParams) (proto.RemoteSecurityEpochStatusResult, error) {
	return proto.RemoteSecurityEpochStatusResult{}, nil
}

type transitionServiceIdentityStub struct{ identity keys.DeviceIdentity }

func (stub transitionServiceIdentityStub) LoadExisting() (keys.DeviceIdentity, error) {
	return stub.identity, nil
}

// checksumValidSyntheticPlan is intentionally not cryptographically valid. It
// is useful for proving that DecodePlan/transport metadata success does not let
// SubmitPlan write to the cloud before the local signed-chain gate runs.
func checksumValidSyntheticPlan(t *testing.T) (devicetransition.PlanV1, []byte) {
	t.Helper()
	plan := devicetransition.PlanV1{
		Version: 1, NamespaceID: "0197f30a-3c58-7000-8000-000000000001",
		PreviousRosterHash: sha256.Sum256([]byte("previous roster")),
		NextRoster:         syntheticRosterAtEpoch(2),
		RescanObligationID: sha256.Sum256([]byte("rescan")), StagedPackageHash: sha256.Sum256([]byte("staged")),
		SignerKeyIDs: [][32]byte{sha256.Sum256([]byte("signer"))}, Signatures: make([][64]byte, 1),
	}
	unsigned, err := json.Marshal(plan)
	require.NoError(t, err)
	plan.Checksum = sha256.Sum256(append([]byte("aplexica/device-rekey-transition-plan/v1\x00"), unsigned...))
	blob, err := devicetransition.EncodePlan(plan)
	require.NoError(t, err)
	return plan, blob
}

func syntheticRosterAtEpoch(epoch uint64) identity.RosterManifestV1 {
	return identity.RosterManifestV1{Manifest: identity.RosterManifestUnsignedV1{Epoch: epoch}}
}

func TestDeviceTransitionSubmitValidatesLocalAuthorityBeforeRelayWrite(t *testing.T) {
	_, blob := checksumValidSyntheticPlan(t)
	remote := &transitionServiceRemoteStub{}
	root := t.TempDir()
	service := &DeviceTransitionService{
		IdentityRoot: root, Runner: remote,
		Identity: transitionServiceIdentityStub{identity: keys.DeviceIdentity{WrapPrivate: sha256.Sum256([]byte("private"))}},
		Security: &securityepoch.Coordinator{Root: root}, Publisher: &RemotePublishAdapter{},
	}
	err := service.SubmitPlan(context.Background(), blob)
	require.Error(t, err)
	require.Zero(t, remote.submitted, "opaque cloud storage must follow local signature/chain validation")
}

func TestDeviceTransitionRemoteObjectRequiresExactImmediateSuccessorBinding(t *testing.T) {
	plan, blob := checksumValidSyntheticPlan(t)
	object := transitionPlanObject(plan, blob)
	decoded, err := validateTransitionPlanObject(plan.NamespaceID, 1, object)
	require.NoError(t, err)
	require.Equal(t, plan.Checksum, decoded.Checksum)

	object.Hash = sha256.Sum256([]byte("rewritten"))
	_, err = validateTransitionPlanObject(plan.NamespaceID, 1, object)
	require.ErrorContains(t, err, "metadata")
}
