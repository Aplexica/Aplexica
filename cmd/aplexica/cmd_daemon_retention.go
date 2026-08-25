package main

import (
	"os"

	"github.com/aplexica/aplexica/internal/config"
	"github.com/aplexica/aplexica/internal/retention"
)

// bytesPerGB converts a gigabyte float to bytes for the watermark math.
const bytesPerGB = 1024 * 1024 * 1024

// retentionConfigGetter is the read surface loadRetentionConfig needs from a
// resolved layered config — exactly what retention.Load consumes.
// *config.Effective satisfies it; the interface keeps the helper fakeable in
// unit tests (a fake getter returns config.Layer(0) for the ignored layer).
type retentionConfigGetter interface {
	Get(key string) (value string, layer config.Layer, ok bool)
}

// retentionFlagOverrides carries the daemon CLI flags that overlay the
// file/layered retention config in the disk-pressure path. Only the legacy
// --store-high-watermark-gb override is wired today (FR-03.20); the field is
// split into value + Changed so the precedence rule can distinguish an
// explicitly-set flag from an unset one.
type retentionFlagOverrides struct {
	// HighWatermarkGB is the value of --store-high-watermark-gb (absolute GB).
	HighWatermarkGB float64
	// HighWatermarkGBChanged reports whether --store-high-watermark-gb was
	// explicitly set on the command line. Only an explicit flag overrides the
	// fractional-of-store_max_gb watermark.
	HighWatermarkGBChanged bool
}

// loadRetentionConfig resolves the typed retention.Config from a layered
// config and computes the effective disk-pressure watermark in BYTES, applying
// the daemon flag overlay.
//
// The returned Config is retention.Load(eff) verbatim — the daemon does
// not mutate the typed policy, it only resolves which watermark to measure
// against. The watermark precedence (highest wins) is:
//
//  1. explicit --store-high-watermark-gb flag (absolute GB) — legacy override
//  2. fractional: StoreMaxGB * StoreHighWatermark (when StoreMaxGB > 0)
//  3. absolute legacy: the --store-high-watermark-gb VALUE even when not
//     explicitly changed (it carries any config-file value applied by
//     applyConfigToFlags), used only when there is no fractional watermark
//
// A watermark of 0 means the disk-pressure path is disabled.
func loadRetentionConfig(eff retentionConfigGetter, ov retentionFlagOverrides) (retention.Config, int64, error) {
	cfg, err := retention.Load(eff)
	if err != nil {
		return retention.Config{}, 0, err
	}

	// (1) explicit flag wins outright.
	if ov.HighWatermarkGBChanged && ov.HighWatermarkGB > 0 {
		return cfg, int64(ov.HighWatermarkGB * bytesPerGB), nil
	}

	// (2) fractional-of-store_max_gb.
	if cfg.StoreMaxGB > 0 && cfg.StoreHighWatermark > 0 {
		return cfg, int64(cfg.StoreMaxGB * cfg.StoreHighWatermark * bytesPerGB), nil
	}

	// (3) absolute legacy value (config-file-applied or default flag).
	if ov.HighWatermarkGB > 0 {
		return cfg, int64(ov.HighWatermarkGB * bytesPerGB), nil
	}

	return cfg, 0, nil
}

// daemonRetentionEffective loads the merged layered config (shipped → system →
// user → project → env → --config-set) for the disk-pressure retention path,
// using the same sources applyDaemonConfigPackage resolves. It is the
// *config.Effective the daemon feeds to loadRetentionConfig at goroutine
// start. Returning the concrete *config.Effective keeps the call site simple;
// it satisfies retentionConfigGetter.
func daemonRetentionEffective() (*config.Effective, error) {
	sys, usr, _ := config.DefaultSources()
	projectPath := ""
	if daemonDir != "" {
		candidate := daemonDir + "/.aplexica/config.toml"
		if _, err := os.Stat(candidate); err == nil {
			projectPath = candidate
		}
	}
	return config.Load(config.LoadOptions{
		SystemPath:   sys,
		UserPath:     usr,
		ProjectPath:  projectPath,
		Env:          os.Environ(),
		CLIOverrides: daemonCLISets,
	})
}
