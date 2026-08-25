package main

import (
	"testing"

	"github.com/aplexica/aplexica/internal/config"
	"github.com/stretchr/testify/require"
)

// fakeRetentionGetter is a stub retentionConfigGetter backed by a flat map.
// The layer is always config.Layer(0) (ignored by retention.Load).
type fakeRetentionGetter map[string]string

func (f fakeRetentionGetter) Get(key string) (string, config.Layer, bool) {
	v, ok := f[key]
	return v, config.Layer(0), ok
}

// TestLoadRetentionConfig_FlagOverride documents and locks the watermark
// precedence: explicit-flag > fractional-of-store_max_gb > absolute.
func TestLoadRetentionConfig_FlagOverride(t *testing.T) {
	const gb = int64(bytesPerGB)

	t.Run("explicit flag wins over fractional", func(t *testing.T) {
		// Config asks for a 10GB cap at 0.80 → 8GB fractional watermark, but
		// the operator explicitly passed --store-high-watermark-gb=2.
		eff := fakeRetentionGetter{
			"retention.store_max_gb":         "10",
			"retention.store_high_watermark": "0.80",
		}
		_, wm, err := loadRetentionConfig(eff, retentionFlagOverrides{
			HighWatermarkGB:        2,
			HighWatermarkGBChanged: true,
		})
		require.NoError(t, err)
		require.Equal(t, 2*gb, wm, "explicit flag overrides the fractional watermark")
	})

	t.Run("fractional used when flag unset", func(t *testing.T) {
		eff := fakeRetentionGetter{
			"retention.store_max_gb":         "10",
			"retention.store_high_watermark": "0.80",
		}
		cfg, wm, err := loadRetentionConfig(eff, retentionFlagOverrides{
			// flag NOT changed
			HighWatermarkGB:        2,
			HighWatermarkGBChanged: false,
		})
		require.NoError(t, err)
		require.Equal(t, int64(float64(10)*0.80*float64(gb)), wm,
			"unset flag falls through to StoreMaxGB*StoreHighWatermark")
		require.InDelta(t, 10.0, cfg.StoreMaxGB, 0.001)
		require.InDelta(t, 0.80, cfg.StoreHighWatermark, 0.001)
	})

	t.Run("absolute legacy value when no fractional watermark", func(t *testing.T) {
		// store_max_gb = 0 → no fractional watermark. The legacy flag value
		// (config-applied, not explicitly changed) is used as an absolute GB.
		eff := fakeRetentionGetter{
			"retention.store_max_gb": "0",
		}
		_, wm, err := loadRetentionConfig(eff, retentionFlagOverrides{
			HighWatermarkGB:        3,
			HighWatermarkGBChanged: false,
		})
		require.NoError(t, err)
		require.Equal(t, 3*gb, wm, "absolute legacy value used when no fractional watermark")
	})

	t.Run("disabled when nothing set and no cap", func(t *testing.T) {
		eff := fakeRetentionGetter{
			"retention.store_max_gb": "0",
		}
		_, wm, err := loadRetentionConfig(eff, retentionFlagOverrides{})
		require.NoError(t, err)
		require.Equal(t, int64(0), wm, "watermark 0 → disk-pressure disabled")
	})

	t.Run("default config disables watermark", func(t *testing.T) {
		// Empty getter → DefaultConfig() (unlimited store, no watermark).
		_, wm, err := loadRetentionConfig(fakeRetentionGetter{}, retentionFlagOverrides{})
		require.NoError(t, err)
		require.Equal(t, int64(0), wm,
			"shipped unlimited default disables the disk-pressure watermark")
	})
}
