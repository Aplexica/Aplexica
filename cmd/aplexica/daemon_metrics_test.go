package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/metrics"
	"github.com/stretchr/testify/require"
)

// The daemon seeding helper must register every NFR-10 §5.2 mandated family so
// the existing /metrics handler exposes the full set on a fresh registry, and
// the sync-latency adapter must record into sync_latency_seconds.
func TestDaemonMetrics_MandatedSetExposedViaHandler(t *testing.T) {
	reg := metrics.NewRegistry(time.Now())
	seedMandatedMetricFamilies(reg)

	// The orchestrator-side adapter records one import->fan-out latency.
	obs := &syncLatencyMetric{reg: reg}
	obs.ObserveSyncLatency(0.42)

	srv := httptest.NewServer(reg.Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/metrics")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body, _ := io.ReadAll(resp.Body)
	out := string(body)

	// Every mandated family appears in the exposition.
	for _, want := range []string{
		"daemon_uptime_seconds",
		metricAdapterEventsInboundTotal,
		metricAdapterEventsOutboundTotal,
		metricAdapterErrorsTotal,
		metricStoreSizeBytes,
		metricStoreEventsTotal,
		metricConflictCount,
		metricQueueDepth,
		metricSyncLatencySeconds,
	} {
		require.Contains(t, out, want, "mandated NFR-10 family %q must be exposed", want)
	}

	// The histogram recorded the observation.
	require.Contains(t, out, "# TYPE sync_latency_seconds histogram")
	require.Contains(t, out, "sync_latency_seconds_count 1")
	require.Contains(t, out, "sync_latency_seconds_sum 0.42")
}
