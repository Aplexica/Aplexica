package main

import "github.com/aplexica/aplexica/internal/metrics"

// NFR-10 §5.2 mandated Prometheus metric family names. Named here (rather than
// repeated as bare string literals at each call site) so the daemon wiring and
// the registry seeding never drift, and the full mandated set is greppable in
// one place. The daemon_uptime_seconds gauge is synthesized by the registry
// itself, so it is not seeded here.
const (
	metricAdapterEventsInboundTotal  = "adapter_events_inbound_total"
	metricAdapterEventsOutboundTotal = "adapter_events_outbound_total"
	metricAdapterErrorsTotal         = "adapter_errors_total"
	metricStoreSizeBytes             = "store_size_bytes"
	metricStoreEventsTotal           = "store_events_total"
	metricConflictCount              = "conflict_count"
	metricQueueDepth                 = "queue_depth"
	metricSyncLatencySeconds         = "sync_latency_seconds"
)

// seedMandatedMetricFamilies pre-declares every NFR-10 §5.2 metric family on the
// registry so a /metrics scrape always advertises the full mandated set with the
// correct TYPE/HELP, even on a freshly-started daemon before any event has moved
// a counter. Families with a live source are subsequently populated:
//   - sync_latency_seconds — observed on the orchestrator import->fan-out path.
//   - queue_depth          — refreshed at scrape time from orch.PendingImports().
//
// The remaining families (adapter_events_inbound_total / _outbound_total,
// adapter_errors_total, store_size_bytes, store_events_total, conflict_count)
// are registered here but not yet fed: the daemon has no zero-cost accessor for
// per-adapter event totals or a live store size/event count today. They render
// as headers with no series until that instrumentation lands; this is the
// documented gap (NFR-10 NOTE) rather than a silent omission.
func seedMandatedMetricFamilies(reg *metrics.Registry) {
	reg.RegisterCounter(metricAdapterEventsInboundTotal)
	reg.RegisterCounter(metricAdapterEventsOutboundTotal)
	reg.RegisterCounter(metricAdapterErrorsTotal)
	reg.RegisterGauge(metricStoreSizeBytes)
	reg.RegisterGauge(metricStoreEventsTotal)
	reg.RegisterGauge(metricConflictCount)
	reg.RegisterGauge(metricQueueDepth)
	reg.RegisterHistogram(metricSyncLatencySeconds)
}

// syncLatencyMetric adapts a *metrics.Registry onto the orchestrator's
// syncd.SyncLatencyObserver seam: each import->fan-out latency the
// orchestrator reports is recorded into the sync_latency_seconds histogram.
// Declared in the daemon (not internal/sync) so the dependency edge stays
// one-way — internal/sync names only its own narrow interface and never
// imports internal/metrics.
type syncLatencyMetric struct {
	reg *metrics.Registry
}

// ObserveSyncLatency records one observation into sync_latency_seconds. No
// labels: the metric is daemon-global, which keeps cardinality bounded (a
// single series) per the task's no-unbounded-labels constraint.
func (m *syncLatencyMetric) ObserveSyncLatency(seconds float64) {
	m.reg.ObserveHistogram(metricSyncLatencySeconds, seconds, nil)
}
