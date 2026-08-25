package main

import (
	"fmt"
	"sort"

	"github.com/aplexica/aplexica/internal/plugin/proto"
)

type generationActivationAdmissionGate interface {
	Check(scope string) error
}

// checkGenerationActivationAdmission checks every scope in one delivery. It
// is deliberately separate from securityepoch admission: a durable pending
// generation is a publication/activation transaction, not a replacement for
// the current security-epoch barrier.
func checkGenerationActivationAdmission(gate generationActivationAdmissionGate, events []proto.RemoteEvent) error {
	if gate == nil || len(events) == 0 {
		return fmt.Errorf("generation activation admission gate unavailable")
	}
	scopes := make(map[string]struct{}, len(events))
	for _, event := range events {
		scope := event.NamespaceID
		if scope == "" {
			scope = "account"
		}
		scopes[scope] = struct{}{}
	}
	ordered := make([]string, 0, len(scopes))
	for scope := range scopes {
		ordered = append(ordered, scope)
	}
	sort.Strings(ordered)
	for _, scope := range ordered {
		if err := gate.Check(scope); err != nil {
			return err
		}
	}
	return nil
}
