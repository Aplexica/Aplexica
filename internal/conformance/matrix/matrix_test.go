// Package matrix runs the BRD-02 §5.4 #4 cross-conversion harness
// across all 5 V1 adapters. It's a separate test package because the
// per-adapter conformance tests live in their own packages and can't
// import each other; this package lives at the lowest common
// dependency point (after the adapters) and produces the full N×N
// (5×5 = 25 cell) report.
package matrix

import (
	"testing"

	"github.com/aplexica/aplexica/internal/adapter"
	"github.com/aplexica/aplexica/internal/adapter/claudecode"
	"github.com/aplexica/aplexica/internal/adapter/codex"
	"github.com/aplexica/aplexica/internal/adapter/hermes"
	"github.com/aplexica/aplexica/internal/adapter/kilo"
	"github.com/aplexica/aplexica/internal/adapter/openclaw"
	"github.com/aplexica/aplexica/internal/conformance"
)

// agentsMDFixture is the BRD-02 §6.1 AAIF AGENTS.md cross-tool fixture
// — every V1 adapter supports this as a memory artifact.
var agentsMDFixture = conformance.Fixture{
	Label:      "agents-md",
	NativeName: "AGENTS.md",
	Body: `# Project agents

Cross-conversion fixture for BRD-02 §5.4 #4.

## Conventions

- TDD
- No magic numbers
`,
}

// allEntries returns one CrossEntry per V1 adapter, each carrying the
// shared AGENTS.md fixture so the 5×5 matrix is over a single
// universally-supported artifact.
func allEntries(t *testing.T) []conformance.CrossEntry {
	t.Helper()
	return []conformance.CrossEntry{
		{
			Opts: conformance.Opts{
				Name: "claude-code",
				Build: func() adapter.Adapter {
					a := claudecode.New()
					a.HomeDir = t.TempDir()
					return a
				},
			},
			Fixture: agentsMDFixture,
		},
		{
			Opts: conformance.Opts{
				Name: "codex",
				Build: func() adapter.Adapter {
					a := codex.New()
					a.HomeDir = t.TempDir()
					return a
				},
			},
			Fixture: agentsMDFixture,
		},
		{
			Opts: conformance.Opts{
				Name: "hermes",
				Build: func() adapter.Adapter {
					a := hermes.New()
					a.HomeDir = t.TempDir()
					return a
				},
			},
			Fixture: agentsMDFixture,
		},
		{
			Opts: conformance.Opts{
				Name: "openclaw",
				Build: func() adapter.Adapter {
					a := openclaw.New()
					a.HomeDir = t.TempDir()
					return a
				},
			},
			Fixture: agentsMDFixture,
		},
		{
			Opts: conformance.Opts{
				Name: "kilo",
				Build: func() adapter.Adapter {
					a := kilo.New()
					a.HomeDir = t.TempDir()
					return a
				},
			},
			Fixture: agentsMDFixture,
		},
	}
}

func TestCrossConversion_5x5_AGENTSMD(t *testing.T) {
	// BRD-02 §5.4 #4 + M1 exit criterion: "aplexica convert <bundle>
	// --to <any-agent> produces a valid target bundle for every
	// (source, target) pair." For AGENTS.md, every cell should be
	// either "ok" (target supports memory artifacts) or one of the
	// documented-by-design outcomes. No "error" outcomes are
	// permitted.
	entries := allEntries(t)
	rep := conformance.RunCrossConversion(t, entries)

	for _, cell := range rep.Cells {
		switch cell.Outcome {
		case "ok", "unsupported", "tombstoned":
			// Acceptable. "ok" is the desired norm; "unsupported"
			// happens when a target adapter genuinely doesn't model
			// the artifact kind (e.g. kilo + conversation, which isn't
			// in this fixture anyway). "tombstoned" can't happen for
			// a fresh AGENTS.md.
		case "skipped":
			// Skipped means the SOURCE couldn't Import its own
			// fixture — that's a separate per-adapter conformance bug
			// (already covered by the per-adapter suites). Surface
			// here too for visibility.
			t.Errorf("cross-conversion %s → %s: source skipped (%s)",
				cell.Source, cell.Target, cell.Detail)
		case "error":
			t.Errorf("cross-conversion %s → %s: error (%s)",
				cell.Source, cell.Target, cell.Detail)
		default:
			t.Errorf("cross-conversion %s → %s: unknown outcome %q (%s)",
				cell.Source, cell.Target, cell.Outcome, cell.Detail)
		}
	}

	// Visibility: print the full matrix so failures and warnings
	// stay grepable even when every cell is ok.
	t.Logf("cross-conversion 5×5 matrix (AGENTS.md fixture):")
	for _, cell := range rep.Cells {
		t.Logf("  %-13s → %-13s : %-12s %s",
			cell.Source, cell.Target, cell.Outcome, cell.Detail)
	}

	// Sanity check on the matrix shape.
	expected := len(entries) * len(entries)
	if len(rep.Cells) != expected {
		t.Errorf("expected %d cells in N×N matrix, got %d", expected, len(rep.Cells))
	}
}
