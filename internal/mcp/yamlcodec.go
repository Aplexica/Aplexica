package mcp

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// FromHermesYAML parses Hermes's config.yaml byte slice into a Canonical.
// The wire shape is the `mcp_servers:` section of `~/.hermes/config.yaml`;
// non-mcp_servers content is intentionally dropped — Hermes's tool
// artifact carries only the MCP server configuration (matching the
// codex pattern for config.toml).
//
// Per-server field names match the standard MCP convention (command,
// args, env, url, headers) so no rename is needed — env is lifted into
// Server.Env and everything else passes through under Server.Other.
func FromHermesYAML(raw []byte) (Canonical, error) {
	var root map[string]any
	if err := yaml.Unmarshal(raw, &root); err != nil {
		return Canonical{}, fmt.Errorf("mcp: parse hermes YAML: %w", err)
	}
	serversAny, ok := root["mcp_servers"]
	if !ok {
		return Canonical{Servers: map[string]Server{}}, nil
	}
	serversMap, ok := serversAny.(map[string]any)
	if !ok {
		return Canonical{}, fmt.Errorf("mcp: hermes mcp_servers is not a mapping")
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

// ToHermesYAML emits a Canonical as Hermes's config.yaml byte slice. The
// output contains only the `mcp_servers:` section; users who maintain
// other Hermes config (memory_enabled, plugin settings, etc.) must
// preserve those sections outside Aplexica until a "partial-file edit"
// mode lands.
func ToHermesYAML(c Canonical) ([]byte, error) {
	servers := make(map[string]any, len(c.Servers))
	for name, srv := range c.Servers {
		servers[name] = mergeEnv(srv)
	}
	wrapper := map[string]any{"mcp_servers": servers}
	out, err := yaml.Marshal(wrapper)
	if err != nil {
		return nil, fmt.Errorf("mcp: emit hermes YAML: %w", err)
	}
	return out, nil
}
