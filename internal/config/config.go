// Package config implements the layered configuration architecture
// described in BRD-10 §10 ("the golden rule"). See defaults.toml at
// the repo root for the shipped defaults.
//
// Layer precedence, lowest → highest:
//
//  1. ShippedDefaults — embedded defaults.toml.
//  2. System          — /etc/aplexica/config.toml (Unix) or
//     %PROGRAMDATA%\Aplexica\config.toml (Windows).
//  3. User            — ~/.aplexica/config.toml.
//  4. Project         — <project-root>/.aplexica/config.toml.
//  5. Environment     — APLEXICA_<KEY>=<value> (not yet wired in this
//     package; reserved for v0.68.0+).
//  6. CLI flags       — per-invocation; supplied by the caller.
//
// Each layer is a flat map[string]string of "<section>.<key>" dotted
// keys. Provenance is tracked per-key so `aplexica config show` can
// report which layer set each effective value.
package config

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

// DefaultsTOML is the embedded defaults file. Build pipelines should
// validate it round-trips through `toml.Decode` as part of CI; this
// package's own tests perform that check.
//
//go:embed defaults.toml
var DefaultsTOML []byte

// Layer is one of the named configuration layers. Order matters: the
// numeric value is the precedence (higher overrides lower).
type Layer int

const (
	LayerShipped Layer = 1
	LayerSystem  Layer = 2
	LayerUser    Layer = 3
	LayerProject Layer = 4
	LayerEnv     Layer = 5
	LayerCLI     Layer = 6
)

func (l Layer) String() string {
	switch l {
	case LayerShipped:
		return "shipped"
	case LayerSystem:
		return "system"
	case LayerUser:
		return "user"
	case LayerProject:
		return "project"
	case LayerEnv:
		return "env"
	case LayerCLI:
		return "cli"
	}
	return fmt.Sprintf("unknown(%d)", int(l))
}

// ParseLayer maps a CLI-friendly name back to a Layer. Returns an error
// for unknown names. The "shipped" and "env" / "cli" layers are NOT
// writeable — only system/user/project are user-writeable file layers.
func ParseLayer(s string) (Layer, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "system":
		return LayerSystem, nil
	case "user":
		return LayerUser, nil
	case "project":
		return LayerProject, nil
	}
	return 0, fmt.Errorf("unknown layer %q (expected: system|user|project)", s)
}

// Effective is the merged configuration with per-key provenance.
type Effective struct {
	// Keys are dotted "<section>.<key>" strings, e.g. "daemon.project_scan_interval".
	Values     map[string]string
	Provenance map[string]Layer
}

// Get returns the effective value for a dotted key, plus the layer that
// set it. ok=false if the key is not present in any layer.
func (e *Effective) Get(key string) (value string, layer Layer, ok bool) {
	v, ok := e.Values[key]
	if !ok {
		return "", 0, false
	}
	return v, e.Provenance[key], true
}

