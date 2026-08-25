// Package skilldialect implements ADR-0043: deterministic, bijective
// translation of the Agent Skills `allowed-tools` frontmatter field between
// each agent's native tool vocabulary and a canonical, agent-neutral token
// namespace (`acf.<name>`).
//
// The skill BODY and every other frontmatter field are never touched — the
// only machine-interpreted, deterministically-breaking surface in cross-agent
// skill sync is the allowed-tools whitelist (tools are referenced by exact
// name, and each agent names its built-ins differently: claude-code `Bash`,
// codex `exec_command`, kilo `bash`, ...).
//
// Normalize runs on Import (native → canonical, stored form); Denormalize
// runs on Export (canonical → target dialect). Both follow the ADR-0043
// resolution rules: mapped names translate, `acf.*` tokens are
// self-identifying (idempotent normalize; emitted verbatim when the target
// has no mapping, which keeps multi-hop fan-out lossless), and every unknown
// token passes through byte-verbatim. Argument specifiers (`Bash(git:*)`)
// keep their suffix verbatim; only the name part is translated.
//
// Safe-bail rule: only a simple single-line `allowed-tools: <tokens>` entry
// at the top level of the leading YAML frontmatter is transformed. Block
// lists, flow lists, quoted values, comments, duplicate keys, CRLF files,
// missing/unclosed frontmatter — anything else returns the input
// byte-verbatim. A bail can only ever mean "not translated", never
// "corrupted".
package skilldialect

import "strings"

// canonPrefix marks canonical tokens. The dot separator keeps tokens valid
// YAML plain scalars and unambiguous against every observed native name.
const canonPrefix = "acf."

const allowedToolsKey = "allowed-tools:"

// agentMaps returns the per-agent canonical→native tables (ADR-0043 v1).
// Each table must be bijective (1:1 both directions) — same-agent round-trip
// byte-stability depends on it; TestMaps_BijectivePerAgent guards this.
// Only evidence-backed pairs are listed; everything else passes through.
func agentMaps() map[string]map[string]string {
	return map[string]map[string]string{
		"claude-code": {
			"acf.exec":       "Bash",
			"acf.read":       "Read",
			"acf.write":      "Write",
			"acf.edit":       "Edit",
			"acf.grep":       "Grep",
			"acf.glob":       "Glob",
			"acf.web-search": "WebSearch",
			"acf.web-fetch":  "WebFetch",
			"acf.todo":       "TodoWrite",
			"acf.task":       "Task",
		},
		"codex": {
			"acf.exec":       "exec_command",
			"acf.edit":       "apply_patch",
			"acf.web-search": "web_search",
		},
		"kilo": {
			"acf.exec":  "bash",
			"acf.read":  "read",
			"acf.write": "write",
			"acf.edit":  "apply_patch",
			"acf.grep":  "grep",
			"acf.todo":  "todowrite",
			"acf.task":  "task",
		},
		"hermes": {
			"acf.exec":       "exec_command",
			"acf.read":       "read",
			"acf.write":      "write",
			"acf.edit":       "apply_patch",
			"acf.web-search": "web_search",
		},
		"openclaw": {}, // no observed vocabulary — pure passthrough
	}
}

// Normalize rewrites the allowed-tools value from agent's native dialect to
// canonical tokens. Safe-bail: returns content unchanged when the entry isn't
// in simple single-line form (see package doc).
func Normalize(agent string, content []byte) []byte {
	canonToNative := agentMaps()[agent]
	nativeToCanon := make(map[string]string, len(canonToNative))
	for c, n := range canonToNative {
		nativeToCanon[n] = c
	}
	return transformAllowedTools(content, func(name string) string {
		if strings.HasPrefix(name, canonPrefix) {
			return name // already canonical — idempotent
		}
		if c, ok := nativeToCanon[name]; ok {
			return c
		}
		return name // unknown native token: never invent a translation
	})
}

// Denormalize rewrites canonical tokens to agent's native dialect. Canonical
// tokens the agent has no mapping for are emitted verbatim (lossless
// multi-hop); non-canonical tokens pass through.
func Denormalize(agent string, content []byte) []byte {
	canonToNative := agentMaps()[agent]
	return transformAllowedTools(content, func(name string) string {
		if !strings.HasPrefix(name, canonPrefix) {
			return name
		}
		if n, ok := canonToNative[name]; ok {
			return n
		}
		return name // target doesn't know this tool — keep canonical
	})
}

// transformAllowedTools applies mapName to each token name of a simple
// single-line allowed-tools entry in the leading frontmatter block, returning
// content byte-verbatim when any safe-bail condition fires.
func transformAllowedTools(content []byte, mapName func(string) string) []byte {
	s := string(content)
	lines := strings.Split(s, "\n")
	if len(lines) == 0 || lines[0] != "---" {
		return content // no frontmatter (or CRLF) — bail
	}
	closeIdx := -1
	for i := 1; i < len(lines); i++ {
		if lines[i] == "---" {
			closeIdx = i
			break
		}
	}
	if closeIdx < 0 {
		return content // unclosed frontmatter — bail
	}
	atLine := -1
	for i := 1; i < closeIdx; i++ {
		if strings.HasPrefix(lines[i], allowedToolsKey) {
			if atLine >= 0 {
				return content // duplicate key — bail
			}
			atLine = i
		}
	}
	if atLine < 0 {
		return content // no allowed-tools — nothing to do
	}
	raw := lines[atLine][len(allowedToolsKey):]
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || strings.ContainsAny(trimmed, "#") ||
		strings.HasPrefix(trimmed, "[") || strings.HasPrefix(trimmed, "\"") || strings.HasPrefix(trimmed, "'") {
		return content // block/flow list, quoted, comment, empty — bail
	}
	lines[atLine] = allowedToolsKey + mapTokens(raw, mapName)
	return []byte(strings.Join(lines, "\n"))
}

// mapTokens walks raw as alternating space-runs and token-runs, preserving
// every separator byte exactly, and applies mapName to each token's name part
// (the prefix before an optional "(specifier)" suffix, which is carried
// through verbatim).
func mapTokens(raw string, mapName func(string) string) string {
	var b strings.Builder
	i := 0
	for i < len(raw) {
		if raw[i] == ' ' {
			j := i
			for j < len(raw) && raw[j] == ' ' {
				j++
			}
			b.WriteString(raw[i:j])
			i = j
			continue
		}
		j := i
		for j < len(raw) && raw[j] != ' ' {
			j++
		}
		token := raw[i:j]
		name := token
		if p := strings.IndexByte(token, '('); p >= 0 {
			name = token[:p]
		}
		b.WriteString(mapName(name))
		b.WriteString(token[len(name):])
		i = j
	}
	return b.String()
}
