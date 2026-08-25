package main

import (
	"bytes"
	"testing"

	"github.com/aplexica/aplexica/internal/daemon"
	"github.com/stretchr/testify/assert"
)

// TestRenderSyncSuppressions_DisabledDeviceLeadsWithTheConsequence verifies
// that a structurally disabled sync engine is named even when the remaining
// status indicators look healthy. The first line must state the consequence in
// plain language and include the command that fixes it.
func TestRenderSyncSuppressions_DisabledDeviceLeadsWithTheConsequence(t *testing.T) {
	var buf bytes.Buffer
	renderSyncSuppressions(&buf, &daemon.StatusInfo{
		SyncDisabledReason: "no sync rules are configured, so nothing is copied between agents on this device",
		SyncSuppressions: []map[string]any{
			{"agent": "codex", "reason": "no_rules_configured", "class": "policy",
				"count": float64(120), "explain": "No sync rules are configured.",
				"remedy": "aplexica rules add <file.toml>"},
		},
	})
	got := buf.String()
	assert.Contains(t, got, "Sync: DISABLED")
	assert.Contains(t, got, "nothing is copied between agents")
	assert.Contains(t, got, "aplexica rules add")
	// The device-wide verdict replaces the per-rule noise: when sync is off
	// entirely, counting the individual denials adds nothing.
	assert.NotContains(t, got, "not copied by your own rules")
}

// TestRenderSyncSuppressions_DefectsAreNamedIndividually: a fault is not the
// user's doing, so it is called out with its remedy rather than aggregated.
func TestRenderSyncSuppressions_DefectsAreNamedIndividually(t *testing.T) {
	var buf bytes.Buffer
	renderSyncSuppressions(&buf, &daemon.StatusInfo{
		SyncSuppressions: []map[string]any{
			{"agent": "claude-code", "reason": "export_failed", "class": "defect",
				"count": float64(3), "explain": "Writing the artifact into this agent failed.",
				"remedy": "aplexica status  (retried automatically)"},
		},
	})
	got := buf.String()
	assert.Contains(t, got, "claude-code")
	assert.Contains(t, got, "Writing the artifact into this agent failed.")
	assert.Contains(t, got, "Fix with:")
}

// TestRenderSyncSuppressions_PolicyIsSummarizedNotNagged: routing rules doing
// exactly what the user configured must be visible (so a missing file is
// explainable) without turning status into a wall of expected behaviour.
func TestRenderSyncSuppressions_PolicyIsSummarizedNotNagged(t *testing.T) {
	var buf bytes.Buffer
	renderSyncSuppressions(&buf, &daemon.StatusInfo{
		SyncSuppressions: []map[string]any{
			{"agent": "kilo", "reason": "rules_denied", "class": "policy", "count": float64(40),
				"explain": "Your sync rules do not route this artifact to this agent."},
			{"agent": "openclaw", "reason": "rules_denied", "class": "policy", "count": float64(2),
				"explain": "Your sync rules do not route this artifact to this agent."},
		},
	})
	got := buf.String()
	assert.Contains(t, got, "42 writes not copied by your own rules")
	assert.Contains(t, got, "--json")
}

// TestRenderSyncSuppressions_CapabilityStaysOutOfPlainStatus: "this agent is
// not installed" is a permanent fact, not something to act on.
func TestRenderSyncSuppressions_CapabilityStaysOutOfPlainStatus(t *testing.T) {
	var buf bytes.Buffer
	renderSyncSuppressions(&buf, &daemon.StatusInfo{
		SyncSuppressions: []map[string]any{
			{"agent": "hermes", "reason": "adapter_not_installed", "class": "capability",
				"count": float64(900), "explain": "This agent is not installed on this device."},
		},
	})
	assert.Empty(t, buf.String())
}

// TestRenderSyncSuppressions_HealthyDeviceIsSilent: a converged device must
// print nothing, or the signal is lost in routine noise.
func TestRenderSyncSuppressions_HealthyDeviceIsSilent(t *testing.T) {
	var buf bytes.Buffer
	renderSyncSuppressions(&buf, &daemon.StatusInfo{})
	assert.Empty(t, buf.String())
}

// D3 at the surface: a stale safety-verification row must not outlive the
// verification state that produced it. A row the daemon has marked stale must
// not be printed as a problem the operator still has.
func TestRenderSyncSuppressions_StaleRowIsNotPrintedAsCurrent(t *testing.T) {
	var buf bytes.Buffer
	renderSyncSuppressions(&buf, &daemon.StatusInfo{
		SyncSuppressions: []map[string]any{
			{"agent": "codex", "reason": "adapter_blocked_safety", "class": "defect",
				"count": float64(1), "stale": true,
				"explain": "Writes to this agent are blocked until its safety snapshot finishes verifying.",
				"remedy":  "aplexica daemon restart"},
		},
	})
	assert.Empty(t, buf.String(), "a resolved condition is history, not a current fault")
}

// The mirror image: the same row while the condition really holds must print.
func TestRenderSyncSuppressions_LiveBlockStillPrints(t *testing.T) {
	var buf bytes.Buffer
	renderSyncSuppressions(&buf, &daemon.StatusInfo{
		SyncSuppressions: []map[string]any{
			{"agent": "codex", "reason": "adapter_blocked_safety", "class": "defect",
				"count":   float64(1),
				"explain": "Writes to this agent are blocked until its safety snapshot finishes verifying.",
				"remedy":  "aplexica daemon restart"},
		},
	})
	assert.Contains(t, buf.String(), "safety snapshot")
}
