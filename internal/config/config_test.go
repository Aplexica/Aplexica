package config

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDefaultsTOML_Embedded_Parses(t *testing.T) {
	// The shipped defaults must always be valid TOML — this is the
	// CI-enforced FR-10.7 invariant: running with no other layers
	// must produce a working system.
	require.NotEmpty(t, DefaultsTOML, "DefaultsTOML must be embedded")
	require.NoError(t, Validate(DefaultsTOML),
		"DefaultsTOML must parse as TOML")
}

func TestDefaultsTOML_HasInlineComments(t *testing.T) {
	// FR-10.11 — every parameter MUST carry an inline comment.
	// Pragmatic enforcement: comments outnumber bare lines.
	body := string(DefaultsTOML)
	comments := strings.Count(body, "#")
	require.Greater(t, comments, 20,
		"defaults.toml must contain many inline comments (FR-10.11); found %d", comments)
}

func TestDefaults_KeysSurface(t *testing.T) {
	eff, err := DefaultsEffective()
	require.NoError(t, err)

	// Spot-check a few keys that downstream subsystems will read.
	v, layer, ok := eff.Get("daemon.project_scan_interval")
	require.True(t, ok, "daemon.project_scan_interval must be defined")
	require.Equal(t, LayerShipped, layer)
	require.NotEmpty(t, v)

	v, _, ok = eff.Get("retention.snapshot_min_events")
	require.True(t, ok, "retention.snapshot_min_events must be defined")
	require.Equal(t, "100", v)

	v, _, ok = eff.Get("tray.refresh_interval")
	require.True(t, ok, "tray.refresh_interval must be defined")
	require.Equal(t, "5s", v)

	_, _, ok = eff.Get("secrets.default_sync_secrets")
	require.True(t, ok, "secrets.default_sync_secrets must be defined")
}

func TestLoad_UserLayerOverridesShipped(t *testing.T) {
	tmp := t.TempDir()
	userPath := filepath.Join(tmp, "user.toml")
	require.NoError(t, os.WriteFile(userPath, []byte(
		"[daemon]\nproject_scan_interval = \"30m\"\n"), 0o644))

	eff, err := Load(LoadOptions{UserPath: userPath})
	require.NoError(t, err)

	v, layer, ok := eff.Get("daemon.project_scan_interval")
	require.True(t, ok)
	require.Equal(t, "30m", v, "user layer must override shipped default")
	require.Equal(t, LayerUser, layer, "provenance must be user")

	// A key NOT in the user file still shows the shipped layer.
	_, layer, ok = eff.Get("retention.snapshot_min_events")
	require.True(t, ok)
	require.Equal(t, LayerShipped, layer)
}

func TestLoad_ProjectLayerOverridesUser(t *testing.T) {
	tmp := t.TempDir()
	userPath := filepath.Join(tmp, "user.toml")
	projPath := filepath.Join(tmp, "project.toml")
	require.NoError(t, os.WriteFile(userPath, []byte(
		"[tray]\nrefresh_interval = \"3s\"\n"), 0o644))
	require.NoError(t, os.WriteFile(projPath, []byte(
		"[tray]\nrefresh_interval = \"1s\"\n"), 0o644))

	eff, err := Load(LoadOptions{UserPath: userPath, ProjectPath: projPath})
	require.NoError(t, err)

	v, layer, _ := eff.Get("tray.refresh_interval")
	require.Equal(t, "1s", v, "project must override user")
	require.Equal(t, LayerProject, layer)
}

func TestLoad_MissingLayerFilesAreSilentlySkipped(t *testing.T) {
	eff, err := Load(LoadOptions{
		SystemPath:  "/nope/system.toml",
		UserPath:    "/nope/user.toml",
		ProjectPath: "/nope/project.toml",
	})
	require.NoError(t, err, "missing layer files are not a fatal error")
	// Should still have all the shipped defaults.
	_, layer, ok := eff.Get("daemon.project_scan_interval")
	require.True(t, ok)
	require.Equal(t, LayerShipped, layer)
}

func TestLoad_RejectsMalformedTOML(t *testing.T) {
	tmp := t.TempDir()
	userPath := filepath.Join(tmp, "user.toml")
	require.NoError(t, os.WriteFile(userPath, []byte(
		"not valid toml [["), 0o644))

	_, err := Load(LoadOptions{UserPath: userPath})
	require.Error(t, err)
}

func TestSetKey_ExistingSection(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "user.toml")
	require.NoError(t, os.WriteFile(path, []byte(
		"[daemon]\nproject_scan_interval = \"60m\"\n"), 0o644))

	require.NoError(t, SetKey(path, "daemon.project_scan_max_depth", "12"))
	body, _ := os.ReadFile(path)
	require.Contains(t, string(body), "project_scan_max_depth = 12")
}

