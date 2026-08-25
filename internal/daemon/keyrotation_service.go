package daemon

import (
	"context"
	"fmt"
	"sync"

	"github.com/aplexica/aplexica/internal/keyrotation"
	"github.com/aplexica/aplexica/internal/keys"
	"github.com/aplexica/aplexica/internal/plugin/proto"
	"github.com/aplexica/aplexica/internal/secrets"
)

// KeyRotationService is the daemon-side wiring for namespace key rotation. It
// bundles a keyrotation.Rotator with the local secrets-backed stores and
// exposes handler methods whose signatures match the RemoteRunner
// notification callbacks, so the daemon startup path can wire them
// directly:
//
//	svc := daemon.NewKeyRotationService(ctx, runner, store, identity, logger)
//	runner.OnNamespaceKeyRotated  = svc.HandleSignal
//	runner.OnNamespaceKeyBroadcast = svc.HandleBroadcast
//
// The notification callbacks carry no context or error return, so the
// service uses the daemon-lifetime context captured at construction and
// logs (rather than propagates) failures.
type KeyRotationService struct {
	rotator    *keyrotation.Rotator
	identityFn func() keyrotation.Identity
	ctx        context.Context
	logger     keyrotation.Logger

	// mu serializes the identity-set-then-call sequence. Notifications
	// dispatch serially from the proxy read pump today, but guarding keeps
	// the service correct if a caller ever invokes the handlers
	// concurrently.
	mu sync.Mutex
}

// NewKeyRotationService builds the service over a remote caller (the
// RemoteRunner), the local secrets store (for content keys + this device's
// keypair), and an identity provider. identityFn is consulted on each
// signal so a device id that becomes known only after pairing is picked up
// without a restart; it must not be nil. logger may be nil.
func NewKeyRotationService(
	ctx context.Context,
	caller remoteKeyRotationCaller,
	store *secrets.Store,
	identityFn func() keyrotation.Identity,
	logger keyrotation.Logger,
) *KeyRotationService {
	rotator := &keyrotation.Rotator{
		Transport:   newKeyRotationTransport(caller),
		ContentKeys: keyrotation.NewSecretsContentKeyStore(store),
		DeviceKeys:  DeviceKeyProvider{store: keys.NewDeviceKeyStore(store)},
		Logger:      logger,
	}
	return &KeyRotationService{rotator: rotator, identityFn: identityFn, ctx: ctx, logger: logger}
}

// HandleSignal is the OnNamespaceKeyRotated callback: turn the inbound
// audit signal into client-side key rotation.
func (s *KeyRotationService) HandleSignal(n proto.RemoteNamespaceKeyRotatedNotification) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rotator.Identity = s.identityFn()
	if err := s.rotator.HandleSignal(s.ctx, keyRotationSignalFromProto(n)); err != nil {
		s.warn("namespace key rotation failed", "namespace", n.NamespaceID, "version", n.NewVersion, "err", err)
	}
}

// HandleBroadcast is the OnNamespaceKeyBroadcast callback: install the
// wrapped key pushed to this device.
func (s *KeyRotationService) HandleBroadcast(n proto.RemoteNamespaceKeyBroadcastNotification) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rotator.Identity = s.identityFn()
	if err := s.rotator.InstallBroadcast(s.ctx, keyRotationBroadcastFromProto(n)); err != nil {
		s.warn("installing rotated key from broadcast failed", "namespace", n.NamespaceID, "version", n.KeyVersion, "err", err)
	}
}

func (s *KeyRotationService) warn(msg string, args ...any) {
	if s.logger != nil {
		s.logger.Warn(msg, args...)
	}
}

// DeviceKeyProvider adapts a keys.DeviceKeyStore to keyrotation.DeviceKeys
// (exposing only the private key the unwrap path needs). It ALSO satisfies
// syncd.DeviceKeyProvider (same Private() signature) so the orchestrator can
// open inbound E2E envelopes with this device's key, and exposes Public() for
// the recipient resolver to seal a sender's own re-imports to itself. The
// underlying keys.DeviceKeyStore.LoadOrCreate is stable across calls, so the
// private/public halves always match the key registered with the cloud at
// pairing.
type DeviceKeyProvider struct {
	store    *keys.DeviceKeyStore
	identity *keys.DeviceIdentityStore
}

// NewDeviceKeyProvider builds a device-key provider over the daemon's secrets
// store. Used to wire the orchestrator's inbound-decrypt key + the recipient
// resolver's self wrap pubkey.
func NewDeviceKeyProvider(store *secrets.Store) DeviceKeyProvider {
	return DeviceKeyProvider{store: keys.NewDeviceKeyStore(store), identity: &keys.DeviceIdentityStore{Secrets: store}}
}

func (p DeviceKeyProvider) Identity() (keys.DeviceIdentity, error) {
	if p.identity == nil {
		return keys.DeviceIdentity{}, fmt.Errorf("daemon: device identity provider unavailable")
	}
	return p.identity.LoadOrCreate()
}

func (p DeviceKeyProvider) Private() ([keys.X25519KeySize]byte, error) {
	priv, _, err := p.store.LoadOrCreate()
	return priv, err
}

// Public returns this device's X25519 wrap public key (the half registered
// with the cloud at pairing).
func (p DeviceKeyProvider) Public() ([keys.X25519KeySize]byte, error) {
	_, pub, err := p.store.LoadOrCreate()
	return pub, err
}