// Keys returns the sorted set of all effective keys.
func (e *Effective) Keys() []string {
	out := make([]string, 0, len(e.Values))
	for k := range e.Values {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Source bundles a layer with its on-disk path (empty for the shipped
// and env/cli layers, which don't have file paths).
type Source struct {
	Layer Layer
	Path  string
}

// LoadOptions controls which layers Load consults.
//
// File layers (system / user / project) are consulted when their path
// is non-empty; missing files are silently skipped (only parse errors
// surface). The platform defaults are exposed via DefaultSources.
//
// Env (layer 5, BRD-10 §10.1) and CLIOverrides (layer 6) are non-nil
// when the caller wants them folded in. Env entries are the standard
// `KEY=VALUE` shape produced by os.Environ(); only keys with the
// APLEXICA_ prefix are inspected. CLIOverrides are dotted keys, e.g.
// "daemon.project_scan_interval=30m", typically populated from a
// repeatable `--config-set` flag on the command line.
type LoadOptions struct {
	SystemPath   string
	UserPath     string
	ProjectPath  string
	Env          []string
	CLIOverrides []string
}

// EnvPrefix is the prefix every environment variable that maps onto a
// config key MUST carry. The BRD-10 §10.1 placeholder said
// "SYNLEXICA_<KEY>=<value>" which is a typo for "APLEXICA_<KEY>". We
// honor the spirit (a single namespaced prefix) and use the project's
// actual name.
const EnvPrefix = "APLEXICA_"

// DefaultSources returns the standard platform-specific paths for the
// system, user, and project layers. The Project path is "" by default
// because project-layer activation depends on the daemon's current
// working context (BRD-02 §4.13) — callers supply it when they have
// one. Empty paths in the returned slice are skipped on Load.
func DefaultSources() (system, user, project string) {
	user = userConfigPath()
	system = systemConfigPath()
	return system, user, ""
}

// Load returns the effective merged configuration across all
// available file layers. Layers whose path is empty are skipped.
// Missing files (the common case for system / project layers) are
// silently skipped — only parse errors are returned.
func Load(opts LoadOptions) (*Effective, error) {
	eff := &Effective{
		Values:     map[string]string{},
		Provenance: map[string]Layer{},
	}

	if err := applyLayer(eff, LayerShipped, "<embedded>", DefaultsTOML); err != nil {
		return nil, fmt.Errorf("config: shipped defaults: %w", err)
	}

	for _, ls := range []struct {
		layer Layer
		path  string
	}{
		{LayerSystem, opts.SystemPath},
		{LayerUser, opts.UserPath},
		{LayerProject, opts.ProjectPath},
	} {
		if ls.path == "" {
			continue
		}
		body, err := os.ReadFile(ls.path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("config: read %s layer %s: %w", ls.layer, ls.path, err)
		}
		if err := applyLayer(eff, ls.layer, ls.path, body); err != nil {
			return nil, fmt.Errorf("config: %s layer %s: %w", ls.layer, ls.path, err)
		}
	}

	// Layer 5 — environment variables. Env entries shaped APLEXICA_<KEY>
	// are mapped to dotted-key config entries. The reverse-mapping helper
	// (dottedFromEnv) consults the schema so it knows where the section
	// boundary lives in keys whose field names contain underscores.
	if len(opts.Env) > 0 {
		applyEnv(eff, opts.Env)
	}

	// Layer 6 — CLI flag overrides. Highest precedence; last write wins.
	for _, kv := range opts.CLIOverrides {
		k, v, ok := splitKV(kv)
		if !ok {
			return nil, fmt.Errorf("config: --config-set expected key=value, got %q", kv)
		}
		eff.Values[k] = v
		eff.Provenance[k] = LayerCLI
	}

	return eff, nil
}

// splitKV splits a "key=value" string on the FIRST '=' so values can
// contain '='. Returns ok=false on missing '='.
func splitKV(s string) (string, string, bool) {
	idx := strings.IndexByte(s, '=')
	if idx <= 0 {
		return "", "", false
	}
	return s[:idx], s[idx+1:], true
}

// applyEnv walks env entries (the os.Environ() shape "KEY=VALUE") and
// folds APLEXICA_<KEY> variables into eff. Mapping: drop the APLEXICA_
// prefix and lowercase the body, then resolve it against the snake-cased
// forms of the ACTUAL schema keys (snakeFromDotted maps each dotted key,
// e.g. "retention.snapshot_after_events.conversation", to its env body
// "retention_snapshot_after_events_conversation"). The longest / most
// specific schema match wins, so a 3-segment dotted key is reachable —
// the env→key mapping can't collapse the second '.' into an underscore
// (BRD-10 §10.1: env is first-class for every tunable).
//
// If the body matches no schema key but still starts with a known
// <section>_ prefix, we fold it in under "<section>.<field>" so the
// schema's unknown-key warning fires (BRD-10 §10.2) rather than silently
// swallowing a likely env typo. A body that doesn't even start with a
// known section is ignored — that keeps unrelated APLEXICA_* env vars
// (test-runner toggles, etc.) from polluting effective config.
func applyEnv(eff *Effective, env []string) {
	snakeToDotted := snakeFromDotted()
	knownSections := knownSchemaSections()
	for _, kv := range env {
		k, v, ok := splitKV(kv)
		if !ok {
			continue
		}
		if !strings.HasPrefix(k, EnvPrefix) {
			continue
		}
		body := strings.ToLower(strings.TrimPrefix(k, EnvPrefix))

		// 1. Exact match against a snake-cased schema key (handles any
		//    number of dotted segments).
		if dotted, ok := snakeToDotted[body]; ok {
			eff.Values[dotted] = v
			eff.Provenance[dotted] = LayerEnv
			continue
		}

		// 2. No schema key matched. Fall back to the longest known section
		//    prefix so the leftover becomes the field name and the
		//    unknown-key warning path stays reachable.
		bestSection := ""
		for section := range knownSections {
			if !strings.HasPrefix(body, section+"_") {
				continue
			}
			if len(section) > len(bestSection) {
				bestSection = section
			}
		}
		if bestSection == "" {
			continue
		}
		field := strings.TrimPrefix(body, bestSection+"_")
		if field == "" {
			continue
		}
		eff.Values[bestSection+"."+field] = v
		eff.Provenance[bestSection+"."+field] = LayerEnv
	}
}

// snakeFromDotted builds the reverse-mapping applyEnv needs: each dotted
// schema key (e.g. "retention.snapshot_after_events.conversation") mapped
// from its env-body form with every '.' replaced by '_'
// ("retention_snapshot_after_events_conversation"). Because the lookup is
// against complete schema keys, the section boundary is implicit and a
// key with any number of dotted segments round-trips unambiguously.
func snakeFromDotted() map[string]string {
	out := make(map[string]string, len(Schema))
	for _, e := range Schema {
		out[strings.ReplaceAll(e.Key, ".", "_")] = e.Key
	}
	return out
}

// applyLayer parses a TOML blob and overlays it onto eff, attributing
// every key to layer. Nested tables are flattened to dotted keys.
func applyLayer(eff *Effective, layer Layer, source string, body []byte) error {
	var raw map[string]any
	if _, err := toml.Decode(string(body), &raw); err != nil {
		return fmt.Errorf("parse: %w", err)
	}
	flatten("", raw, func(k, v string) {
		eff.Values[k] = v
		eff.Provenance[k] = layer
	})
	_ = source // reserved for future error context
	return nil
}

// flatten walks a TOML-decoded nested map and invokes emit(key, value)
// for every leaf, joining nested table names with '.'.
func flatten(prefix string, m map[string]any, emit func(k, v string)) {
	for k, v := range m {
		key := k
		if prefix != "" {
			key = prefix + "." + k
		}
		switch val := v.(type) {
		case map[string]any:
			flatten(key, val, emit)
		default:
			emit(key, scalarString(val))
		}
	}
}

// scalarString renders a TOML scalar (string, int, float, bool, time)
// or a homogeneous array as a stable string representation. We don't
// round-trip rich types through provenance — config is fundamentally
// a string surface for the CLI.
func scalarString(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case bool:
		if x {
			return "true"
		}
		return "false"
	case []any:
		parts := make([]string, 0, len(x))
		for _, e := range x {
			parts = append(parts, scalarString(e))
		}
		return "[" + strings.Join(parts, ", ") + "]"
	default:
		return fmt.Sprintf("%v", x)
	}
}

// systemConfigPath returns the conventional system-layer config path
// per BRD-10 §10.1 / 10.5.
func systemConfigPath() string {
	if pd := os.Getenv("PROGRAMDATA"); pd != "" {
		return filepath.Join(pd, "Aplexica", "config.toml")
	}
	return "/etc/aplexica/config.toml"
}

// userConfigPath returns the conventional user-layer config path.
func userConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".aplexica", "config.toml")
}