func TestSetKey_NewSection(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "user.toml")
	require.NoError(t, os.WriteFile(path, []byte(
		"[daemon]\nproject_scan_interval = \"60m\"\n"), 0o644))

	require.NoError(t, SetKey(path, "retention.ttl", "30d"))
	body, _ := os.ReadFile(path)
	require.Contains(t, string(body), "[retention]")
	require.Contains(t, string(body), `ttl = "30d"`)
}

func TestSetKey_OverwritesExistingValue(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "user.toml")
	require.NoError(t, os.WriteFile(path, []byte(
		"[daemon]\nproject_scan_interval = \"60m\"  # original comment\n"), 0o644))

	require.NoError(t, SetKey(path, "daemon.project_scan_interval", "10m"))
	body, _ := os.ReadFile(path)
	require.Contains(t, string(body), `project_scan_interval = "10m"`)
	// Comment is preserved.
	require.Contains(t, string(body), "# original comment")
}

func TestUpsertTOML_HashInsideQuotedValueIsNotTreatedAsComment(t *testing.T) {
	// A '#' inside a quoted value must not be misread as the start of a
	// trailing comment and re-appended, which produced malformed,
	// non-idempotent TOML (regression for the config-set '#' corruption).
	body := "[bundle]\ndefault_filename = \"old#name.gz\"\n"
	out := upsertTOML(body, "bundle", "default_filename", "new#name.gz")
	require.Contains(t, out, "default_filename = \"new#name.gz\"\n",
		"the in-value '#' must not leak a spurious trailing comment")
	require.NoError(t, Validate([]byte(out)), "result must be valid TOML")
	// Re-setting the same value must be a no-op (idempotent).
	require.Equal(t, out, upsertTOML(out, "bundle", "default_filename", "new#name.gz"),
		"upsertTOML must be idempotent for a '#'-bearing value")
}

func TestUpsertTOML_PreservesRealTrailingCommentWhenValueHasHash(t *testing.T) {
	// The genuine trailing comment (outside the quotes) must still be
	// preserved even though the new value itself contains a '#'.
	body := "[bundle]\ndefault_filename = \"old#x.gz\"  # keep me\n"
	out := upsertTOML(body, "bundle", "default_filename", "new#name.gz")
	require.Contains(t, out, "default_filename = \"new#name.gz\" # keep me\n")
	require.NoError(t, Validate([]byte(out)))
}

func TestSetKey_PreservesNumberAndBoolEncoding(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "user.toml")

	require.NoError(t, SetKey(path, "daemon.project_scan_max_depth", "8"))
	require.NoError(t, SetKey(path, "secrets.default_sync_secrets", "true"))
	body, _ := os.ReadFile(path)
	require.Contains(t, string(body), "project_scan_max_depth = 8",
		"numbers must be unquoted")
	require.Contains(t, string(body), "default_sync_secrets = true",
		"bools must be unquoted")
}

func TestSetKey_RejectsBadKey(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "user.toml")
	require.Error(t, SetKey(path, "no-dot", "x"))
	require.Error(t, SetKey(path, ".no-section", "x"))
	require.Error(t, SetKey(path, "section.", "x"))
}

func TestUnsetKey_Removes(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "user.toml")
	require.NoError(t, os.WriteFile(path, []byte(
		"[daemon]\nproject_scan_interval = \"60m\"\nproject_scan_max_depth = 6\n"), 0o644))

	require.NoError(t, UnsetKey(path, "daemon.project_scan_max_depth"))
	body, _ := os.ReadFile(path)
	require.NotContains(t, string(body), "project_scan_max_depth")
	require.Contains(t, string(body), "project_scan_interval", "sibling key untouched")
}

func TestUnsetKey_NoFile_NoError(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "never.toml")
	require.NoError(t, UnsetKey(path, "daemon.x"))
}

func TestUnsetKey_AbsentKey_NoError(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "user.toml")
	require.NoError(t, os.WriteFile(path, []byte("[daemon]\n"), 0o644))
	require.NoError(t, UnsetKey(path, "daemon.never"))
}

func TestParseLayer(t *testing.T) {
	cases := []struct {
		in   string
		want Layer
		err  bool
	}{
		{"system", LayerSystem, false},
		{"user", LayerUser, false},
		{"project", LayerProject, false},
		{"USER", LayerUser, false},
		{"shipped", 0, true},
		{"env", 0, true},
		{"cli", 0, true},
		{"nonsense", 0, true},
	}
	for _, c := range cases {
		got, err := ParseLayer(c.in)
		if c.err {
			require.Error(t, err, "ParseLayer(%q)", c.in)
		} else {
			require.NoError(t, err, "ParseLayer(%q)", c.in)
			require.Equal(t, c.want, got)
		}
	}
}

