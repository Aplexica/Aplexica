package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/aplexica/aplexica/internal/syncrules"
)

// buildRulesEngine constructs the orchestrator's syncrules.Engine from
// the user's rules file (~/.aplexica/rules.toml by default) ONLY.
//
// Safe-by-default (reverses BRD-05 §6 #1): no rules ship as always-on
// defaults. On a fresh install the user file is absent → the engine
// holds zero rules → Evaluate denies all cross-agent fan-out until the
// user adds a rule (or applies a preset; see syncrules.ParseDefault /
// DefaultRulesTOML which are kept for the presets catalog).
//
// On a user-file parse error, falls back to an EMPTY engine (NOT the
// shipped defaults) and returns the error so the daemon can log a
// warning — a broken rules file must never silently re-enable fan-out.
func buildRulesEngine() (*syncrules.Engine, error) {
	home, _ := os.UserHomeDir()
	userPath := filepath.Join(home, ".aplexica", "rules.toml")
	return buildRulesEngineFromPath(userPath)
}

// buildRulesEngineFromPath is buildRulesEngine over an explicit user
// rules file. The local web UI's rules accessor calls this with the
// daemon's configured rulesPath after each Add/Update/Delete so the live
// orchestrator engine can be hot-swapped (see rulesWebAccessor) without
// a daemon restart. Same safe-by-default + empty-on-parse-error contract
// as buildRulesEngine.
func buildRulesEngineFromPath(userPath string) (*syncrules.Engine, error) {
	user, uerr := loadUserRulesQuiet(userPath)
	if uerr != nil {
		eng, _ := syncrules.New(nil)
		return eng, uerr
	}
	return syncrules.New(user.Sync.Rules)
}

// loadUserRulesQuiet reads + parses a user rules file, returning an
// empty Config (no error) when the file does not exist.
func loadUserRulesQuiet(path string) (syncrules.Config, error) {
	var out syncrules.Config
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return out, err
	}
	return syncrules.Parse(data)
}

// rulesSignature returns a stable fingerprint of a merged ruleset so the
// reload path can detect whether a rebuild actually changed anything
// (and only then trigger a fan-out backfill). JSON is fine here: the
// signature only ever compares against itself.
func rulesSignature(rules []syncrules.Rule) string {
	data, err := json.Marshal(rules)
	if err != nil {
		return fmt.Sprintf("unmarshalable:%d", len(rules))
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
