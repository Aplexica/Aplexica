package kilo

import (
	"os"
	"testing"

	"github.com/aplexica/aplexica/internal/adapter"
	"github.com/aplexica/aplexica/internal/conformance"
)

// Kilo has narrower native-form support than the other V1 adapters:
// no AGENTS.md, no SKILL.md (modes/custom-instructions instead), no
// CLAUDE.md/.mcp.json. The DefaultFixtures() SkipIfAdapter list
// already excludes kilo from those — this test still runs the
// surface (and exercises the harness scaffolding for the kilo
// package), it just lands on the SkipIfAdapter branch for every
// fixture today. Adding kilo-native fixtures (modes, custom
// instructions) is M1 work.
func TestConformance_Kilo(t *testing.T) {
	opts := conformance.Opts{
		Name: "kilo",
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