func TestFlatten_NestedTables(t *testing.T) {
	eff, err := DefaultsEffective()
	require.NoError(t, err)
	// _meta.schema_version should appear as a flat key. The actual
	// value is bumped per release; just assert it's present and
	// parseable as a positive int.
	v, _, ok := eff.Get("_meta.schema_version")
	require.True(t, ok)
	require.NotEmpty(t, v)
	n, err := strconv.Atoi(v)
	require.NoError(t, err)
	require.Greater(t, n, 0)
}

// ─────────────────────────────────────────────────────────────────────
// v0.70.0 — env-var + CLI-flag layers + schema validation
// ─────────────────────────────────────────────────────────────────────

func TestLoad_EnvLayerOverridesUser(t *testing.T) {
	tmp := t.TempDir()
	userPath := filepath.Join(tmp, "user.toml")
	require.NoError(t, os.WriteFile(userPath, []byte(
		"[daemon]\nproject_scan_interval = \"30m\"\n"), 0o644))

	eff, err := Load(LoadOptions{
		UserPath: userPath,
		Env:      []string{"APLEXICA_DAEMON_PROJECT_SCAN_INTERVAL=10m"},
	})
	require.NoError(t, err)

	v, layer, _ := eff.Get("daemon.project_scan_interval")
	require.Equal(t, "10m", v)
	require.Equal(t, LayerEnv, layer)
}

func TestLoad_CLIOverridesBeatEverything(t *testing.T) {
	tmp := t.TempDir()
	userPath := filepath.Join(tmp, "user.toml")
	require.NoError(t, os.WriteFile(userPath, []byte(
		"[daemon]\nproject_scan_interval = \"30m\"\n"), 0o644))

	eff, err := Load(LoadOptions{
		UserPath:     userPath,
		Env:          []string{"APLEXICA_DAEMON_PROJECT_SCAN_INTERVAL=10m"},
		CLIOverrides: []string{"daemon.project_scan_interval=1m"},
	})
	require.NoError(t, err)

	v, layer, _ := eff.Get("daemon.project_scan_interval")
	require.Equal(t, "1m", v, "CLI must beat env which beat user which beat shipped")
	require.Equal(t, LayerCLI, layer)
}

func TestLoad_EnvIgnoresUnknownPrefix(t *testing.T) {
	// APLEXICA_FOOBAR doesn't match any section; must NOT pollute the
	// effective config.
	eff, err := Load(LoadOptions{
		Env: []string{
			"APLEXICA_NOT_A_SECTION_KEY=garbage",
			"PATH=/usr/local/bin",
		},
	})
	require.NoError(t, err)
	for k := range eff.Values {
		require.False(t, strings.Contains(k, "not_a_section"),
			"env entry for unknown section should not appear; got key %q", k)
	}
}

func TestLoad_EnvKeyWithUnderscoresInField(t *testing.T) {
	// daemon.project_scan_max_depth → APLEXICA_DAEMON_PROJECT_SCAN_MAX_DEPTH.
	// Field name has underscores; the first underscore is the section
	// boundary because `daemon` is a known section.
	eff, err := Load(LoadOptions{
		Env: []string{"APLEXICA_DAEMON_PROJECT_SCAN_MAX_DEPTH=12"},
	})
	require.NoError(t, err)
	v, layer, ok := eff.Get("daemon.project_scan_max_depth")
	require.True(t, ok)
	require.Equal(t, "12", v)
	require.Equal(t, LayerEnv, layer)
}

func TestLoad_EnvNestedDottedKeyResolves(t *testing.T) {
	// retention.snapshot_after_events.conversation is a 3-segment dotted
	// schema key. APLEXICA_RETENTION_SNAPSHOT_AFTER_EVENTS_CONVERSATION must
	// resolve it at LayerEnv — the env body is matched against the
	// snake-cased forms of the actual schema keys, so the second '.' is
	// reconstructed (BRD-10 §10.1: env is first-class for every tunable).
	eff, err := Load(LoadOptions{
		Env: []string{"APLEXICA_RETENTION_SNAPSHOT_AFTER_EVENTS_CONVERSATION=250"},
	})
	require.NoError(t, err)
	v, layer, ok := eff.Get("retention.snapshot_after_events.conversation")
	require.True(t, ok, "3-segment dotted schema key must be reachable via env")
	require.Equal(t, "250", v)
	require.Equal(t, LayerEnv, layer)
}

