// Retention policy configuration (BRD-03 §4.8.4): the typed Config the
// retention engine consumes, its shipped defaults, validation, and a pure
// loader that maps the resolved layered config (internal/config) onto it.
//
// This file is intentionally side-effect free. DefaultConfig() is the
// SINGLE Go-side source of shipped defaults; internal/config/defaults.toml
// is its layer-1 mirror (the config round-trip test enforces they agree).
// Load() reads the retention.* keys from a resolved config and maps them;
// it does NOT overlay daemon CLI flags — the daemon does that in a later
// PR. Keeping Load pure makes it trivially testable.
package retention

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/config"
)

// Config is the resolved, typed retention policy.
type Config struct {
	// StoreMaxGB caps the canonical store size in gigabytes. 0 = unlimited.
	StoreMaxGB float64
	// StoreHighWatermark is the fraction of StoreMaxGB (0..1) at which
	// emergency pruning triggers.
	StoreHighWatermark float64
	// StoreEmergencyQuota is the fraction of StoreMaxGB (0..1) above which
	// new ingestion is refused. Must be >= StoreHighWatermark.
	StoreEmergencyQuota float64
	// AttachmentsOnly selects attachment-byte eviction (true, OSS default)
	// vs. full event pruning (false) at the watermark.
	AttachmentsOnly bool
	// AttachmentMinAge protects attachments younger than this from eviction.
	AttachmentMinAge time.Duration
	// KeepLastNSnapshots is the per-artifact snapshot retention cap.
	// -1 means "keep every snapshot forever" (the "all" sentinel).
	KeepLastNSnapshots int
	// PinTags exempt any artifact carrying one of them from all pruning.
	PinTags []string
	// SnapshotAfterEvents is the per-kind event-count snapshot threshold.
	SnapshotAfterEvents map[acf.Kind]int
	// SnapshotAfterTime is the per-kind HEAD-age snapshot threshold.
	SnapshotAfterTime map[acf.Kind]time.Duration
}

// Shipped defaults (BRD-03 §4.8.4). Named consts so the magic-number
// lint (FR-10.6) stays green — DefaultConfig() is the layer-0 mirror of
// defaults.toml, not a scattering of literals.
const (
	// Unlimited by default. A finite cap only helps if the sweep can
	// actually reclaim, and the shipped keep_last_n_snapshots = keepAll
	// forbids the prune phase, so a capped store could only ever escalate
	// to the emergency ingest refusal with no automatic way back down.
	// Operators who want the cap set retention.store_max_gb explicitly.
	defaultStoreMaxGB          = 0.0  // 0 = unlimited (disk-pressure disabled)
	defaultStoreHighWatermark  = 0.80 // BRD-03 4.8.4; sweep/prune triggers at 80% of the cap
	defaultStoreEmergencyQuota = 0.95 // refuse ingestion above 95% of the cap
	defaultAttachmentsOnly     = true // OSS default: evict blobs, keep text
	defaultAttachmentMinAge    = 30 * 24 * time.Hour
	defaultKeepLastNSnapshots  = keepAll // "all" → keep every snapshot forever

	// Per-kind snapshot-after-events cadence.
	defaultSnapshotEventsConversation = 100
	defaultSnapshotEventsMemory       = 50
	defaultSnapshotEventsSkill        = 50
	defaultSnapshotEventsTool         = 50

	// Per-kind snapshot-after-time cadence.
	defaultSnapshotTimeConversation = 24 * time.Hour
	defaultSnapshotTimeMemory       = 7 * 24 * time.Hour
	defaultSnapshotTimeSkill        = 7 * 24 * time.Hour
	defaultSnapshotTimeTool         = 7 * 24 * time.Hour
)

// keepAll is the internal sentinel for KeepLastNSnapshots meaning "keep
// every snapshot forever" — the typed form of the config "all" literal.
const keepAll = -1

// allSentinel is the config-string spelling of keepAll.
const allSentinel = "all"

// float64Bits is the bit size passed to strconv.ParseFloat (not a
// tunable — a stdlib protocol argument). hoursPerDay converts the
// attachment_min_age_days int to a Duration.
const (
	float64Bits = 64
	hoursPerDay = 24
)

