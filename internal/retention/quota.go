// Emergency-quota ingest refusal (BRD-03 §4.8 / FR-03.21): the last-resort
// backstop that refuses NEW ingestion once the canonical store crosses the
// emergency ceiling, after the high-watermark sweep has already had its
// chance to reclaim space.
//
// This file is the retention-side half of the gate. The acf store exposes a
// plain `IngestGate func() error` hook (no dependency on this package); the
// daemon wires EmergencyQuotaGate into it. The gate is intentionally CHEAP —
// it reads a caller-supplied cached size accessor and never walks the store —
// so it can sit on the hot AppendEvent path without cost.
package retention

import "errors"

// bytesPerGB converts a gigabyte float to bytes for the quota/watermark math
// (1 GB = 1024^3 bytes). Named const so the magic-number lint (FR-10.6) stays
// green and the ceiling computation matches the daemon's watermark math.
const bytesPerGB = 1024 * 1024 * 1024

// ErrEmergencyQuotaExceeded is the typed error EmergencyQuotaGate returns
// (and AppendEvent propagates) when the store is at/over the emergency
// ceiling and new ingestion is refused. Callers can detect it with
// errors.Is to distinguish a quota refusal from any other append failure.
var ErrEmergencyQuotaExceeded = errors.New("retention: store emergency quota exceeded; new ingestion refused")

// EmergencyQuotaGate returns a closure suitable for acf.Store.IngestGate. The
// closure refuses ingestion (returns ErrEmergencyQuotaExceeded) when, AND ONLY
// when, both StoreMaxGB > 0 (a finite cap is configured) AND
// StoreEmergencyQuota > 0 (the refusal is enabled) AND the current size
// reported by curBytes is at/above the emergency ceiling:
//
//	ceiling = StoreEmergencyQuota * StoreMaxGB * bytesPerGB
//
// When StoreMaxGB == 0 (unlimited) or StoreEmergencyQuota == 0 (disabled), the
// returned closure always allows (returns nil). The closure only reads the
// supplied cached size accessor — it NEVER walks the store — so it is safe on
// the hot append path.
func EmergencyQuotaGate(curBytes func() int64, cfg Config) func() error {
	if cfg.StoreMaxGB <= 0 || cfg.StoreEmergencyQuota <= 0 {
		return func() error { return nil }
	}
	ceiling := int64(cfg.StoreEmergencyQuota * cfg.StoreMaxGB * bytesPerGB)
	return func() error {
		if curBytes() >= ceiling {
			return ErrEmergencyQuotaExceeded
		}
		return nil
	}
}

// PressureState is a point-in-time view of the canonical store's disk
// footprint relative to the configured retention thresholds. Used by the
// daemon to surface disk pressure on the status RPC (FR-03.21). All byte
// fields are absolute; MaxBytes == 0 means the cap is disabled
// (StoreMaxGB == 0), in which case both Over* flags are false.
type PressureState struct {
	// Bytes is the measured store footprint.
	Bytes int64
	// ReclaimableBytes is the portion of Bytes retention could actually free
	// (grace-deletable .compacted segments — see StoreSize.ReclaimableBytes).
	ReclaimableBytes int64
	// PinnedBytes is the portion of Bytes no retention pass can legally free:
	// active append-only event logs, live artifact metadata, and blobs
	// presumed referenced by live heads (see StoreSize.PinnedBytes).
	PinnedBytes int64
	// EventLogBytes is the active per-artifact event-log portion of
	// PinnedBytes — the dominant pinned area on stores where the watermark is
	// unreachable, surfaced so status can say WHY the bytes are pinned.
	EventLogBytes int64
	// MaxBytes is the configured cap in bytes (0 = unlimited/disabled).
	MaxBytes int64
	// HighWatermarkBytes is the absolute high-watermark threshold (0 when
	// disabled).
	HighWatermarkBytes int64
	// EmergencyBytes is the absolute emergency-quota ceiling (0 when
	// disabled).
	EmergencyBytes int64
	// OverHighWatermark reports Bytes >= HighWatermarkBytes (a configured,
	// non-zero watermark).
	OverHighWatermark bool
	// OverEmergency reports Bytes >= EmergencyBytes (a configured, non-zero
	// emergency ceiling) — ingestion is being refused.
	OverEmergency bool
	// WatermarkUnreachable reports PinnedBytes >= HighWatermarkBytes (a
	// configured, non-zero watermark): even if retention reclaimed every
	// reclaimable byte the store would still be at/over the watermark, so
	// sweeping harder cannot help. This is the honest signal that replaces
	// the aspirational "over watermark, 0 reclaimed" report.
	WatermarkUnreachable bool
}

// ComputePressureState derives a PressureState from a measured, classified
// store size and the retention config. When StoreMaxGB == 0 the cap is
// disabled and the result reports MaxBytes == 0 with all threshold flags
// false (no thresholds to cross); the honest split is still populated. The
// thresholds mirror EmergencyQuotaGate's ceiling math and the daemon's
// high-watermark computation.
func ComputePressureState(size StoreSize, cfg Config) PressureState {
	ps := PressureState{
		Bytes:            size.Bytes,
		ReclaimableBytes: size.ReclaimableBytes(),
		PinnedBytes:      size.PinnedBytes(),
		EventLogBytes:    size.EventLogBytes,
	}
	if cfg.StoreMaxGB <= 0 {
		return ps
	}
	ps.MaxBytes = int64(cfg.StoreMaxGB * bytesPerGB)
	if cfg.StoreHighWatermark > 0 {
		ps.HighWatermarkBytes = int64(cfg.StoreHighWatermark * cfg.StoreMaxGB * bytesPerGB)
		ps.OverHighWatermark = size.Bytes >= ps.HighWatermarkBytes
		ps.WatermarkUnreachable = ps.PinnedBytes >= ps.HighWatermarkBytes
	}
	if cfg.StoreEmergencyQuota > 0 {
		ps.EmergencyBytes = int64(cfg.StoreEmergencyQuota * cfg.StoreMaxGB * bytesPerGB)
		ps.OverEmergency = size.Bytes >= ps.EmergencyBytes
	}
	return ps
}