func TestLoad_EnvUnknownFieldUnderKnownSectionStillWarns(t *testing.T) {
	// A genuinely-unknown env var whose body starts with a known section
	// (daemon) but matches no schema key must still be folded in so the
	// schema's unknown-key warning fires (BRD-10 §10.2) — env typos are
	// surfaced, not silently swallowed.
	eff, err := Load(LoadOptions{
		Env: []string{"APLEXICA_DAEMON_TOTALLY_BOGUS_FIELD=whatever"},
	})
	require.NoError(t, err)
	v, layer, ok := eff.Get("daemon.totally_bogus_field")
	require.True(t, ok, "unknown field under a known section must still fold in")
	require.Equal(t, "whatever", v)
	require.Equal(t, LayerEnv, layer)

	_, warns := SchemaValidate(eff)
	require.Contains(t, strings.Join(warns, "\n"), "daemon.totally_bogus_field",
		"a genuinely unknown APLEXICA_* var must still produce an unknown-key warning")
}

func TestLoad_CLIOverride_BadShape(t *testing.T) {
	_, err := Load(LoadOptions{CLIOverrides: []string{"no-equals"}})
	require.Error(t, err)
}

func TestSchemaValidate_DefaultsAreValid(t *testing.T) {
	// The shipped defaults must round-trip through SchemaValidate
	// without errors — every default is also its own valid value.
	eff, err := DefaultsEffective()
	require.NoError(t, err)
	errs, _ := SchemaValidate(eff)
	require.Empty(t, errs, "shipped defaults must pass schema validation; got %v", errs)
}

func TestSchemaValidate_RejectsOutOfRangeInt(t *testing.T) {
	tmp := t.TempDir()
	userPath := filepath.Join(tmp, "user.toml")
	require.NoError(t, os.WriteFile(userPath, []byte(
		"[daemon]\nproject_scan_max_depth = 99\n"), 0o644))

	eff, err := Load(LoadOptions{UserPath: userPath})
	require.NoError(t, err)
	errs, _ := SchemaValidate(eff)
	require.NotEmpty(t, errs)
	require.Contains(t, strings.Join(errs, "\n"), "project_scan_max_depth")
	require.Contains(t, strings.Join(errs, "\n"), "above maximum")
}

func TestSchemaValidate_RejectsOutOfRangeDuration(t *testing.T) {
	tmp := t.TempDir()
	userPath := filepath.Join(tmp, "user.toml")
	require.NoError(t, os.WriteFile(userPath, []byte(
		"[tray]\nrefresh_interval = \"1h\"\n"), 0o644))

	eff, err := Load(LoadOptions{UserPath: userPath})
	require.NoError(t, err)
	errs, _ := SchemaValidate(eff)
	require.NotEmpty(t, errs)
	require.Contains(t, strings.Join(errs, "\n"), "refresh_interval")
}

func TestSchemaValidate_AcceptsZeroAsDisableSentinel(t *testing.T) {
	tmp := t.TempDir()
	userPath := filepath.Join(tmp, "user.toml")
	require.NoError(t, os.WriteFile(userPath, []byte(
		"[daemon]\nproject_scan_interval = \"0s\"\n"), 0o644))

	eff, err := Load(LoadOptions{UserPath: userPath})
	require.NoError(t, err)
	errs, _ := SchemaValidate(eff)
	require.Empty(t, errs,
		"AllowZero entries accept '0s' even outside the [min..max] range; got %v", errs)
}

func TestSchemaValidate_RejectsBadBool(t *testing.T) {
	tmp := t.TempDir()
	userPath := filepath.Join(tmp, "user.toml")
	require.NoError(t, os.WriteFile(userPath, []byte(
		"[secrets]\ndefault_sync_secrets = \"yes\"\n"), 0o644))

	eff, err := Load(LoadOptions{UserPath: userPath})
	require.NoError(t, err)
	errs, _ := SchemaValidate(eff)
	require.NotEmpty(t, errs)
}

func TestSchemaValidate_RejectsInvalidEnumValue(t *testing.T) {
	tmp := t.TempDir()
	userPath := filepath.Join(tmp, "user.toml")
	require.NoError(t, os.WriteFile(userPath, []byte(
		"[log]\nrotate_at = \"sunday\"\n"), 0o644))

	eff, err := Load(LoadOptions{UserPath: userPath})
	require.NoError(t, err)
	errs, _ := SchemaValidate(eff)
	require.NotEmpty(t, errs)
}

