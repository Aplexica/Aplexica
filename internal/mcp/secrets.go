package mcp

import (
	"fmt"
	"regexp"
	"sort"
)

// secretRef matches `${secret:<name>}` placeholders used to redact env
// values in the canonical form. The same syntax was used by the legacy
// per-adapter codecs; v0.3.0 just lifts the regex up to the canonical layer.
var secretRef = regexp.MustCompile(`^\$\{secret:([^}]+)\}$`)

// ExtractSecrets walks every server's Env map, lifts each value into the
// returned secrets map (keyed by `<serverName>.<envKey>`), and replaces
// the in-place value with a `${secret:<name>}` reference. Mutates the
// passed Canonical. Per ADR-0027, raw secret values never reach the
// canonical wire format.
func ExtractSecrets(c *Canonical) map[string]string {
	secrets := map[string]string{}
	for serverName, srv := range c.Servers {
		if len(srv.Env) == 0 {
			continue
		}
		for envKey, envVal := range srv.Env {
			name := serverName + "." + envKey
			secrets[name] = envVal
			srv.Env[envKey] = "${secret:" + name + "}"
		}
		c.Servers[serverName] = srv
	}
	return secrets
}

// ExpandSecrets walks every server's Env map and replaces each
// `${secret:<name>}` placeholder with its value from the provided map.
// Returns an error listing every missing secret. Values that are not
// placeholder-shaped are left alone (so non-secret env values pass through).
func ExpandSecrets(c *Canonical, secrets map[string]string) error {
	var missing []string
	for serverName, srv := range c.Servers {
		if len(srv.Env) == 0 {
			continue
		}
		for envKey, envVal := range srv.Env {
			m := secretRef.FindStringSubmatch(envVal)
			if m == nil {
				continue
			}
			name := m[1]
			val, ok := secrets[name]
			if !ok {
				missing = append(missing, name)
				continue
			}
			srv.Env[envKey] = val
		}
		c.Servers[serverName] = srv
	}
	if len(missing) > 0 {
		// Sort for a deterministic error message: missing is built from map
		// iteration (over Servers and each Env), whose order is randomized.
		sort.Strings(missing)
		return fmt.Errorf("mcp: missing secrets: %v", missing)
	}
	return nil
}
