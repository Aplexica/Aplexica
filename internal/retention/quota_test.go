package retention

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

// gb returns a byte count for the given number of gigabytes, matching the
// EmergencyQuotaGate ceiling math (1 GB = 1024^3 bytes).
func gb(n float64) int64 {
	return int64(n * bytesPerGB)
}

func TestEmergencyQuotaGate_OverCeiling(t *testing.T) {
	cfg := Config{StoreMaxGB: 10, StoreHighWatermark: 0.8, StoreEmergencyQuota: 0.95}
	// Ceiling = 0.95 * 10 GB = 9.5 GB. At/over the ceiling the gate refuses.
	gate := EmergencyQuotaGate(func() int64 { return gb(9.5) }, cfg)
	require.ErrorIs(t, gate(), ErrEmergencyQuotaExceeded, "exactly at the ceiling must refuse")

	gate = EmergencyQuotaGate(func() int64 { return gb(9.6) }, cfg)
	require.ErrorIs(t, gate(), ErrEmergencyQuotaExceeded, "over the ceiling must refuse")
}

func TestEmergencyQuotaGate_UnderCeiling(t *testing.T) {
	cfg := Config{StoreMaxGB: 10, StoreHighWatermark: 0.8, StoreEmergencyQuota: 0.95}
	gate := EmergencyQuotaGate(func() int64 { return gb(9.0) }, cfg)
	require.NoError(t, gate(), "under the ceiling must allow")
}

func TestEmergencyQuotaGate_Unlimited(t *testing.T) {
	// StoreMaxGB == 0 means unlimited — the gate never refuses regardless of
	// how large the store grows.
	cfg := Config{StoreMaxGB: 0, StoreHighWatermark: 0.8, StoreEmergencyQuota: 0.95}
	gate := EmergencyQuotaGate(func() int64 { return gb(1000) }, cfg)
	require.NoError(t, gate(), "unlimited store (StoreMaxGB=0) must always allow")
}

func TestEmergencyQuotaGate_QuotaDisabled(t *testing.T) {
	// StoreEmergencyQuota == 0 disables the refusal even with a finite cap.
	cfg := Config{StoreMaxGB: 10, StoreHighWatermark: 0.8, StoreEmergencyQuota: 0}
	gate := EmergencyQuotaGate(func() int64 { return gb(100) }, cfg)
	require.NoError(t, gate(), "StoreEmergencyQuota=0 must always allow")
}

func TestEmergencyQuotaGate_ErrorIsTyped(t *testing.T) {
	cfg := Config{StoreMaxGB: 1, StoreHighWatermark: 0.8, StoreEmergencyQuota: 0.95}
	gate := EmergencyQuotaGate(func() int64 { return gb(1) }, cfg)
	err := gate()
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrEmergencyQuotaExceeded))
}

func TestPressureState_UnderWatermark(t *testing.T) {
	cfg := Config{StoreMaxGB: 10, StoreHighWatermark: 0.8, StoreEmergencyQuota: 0.95}
	ps := ComputePressureState(StoreSize{Bytes: gb(5)}, cfg)
	require.Equal(t, gb(5), ps.Bytes)
	require.Equal(t, gb(10), ps.MaxBytes)
	require.Equal(t, gb(8), ps.HighWatermarkBytes)
	require.Equal(t, int64(9.5*bytesPerGB), ps.EmergencyBytes)
	require.False(t, ps.OverHighWatermark)
	require.False(t, ps.OverEmergency)
}

func TestPressureState_OverHighWatermark(t *testing.T) {
	cfg := Config{StoreMaxGB: 10, StoreHighWatermark: 0.8, StoreEmergencyQuota: 0.95}
	ps := ComputePressureState(StoreSize{Bytes: gb(8.5)}, cfg)
	require.True(t, ps.OverHighWatermark, "8.5 GB > 8 GB watermark")
	require.False(t, ps.OverEmergency, "8.5 GB < 9.5 GB emergency ceiling")
}

