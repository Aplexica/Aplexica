package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/aplexica/aplexica/internal/adapter"
	"github.com/aplexica/aplexica/internal/adapter/claudecode"
	"github.com/aplexica/aplexica/internal/adapter/codex"
	"github.com/aplexica/aplexica/internal/adapter/hermes"
	"github.com/aplexica/aplexica/internal/adapter/kilo"
	"github.com/aplexica/aplexica/internal/adapter/openclaw"
	"github.com/aplexica/aplexica/internal/secrets"
)

// cliCloudDeviceID asks the running daemon for the cloud device identity it
// stamps on outbound event provenance.
//
// CLI-authored store events must carry the SAME identity: the outbound sweep
// (remoteSweepLocalHead) publishes only heads whose provenance names this
// device's cloud id, and the adapters' constructor default is os.Hostname().
// A head authored under the hostname is therefore never published. It remains
// pending until a daemon-authored event triggers gap backfill.
//
// Best-effort by design: when the daemon is down, remote sync is disabled, or
// the plugin is unpaired, the empty result leaves the hostname default in
// place, which matches today's behavior for a store that is not synced.
func cliCloudDeviceID() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	info, err := queryDaemonStatus(filepath.Join(home, ".aplexica", "state", "aplexicad.sock"), false)
	if err != nil {
		return ""
	}
	return info.LocalDeviceID
}

// buildAdapter constructs the adapter for the given name and overrides its
// SecretsStore to use secretsRoot. The returned value is the adapter.Adapter
// interface so the CLI can be agnostic about the concrete type.
func buildAdapter(name, secretsRoot string) (adapter.Adapter, error) {
	ss := &secrets.Store{Root: secretsRoot}
	if err := ss.Init(); err != nil {
		return nil, err
	}
	deviceID := cliCloudDeviceID()
	switch name {
	case "claude-code":
		a := claudecode.New()
		a.SecretsStore = ss
		if deviceID != "" {
			a.SetDeviceID(deviceID)
		}
		return a, nil
	case "codex":
		a := codex.New()
		a.SecretsStore = ss
		if deviceID != "" {
			a.SetDeviceID(deviceID)
		}
		return a, nil
	case "kilo":
		a := kilo.New()
		a.SecretsStore = ss
		if deviceID != "" {
			a.SetDeviceID(deviceID)
		}
		return a, nil
	case "hermes":
		a := hermes.New()
		a.SecretsStore = ss
		if deviceID != "" {
			a.SetDeviceID(deviceID)
		}
		return a, nil
	case "openclaw":
		a := openclaw.New()
		a.SecretsStore = ss
		if deviceID != "" {
			a.SetDeviceID(deviceID)
		}
		return a, nil
	default:
		return nil, fmt.Errorf("unknown adapter %q (expected claude-code, codex, kilo, hermes, or openclaw)", name)
	}
}
