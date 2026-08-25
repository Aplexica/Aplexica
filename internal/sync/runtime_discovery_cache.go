package syncd

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/aplexica/aplexica/internal/atomicfile"
)

// runtimeDiscoveryCacheName records the last successfully processed runtime
// adapter topology. It lets the daemon distinguish an unchanged restart from
// an adapter that was installed while the daemon was stopped.
const runtimeDiscoveryCacheName = ".runtime-discovery-cache.json"

type runtimeDiscoveryState struct {
	Token     string `json:"token"`
	Installed bool   `json:"installed"`
}

func loadRuntimeDiscoveryCache(storeRoot string) (map[string]runtimeDiscoveryState, bool) {
	if storeRoot == "" {
		return nil, false
	}
	data, err := os.ReadFile(filepath.Join(storeRoot, runtimeDiscoveryCacheName))
	if err != nil {
		return nil, false
	}
	var states map[string]runtimeDiscoveryState
	if json.Unmarshal(data, &states) != nil || states == nil {
		return nil, false
	}
	return states, true
}

func writeRuntimeDiscoveryCache(storeRoot string, states map[string]runtimeDiscoveryState) error {
	if storeRoot == "" {
		return nil
	}
	data, err := json.Marshal(states)
	if err != nil {
		return err
	}
	return atomicfile.WriteFile(filepath.Join(storeRoot, runtimeDiscoveryCacheName), data, 0o600)
}
