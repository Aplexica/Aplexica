package metrics

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRegistry_RendersUptime(t *testing.T) {
	r := NewRegistry(time.Date(2026, 5, 25, 8, 0, 0, 0, time.UTC))
	out := r.Render(time.Date(2026, 5, 25, 8, 1, 30, 0, time.UTC))
	require.Contains(t, out, "# HELP aplexica_uptime_seconds")
	require.Contains(t, out, "# TYPE aplexica_uptime_seconds gauge")
	require.Contains(t, out, "aplexica_uptime_seconds 90")
}

func TestRegistry_CounterWithLabels(t *testing.T) {
	r := NewRegistry(time.Now())
	r.IncCounter("aplexica_events_total", map[string]string{
		"kind":      "memory",
		"direction": "inbound",
	})
	r.IncCounter("aplexica_events_total", map[string]string{
		"kind":      "memory",
		"direction": "inbound",
	})
	r.IncCounter("aplexica_events_total", map[string]string{
		"kind":      "skill",
		"direction": "outbound",
	})

	out := r.Render(time.Now())
	require.Contains(t, out, "# TYPE aplexica_events_total counter")
	require.Contains(t, out,
		`aplexica_events_total{direction="inbound",kind="memory"} 2`)
	require.Contains(t, out,
		`aplexica_events_total{direction="outbound",kind="skill"} 1`)
}

func TestRegistry_Gauge(t *testing.T) {
	r := NewRegistry(time.Now())
	r.SetGauge("aplexica_pending_projects", 3, nil)
	out := r.Render(time.Now())
	require.Contains(t, out, "# TYPE aplexica_pending_projects gauge")
	require.Contains(t, out, "aplexica_pending_projects 3")
}

func TestRegistry_AdapterStates(t *testing.T) {
	r := NewRegistry(time.Now())
	r.SetAdapterState("claude-code", "active")
	r.SetAdapterState("codex", "quarantined")
	out := r.Render(time.Now())
	require.Contains(t, out, `aplexica_adapter_state{name="claude-code",state="active"} 1`)
	require.Contains(t, out, `aplexica_adapter_state{name="codex",state="quarantined"} 1`)
}

func TestRegistry_Handler_ServesPlainTextWithCorrectType(t *testing.T) {
	r := NewRegistry(time.Now())
	r.IncCounter("aplexica_conflicts_total", nil)

	srv := httptest.NewServer(r.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/metrics")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.True(t, strings.HasPrefix(resp.Header.Get("Content-Type"), "text/plain"))

	body, _ := io.ReadAll(resp.Body)
	require.Contains(t, string(body), "aplexica_conflicts_total 1")
}

func TestRegistry_LabelEscaping(t *testing.T) {
	r := NewRegistry(time.Now())
	r.IncCounter("aplexica_events_total", map[string]string{
		"kind": `weird "value" with \backslashes`,
	})
	out := r.Render(time.Now())
	require.Contains(t, out,
		`aplexica_events_total{kind="weird \"value\" with \\backslashes"} 1`)
}

// TestRegistry_HistogramRendersBucketsSumCount asserts that observing into a
// histogram renders the Prometheus _bucket / _sum / _count exposition triplet,
// with cumulative bucket counts and the mandatory +Inf bucket.
func TestRegistry_HistogramRendersBucketsSumCount(t *testing.T) {
	r := NewRegistry(time.Now())
	// Two observations that land in different buckets given the default
	// SyncLatencyBuckets (seconds): 0.05 and 1.5.
	r.ObserveHistogram("sync_latency_seconds", 0.05, nil)
	r.ObserveHistogram("sync_latency_seconds", 1.5, nil)

	out := r.Render(time.Now())
	require.Contains(t, out, "# TYPE sync_latency_seconds histogram")
	// +Inf bucket counts every observation.
	require.Contains(t, out, `sync_latency_seconds_bucket{le="+Inf"} 2`)
	// _count equals the number of observations.
	require.Contains(t, out, "sync_latency_seconds_count 2")
	// _sum is the total of observed values (0.05 + 1.5 = 1.55).
	require.Contains(t, out, "sync_latency_seconds_sum 1.55")
	// A finite bucket at/above 0.05 but below 1.5 sees exactly one observation
	// (cumulative). 0.1 is one of the default bucket bounds.
	require.Contains(t, out, `sync_latency_seconds_bucket{le="0.1"} 1`)
}

// TestRegistry_HistogramWithLabels asserts labeled histograms render one
// bucket/sum/count series per label set.
func TestRegistry_HistogramWithLabels(t *testing.T) {
	r := NewRegistry(time.Now())
	r.ObserveHistogram("sync_latency_seconds", 0.2, map[string]string{"phase": "fanout"})

	out := r.Render(time.Now())
	require.Contains(t, out, `sync_latency_seconds_bucket{phase="fanout",le="+Inf"} 1`)
	require.Contains(t, out, `sync_latency_seconds_count{phase="fanout"} 1`)
	require.Contains(t, out, `sync_latency_seconds_sum{phase="fanout"} 0.2`)
}

// TestRegistry_RegisterEmitsTypeHelpWithNoSeries asserts that registering a
// family with no observations still emits its # TYPE / # HELP header (so a
// scrape advertises the full mandated set), with no child series.
func TestRegistry_RegisterEmitsTypeHelpWithNoSeries(t *testing.T) {
	r := NewRegistry(time.Now())
	r.RegisterCounter("adapter_errors_total")
	r.RegisterGauge("store_size_bytes")
	r.RegisterHistogram("sync_latency_seconds")

	out := r.Render(time.Now())
	require.Contains(t, out, "# TYPE adapter_errors_total counter")
	require.Contains(t, out, "# TYPE store_size_bytes gauge")
	require.Contains(t, out, "# TYPE sync_latency_seconds histogram")
	// No spurious child series for the un-observed counter: the only
	// adapter_errors_total line is the TYPE/HELP, never a bare value row.
	require.NotContains(t, out, "\nadapter_errors_total 0\n")
}

// TestRegistry_NFR10MandatedFamilies asserts the registry can render every
// metric family NFR-10 §5.2 mandates, using the EXACT spec names. Each is
// touched once so it appears in the exposition output.
func TestRegistry_NFR10MandatedFamilies(t *testing.T) {
	r := NewRegistry(time.Now())
	r.AddCounter("adapter_events_inbound_total", 1, map[string]string{"adapter": "claudecode"})
	r.AddCounter("adapter_events_outbound_total", 1, map[string]string{"adapter": "claudecode"})
	r.AddCounter("adapter_errors_total", 1, map[string]string{"adapter": "claudecode"})
	r.SetGauge("store_size_bytes", 4096, nil)
	r.SetGauge("store_events_total", 12, nil)
	r.SetGauge("conflict_count", 0, nil)
	r.SetGauge("queue_depth", 3, nil)
	r.ObserveHistogram("sync_latency_seconds", 0.3, nil)

	out := r.Render(time.Now())
	for _, want := range []string{
		"daemon_uptime_seconds",
		"adapter_events_inbound_total",
		"adapter_events_outbound_total",
		"adapter_errors_total",
		"store_size_bytes",
		"store_events_total",
		"conflict_count",
		"queue_depth",
		"sync_latency_seconds",
	} {
		require.Contains(t, out, want, "mandated NFR-10 metric family %q must render", want)
	}
	// daemon_uptime_seconds is a synthesized gauge (no caller touches it).
	require.Contains(t, out, "# TYPE daemon_uptime_seconds gauge")
}
