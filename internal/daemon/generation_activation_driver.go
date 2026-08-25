package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/generationactivation"
	"github.com/aplexica/aplexica/internal/identity"
	"github.com/aplexica/aplexica/internal/plugin/proto"
)

type generationActivationLogger interface {
	Info(string, ...any)
}

// GenerationActivationDriver watches only already-provisioned identity state.
// Missing chains, epochs, or authority keys are waiting states: the driver
// never creates or repairs them. An old plugin safely returns method-not-found;
// the exact pending statement remains durable while legacy/shadow sync runs.
type GenerationActivationDriver struct {
	IdentityRoot string
	Runner       *RemoteRunner
	Identity     generationactivation.ExistingIdentitySource
	Logger       generationActivationLogger
	Interval     time.Duration
	// Trigger requests an immediate pass after an explicit local genesis
	// install. It carries no identity or secret content; a nil channel leaves
	// the periodic behavior unchanged.
	Trigger   <-chan struct{}
	lastError map[string]string
}

type activationScope struct {
	namespaceID string
	dir         string
}

const generationActivationProtocolV1 uint16 = 1

func (d *GenerationActivationDriver) Run(ctx context.Context) {
	if d == nil || d.Runner == nil || d.Identity == nil || d.IdentityRoot == "" {
		return
	}
	interval := d.Interval
	if interval <= 0 {
		interval = 5 * time.Second
	}
	d.runPass(ctx)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	trigger := d.Trigger
	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-trigger:
			if !ok {
				trigger = nil
				continue
			}
			d.runPass(ctx)
		case <-ticker.C:
			d.runPass(ctx)
		}
	}
}

func (d *GenerationActivationDriver) runPass(ctx context.Context) {
	deviceID := d.Runner.CurrentDeviceID()
	if deviceID == "" {
		return
	}
	negotiation := d.Runner.SyncNegotiation()
	for _, scope := range d.scopes() {
		streamEpoch := generationStreamEpoch(negotiation, scope.namespaceID)
		if streamEpoch == "" {
			continue
		}
		epochPath := filepath.Join(scope.dir, "security-epoch.json")
		chainPath := filepath.Join(scope.dir, "chain.cbor")
		statePath := filepath.Join(scope.dir, "generation-activation.json")
		hasRecoveryState := regularFileExists(statePath)
		if !hasRecoveryState && (!regularFileExists(chainPath) || !regularFileExists(epochPath)) {
			continue
		}
		epoch, epochErr := generationactivation.LoadSecurityEpoch(epochPath)
		err := epochErr
		if epochErr == nil || hasRecoveryState {
			chain := &identity.ChainStore{Path: chainPath}
			coordinator := generationactivation.Coordinator{
				Chain: chain, Epoch: epoch, StreamEpoch: streamEpoch,
				NamespaceID: scope.namespaceID, DeviceID: deviceID, Identity: d.Identity,
				State:     generationactivation.FileStateStore{Path: statePath},
				Transport: RemoteGenerationActivationTransport{Runner: d.Runner},
			}
			now := time.Now().UTC()
			if snapshot, snapshotErr := chain.PublicationSnapshot(now); snapshotErr == nil {
				if deviceIdentity, identityErr := d.Identity.LoadExisting(); identityErr == nil {
					coordinator.Collector = &GenerationActivationEndorsementCollector{Exchange: d.Runner, Input: generationactivation.BuildInput{
						AccountID: snapshot.AccountID, NamespaceID: scope.namespaceID, StreamEpoch: streamEpoch,
						Roster: snapshot.Current, SecurityEpoch: epoch, DeviceID: deviceID, DeviceIdentity: deviceIdentity, Now: now,
					}}
					coordinator.Endorsement = generationactivation.FileEndorsementJournal{Path: filepath.Join(scope.dir, "generation-activation-endorsements.json")}
				}
			}
			_, err = coordinator.RunOnce(ctx)
		}
		d.record(scope.namespaceID, err)
	}
}

func (d *GenerationActivationDriver) scopes() []activationScope {
	result := []activationScope{{dir: filepath.Join(d.IdentityRoot, "account")}}
	namespacesRoot := filepath.Join(d.IdentityRoot, "namespaces")
	entries, err := os.ReadDir(namespacesRoot)
	if err != nil {
		return result
	}
	for _, entry := range entries {
		if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || acf.ValidateWireUUIDv7(entry.Name()) != nil {
			continue
		}
		result = append(result, activationScope{namespaceID: entry.Name(), dir: filepath.Join(namespacesRoot, entry.Name())})
	}
	sort.Slice(result[1:], func(i, j int) bool { return result[i+1].namespaceID < result[j+1].namespaceID })
	return result
}

func (d *GenerationActivationDriver) record(namespaceID string, err error) {
	key := namespaceID
	if key == "" {
		key = "account"
	}
	if d.lastError == nil {
		d.lastError = map[string]string{}
	}
	if err == nil {
		if d.lastError[key] != "" && d.Logger != nil {
			d.Logger.Info("remote: durable generation activation recovered", "scope", key)
		}
		delete(d.lastError, key)
		return
	}
	message := err.Error()
	if d.lastError[key] == message {
		return
	}
	d.lastError[key] = message
	if d.Logger != nil {
		status := "retrying"
		if errors.Is(err, generationactivation.ErrSigningAuthorityUnavailable) {
			status = "waiting-for-existing-authority-key"
		} else if errors.Is(err, generationactivation.ErrPendingActivation) {
			status = "pending-exact-recovery"
		}
		d.Logger.Info("remote: durable generation activation unavailable", "scope", key, "status", status, "err", err)
	}
}

func generationStreamEpoch(negotiation proto.RemoteNegotiateSyncV1Result, namespaceID string) string {
	if negotiation.SelectedProtocol != generationActivationProtocolV1 {
		return ""
	}
	for _, stream := range negotiation.Streams {
		if stream.NamespaceID == namespaceID && stream.StreamEpoch != "" {
			return stream.StreamEpoch
		}
	}
	if namespaceID == "" {
		return negotiation.StreamEpoch
	}
	return ""
}

func regularFileExists(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode().IsRegular()
}
