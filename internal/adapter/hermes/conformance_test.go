package hermes

import (
	"os"
	"testing"

	"github.com/aplexica/aplexica/internal/adapter"
	"github.com/aplexica/aplexica/internal/conformance"
)

func TestConformance_Hermes(t *testing.T) {
	opts := conformance.Opts{
		Name: "hermes",
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
		seq := conformance.DefaultWatchSequence("AGENTS.md")
		for _, r := range conformance.RunWatchCorrectness(t, opts, seq) {
			t.Errorf("step %d: %s", r.Step, r.Failure)
		}
	})
	t.Run("performance-scan", func(t *testing.T) {
		budget := conformance.ScaledPerfBudget()
		if os.Getenv("APLEXICA_PERF_FULL") == "1" {
			budget = conformance.DefaultPerfBudget()
		}
		res := conformance.RunPerformanceScan(t, opts, budget)
		if res.Failure != "" {
			t.Errorf("perf: %s — %.1f MiB/s", res.Failure, res.ThroughputMBs)
		}
		t.Logf("perf: %d files in %s — %.1f MiB/s",
			res.FilesScanned, res.Duration, res.ThroughputMBs)
	})
}