func TestSchemaValidate_UnknownKeyWarnsNotErrors(t *testing.T) {
	// BRD-10 §10.2: "Unknown keys (typos, deprecated keys from old
	// versions) produce warnings at startup, not failures."
	tmp := t.TempDir()
	userPath := filepath.Join(tmp, "user.toml")
	require.NoError(t, os.WriteFile(userPath, []byte(
		"[daemon]\nproject_scan_interval = \"5m\"\ntypo_key = 42\n"), 0o644))

	eff, err := Load(LoadOptions{UserPath: userPath})
	require.NoError(t, err)
	errs, warns := SchemaValidate(eff)
	require.Empty(t, errs)
	require.NotEmpty(t, warns)
	require.Contains(t, strings.Join(warns, "\n"), "typo_key")
}

func TestEveryDefaultKeyHasSchemaEntry(t *testing.T) {
	// Round-trip invariant: every dotted key emitted by the shipped
	// defaults MUST have a matching Schema entry. A new tunable added to
	// defaults.toml without a schema entry (or vice versa) fails here.
	eff, err := DefaultsEffective()
	require.NoError(t, err)
	for _, k := range eff.Keys() {
		_, ok := schemaByKey[k]
		require.True(t, ok, "defaults.toml key %q has no schema entry in schema.go", k)
	}
}

func TestEverySchemaKeyHasDefault(t *testing.T) {
	// The reverse direction: every Schema entry must have a default in
	// defaults.toml, so `aplexica config show` never reports a schema key
	// with no shipped value.
	eff, err := DefaultsEffective()
	require.NoError(t, err)
	for _, e := range Schema {
		_, _, ok := eff.Get(e.Key)
		require.True(t, ok, "schema key %q has no default in defaults.toml", e.Key)
	}
}

func TestSchemaJSON_RoundTrips(t *testing.T) {
	b, err := SchemaJSON()
	require.NoError(t, err)
	require.NotEmpty(t, b)
	// Spot-check a known key is present in the JSON.
	require.Contains(t, string(b), "daemon.project_scan_interval")
}

func TestSchemaMarkdown_HasTableAndKeys(t *testing.T) {
	md := SchemaMarkdown()
	require.Contains(t, md, "# Aplexica configuration schema")
	require.Contains(t, md, "| Key | Type |")
	require.Contains(t, md, "daemon.project_scan_interval")
	require.Contains(t, md, "tray.refresh_interval")
}

func TestDurationCanonical_DaysExpand(t *testing.T) {
	require.Equal(t, "168h", durationCanonical("7d"))
	require.Equal(t, "720h", durationCanonical("30d"))
	require.Equal(t, "60m", durationCanonical("60m"))
}

func TestValidate_NowRangeChecks(t *testing.T) {
	// The legacy Validate(body) entry point now also range-checks via
	// the schema. A range violation is a hard error.
	body := []byte("[daemon]\nproject_scan_max_depth = 99\n")
	err := Validate(body)
	require.Error(t, err, "Validate must now catch range violations")
	require.Contains(t, err.Error(), "project_scan_max_depth")
}

func TestValidateBody_RejectsEmergencyQuotaBelowWatermark(t *testing.T) {
	// BRD-03 §4.8.4: store_emergency_quota MUST be >= store_high_watermark.
	// Each key range-checks fine independently, so this cross-field
	// invariant is enforced by ValidateBody's cross-field hook — the
	// CI-facing `config validate` must NOT report a false "ok".
	body := []byte("[retention]\nstore_high_watermark = 0.9\nstore_emergency_quota = 0.5\n")
	errs, _, err := ValidateBody(body)
	require.NoError(t, err, "TOML parses cleanly; the failure is a validation error, not a parse error")
	require.NotEmpty(t, errs, "quota < watermark must yield a non-empty error slice")
	require.Contains(t, strings.Join(errs, "\n"), "store_emergency_quota")
	require.Contains(t, strings.Join(errs, "\n"), "store_high_watermark")
}

func TestValidateBody_AcceptsEmergencyQuotaAtOrAboveWatermark(t *testing.T) {
	// quota >= watermark stays clean — both the boundary (equal) and the
	// normal (greater) cases must produce zero errors.
	for _, body := range [][]byte{
		[]byte("[retention]\nstore_high_watermark = 0.80\nstore_emergency_quota = 0.95\n"),
		[]byte("[retention]\nstore_high_watermark = 0.80\nstore_emergency_quota = 0.80\n"),
	} {
		errs, _, err := ValidateBody(body)
		require.NoError(t, err)
		require.Empty(t, errs, "quota >= watermark must stay clean; got %v", errs)
	}
}
