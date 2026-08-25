package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/aplexica/aplexica/internal/generationactivation"
	"github.com/aplexica/aplexica/internal/identity"
	"github.com/aplexica/aplexica/internal/rosterenewal"
	"github.com/aplexica/aplexica/internal/securityepoch"
)

const (
	defaultRosterRenewAfter            = 6 * time.Hour
	defaultRosterRenewRetry            = time.Minute
	defaultRosterCredentialRenewBefore = 30 * 24 * time.Hour
)

// RosterRenewalDriver production-wires the crash-safe short-lived roster
// renewal coordinator for every locally pinned account/namespace scope. It
// never invents identity state and never asks the service to sign freshness.
type RosterRenewalDriver struct {
	IdentityRoot string
	Runner       *RemoteRunner
	Identity     generationactivation.ExistingIdentitySource
	Security     *securityepoch.Coordinator
	Logger       generationActivationLogger
	Interval     time.Duration
	lastError    map[string]string
}

func (d *RosterRenewalDriver) Run(ctx context.Context) {
	if d == nil || d.Runner == nil || d.Identity == nil || d.IdentityRoot == "" {
		return
	}
	d.runPass(ctx)
	interval := d.Interval
	if interval <= 0 {
		interval = defaultRosterRenewRetry
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			d.runPass(ctx)
		}
	}
}

func (d *RosterRenewalDriver) runPass(ctx context.Context) {
	for _, scope := range (&GenerationActivationDriver{IdentityRoot: d.IdentityRoot}).scopes() {
		chainPath := filepath.Join(scope.dir, "chain.cbor")
		if !regularFileExists(chainPath) || !regularFileExists(filepath.Join(scope.dir, "security-epoch.json")) {
			continue
		}
		chain := &identity.ChainStore{Path: chainPath}
		coordinator := rosterenewal.Coordinator{
			IdentityRoot: d.IdentityRoot, Chain: chain, Security: d.Security,
			Collector: &FreshnessEndorsementCollector{Exchange: d.Runner, Identity: d.Identity},
			Policy:    rosterenewal.Policy{RenewAfter: defaultRosterRenewAfter, RetryInterval: defaultRosterRenewRetry, CredentialRenewBefore: defaultRosterCredentialRenewBefore},
		}
		result, err := coordinator.RunOnce(ctx)
		d.recordRenewal(scope, result, err)
	}
}

func (d *RosterRenewalDriver) recordRenewal(scope activationScope, result rosterenewal.Result, err error) {
	key := scope.namespaceID
	if key == "" {
		key = "account"
	}
	if d.lastError == nil {
		d.lastError = make(map[string]string)
	}
	if err == nil {
		if result.Renewed && d.Logger != nil {
			d.Logger.Info("remote: signed roster freshness renewed", "scope", key, "roster_hash", result.RosterHash)
		}
		if d.lastError[key] != "" && d.Logger != nil {
			d.Logger.Info("remote: roster renewal recovered", "scope", key)
		}
		delete(d.lastError, key)
		return
	}
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	message := err.Error()
	if d.lastError[key] == message {
		return
	}
	d.lastError[key] = message
	if d.Logger != nil {
		status := "retrying"
		if errors.Is(err, rosterenewal.ErrRosterExpired) {
			status = "expired-cloud-sync-paused"
		} else if errors.Is(err, identity.ErrFreshnessAuthorityUnavailable) {
			status = "waiting-for-authority-threshold"
		} else if errors.Is(err, rosterenewal.ErrPendingRenewal) {
			status = "recovering-durable-renewal"
		}
		d.Logger.Info("remote: signed roster renewal unavailable", "scope", key, "status", status, "err", err)
	}
}
