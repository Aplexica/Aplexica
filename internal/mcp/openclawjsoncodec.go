package mcp

import (
	"encoding/json"
	"fmt"
)

// FromOpenClawJSON parses an openclaw.json file's `mcp.servers` section into
// a Canonical. The wire shape is {"mcp": {"servers": {<name>: <server-config>}}}.
//
// Input without a "mcp" or "mcp.servers" key is valid and produces an empty
// Canonical (consistent with FromMCPJSON's tolerant behavior).
//
// The server-config shape is identical to claudecode's .mcp.json
// per-server entries: command/args/env (stdio) OR url/headers
// (streamable-http/sse), plus a `transport` discriminator.
// Per-server parsing is delegated to FromMCPJSON by re-wrapping the
// extracted servers map into the claudecode-shape JSON.
func FromOpenClawJSON(raw []byte) (Canonical, error) {
	// OpenClaw reads its config with json5.parse (clone: src/config/io.ts), so
	// real files may carry comments and trailing commas. Normalize to strict
	// JSON before stdlib parsing — a raw json.Unmarshal hard-fails on them.
	var root map[string]any
	if err := json.Unmarshal(stripTrailingCommas(StripComments(raw)), &root); err != nil {
		return Canonical{}, fmt.Errorf("mcp: parse openclaw.json: %w", err)
	}
	mcpAny, ok := root["mcp"]
	if !ok {
		return Canonical{Servers: map[string]Server{}}, nil
	}
	mcpMap, ok := mcpAny.(map[string]any)
	if !ok {
		return Canonical{}, fmt.Errorf("mcp: openclaw.json `mcp` field is not an object")
	}
	serversAny, ok := mcpMap["servers"]
	if !ok {
		return Canonical{Servers: map[string]Server{}}, nil
	}
	serversMap, ok := serversAny.(map[string]any)
	if !ok {
		return Canonical{}, fmt.Errorf("mcp: openclaw.json `mcp.servers` is not an object")
	}
	// Wrap into the claudecode shape and reuse FromMCPJSON's per-server logic
	// by re-marshalling. Cleaner than duplicating the per-server parser.
	wrapped := map[string]any{"mcpServers": serversMap}
	wrappedJSON, err := json.Marshal(wrapped)
	if err != nil {
		return Canonical{}, fmt.Errorf("mcp: rewrap openclaw servers: %w", err)
	}
	return FromMCPJSON(wrappedJSON)
}

// ToOpenClawJSON serializes a Canonical into an openclaw.json FRAGMENT
// containing only the `mcp.servers` section. Callers that need to preserve
// other top-level keys from an existing openclaw.json should use
// MergeIntoOpenClawJSON instead, which reads the existing file and overlays
// only the servers map.
func ToOpenClawJSON(c Canonical) ([]byte, error) {
	// Produce the claudecode-shape JSON first, then re-nest.
	mcpJSON, err := ToMCPJSON(c)
	if err != nil {
		return nil, err
	}
	var mcpRoot map[string]any
	if err := json.Unmarshal(mcpJSON, &mcpRoot); err != nil {
		return nil, fmt.Errorf("mcp: re-parse claudecode-shape JSON: %w", err)
	}
	servers := mcpRoot["mcpServers"]
	out := map[string]any{
		"mcp": map[string]any{
			"servers": servers,
		},
	}
	return json.MarshalIndent(out, "", "  ")
}

// MergeIntoOpenClawJSON takes an existing openclaw.json byte stream (or
// nil/empty) and writes the canonical's mcp.servers section into it,
// preserving all other top-level keys AND any non-servers fields under mcp.
// The servers map itself is fully replaced (not deep-merged) — each
// Aplexica export is the source of truth for the MCP-server set, and
// per-server fields are preserved verbatim by the canonical schema.
//
// If existing is nil/empty, behaves like ToOpenClawJSON.
func MergeIntoOpenClawJSON(existing []byte, c Canonical) ([]byte, error) {
	if len(existing) == 0 {
		return ToOpenClawJSON(c)
	}
	// Existing file may be user-edited JSON5 (comments / trailing commas).
	var root map[string]any
	if err := json.Unmarshal(stripTrailingCommas(StripComments(existing)), &root); err != nil {
		return nil, fmt.Errorf("mcp: parse existing openclaw.json for merge: %w", err)
	}

	// Produce the fragment to extract its servers payload.
	frag, err := ToOpenClawJSON(c)
	if err != nil {
		return nil, err
	}
	var fragRoot map[string]any
	if err := json.Unmarshal(frag, &fragRoot); err != nil {
		return nil, fmt.Errorf("mcp: re-parse fragment: %w", err)
	}
	fragMCP, _ := fragRoot["mcp"].(map[string]any)
	fragServers, _ := fragMCP["servers"].(map[string]any)

	// Ensure mcp key exists in the merged root.
	mcpAny, mcpPresent := root["mcp"]
	var mcpMap map[string]any
	if mcpPresent {
		var ok bool
		mcpMap, ok = mcpAny.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("mcp: existing openclaw.json `mcp` is not an object")
		}
	} else {
		mcpMap = map[string]any{}
	}
	mcpMap["servers"] = fragServers // full replacement of the servers map
	root["mcp"] = mcpMap

	return json.MarshalIndent(root, "", "  ")
}