// DefaultConfig returns the shipped retention defaults. This is the
// single source of truth on the Go side; defaults.toml mirrors it.
func DefaultConfig() Config {
	return Config{
		StoreMaxGB:          defaultStoreMaxGB,
		StoreHighWatermark:  defaultStoreHighWatermark,
		StoreEmergencyQuota: defaultStoreEmergencyQuota,
		AttachmentsOnly:     defaultAttachmentsOnly,
		AttachmentMinAge:    defaultAttachmentMinAge,
		KeepLastNSnapshots:  defaultKeepLastNSnapshots,
		PinTags:             []string{"pinned", "keep-forever"},
		SnapshotAfterEvents: map[acf.Kind]int{
			acf.KindConversation: defaultSnapshotEventsConversation,
			acf.KindMemory:       defaultSnapshotEventsMemory,
			acf.KindSkill:        defaultSnapshotEventsSkill,
			acf.KindTool:         defaultSnapshotEventsTool,
		},
		SnapshotAfterTime: map[acf.Kind]time.Duration{
			acf.KindConversation: defaultSnapshotTimeConversation,
			acf.KindMemory:       defaultSnapshotTimeMemory,
			acf.KindSkill:        defaultSnapshotTimeSkill,
			acf.KindTool:         defaultSnapshotTimeTool,
		},
	}
}

// Validate rejects nonsensical combinations. The schema range-checks each
// key independently; Validate enforces the cross-field invariants the
// schema can't express (BRD-03 §4.8.4): watermark/quota ordering, the
// 0..1 fraction bounds, and non-negative sizes/ages.
func (c Config) Validate() error {
	if c.StoreMaxGB < 0 {
		return fmt.Errorf("retention: store_max_gb must be >= 0, got %g", c.StoreMaxGB)
	}
	if c.StoreHighWatermark < 0 || c.StoreHighWatermark > 1 {
		return fmt.Errorf("retention: store_high_watermark must be in [0,1], got %g", c.StoreHighWatermark)
	}
	if c.StoreEmergencyQuota < 0 || c.StoreEmergencyQuota > 1 {
		return fmt.Errorf("retention: store_emergency_quota must be in [0,1], got %g", c.StoreEmergencyQuota)
	}
	if c.StoreEmergencyQuota < c.StoreHighWatermark {
		return fmt.Errorf("retention: store_emergency_quota (%g) must be >= store_high_watermark (%g)",
			c.StoreEmergencyQuota, c.StoreHighWatermark)
	}
	if c.AttachmentMinAge < 0 {
		return fmt.Errorf("retention: attachment_min_age must be >= 0, got %s", c.AttachmentMinAge)
	}
	// keep_last_n_snapshots is either the "all" sentinel (keepAll == -1, keep
	// every snapshot forever) or a numeric cap at/above the FR-03.25 floor of
	// minKeepLastN. Values between (e.g. 0, 1) are rejected: the cap never
	// retains fewer than two snapshots.
	if c.KeepLastNSnapshots != keepAll && c.KeepLastNSnapshots < minKeepLastN {
		return fmt.Errorf("retention: keep_last_n_snapshots must be %q or >= %d, got %d",
			allSentinel, minKeepLastN, c.KeepLastNSnapshots)
	}
	for _, d := range c.SnapshotAfterTime {
		if d < 0 {
			return fmt.Errorf("retention: snapshot_after_time must be >= 0, got %s", d)
		}
	}
	for _, n := range c.SnapshotAfterEvents {
		if n < 0 {
			return fmt.Errorf("retention: snapshot_after_events must be >= 0, got %d", n)
		}
	}
	return nil
}

// configGetter is the read surface Load needs from a resolved config.
// *config.Effective satisfies it; defining the interface keeps Load
// decoupled from the concrete type and trivially fakeable in tests.
type configGetter interface {
	Get(key string) (value string, layer config.Layer, ok bool)
}

