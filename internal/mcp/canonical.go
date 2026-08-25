// Package mcp defines the canonical schema for MCP (Model Context Protocol)
// server configurations used inside Aplexica's tool artifacts. Both per-agent
// adapters (claudecode JSON, codex TOML) parse their native format into this
// schema on Import and emit their native format from this schema on Export.
// This makes tool artifacts cross-adapter portable: an artifact imported via
// claudecode can be exported as codex's native TOML and vice versa.
//
// The wire format stored in ToolPayload.Content is JSON-encoded Canonical
// with ToolPayload.Format = "acf.mcp.v1".
package mcp

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// Format is the Format-string written into ToolPayload.Format for v0.3.0+
// tool artifacts. Adapters refuse to export artifacts whose Format does not
// match this constant — pre-v0.3.0 artifacts must be re-imported.
const Format = "acf.mcp.v1"

// Canonical is the top-level canonical MCP-tool-config schema.
type Canonical struct {
	Servers map[string]Server `json:"servers"`
}

// Server is a single MCP server's configuration in canonical form.
//
//   - Env captures the env block as a typed map. Adapters extract env values
//     into the secrets store on Import (so secrets never reach the canonical
//     store, per ADR-0027) and replace them with `${secret:<server>.<key>}`
//     reference strings here. On Export, the references are expanded back
//     using the secrets store before native emission.
//   - Other captures every other field on the server config (type, command,
//     args, url, headers, timeout, etc.) verbatim. Both JSON and TOML formats
//     can represent the same map[string]any shape, so cross-adapter round
//     trip works for any value type both formats support.
type Server struct {
	Env   map[string]string `json:"env,omitempty"`
	Other map[string]any    `json:"other,omitempty"`
}

// Encode marshals a Canonical to its wire-format JSON bytes.
func Encode(c Canonical) ([]byte, error) {
	b, err := json.Marshal(c)
	if err != nil {
		return nil, fmt.Errorf("mcp: encode canonical: %w", err)
	}
	return b, nil
}

// Decode parses wire-format JSON bytes back into a Canonical. Unknown
// top-level fields cause a parse error; this is intentional (a forward-incompat
// schema bump should fail loudly rather than silently drop data).
func Decode(raw []byte) (Canonical, error) {
	var c Canonical
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return c, fmt.Errorf("mcp: decode canonical: %w", err)
	}
	return c, nil
}
