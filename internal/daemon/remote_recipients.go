package daemon

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/aplexica/aplexica/internal/keys"
	"github.com/aplexica/aplexica/internal/plugin/proto"
	syncd "github.com/aplexica/aplexica/internal/sync"
)

// accountDeviceLister is the slice of RemoteRunner the recipient resolver needs
// — the account-scoped remote.list_account_devices call. The RemoteRunner
// satisfies it. Account-scoped means NO namespace id is required, which is what
// makes Personal-tier sync work (the Personal namespace UUID is never known
// daemon-side; the account is resolved server-side from the device proof).
type accountDeviceLister interface {
	ListAccountDevices(ctx context.Context) (proto.RemoteListAccountDevicesResult, error)
}

// RecipientResolver resolves the device recipient set each outbound event is
// end-to-end encrypted for. It satisfies syncd.RecipientResolver.
//
// It calls remote.list_account_devices (via the RemoteRunner) to learn every
// active device in the caller's ACCOUNT that has a registered X25519 wrap
// pubkey, CACHED for a short TTL so we never hit the plugin per event. THIS
// device is ALWAYS added to the set (from its own wrap pubkey) so a sender can
// decrypt its own re-imports even when it is the only paired device or the list
// call is momentarily unavailable (the account list already includes self once
// it has registered, but we add it unconditionally for safety + the pre-
// registration window).
//
// ZERO-KNOWLEDGE: the resolver only ever returns devices whose wrap pubkey it
// actually has (from the cloud's authoritative account device list, or this
// device's own key). It NEVER fabricates a recipient, so the orchestrator
// either encrypts to real, authorised keys or — when even this device's key is
// unavailable — returns empty and the orchestrator DROPS the event (never
// plaintext).
type RecipientResolver struct {
	ctx        context.Context
	lister     accountDeviceLister
	deviceIDFn func() string
	selfPubFn  func() ([keys.X25519KeySize]byte, error)
	logger     interface {
		Info(msg string, args ...any)
		Warn(msg string, args ...any)
		Error(msg string, args ...any)
	}

	ttl time.Duration

	mu     sync.Mutex
	cached *cachedRecipients // account-scoped: a single cached set (not per-namespace)
}

type cachedRecipients struct {
	at         time.Time
	recipients []syncd.Recipient
}

// recipientCacheTTL bounds how stale a cached device list may be. 30s keeps the
// plugin un-hammered while a freshly-registered peer device becomes a recipient
// within the window (and a roster / membership change re-resolves via the same
// TTL). InvalidateAll forces an immediate refresh.
const recipientCacheTTL = 30 * time.Second

// NewRecipientResolverFromRunner builds a recipient resolver bound to a
// RemoteRunner (for remote.list_account_devices) + this device's key provider
// (for the always-included self recipient). deviceIDFn is read lazily so a
// device id learned at a later pairing is picked up without a restart. logger
// may be nil.
func NewRecipientResolverFromRunner(
	ctx context.Context,
	runner *RemoteRunner,
	deviceIDFn func() string,
	keyProv DeviceKeyProvider,
	logger interface {
		Info(msg string, args ...any)
		Warn(msg string, args ...any)
		Error(msg string, args ...any)
	},
) *RecipientResolver {
	return newRecipientResolver(ctx, runner, deviceIDFn, keyProv.Public, logger)
}

// newRecipientResolver builds a resolver. deviceIDFn returns this device's
// cloud device id (empty when unpaired); selfPubFn returns this device's wrap
// public key. logger may be nil.
func newRecipientResolver(
	ctx context.Context,
	lister accountDeviceLister,
	deviceIDFn func() string,
	selfPubFn func() ([keys.X25519KeySize]byte, error),
	logger interface {
		Info(msg string, args ...any)
		Warn(msg string, args ...any)
		Error(msg string, args ...any)
	},
) *RecipientResolver {
	return &RecipientResolver{
		ctx:        ctx,
		lister:     lister,
		deviceIDFn: deviceIDFn,
		selfPubFn:  selfPubFn,
		logger:     logger,
		ttl:        recipientCacheTTL,
	}
}

