package mcp

import (
	"encoding/json"
	"fmt"
)

// FromKiloJSONC parses Kilo Code's native kilo.jsonc byte slice into a
// Canonical. Handles the four Kilo-specific quirks:
//
//   - Top-level wrapper key is "mcp" (not "mcpServers" / "mcp_servers")
//   - Per-server env block is named "environment" (not "env")
//   - Per-server "command" is a single array [bin, ...args] (not a string + separate args)
//   - Per-server "type" uses Kilo names: "local" (not "stdio"), "remote" (not "http").
//     The legacy value "streamable-http" is still accepted on input.
//
// JSONC comments are stripped before JSON parsing. Unknown fields not
// listed above are preserved verbatim under Server.Other so vendor-specific
// settings (enabled, timeout, alwaysAllow, disabled, etc.) survive same-
// adapter round trips and pass through cross-adapter exports.
func FromKiloJSONC(raw []byte) (Canonical, error) {
	cleaned := StripComments(raw)
	var root map[string]any
	if err := json.Unmarshal(cleaned, &root); err != nil {
		return Canonical{}, fmt.Errorf("mcp: parse kilo.jsonc: %w", err)
	}
	serversAny, ok := root["mcp"]
	if !ok {
		return Canonical{Servers: map[string]Server{}}, nil
	}
	serversMap, ok := serversAny.(map[string]any)
	if !ok {
		return Canonical{}, fmt.Errorf("mcp: kilo 'mcp' field is not an object")
	}
	out := Canonical{Servers: make(map[string]Server, len(serversMap))}
	for name, sv := range serversMap {
		smap, ok := sv.(map[string]any)
		if !ok {
			continue
		}
		out.Servers[name] = splitEnvKilo(smap)
	}
	return out, nil
}

// ToKiloJSONC emits a Canonical as native Kilo kilo.jsonc bytes. Comments
// are not produced (the output is plain JSON that parses as valid JSONC).
func ToKiloJSONC(c Canonical) ([]byte, error) {
	servers := make(map[string]any, len(c.Servers))
	for name, srv := range c.Servers {
		servers[name] = mergeEnvKilo(srv)
	}
	out := map[string]any{"mcp": servers}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("mcp: emit kilo.jsonc: %w", err)
	}
	return b, nil
}

// splitEnvKilo lifts "environment" to Server.Env, splits command-array into
// command+args, renames Kilo type values to canonical names. Everything else
// passes through under Other.
func splitEnvKilo(smap map[string]any) Server {
	srv := Server{Other: map[string]any{}}
	for k, v := range smap {
		switch k {
		case "environment":
			if envMap, ok := v.(map[string]any); ok {
				splitEnvMap(&srv, envMap)
			}
		case "command":
			if arr, ok := v.([]any); ok && len(arr) > 0 {
				if first, ok := arr[0].(string); ok {
					srv.Other["command"] = first
					if len(arr) > 1 {
						srv.Other["args"] = arr[1:]
					}
				}
			} else if s, ok := v.(string); ok {
				srv.Other["command"] = s
			}
		case "type":
			if s, ok := v.(string); ok {
				srv.Other["type"] = kiloToCanonicalType(s)
			}
		default:
			srv.Other[k] = v
		}
	}
	if len(srv.Other) == 0 {
		srv.Other = nil
	}
	return srv
}

// mergeEnvKilo is the inverse of splitEnvKilo.
func mergeEnvKilo(srv Server) map[string]any {
	out := make(map[string]any, len(srv.Other)+2)
	for k, v := range srv.Other {
		switch k {
		case "args":
		case "command":
		case envNonStringKey:
			// Re-nested into the environment block below, never emitted raw.
		case "type":
			if s, ok := v.(string); ok {
				out["type"] = canonicalToKiloType(s)
			} else {
				out["type"] = v
			}
		default:
			out[k] = v
		}
	}

	// Reconstruct Kilo's single command-array [bin, ...args] whenever EITHER a
	// command or args is present. Starting the array from the command string
	// (when present, else empty) ensures args never disappear when a canonical
	// server carries args without a command — matching the claudecode/codex
	// exporters, which preserve such args verbatim.
	arr := []any{}
	if cmdAny, ok := srv.Other["command"]; ok {
		cmdStr, _ := cmdAny.(string)
		arr = append(arr, cmdStr)
	}
	if argsAny, ok := srv.Other["args"]; ok {
		if argsSlice, ok := argsAny.([]any); ok {
			arr = append(arr, argsSlice...)
		}
	}
	if len(arr) > 0 {
		out["command"] = arr
		// Current Kilo validates every command-backed MCP entry against its
		// local-server schema. Cross-adapter sources such as Claude's
		// .mcp.json legitimately omit both transport type and enabled, but
		// emitting that shape verbatim makes the entire kilo.jsonc invalid and
		// prevents unrelated operations such as `kilo import` from starting.
		// Supply only Kilo's required native defaults; explicit values (most
		// importantly enabled:false) remain authoritative.
		if _, exists := out["type"]; !exists {
			out["type"] = canonicalToKiloType("stdio")
		}
		if _, exists := out["enabled"]; !exists {
			out["enabled"] = true
		}
	}

	if envMap := mergeEnvMap(srv); envMap != nil {
		out["environment"] = envMap
	}
	return out
}

// kiloToCanonicalType maps Kilo's per-server transport names to canonical ones.
// Current Kilo Code uses "local"/"remote" (kilo.ai/docs/automate/mcp);
// "streamable-http" is a legacy value still accepted on input for back-compat.
func kiloToCanonicalType(t string) string {
	switch t {
	case "local":
		return "stdio"
	case "remote", "streamable-http":
		return "http"
	}
	return t
}

// canonicalToKiloType is the inverse, emitting Kilo's CURRENT value ("remote",
// not the legacy "streamable-http") so exported kilo.jsonc is recognized by
// present-day Kilo Code.
func canonicalToKiloType(t string) string {
	switch t {
	case "stdio":
		return "local"
	case "http":
		return "remote"
	}
	return t
}