// Load maps the retention.* keys of a resolved config onto a typed
// Config. It starts from DefaultConfig() and overlays only keys that are
// present, so a partial config (or a fake getter) degrades gracefully to
// the shipped defaults. Pure: no flag overlay, no I/O, no globals.
func Load(eff configGetter) (Config, error) {
	c := DefaultConfig()

	if v, _, ok := eff.Get("retention.store_max_gb"); ok {
		f, err := strconv.ParseFloat(v, float64Bits)
		if err != nil {
			return Config{}, fmt.Errorf("retention: store_max_gb: %w", err)
		}
		c.StoreMaxGB = f
	}
	if v, _, ok := eff.Get("retention.store_high_watermark"); ok {
		f, err := strconv.ParseFloat(v, float64Bits)
		if err != nil {
			return Config{}, fmt.Errorf("retention: store_high_watermark: %w", err)
		}
		c.StoreHighWatermark = f
	}
	if v, _, ok := eff.Get("retention.store_emergency_quota"); ok {
		f, err := strconv.ParseFloat(v, float64Bits)
		if err != nil {
			return Config{}, fmt.Errorf("retention: store_emergency_quota: %w", err)
		}
		c.StoreEmergencyQuota = f
	}
	if v, _, ok := eff.Get("retention.attachments_only"); ok {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return Config{}, fmt.Errorf("retention: attachments_only: %w", err)
		}
		c.AttachmentsOnly = b
	}
	if v, _, ok := eff.Get("retention.attachment_min_age_days"); ok {
		days, err := strconv.Atoi(v)
		if err != nil {
			return Config{}, fmt.Errorf("retention: attachment_min_age_days: %w", err)
		}
		c.AttachmentMinAge = time.Duration(days) * hoursPerDay * time.Hour
	}
	if v, _, ok := eff.Get("retention.keep_last_n_snapshots"); ok {
		n, err := parseKeepLastN(v)
		if err != nil {
			return Config{}, err
		}
		c.KeepLastNSnapshots = n
	}
	if v, _, ok := eff.Get("retention.pin_tags"); ok {
		c.PinTags = parseStringArray(v)
	}

	events := map[acf.Kind]string{
		acf.KindConversation: "retention.snapshot_after_events.conversation",
		acf.KindMemory:       "retention.snapshot_after_events.memory",
		acf.KindSkill:        "retention.snapshot_after_events.skill",
		acf.KindTool:         "retention.snapshot_after_events.tool",
	}
	for kind, key := range events {
		if v, _, ok := eff.Get(key); ok {
			n, err := strconv.Atoi(v)
			if err != nil {
				return Config{}, fmt.Errorf("retention: %s: %w", key, err)
			}
			c.SnapshotAfterEvents[kind] = n
		}
	}

	times := map[acf.Kind]string{
		acf.KindConversation: "retention.snapshot_after_time.conversation",
		acf.KindMemory:       "retention.snapshot_after_time.memory",
		acf.KindSkill:        "retention.snapshot_after_time.skill",
		acf.KindTool:         "retention.snapshot_after_time.tool",
	}
	for kind, key := range times {
		if v, _, ok := eff.Get(key); ok {
			d, err := config.ParseDuration(v)
			if err != nil {
				return Config{}, fmt.Errorf("retention: %s: %w", key, err)
			}
			c.SnapshotAfterTime[kind] = d
		}
	}

	return c, nil
}

// parseKeepLastN maps the "all" sentinel to keepAll (-1) and otherwise
// parses a non-negative integer.
func parseKeepLastN(v string) (int, error) {
	if strings.TrimSpace(v) == allSentinel {
		return keepAll, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("retention: keep_last_n_snapshots: expected int or %q, got %q", allSentinel, v)
	}
	return n, nil
}

// parseStringArray parses the config string-array rendering. The config
// layer flattens a TOML array to "[a, b, c]" (unquoted, comma-space
// separated; see internal/config scalarString). An empty list renders as
// "[]" → nil. Bare unbracketed input is tolerated as a single element.
func parseStringArray(v string) []string {
	s := strings.TrimSpace(v)
	s = strings.TrimPrefix(s, "[")
	s = strings.TrimSuffix(s, "]")
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		p = strings.Trim(p, `"'`)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
