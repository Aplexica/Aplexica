package generationactivation

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPendingGateBlocksExactPendingAndReopensAfterActivation(t *testing.T) {
	fixture := newActivationFixture(t)
	root := t.TempDir()
	require.NoError(t, os.Chmod(root, 0o700))
	statePath := filepath.Join(root, "account", "generation-activation.json")
	state := FileStateStore{Path: statePath}
	transport := &recordingTransport{
		failActivate: true,
		receipt:      ActivationReceipt{AuthorityDigest: sha256Hex("authority-one"), Revision: 1},
	}

	_, err := fixture.coordinator(state, transport).RunOnce(context.Background())
	require.Error(t, err)
	gate := PendingGate{IdentityRoot: root}
	require.ErrorIs(t, gate.Check("account"), ErrPendingActivation)

	transport.failActivate = false
	_, err = fixture.coordinator(state, transport).RunOnce(context.Background())
	require.NoError(t, err)
	require.NoError(t, gate.Check("account"), "completed state must not remain a traffic barrier")
}

func TestPendingGateAllowsMissingAndPublicationOnlyState(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.Chmod(root, 0o700))
	gate := PendingGate{IdentityRoot: root}
	require.NoError(t, gate.Check("account"))
	_, err := os.Stat(filepath.Join(root, "account"))
	require.ErrorIs(t, err, os.ErrNotExist, "a read-only gate check must not create missing state directories")

	state := durableState{Version: stateVersion, StreamEpoch: "stream-a", PublishedDigest: [32]byte{1}}
	require.NoError(t, (FileStateStore{Path: filepath.Join(root, "account", "generation-activation.json")}).Save(state))
	require.NoError(t, gate.Check("account"), "state-file existence alone must not block traffic")
}

func TestPendingGateFailsClosedOnCorruptOrEquivocalState(t *testing.T) {
	fixture := newActivationFixture(t)
	root := t.TempDir()
	require.NoError(t, os.Chmod(root, 0o700))
	statePath := filepath.Join(root, "account", "generation-activation.json")
	store := FileStateStore{Path: statePath}
	transport := &recordingTransport{failActivate: true}
	_, err := fixture.coordinator(store, transport).RunOnce(context.Background())
	require.Error(t, err)

	state, err := store.Load()
	require.NoError(t, err)
	require.NotNil(t, state.Pending)
	state.ActivatedBindingDigest = state.Pending.BindingDigest
	state.AuthorityDigest = [32]byte{2}
	state.AuthorityRevision = 1
	require.NoError(t, store.Save(state))
	err = (PendingGate{IdentityRoot: root}).Check("account")
	require.ErrorIs(t, err, ErrInvalidState)

	require.NoError(t, os.WriteFile(statePath, []byte("{not-json"), 0o600))
	err = (PendingGate{IdentityRoot: root}).Check("account")
	require.True(t, errors.Is(err, ErrInvalidState))
}

func TestPendingGateRejectsScopeConfusionAndUnsafeScope(t *testing.T) {
	fixture := newActivationFixture(t)
	root := t.TempDir()
	require.NoError(t, os.Chmod(root, 0o700))
	accountPath := filepath.Join(root, "account", "generation-activation.json")
	transport := &recordingTransport{failActivate: true}
	_, err := fixture.coordinator(FileStateStore{Path: accountPath}, transport).RunOnce(context.Background())
	require.Error(t, err)

	namespaceID := "0197f30a-3c58-7000-8000-000000000001"
	namespaceDir := filepath.Join(root, "namespaces", namespaceID)
	require.NoError(t, os.MkdirAll(namespaceDir, 0o700))
	raw, err := os.ReadFile(accountPath)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(namespaceDir, "generation-activation.json"), raw, 0o600))

	gate := PendingGate{IdentityRoot: root}
	require.ErrorIs(t, gate.Check(namespaceID), ErrInvalidState)
	require.ErrorIs(t, gate.Check("../account"), ErrInvalidState)
}
