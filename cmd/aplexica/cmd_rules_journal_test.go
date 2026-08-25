package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/aplexica/aplexica/internal/syncrules"
	"github.com/stretchr/testify/require"
)

// TestRulesWebAccessor_JournalsChangesToAuditLog: FR-05.13 requires every rule
// change to be journaled to ~/.aplexica/logs/rule-changes.jsonl, unconditionally.
// The CLI paths journal; the portal/web accessor must too.
func TestRulesWebAccessor_JournalsChangesToAuditLog(t *testing.T) {
	tmp := t.TempDir()
	rulesPath := filepath.Join(tmp, "cfg", "rules.toml")
	require.NoError(t, os.MkdirAll(filepath.Dir(rulesPath), 0o755))
	require.NoError(t, writeUserRules(rulesPath, syncrules.Config{}))
	acc := &rulesWebAccessor{deps: &webAPIDeps{rulesPath: rulesPath, cloudRules: newCloudRuleStore()}}

	journalPath := filepath.Join(tmp, "logs", "rule-changes.jsonl")

	require.NoError(t, acc.Add(syncrules.Rule{Name: "audit-test"}))
	data, err := os.ReadFile(journalPath)
	require.NoError(t, err, "FR-05.13: a portal Add must journal to rule-changes.jsonl")
	require.Contains(t, string(data), `"op":"add"`)
	require.Contains(t, string(data), `"name":"audit-test"`)

	require.NoError(t, acc.Delete("audit-test"))
	data, err = os.ReadFile(journalPath)
	require.NoError(t, err)
	require.Contains(t, string(data), `"op":"remove"`)
}