// Recipients satisfies syncd.RecipientResolver. namespaceID is ACCEPTED for
// interface compatibility but IGNORED — recipient discovery is account-scoped
// (remote.list_account_devices), which is what lets Personal-tier sync work
// without a namespace id. Returns the cached (or freshly-fetched) account
// device set, always including this device.
func (r *RecipientResolver) Recipients(_ string) ([]syncd.Recipient, error) {
	r.mu.Lock()
	if r.cached != nil && time.Since(r.cached.at) < r.ttl {
		out := append([]syncd.Recipient(nil), r.cached.recipients...)
		r.mu.Unlock()
		return out, nil
	}
	r.mu.Unlock()

	recipients, err := r.fetch()
	if err != nil {
		// An unsigned server device list is a legacy compatibility seam only.
		// It must never fall back to stale peers or a self-only recipient set:
		// either behavior can continue publication across a revocation cutover.
		return nil, err
	}

	// Only cache a COMPLETE resolution (the account device list succeeded, or
	// there is no lister to consult). A DEGRADED self-only fallback — returned
	// when ListAccountDevices fails because the plugin is reconnecting — is
	// deliberately NOT cached, so the very next event re-fetches the moment the
	// plugin is back. Caching the degraded result was the cross-device sync bug:
	// a sub-second reconnect poisoned recipient resolution for the whole TTL, so
	// every event in that window was sealed for THIS device only and peers
	// dropped the ciphertext they could not decrypt.
	r.mu.Lock()
	r.cached = &cachedRecipients{at: time.Now(), recipients: recipients}
	r.mu.Unlock()
	return append([]syncd.Recipient(nil), recipients...), nil
}

// InvalidateAll drops the cache so the next Recipients re-fetches. Wire to the
// membership_changed enumerate-hint so a roster change reaches the recipient
// set promptly.
func (r *RecipientResolver) InvalidateAll() {
	r.mu.Lock()
	r.cached = nil
	r.mu.Unlock()
}

// fetch resolves the account recipient set: every account device with a
// registered wrap pubkey (best-effort via the plugin) UNION this device.
//
// It returns listOK to tell the caller whether this is a COMPLETE resolution
// that may be cached. listOK is true when the account list call succeeded (or
// there is no lister — a static self-only config with nothing to retry). listOK
// is FALSE when ListAccountDevices errored: fetch still returns self-only (this
// device can always decrypt its own re-imports, and the orchestrator drops only
// when even self is unavailable), but the caller MUST NOT cache it — the next
// event must re-fetch so peers are picked up the instant the plugin reconnects.
func (r *RecipientResolver) fetch() ([]syncd.Recipient, error) {
	seen := map[string]bool{}
	out := make([]syncd.Recipient, 0, 4)

	// Always include this device first.
	selfID := ""
	if r.deviceIDFn != nil {
		selfID = r.deviceIDFn()
	}
	if selfID != "" && r.selfPubFn != nil {
		if pub, err := r.selfPubFn(); err == nil {
			out = append(out, syncd.Recipient{DeviceID: selfID, PubKey: pub})
			seen[selfID] = true
		} else if r.logger != nil {
			r.logger.Warn("recipient resolver: load self wrap pubkey failed", "err", err)
		}
	}

	// No lister: a static self-only configuration (no remote transport). There
	// is nothing to retry, so this is a complete, cacheable result.
	if r.lister == nil {
		return out, nil
	}

	// Account devices from the cloud. Account-scoped — no namespace id needed,
	// so this works for Personal tier (unlike the namespace device list).
	ctx, cancel := context.WithTimeout(r.ctx, 10*time.Second)
	res, err := r.lister.ListAccountDevices(ctx)
	cancel()
	if err != nil {
		// Degraded: self-only for THIS call, but report listOK=false so the
		// caller does not cache it (the next event re-fetches).
		if r.logger != nil {
			r.logger.Warn("recipient resolver: list account devices failed; degrading to self-only (not cached)",
				"err", err)
		}
		return nil, fmt.Errorf("recipient resolver: refresh failed: %w", err)
	}
	for _, d := range res.Devices {
		if d.DeviceID == "" || seen[d.DeviceID] || len(d.PubKey) != keys.X25519KeySize {
			continue
		}
		var pub [keys.X25519KeySize]byte
		copy(pub[:], d.PubKey)
		out = append(out, syncd.Recipient{DeviceID: d.DeviceID, PubKey: pub})
		seen[d.DeviceID] = true
	}
	if r.logger != nil {
		r.logger.Info("recipient resolver: account devices resolved",
			"recipient_count", len(out),
			"device_count", len(res.Devices))
	}
	return out, nil
}

// compile-time assertion: *RecipientResolver satisfies syncd.RecipientResolver.
var _ syncd.RecipientResolver = (*RecipientResolver)(nil)
