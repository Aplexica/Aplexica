//go:build !windows

package syncd

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter"
	"github.com/aplexica/aplexica/internal/adapter/claudecode"
	"github.com/stretchr/testify/require"
)

func TestNonWindowsStartupDoesNotRunMissingRemoteConversationRepair(t *testing.T) {
	root := realTempDir(t)
	store := &acf.Store{Root: filepath.Join(root, "store")}
	require.NoError(t, store.Init())
	art, head := seedRemoteConversationProjectionFixture(t, store, "peer-device")

	claude := claudecode.New()
	claude.HomeDir = root
	dest, supported, err := claude.ConversationSessionPath(art, head, "codex")
	require.NoError(t, err)
	require.True(t, supported)

	orch, err := NewOrchestrator(Config{
		Dir:           root,
		Adapters:      []adapter.Adapter{claude},
		Store:         store,
		LocalDeviceID: "receiving-device",
	})
	require.NoError(t, err)
	defer orch.Close()

	time.Sleep(100 * time.Millisecond)
	require.NoFileExists(t, dest)
}
