package claudecode

import (
	"os"
	"testing"

	"github.com/aplexica/aplexica/internal/adapter"
	"github.com/aplexica/aplexica/internal/conformance"
)

// TestConformance_ClaudeCode runs the shared BRD-02 §5.4 conformance
// suite against the claude-code adapter.
//
// v0.76.0 wires claude-code as the first adapter through the harness.
// Other adapters (codex, hermes, openclaw, kilo) follow the same
// one-liner pattern; they're added incrementally as their
// SkipIfAdapter exclusion lists are validated.
func TestConformance_ClaudeCode(t *testing.T) {
	opts := conformance.Opts{
		Name: "claude-code",
		Build: func() adapter.Adapter {
			a := New()
			a.HomeDir = t.TempDir()
			return a
		},
	}
	conformance.Run(t, opts)
	t.Run("capability-declaration", func(t *testing.T) {
		if fails := conformance.RunCapabilityCheck(t, opts); len(fails) > 0 {
			for _, f := range fails {
				t.Error(f)
			}
		}
	})
	t.Run("watch-correctness", func(t *testing.T) {
		// BRD-02 §5.4 #3 — write-twice produces create then update.
		seq := conformance.DefaultWatchSequence("AGENTS.md")
		for _, r := range conformance.RunWatchCorrectness(t, opts, seq) {
			t.Errorf("step %d: %s", r.Step, r.Failure)
		}
	})
	t.Run("performance-scan", func(t *testing.T) {
		// BRD-02 §5.4 #6 — initial scan of corpus completes under budget.
		budget := conformance.ScaledPerfBudget()
		if os.Getenv("APLEXICA_PERF_FULL") == "1" {
			budget = conformance.DefaultPerfBudget() // 1 GiB / 30s
		}
		res := conformance.RunPerformanceScan(t, opts, budget)
		if res.Failure != "" {
			t.Errorf("perf: %s (scanned %d files, %.1f MiB in %s; %.1f MiB/s)",
				res.Failure, res.FilesScanned,
				float64(res.BytesScanned)/(1<<20), res.Duration, res.ThroughputMBs)
		}
		t.Logf("perf: scanned %d files (%.1f MiB) in %s — %.1f MiB/s",
			res.FilesScanned, float64(res.BytesScanned)/(1<<20),
			res.Duration, res.ThroughputMBs)
	})
}