func TestPressureState_OverEmergency(t *testing.T) {
	cfg := Config{StoreMaxGB: 10, StoreHighWatermark: 0.8, StoreEmergencyQuota: 0.95}
	ps := ComputePressureState(StoreSize{Bytes: gb(9.7)}, cfg)
	require.True(t, ps.OverHighWatermark, "over emergency implies over watermark")
	require.True(t, ps.OverEmergency, "9.7 GB > 9.5 GB emergency ceiling")
}

func TestPressureState_HonestSplit_WatermarkUnreachable(t *testing.T) {
	// A store can be over the watermark while dominated by append-only event
	// logs. Pinned bytes alone meet the watermark, so the
	// state must say the watermark is UNREACHABLE — retention reclaiming its
	// entire reclaimable share still leaves the store over.
	cfg := Config{StoreMaxGB: 10, StoreHighWatermark: 0.8, StoreEmergencyQuota: 0.95}
	size := StoreSize{
		Bytes:          gb(9.5),
		EventLogBytes:  gb(8.9),
		CompactedBytes: gb(0.2),
		BlobBytes:      gb(0.3),
		OtherBytes:     gb(0.1),
	}
	ps := ComputePressureState(size, cfg)
	require.Equal(t, gb(0.2), ps.ReclaimableBytes, "only .compacted segments are reclaimable")
	require.Equal(t, gb(9.5)-gb(0.2), ps.PinnedBytes)
	require.Equal(t, gb(8.9), ps.EventLogBytes)
	require.True(t, ps.OverHighWatermark, "9.5 GB > 8 GB watermark")
	require.True(t, ps.WatermarkUnreachable, "9.3 GB pinned > 8 GB watermark — no sweep can relieve this")
}

func TestPressureState_HonestSplit_WatermarkReachable(t *testing.T) {
	// Over the watermark but with enough reclaimable bytes that retention CAN
	// get back under it: the unreachable flag must stay false so the existing
	// "emergency pruning active" message keeps its meaning.
	cfg := Config{StoreMaxGB: 10, StoreHighWatermark: 0.8, StoreEmergencyQuota: 0.95}
	size := StoreSize{
		Bytes:          gb(8.5),
		EventLogBytes:  gb(6.0),
		CompactedBytes: gb(1.5),
		BlobBytes:      gb(0.9),
		OtherBytes:     gb(0.1),
	}
	ps := ComputePressureState(size, cfg)
	require.True(t, ps.OverHighWatermark, "8.5 GB > 8 GB watermark")
	require.False(t, ps.WatermarkUnreachable, "7 GB pinned < 8 GB watermark — reclaiming compacted bytes relieves pressure")
	require.Equal(t, gb(1.5), ps.ReclaimableBytes)
	require.Equal(t, gb(7), ps.PinnedBytes)
}

func TestPressureState_HonestSplit_PopulatedWhenCapDisabled(t *testing.T) {
	// StoreMaxGB == 0 disables the thresholds but the honest split is still
	// derived — the accounting is a property of the store, not the cap.
	cfg := Config{StoreMaxGB: 0}
	size := StoreSize{Bytes: gb(3), EventLogBytes: gb(2), CompactedBytes: gb(1)}
	ps := ComputePressureState(size, cfg)
	require.Equal(t, gb(1), ps.ReclaimableBytes)
	require.Equal(t, gb(2), ps.PinnedBytes)
	require.False(t, ps.WatermarkUnreachable, "no watermark configured — nothing to be unreachable")
}

func TestPressureState_Unlimited(t *testing.T) {
	// StoreMaxGB == 0: disk-pressure tracking disabled. No threshold is ever
	// crossed and MaxBytes is reported as 0 (the "disabled" sentinel).
	cfg := Config{StoreMaxGB: 0, StoreHighWatermark: 0.8, StoreEmergencyQuota: 0.95}
	ps := ComputePressureState(StoreSize{Bytes: gb(1000)}, cfg)
	require.Equal(t, int64(0), ps.MaxBytes)
	require.False(t, ps.OverHighWatermark)
	require.False(t, ps.OverEmergency)
}