// SetKey writes `key = value` into the named layer's TOML file. If the
// key exists, its value is updated; otherwise it's appended at the end
// of the file (preserving any existing user formatting / comments).
// The file is created with 0o644 if missing.
//
// Note: this is intentionally a line-oriented in-place edit, not a
// re-marshal of the whole document. Re-marshaling would lose user
// comments and rearrange key order. The lightweight in-place approach
// keeps user-edited files diff-friendly.
func SetKey(path, key, value string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("config: mkdir %s: %w", filepath.Dir(path), err)
	}

	// Validate: key must be section.field, both lowercase + non-empty.
	parts := strings.SplitN(key, ".", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return fmt.Errorf("config: key must be <section>.<field>, got %q", key)
	}
	section, field := parts[0], parts[1]

	var existing []byte
	if b, err := os.ReadFile(path); err == nil {
		existing = b
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("config: read %s: %w", path, err)
	}

	updated := upsertTOML(string(existing), section, field, value)
	if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
		return fmt.Errorf("config: write %s: %w", path, err)
	}
	return nil
}

// UnsetKey removes a key from the named layer's TOML file. Missing key
// is treated as a no-op (idempotent). Missing file is also a no-op.
func UnsetKey(path, key string) error {
	parts := strings.SplitN(key, ".", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return fmt.Errorf("config: key must be <section>.<field>, got %q", key)
	}
	section, field := parts[0], parts[1]

	body, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("config: read %s: %w", path, err)
	}
	updated, removed := removeTOMLKey(string(body), section, field)
	if !removed {
		return nil
	}
	return os.WriteFile(path, []byte(updated), 0o644)
}

