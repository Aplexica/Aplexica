package daemon

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"github.com/aplexica/aplexica/internal/devicetransition"
	"github.com/aplexica/aplexica/internal/identity"
	"github.com/aplexica/aplexica/internal/keyrotation"
	"github.com/aplexica/aplexica/internal/keys"
	"github.com/aplexica/aplexica/internal/plugin/proto"
	"github.com/aplexica/aplexica/internal/securityepoch"
)

const (
	deviceTransitionPollInterval = 5 * time.Second
	deviceTransitionRPCDeadline  = 20 * time.Second
	deviceTransitionKind         = "device-transition-plan"
)

type deviceTransitionRemote interface {
	devicetransition.BarrierTransport
	SubmitDeviceTransitionPlan(context.Context, proto.RemoteSubmitDeviceTransitionPlanParams) error
	GetDeviceTransitionPlans(context.Context, proto.RemoteGetDeviceTransitionPlansParams) (proto.RemoteGetDeviceTransitionPlansResult, error)
	CurrentDeviceID() string
}

type existingDeviceTransitionIdentity interface {
	LoadExisting() (keys.DeviceIdentity, error)
}

type deviceTransitionLogger interface {
	Info(string, ...any)
	Warn(string, ...any)
}

// DeviceTransitionService is the production wiring around the pure durable
// installer. It polls only for the immediate successor of locally pinned
// namespace chains and accepts a local signed-plan submission over the private
// daemon control socket. Cloud/plugin bytes are routing input, never authority:
// DecodePlan and the local chain verify every signature before cutover.
type DeviceTransitionService struct {
	IdentityRoot string
	Runner       deviceTransitionRemote
	Identity     existingDeviceTransitionIdentity
	Security     *securityepoch.Coordinator
	Publisher    *RemotePublishAdapter
	Recipients   *RecipientResolver
	Logger       deviceTransitionLogger
	Interval     time.Duration

	mu        sync.Mutex
	lastError string
}

type deviceTransitionCutover struct {
	publisher  *RemotePublishAdapter
	recipients *RecipientResolver
}

func (c deviceTransitionCutover) RescanCanonical(_ context.Context, plan devicetransition.PlanV1) error {
	if c.publisher == nil {
		return errors.New("device transition: remote publisher unavailable")
	}
	next := securityepoch.SecurityEpoch{CoordinatorGeneration: plan.SecurityEpoch.CoordinatorGeneration, AccessGeneration: plan.SecurityEpoch.AccessGeneration,
		AccessSetHash: plan.SecurityEpoch.AccessSetHash, BarrierID: plan.SecurityEpoch.BarrierID, KeyMode: plan.SecurityEpoch.KeyMode, KeyVersion: plan.SecurityEpoch.KeyVersion}
	return c.publisher.RequireSecurityCutover(plan.NamespaceID, next)
}

func (c deviceTransitionCutover) PurgeOldSealingMaterial(_ context.Context, plan devicetransition.PlanV1) error {
	if c.publisher == nil {
		return errors.New("device transition: remote publisher unavailable")
	}
	next := securityepoch.SecurityEpoch{CoordinatorGeneration: plan.SecurityEpoch.CoordinatorGeneration, AccessGeneration: plan.SecurityEpoch.AccessGeneration,
		AccessSetHash: plan.SecurityEpoch.AccessSetHash, BarrierID: plan.SecurityEpoch.BarrierID, KeyMode: plan.SecurityEpoch.KeyMode, KeyVersion: plan.SecurityEpoch.KeyVersion}
	if _, err := c.publisher.PurgeSecurityScope(plan.NamespaceID, next); err != nil {
		return err
	}
	// This cache contains only public recipient keys, but retaining a removed
	// key could reseal new canonical work to the old access set.
	if c.recipients != nil {
		c.recipients.InvalidateAll()
	}
	return nil
}

func (s *DeviceTransitionService) installer() (*devicetransition.Installer, error) {
	if s == nil || s.Runner == nil || s.Identity == nil || s.Security == nil || s.Publisher == nil || !filepath.IsAbs(s.IdentityRoot) {
		return nil, errors.New("device transition: service unavailable")
	}
	deviceID := s.Runner.CurrentDeviceID()
	if deviceID == "" {
		return nil, errors.New("device transition: paired device identity unavailable")
	}
	device, err := s.Identity.LoadExisting()
	if err != nil {
		return nil, err
	}
	return &devicetransition.Installer{
		IdentityRoot: s.IdentityRoot, Keys: &keyrotation.NamespaceKeyStore{Root: filepath.Join(s.IdentityRoot, "namespace-keys")},
		Coordinator: s.Security, RecipientPrivate: device.WrapPrivate, RecipientType: "device", RecipientID: deviceID,
		Barrier: s.Runner, Cutover: deviceTransitionCutover{publisher: s.Publisher, recipients: s.Recipients},
	}, nil
}

// SubmitPlan authenticates, durably stages, publishes, and installs one exact
// signed plan. The awaiting-distribution journal blocks publication before any
// opaque relay write; an exact cloud receipt advances it before the first
// plugin barrier mutation. Every interrupted boundary is safe to repeat.
func (s *DeviceTransitionService) SubmitPlan(ctx context.Context, blob []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	plan, err := devicetransition.DecodePlan(blob)
	if err != nil {
		return err
	}
	installer, err := s.installer()
	if err != nil {
		return err
	}
	if err := installer.StageForDistribution(ctx, plan); err != nil {
		return err
	}
	if err := s.relayPlan(ctx, plan, blob); err != nil {
		return err
	}
	if err := installer.MarkDistributed(plan); err != nil {
		return err
	}
	return installer.Install(ctx, plan)
}

