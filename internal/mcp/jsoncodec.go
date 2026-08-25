package mcp

import (
	"encoding/json"
	"fmt"
)

// FromMCPJSON parses a native claude-code .mcp.json byte slice into a
// Canonical. The wire shape is {"mcpServers": {<name>: <server-config>}}.
// Env blocks are pulled out into the typed Env map; all other server
// fields are preserved verbatim under Other.
//
// Input without a "mcpServers" top-level field is valid and produces an
// empty Canonical (consistent with the legacy claudecode adapter behavior).
func FromMCPJSON(raw []byte) (Canonical, error) {
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		return Canonical{}, fmt.Errorf("mcp: parse .mcp.json: %w", err)
	}
	serversAny, ok := root["mcpServers"]
	if !ok {
		return Canonical{Servers: map[string]Server{}}, nil
	}
	serversMap, ok := serversAny.(map[string]any)
	if !ok {
		return Canonical{}, fmt.Errorf("mcp: mcpServers is not an object")
	}
	out := Canonical{Servers: make(map[string]Server, len(serversMap))}
	for name, sv := range serversMap {
		smap, ok := sv.(map[string]any)
		if !ok {
			continue
		}
		out.Servers[name] = splitEnv(smap)
	}
	return out, nil
}

// ToMCPJSON emits a Canonical as native claude-code .mcp.json bytes. The
// wire shape is {"mcpServers": {<name>: <server-config>}} with the Env
// map merged back under the "env" key on each server.
func ToMCPJSON(c Canonical) ([]byte, error) {
	servers := make(map[string]any, len(c.Servers))
	for name, srv := range c.Servers {
		servers[name] = mergeEnv(srv)
	}
	out := map[string]any{"mcpServers": servers}
	b, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("mcp: emit .mcp.json: %w", err)
	}
	return b, nil
}

// envNonStringKey is a reserved Other key under which splitEnv/splitEnvKilo
// stash env entries whose value is NOT a string (int/bool/float — common for
// unquoted YAML/TOML/JSON5 scalars). The typed Env map only carries strings
// (so secret extraction, which keys off Env, never touches non-string values),
// while these preserved entries are re-nested back under the env block on
// export. The key is namespaced to avoid colliding with any real server field
// and never reaches a native config file.
const envNonStringKey = "__aplexica_env_nonstring"

// splitEnv lifts a server's "env" submap (if any) into the typed Env field
// and leaves everything else under Other. String env values go into the typed
// Env map; non-string values are preserved verbatim under Other so they
// survive the round trip (see envNonStringKey).
func splitEnv(smap map[string]any) Server {
	srv := Server{Other: map[string]any{}}
	for k, v := range smap {
		if k == "env" {
			if envMap, ok := v.(map[string]any); ok {
				splitEnvMap(&srv, envMap)
			}
			continue
		}
		srv.Other[k] = v
	}
	if len(srv.Other) == 0 {
		srv.Other = nil
	}
	return srv
}

// splitEnvMap partitions a native env map into the typed string Env field and
// the non-string preservation map under Other[envNonStringKey].
func splitEnvMap(srv *Server, envMap map[string]any) {
	srv.Env = make(map[string]string, len(envMap))
	var nonString map[string]any
	for ek, ev := range envMap {
		if s, ok := ev.(string); ok {
			srv.Env[ek] = s
			continue
		}
		if nonString == nil {
			nonString = map[string]any{}
		}
		nonString[ek] = ev
	}
	if len(nonString) > 0 {
		srv.Other[envNonStringKey] = nonString
	}
}

// mergeEnv is the inverse of splitEnv: merges the typed string Env and any
// preserved non-string env entries back into a single env map suitable for
// native JSON/TOML emission.
func mergeEnv(srv Server) map[string]any {
	out := make(map[string]any, len(srv.Other)+1)
	for k, v := range srv.Other {
		if k == envNonStringKey {
			continue
		}
		out[k] = v
	}
	if envMap := mergeEnvMap(srv); envMap != nil {
		out["env"] = envMap
	}
	return out
}

// mergeEnvMap reconstructs the native env map from the typed string Env and
// the preserved non-string entries. Returns nil when there is nothing to emit.
func mergeEnvMap(srv Server) map[string]any {
	nonString, _ := srv.Other[envNonStringKey].(map[string]any)
	if len(srv.Env) == 0 && len(nonString) == 0 {
		return nil
	}
	envMap := make(map[string]any, len(srv.Env)+len(nonString))
	for ek, ev := range srv.Env {
		envMap[ek] = ev
	}
	for ek, ev := range nonString {
		envMap[ek] = ev
	}
	return envMap
}
