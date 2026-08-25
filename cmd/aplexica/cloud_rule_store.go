package main

import (
	"sync"

	"github.com/aplexica/aplexica/internal/syncrules"
)

// cloudRuleStore holds the most-recent cloud-authored selective-sync
// ruleset pushed via remote.rules_update. It is the daemon-side source of
// truth for two things:
//
//  1. Merging cloud rules into the live engine alongside the user's
//     rules.toml (so a local rule edit no longer clobbers cloud rules, and
//     a cloud push no longer clobbers local rules).
//  2. Surfacing cloud rules read-only in the local portal's rules list.
//
// Concurrency-safe: written from the remote-runner OnRulesUpdate callback,
// read from the web API rules accessor.
type cloudRuleStore struct {
	mu    sync.RWMutex
	rules []syncrules.Rule
}

func newCloudRuleStore() *cloudRuleStore { return &cloudRuleStore{} }

func (s *cloudRuleStore) set(rules []syncrules.Rule) {
	cp := append([]syncrules.Rule(nil), rules...)
	s.mu.Lock()
	s.rules = cp
	s.mu.Unlock()
}

// get returns a copy of the current cloud ruleset. Safe on a nil receiver
// (returns nil) so accessors built without a store — e.g. unit tests — do
// not panic.
func (s *cloudRuleStore) get() []syncrules.Rule {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]syncrules.Rule(nil), s.rules...)
}

// mergeRules layers cloud rules over user rules: a cloud rule replaces a
// user rule of the same name (cloud wins), and the remaining user rules
// are kept. Order is surviving user rules first (declaration order), then
// the cloud rules. The result feeds syncrules.New for the live engine and
// (after Source tagging) the local rules list.
func mergeRules(user, cloud []syncrules.Rule) []syncrules.Rule {
	cloudNames := make(map[string]struct{}, len(cloud))
	for _, c := range cloud {
		cloudNames[c.Name] = struct{}{}
	}
	out := make([]syncrules.Rule, 0, len(user)+len(cloud))
	for _, u := range user {
		if _, clobbered := cloudNames[u.Name]; !clobbered {
			out = append(out, u)
		}
	}
	out = append(out, cloud...)
	return out
}
