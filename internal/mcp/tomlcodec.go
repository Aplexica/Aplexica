package mcp

import (
	"bytes"
	"fmt"

	"github.com/BurntSushi/toml"
)

// FromMCPTOML parses a native codex config.toml byte slice into a Canonical.
// The wire shape is `[mcp_servers.<name>]` tables. Env blocks are pulled out
// into the typed Env map; all other server fields are preserved under Other.
//
// Non-mcp_servers top-level keys (model, plugins, marketplaces, etc.) are
// intentionally dropped — tool artifacts carry only the mcp_servers config,
// matching the legacy codex adapter's behavior.
func FromMCPTOML(raw []byte) (Canonical, error) {
	var root map[string]any
	if err := toml.Unmarshal(raw, &root); err != nil {
		return Canonical{}, fmt.Errorf("mcp: parse codex TOML: %w", err)
	}
	serversAny, ok := root["mcp_servers"]
	if !ok {
		return Canonical{Servers: map[string]Server{}}, nil
	}
	serversMap, ok := serversAny.(map[string]any)
	if !ok {
		return Canonical{}, fmt.Errorf("mcp: mcp_servers is not a table")
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

// ToMCPTOML emits a Canonical as native codex config.toml bytes. The
// wire shape is `[mcp_servers.<name>]` tables. The Env map is merged
// back under the "env" key on each server.
func ToMCPTOML(c Canonical) ([]byte, error) {
	servers := make(map[string]any, len(c.Servers))
	for name, srv := range c.Servers {
		servers[name] = mergeEnv(srv)
	}
	wrapper := map[string]any{"mcp_servers": servers}
	var buf bytes.Buffer
	enc := toml.NewEncoder(&buf)
	if err := enc.Encode(wrapper); err != nil {
		return nil, fmt.Errorf("mcp: emit codex TOML: %w", err)
	}
	return buf.Bytes(), nil
}