func (s *DeviceTransitionService) relayPlan(ctx context.Context, plan devicetransition.PlanV1, blob []byte) error {
	object := transitionPlanObject(plan, blob)
	callCtx, cancel := context.WithTimeout(ctx, deviceTransitionRPCDeadline)
	err := s.Runner.SubmitDeviceTransitionPlan(callCtx, proto.RemoteSubmitDeviceTransitionPlanParams{Object: object})
	cancel()
	return err
}

func transitionPlanObject(plan devicetransition.PlanV1, blob []byte) proto.RemoteOpaqueSignedObject {
	return proto.RemoteOpaqueSignedObject{ScopeType: "namespace", ScopeID: plan.NamespaceID, Kind: deviceTransitionKind,
		Sequence: plan.NextRoster.Manifest.Epoch, PreviousHash: plan.PreviousRosterHash, Hash: sha256.Sum256(blob), Blob: append([]byte(nil), blob...)}
}

func validateTransitionPlanObject(scopeID string, from uint64, object proto.RemoteOpaqueSignedObject) (devicetransition.PlanV1, error) {
	if object.ScopeType != "namespace" || object.ScopeID != scopeID || object.Kind != deviceTransitionKind || object.Sequence != from+1 ||
		object.PreviousHash == ([32]byte{}) || object.Hash != sha256.Sum256(object.Blob) || len(object.ProofBlob) != 0 {
		return devicetransition.PlanV1{}, errors.New("device transition: invalid remote plan metadata")
	}
	plan, err := devicetransition.DecodePlan(object.Blob)
	if err != nil || plan.NamespaceID != scopeID || plan.NextRoster.Manifest.Epoch != object.Sequence || plan.PreviousRosterHash != object.PreviousHash {
		return devicetransition.PlanV1{}, errors.New("device transition: remote plan binding mismatch")
	}
	return plan, nil
}

// Run performs startup roll-forward immediately and then polls bounded
// one-successor pages. A plugin outage leaves the journal/roster cursor exactly
// where it was and simply retries; no later scope plan is skipped.
func (s *DeviceTransitionService) Run(ctx context.Context) {
	if s == nil {
		return
	}
	s.runPass(ctx)
	interval := s.Interval
	if interval <= 0 {
		interval = deviceTransitionPollInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.runPass(ctx)
		}
	}
}

func (s *DeviceTransitionService) runPass(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	installer, err := s.installer()
	if err == nil {
		var pending []devicetransition.PlanV1
		pending, err = installer.PendingDistribution()
		for _, plan := range pending {
			if err != nil {
				break
			}
			var blob []byte
			blob, err = devicetransition.EncodePlan(plan)
			if err == nil {
				err = s.relayPlan(ctx, plan, blob)
			}
			if err == nil {
				err = installer.MarkDistributed(plan)
			}
		}
	}
	if err == nil {
		_, err = installer.Recover(ctx)
	}
	if err == nil {
		for _, scope := range (&GenerationActivationDriver{IdentityRoot: s.IdentityRoot}).scopes() {
			if scope.namespaceID == "" || !regularFileExists(filepath.Join(scope.dir, "chain.cbor")) {
				continue
			}
			if err = s.pollScope(ctx, installer, scope); err != nil {
				break
			}
		}
	}
	s.record(err)
}

func (s *DeviceTransitionService) pollScope(ctx context.Context, installer *devicetransition.Installer, scope activationScope) error {
	chain := &identity.ChainStore{Path: filepath.Join(scope.dir, "chain.cbor")}
	// Bound catch-up work per tick even if a test/compromised service produces
	// a very long sequence. Every call itself returns at most one object.
	for count := 0; count < 32; count++ {
		head, err := chain.Head()
		if err != nil {
			return err
		}
		from := head.Manifest.Manifest.Epoch
		callCtx, cancel := context.WithTimeout(ctx, deviceTransitionRPCDeadline)
		result, err := s.Runner.GetDeviceTransitionPlans(callCtx, proto.RemoteGetDeviceTransitionPlansParams{ScopeID: scope.namespaceID, FromExclusive: from})
		cancel()
		if err != nil {
			return err
		}
		if len(result.Objects) == 0 {
			return nil
		}
		if len(result.Objects) != 1 || from == ^uint64(0) {
			return errors.New("device transition: invalid remote plan page")
		}
		plan, err := validateTransitionPlanObject(scope.namespaceID, from, result.Objects[0])
		if err != nil {
			return err
		}
		if err := installer.Install(ctx, plan); err != nil {
			return err
		}
	}
	return fmt.Errorf("device transition: catch-up bound reached")
}

func (s *DeviceTransitionService) record(err error) {
	message := ""
	if err != nil {
		message = err.Error()
	}
	if message == s.lastError {
		return
	}
	if message == "" {
		if s.lastError != "" && s.Logger != nil {
			s.Logger.Info("remote: device transition recovery restored")
		}
	} else if s.Logger != nil {
		s.Logger.Info("remote: device transition pending", "status", "retrying-fail-closed", "err", err)
	}
	s.lastError = message
}