// commentIndex returns the byte index of the '#' that begins a trailing
// TOML comment on the line, or -1 if there is none. A '#' inside a basic
// ("...") or literal ('...') string is part of the value, not a comment,
// so it must not be treated as a comment delimiter.
func commentIndex(line string) int {
	inBasic, inLiteral := false, false
	for i := 0; i < len(line); i++ {
		switch c := line[i]; {
		case inBasic:
			if c == '\\' {
				i++ // skip the escaped character
			} else if c == '"' {
				inBasic = false
			}
		case inLiteral:
			if c == '\'' {
				inLiteral = false
			}
		case c == '"':
			inBasic = true
		case c == '\'':
			inLiteral = true
		case c == '#':
			return i
		}
	}
	return -1
}

// upsertTOML produces a TOML document with section.field = value. If
// the section + field already exist, the value is replaced in place
// (preserving comments on the same line, if any). Otherwise the key
// is added under an existing [section] header, or a new [section]
// block is appended at the end of the document.
func upsertTOML(body, section, field, value string) string {
	lines := strings.Split(body, "\n")
	encoded := encodeScalar(value)

	curSection := ""
	keyPat := field + " "
	keyPatEq := field + "="
	inSection := false
	updated := false
	insertAtLine := -1

	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			curSection = strings.TrimSpace(strings.Trim(trimmed, "[]"))
			inSection = curSection == section
			if inSection {
				insertAtLine = i // start of section header
			}
			continue
		}
		if !inSection {
			continue
		}
		// Look for "<field>" as first token of the trimmed line.
		if strings.HasPrefix(trimmed, keyPat) || strings.HasPrefix(trimmed, keyPatEq) ||
			trimmed == field {
			// Replace this line, preserving any trailing comment.
			comment := ""
			if idx := commentIndex(line); idx >= 0 {
				comment = " " + strings.TrimSpace(line[idx:])
			}
			indent := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
			lines[i] = indent + field + " = " + encoded + comment
			updated = true
			break
		}
		insertAtLine = i // last seen line within section
	}

	if updated {
		return strings.Join(lines, "\n")
	}

	if insertAtLine >= 0 {
		// Section exists; insert after the last line we saw inside it.
		newLines := append([]string{}, lines[:insertAtLine+1]...)
		newLines = append(newLines, field+" = "+encoded)
		newLines = append(newLines, lines[insertAtLine+1:]...)
		return strings.Join(newLines, "\n")
	}

	// Section doesn't exist; append a new block at the end. Ensure a
	// trailing newline before the new section so we don't merge into
	// the prior line.
	tail := "\n[" + section + "]\n" + field + " = " + encoded + "\n"
	if body == "" {
		return strings.TrimLeft(tail, "\n")
	}
	if !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	return body + tail
}

// removeTOMLKey strips the named field from its section. Returns the
// updated body and a bool indicating whether anything was removed.
func removeTOMLKey(body, section, field string) (string, bool) {
	lines := strings.Split(body, "\n")
	curSection := ""
	keyPat := field + " "
	keyPatEq := field + "="
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			curSection = strings.TrimSpace(strings.Trim(trimmed, "[]"))
			continue
		}
		if curSection != section {
			continue
		}
		if strings.HasPrefix(trimmed, keyPat) || strings.HasPrefix(trimmed, keyPatEq) ||
			trimmed == field {
			lines = append(lines[:i], lines[i+1:]...)
			return strings.Join(lines, "\n"), true
		}
	}
	return body, false
}

// encodeScalar formats a string value as TOML. Strings get quoted;
// bools/numbers/arrays pass through as-is. The CLI surface accepts
// raw strings from the user; this helper decides whether to wrap
// them in quotes. The rule is intentionally simple: if the value
// parses as a bare TOML scalar (true/false/number) or is already a
// bracketed array, leave it alone; otherwise quote it.
func encodeScalar(v string) string {
	s := strings.TrimSpace(v)
	switch s {
	case "true", "false":
		return s
	}
	// Numbers (decimal int, float, negative, scientific).
	if isTOMLNumeric(s) {
		return s
	}
	if strings.HasPrefix(s, "[") && strings.HasSuffix(s, "]") {
		return s
	}
	// Already quoted? Trust the user.
	if strings.HasPrefix(s, `"`) && strings.HasSuffix(s, `"`) {
		return s
	}
	if strings.HasPrefix(s, `'`) && strings.HasSuffix(s, `'`) {
		return s
	}
	// Escape backslashes and double quotes; basic TOML string.
	esc := strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s)
	return `"` + esc + `"`
}

