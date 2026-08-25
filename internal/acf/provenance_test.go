package acf

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestProvenance_CausedByRoundTrip(t *testing.T) {
	p := Provenance{DeviceID: "dev1", SourceAgent: "claude-code", AdapterVersion: "0.1.0", CausedBy: "abc123"}
	b, err := json.Marshal(p)
	require.NoError(t, err)
	require.Contains(t, string(b), `"causedBy":"abc123"`)

	var got Provenance
	require.NoError(t, json.Unmarshal(b, &got))
	require.Equal(t, "abc123", got.CausedBy)
}

func TestProvenance_CausedByOmittedWhenEmpty(t *testing.T) {
	p := Provenance{DeviceID: "dev1", SourceAgent: "claude-code", AdapterVersion: "0.1.0"}
	b, err := json.Marshal(p)
	require.NoError(t, err)
	require.NotContains(t, string(b), "causedBy", "empty CausedBy must be omitted by omitempty")
}
