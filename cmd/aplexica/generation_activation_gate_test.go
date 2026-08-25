package main

import (
	"errors"
	"testing"

	"github.com/aplexica/aplexica/internal/plugin/proto"
	"github.com/stretchr/testify/require"
)

type recordingAdmissionActivationGate struct {
	blocked string
	scopes  []string
}

func (g *recordingAdmissionActivationGate) Check(scope string) error {
	g.scopes = append(g.scopes, scope)
	if scope == g.blocked {
		return errors.New("pending activation")
	}
	return nil
}

func TestGenerationActivationAdmissionBlocksPendingScopeAndReopens(t *testing.T) {
	gate := &recordingAdmissionActivationGate{blocked: "account"}
	events := []proto.RemoteEvent{{EventID: "event-1"}}
	require.Error(t, checkGenerationActivationAdmission(gate, events))
	require.Equal(t, []string{"account"}, gate.scopes)

	gate.blocked = ""
	gate.scopes = nil
	require.NoError(t, checkGenerationActivationAdmission(gate, events))
	require.Equal(t, []string{"account"}, gate.scopes)
}

func TestGenerationActivationAdmissionChecksEveryScope(t *testing.T) {
	gate := &recordingAdmissionActivationGate{}
	events := []proto.RemoteEvent{
		{NamespaceID: "0197f30a-3c58-7000-8000-000000000002"},
		{},
		{NamespaceID: "0197f30a-3c58-7000-8000-000000000001"},
	}
	require.NoError(t, checkGenerationActivationAdmission(gate, events))
	require.Equal(t, []string{
		"0197f30a-3c58-7000-8000-000000000001",
		"0197f30a-3c58-7000-8000-000000000002",
		"account",
	}, gate.scopes)
}