func isTOMLNumeric(s string) bool {
	if s == "" {
		return false
	}
	// Permissive: starts with a digit or sign, contains only digits, '.', 'e', 'E', '_', '+', '-'.
	start := s[0]
	if !(start == '-' || start == '+' || (start >= '0' && start <= '9')) {
		return false
	}
	for _, r := range s[1:] {
		switch {
		case r >= '0' && r <= '9':
		case r == '.' || r == 'e' || r == 'E' || r == '_' || r == '+' || r == '-':
		default:
			return false
		}
	}
	return true
}

// Validate parses a TOML body and then range-checks every key against
// the schema (FR-10.9 / FR-10.10). Returns an error joining all
// validation failures; warnings (unknown keys, per BRD-10 §10.2) are
// emitted via the second return on the structured ValidateBody form,
// not here — callers that want warnings should use ValidateBody.
func Validate(body []byte) error {
	errs, _, err := ValidateBody(body)
	if err != nil {
		return err
	}
	if len(errs) > 0 {
		return fmt.Errorf("config: validate failed:\n  - %s", strings.Join(errs, "\n  - "))
	}
	return nil
}

// ValidateBody is the structured form of Validate. It parses the TOML
// body, applies just-this-file as a synthetic layer onto an empty
// Effective, then range-checks via SchemaValidate. Returns (errors,
// warnings, parseError). Designed for `aplexica config validate <file>`
// + future config-edit re-validation hooks.
func ValidateBody(body []byte) ([]string, []string, error) {
	var raw map[string]any
	if _, err := toml.Decode(string(body), &raw); err != nil {
		return nil, nil, fmt.Errorf("config: validate parse: %w", err)
	}
	eff := &Effective{
		Values:     map[string]string{},
		Provenance: map[string]Layer{},
	}
	flatten("", raw, func(k, v string) {
		eff.Values[k] = v
		eff.Provenance[k] = LayerShipped
	})
	errs, warns := SchemaValidate(eff)
	errs = append(errs, validateCrossField(eff)...)
	sort.Strings(errs)
	return errs, warns, nil
}

// validateCrossField enforces the cross-field invariants the per-key
// SchemaValidate cannot express, returning one human-readable error per
// violation (empty slice = ok). It exists because some constraints span
// two keys whose individual ranges are each satisfied.
//
// The retention emergency-quota / high-watermark ordering (BRD-03 §4.8.4)
// is asserted here and NOT delegated to internal/retention.Config.Validate:
// internal/retention imports internal/config, so importing it back would
// create an import cycle. The predicate is therefore inlined; keep it in
// sync with retention.Config.Validate's wording.
func validateCrossField(eff *Effective) []string {
	// float64BitSize is the bit size strconv.ParseFloat uses for the
	// fraction-valued retention tunables (named to avoid a bare-literal
	// magic number; FR-10.6).
	const float64BitSize = 64

	var errs []string

	q, qok := eff.Values["retention.store_emergency_quota"]
	w, wok := eff.Values["retention.store_high_watermark"]
	if qok && wok {
		qf, qerr := strconv.ParseFloat(q, float64BitSize)
		wf, werr := strconv.ParseFloat(w, float64BitSize)
		// Only assert ordering when both parse; malformed values are
		// already reported by the per-key float check in SchemaValidate.
		if qerr == nil && werr == nil && qf < wf {
			errs = append(errs, fmt.Sprintf(
				"retention.store_emergency_quota (%s) must be >= store_high_watermark (%s)", q, w))
		}
	}

	return errs
}

// DefaultsEffective returns just the shipped-defaults layer as an
// Effective, useful for `aplexica config diff`.
func DefaultsEffective() (*Effective, error) {
	eff := &Effective{
		Values:     map[string]string{},
		Provenance: map[string]Layer{},
	}
	if err := applyLayer(eff, LayerShipped, "<embedded>", DefaultsTOML); err != nil {
		return nil, err
	}
	return eff, nil
}
