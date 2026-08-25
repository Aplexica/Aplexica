package retention

import (
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/config"
)

// TestDefaultsLoadToDefaultConfig is the keystone round-trip: loading the
// shipped defaults.toml through Load() MUST reproduce DefaultConfig()
// exactly. DefaultConfig() is the single Go-side source of shipped
// defaults; defaults.toml is its layer-1 mirror. If they drift, this
// fails.
func TestDefaultsLoadToDefaultConfig(t *testing.T) {
	eff, err := config.DefaultsEffective()
	require.NoError(t, err)

	got, err := Load(eff)
	require.NoError(t, err)

	require.Equal(t, DefaultConfig(), got,
		"defaults.toml must load to exactly DefaultConfig()")
}

func TestDefaultConfig_Validates(t *testing.T) {
	require.NoError(t, DefaultConfig().Validate(),
		"the shipped defaults must themselves be a valid config")
}

// schemaDefaultFloat returns the schema-published Default for key, parsed
// as a float. It fails the test if the key is absent or unparseable.
func schemaDefaultFloat(t *testing.T, key string) float64 {
	t.Helper()
	for _, e := range config.Schema {
		if e.Key == key {
			f, err := strconv.ParseFloat(e.Default, 64)
			require.NoError(t, err, "schema Default for %q must parse as float", key)
			return f
		}
	}
	t.Fatalf("no schema entry for %q", key)
	return 0
}

// TestStoreHighWatermarkDefault_AllSourcesAgree guards the three independent
// shipped-default sources for retention.store_high_watermark against drift:
// the schema-published Default (internal/config/schema.go), the layer-1
// defaults.toml value, and the typed retention default (DefaultConfig()).
// BRD-03 §4.8.4 / FR-03.21 fix the canonical value at 0.80 (80% of the cap).
func TestStoreHighWatermarkDefault_AllSourcesAgree(t *testing.T) {
	const canonical = 0.80

	// 1. Typed retention default.
	require.Equal(t, canonical, DefaultConfig().StoreHighWatermark,
		"typed DefaultConfig().StoreHighWatermark must be the BRD-03 §4.8.4 canonical 0.80")

	// 2. Layer-1 defaults.toml (loaded through the config layer).
	eff, err := config.DefaultsEffective()
	require.NoError(t, err)
	v, _, ok := eff.Get("retention.store_high_watermark")
	require.True(t, ok, "defaults.toml must define retention.store_high_watermark")
	tomlVal, err := strconv.ParseFloat(v, 64)
	require.NoError(t, err)
	require.Equal(t, canonical, tomlVal,
		"defaults.toml store_high_watermark must be the canonical 0.80")

	// 3. Schema-published Default (internal/config/schema.go).
	require.Equal(t, canonical, schemaDefaultFloat(t, "retention.store_high_watermark"),
		"schema-published default for retention.store_high_watermark must agree with defaults.toml and the typed default (BRD-03 §4.8.4)")
}

// TestStoreMaxGBDefault_AllSourcesAgree guards the three independent
// shipped-default sources for retention.store_max_gb against drift: the
// schema-published Default (internal/config/schema.go), the layer-1
// defaults.toml value, and the typed retention default (DefaultConfig()).
// BRD-03 §4.8.4 specifies 10 GB, but the shipped default is 0 (unlimited):
// the cap's escalation path ends in the emergency ingest refusal, and the
// shipped keep_last_n_snapshots = keepAll forbids the prune that would
// relieve it, so a capped store could reach a hard stop with no automatic
// way back down. A `config show` / `config diff` reads the schema default,
// so a schema/toml mismatch would report an inconsistent default to the user.
func TestStoreMaxGBDefault_AllSourcesAgree(t *testing.T) {
	const canonical = 0.0

	// 1. Typed retention default.
	require.Equal(t, canonical, DefaultConfig().StoreMaxGB,
		"typed DefaultConfig().StoreMaxGB must be the shipped unlimited default")

	// 2. Layer-1 defaults.toml (loaded through the config layer).
	eff, err := config.DefaultsEffective()
	require.NoError(t, err)
	v, _, ok := eff.Get("retention.store_max_gb")
	require.True(t, ok, "defaults.toml must define retention.store_max_gb")
	tomlVal, err := strconv.ParseFloat(v, 64)
	require.NoError(t, err)
	require.Equal(t, canonical, tomlVal,
		"defaults.toml store_max_gb must be the shipped unlimited default")

	// 3. Schema-published Default (internal/config/schema.go).
	require.Equal(t, canonical, schemaDefaultFloat(t, "retention.store_max_gb"),
		"schema-published default for retention.store_max_gb must agree with defaults.toml and the typed default")
}

