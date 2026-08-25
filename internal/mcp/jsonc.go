package mcp

import "bytes"

// StripComments removes JSONC-style comments (`// line` and `/* block */`)
// from a JSON-with-comments byte slice, returning a byte slice that the
// stdlib `encoding/json` package can parse. The stripper is string-aware:
// comment-like sequences inside JSON string literals (e.g. a URL containing
// `//`) are preserved verbatim. Backslash-escaped quotes are honored as
// part of the string.
//
// An unterminated block comment is treated as continuing to EOF (rather than
// erroring); this matches the behavior of common JSONC parsers and keeps
// the API panic-free for malformed input.
func StripComments(raw []byte) []byte {
	var out bytes.Buffer
	out.Grow(len(raw))

	const (
		stNormal = iota
		stString
		stLineComment
		stBlockComment
	)
	state := stNormal

	for i := 0; i < len(raw); i++ {
		c := raw[i]
		switch state {
		case stNormal:
			if c == '"' {
				out.WriteByte(c)
				state = stString
				continue
			}
			if c == '/' && i+1 < len(raw) {
				next := raw[i+1]
				if next == '/' {
					state = stLineComment
					i++
					continue
				}
				if next == '*' {
					state = stBlockComment
					i++
					continue
				}
			}
			out.WriteByte(c)

		case stString:
			out.WriteByte(c)
			if c == '\\' && i+1 < len(raw) {
				out.WriteByte(raw[i+1])
				i++
				continue
			}
			if c == '"' {
				state = stNormal
			}

		case stLineComment:
			if c == '\n' {
				out.WriteByte(c)
				state = stNormal
			}

		case stBlockComment:
			if c == '*' && i+1 < len(raw) && raw[i+1] == '/' {
				state = stNormal
				i++
			}
		}
	}
	return out.Bytes()
}

// stripTrailingCommas removes a comma that immediately precedes a closing
// `}` or `]` (ignoring intervening whitespace). String-aware: commas inside
// JSON string literals are preserved. Intended to run AFTER StripComments so
// that JSON5/JSONC inputs (which permit trailing commas — e.g. OpenClaw's
// json5.parse) become parseable by stdlib encoding/json.
func stripTrailingCommas(raw []byte) []byte {
	var out bytes.Buffer
	out.Grow(len(raw))
	inString := false
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if inString {
			out.WriteByte(c)
			if c == '\\' && i+1 < len(raw) {
				out.WriteByte(raw[i+1])
				i++
				continue
			}
			if c == '"' {
				inString = false
			}
			continue
		}
		if c == '"' {
			inString = true
			out.WriteByte(c)
			continue
		}
		if c == ',' {
			j := i + 1
			for j < len(raw) && (raw[j] == ' ' || raw[j] == '\t' || raw[j] == '\n' || raw[j] == '\r') {
				j++
			}
			if j < len(raw) && (raw[j] == '}' || raw[j] == ']') {
				continue // drop the trailing comma; following whitespace is emitted normally
			}
		}
		out.WriteByte(c)
	}
	return out.Bytes()
}
