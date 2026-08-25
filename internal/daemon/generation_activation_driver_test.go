package daemon

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/aplexica/aplexica/internal/plugin/proto"
	"github.com/stretchr/testify/require"
)

func TestGenerationStreamEpochUsesExactNegotiatedScope(t *testing.T) {
	negotiation := proto.RemoteNegotiateSyncV1Result{
		SelectedProtocol: generationActivationProtocolV1, StreamEpoch: "account-legacy",
		Streams: []proto.RemoteStreamDescriptorV1{
			{StreamEpoch: "account-current"},
			{NamespaceID: "0197f30a-3c58-7000-8000-000000000001", StreamEpoch: "namespace-current"},
		},
	}
	require.Equal(t, "account-current", generationStreamEpoch(negotiation, ""))
	require.Equal(t, "namespace-current", generationStreamEpoch(negotiation, "0197f30a-3c58-7000-8000-000000000001"))
	require.Empty(t, generationStreamEpoch(negotiation, "0197f30a-3c58-7000-8000-000000000002"))
	negotiation.SelectedProtocol = 0
	require.Empty(t, generationStreamEpoch(negotiation, ""))
}

func TestGenerationActivationScopesRejectSymlinkAndInvalidNamespace(t *testing.T) {
	root := t.TempDir()
	namespaces := filepath.Join(root, "namespaces")
	require.NoError(t, os.MkdirAll(filepath.Join(namespaces, "0197f30a-3c58-7000-8000-000000000001"), 0o700))
	require.NoError(t, os.MkdirAll(filepath.Join(namespaces, "not-a-uuid"), 0o700))
	if err := os.Symlink(filepath.Join(namespaces, "0197f30a-3c58-7000-8000-000000000001"), filepath.Join(namespaces, "0197f30a-3c58-7000-8000-000000000002")); err != nil {
		t.Logf("symlink unavailable: %v", err)
	}
	driver := GenerationActivationDriver{IdentityRoot: root}
	scopes := driver.scopes()
	require.Len(t, scopes, 2)
	require.Empty(t, scopes[0].namespaceID)
	require.Equal(t, "0197f30a-3c58-7000-8000-000000000001", scopes[1].namespaceID)
}