func TestConfigValidate_RejectsEmergencyOverOne(t *testing.T) {
	c := DefaultConfig()
	c.StoreEmergencyQuota = 1.5
	require.Error(t, c.Validate())
}

func TestConfigValidate_RejectsWatermarkOverOne(t *testing.T) {
	c := DefaultConfig()
	c.StoreHighWatermark = 1.2
	require.Error(t, c.Validate())
}

func TestConfigValidate_RejectsEmergencyBelowWatermark(t *testing.T) {
	c := DefaultConfig()
	c.StoreHighWatermark = 0.90
	c.StoreEmergencyQuota = 0.85 // emergency < watermark is nonsensical
	require.Error(t, c.Validate())
}

func TestConfigValidate_RejectsNegativeAttachmentAge(t *testing.T) {
	c := DefaultConfig()
	c.AttachmentMinAge = -1 * time.Hour
	require.Error(t, c.Validate())
}

func TestConfigValidate_RejectsNegativeStoreMaxGB(t *testing.T) {
	c := DefaultConfig()
	c.StoreMaxGB = -1
	require.Error(t, c.Validate())
}

// TestKeepLastNAllMapsToMinusOne: the int-or-all "all" sentinel maps to
// the internal -1 ("keep every snapshot forever").
func TestKeepLastNAllMapsToMinusOne(t *testing.T) {
	eff, err := config.DefaultsEffective()
	require.NoError(t, err)
	got, err := Load(eff)
	require.NoError(t, err)
	require.Equal(t, -1, got.KeepLastNSnapshots,
		`keep_last_n_snapshots default "all" must map to -1`)
}

func TestKeepLastN_IntegerMapsThrough(t *testing.T) {
	eff := loadWithOverrides(t, "retention.keep_last_n_snapshots=7")
	got, err := Load(eff)
	require.NoError(t, err)
	require.Equal(t, 7, got.KeepLastNSnapshots)
}

// TestConfigValidate_RejectsKeepLastNBelowFloor guards FR-03.25's floor: a
// numeric keep_last_n_snapshots below 2 is invalid (the cap never retains
// fewer than two snapshots). The "all" sentinel (-1) stays valid.
func TestConfigValidate_RejectsKeepLastNBelowFloor(t *testing.T) {
	for _, n := range []int{0, 1} {
		c := DefaultConfig()
		c.KeepLastNSnapshots = n
		require.Error(t, c.Validate(),
			"keep_last_n_snapshots=%d is below the floor of 2 and must be rejected", n)
	}
}

func TestConfigValidate_AcceptsKeepLastNAtFloorAndAll(t *testing.T) {
	c := DefaultConfig()
	c.KeepLastNSnapshots = 2
	require.NoError(t, c.Validate(), "keep_last_n_snapshots=2 is the floor and must validate")

	c.KeepLastNSnapshots = -1 // "all"
	require.NoError(t, c.Validate(), `keep_last_n_snapshots "all" (-1) must validate`)
}

func TestLoad_PerKindMapsPopulated(t *testing.T) {
	eff, err := config.DefaultsEffective()
	require.NoError(t, err)
	got, err := Load(eff)
	require.NoError(t, err)

	require.Equal(t, 100, got.SnapshotAfterEvents[acf.KindConversation])
	require.Equal(t, 50, got.SnapshotAfterEvents[acf.KindMemory])
	require.Equal(t, 50, got.SnapshotAfterEvents[acf.KindSkill])
	require.Equal(t, 50, got.SnapshotAfterEvents[acf.KindTool])

	require.Equal(t, 24*time.Hour, got.SnapshotAfterTime[acf.KindConversation])
	require.Equal(t, 7*24*time.Hour, got.SnapshotAfterTime[acf.KindMemory])
	require.Equal(t, 7*24*time.Hour, got.SnapshotAfterTime[acf.KindSkill])
	require.Equal(t, 7*24*time.Hour, got.SnapshotAfterTime[acf.KindTool])
}

func TestLoad_PinTagsDefault(t *testing.T) {
	eff, err := config.DefaultsEffective()
	require.NoError(t, err)
	got, err := Load(eff)
	require.NoError(t, err)
	require.Equal(t, []string{"pinned", "keep-forever"}, got.PinTags)
}

func loadWithOverrides(t *testing.T, kv ...string) *config.Effective {
	t.Helper()
	eff, err := config.Load(config.LoadOptions{CLIOverrides: kv})
	require.NoError(t, err)
	return eff
}
