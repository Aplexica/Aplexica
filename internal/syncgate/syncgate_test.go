package syncgate

import "testing"

func TestGate_DefaultDeniesUntilEnabled(t *testing.T) {
	g := New(Config{}) // empty config = nothing enabled
	if g.Enabled("codex") {
		t.Errorf("default gate must deny fan-out until configured")
	}
}

func TestGate_EnableAll(t *testing.T) {
	g := New(Config{All: true})
	if !g.Enabled("codex") {
		t.Errorf("All=true must enable every agent")
	}
}

func TestGate_PerAgent(t *testing.T) {
	g := New(Config{Agents: map[string]bool{"codex": true}})
	if !g.Enabled("codex") {
		t.Errorf("codex explicitly enabled")
	}
	if g.Enabled("hermes") {
		t.Errorf("hermes not enabled -> denied")
	}
}

func TestGate_PerAgentOverridesAll(t *testing.T) {
	// An explicit false override wins even when All=true (lets a user
	// enable sync broadly but exclude one agent).
	g := New(Config{All: true, Agents: map[string]bool{"hermes": false}})
	if !g.Enabled("codex") {
		t.Errorf("codex should follow All=true")
	}
	if g.Enabled("hermes") {
		t.Errorf("explicit hermes=false must override All=true")
	}
}

func TestGate_NilIsPermissive(t *testing.T) {
	// A nil *Gate means "no gating configured" — pre-Slice-3 behavior, so
	// existing callers/tests that don't set a gate keep fanning out.
	var g *Gate
	if !g.Enabled("anything") {
		t.Errorf("nil gate must be permissive (no gating)")
	}
}
