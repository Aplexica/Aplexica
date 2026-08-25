//go:build windows

package syncd

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter"
	"github.com/aplexica/aplexica/internal/adapter/claudecode"
	"github.com/stretchr/testify/require"
)

func TestWindowsStartupRepairsMissingRemoteConversationProjection(t *testing.T) {
	root := realTempDir(t)
	store := &acf.Store{Root: filepath.Join(root, "store")}
	require.NoError(t, store.Init())
	art, head := seedRemoteConversationProjectionFixture(t, store, "peer-device")

	claude := claudecode.New()
	claude.HomeDir = root
	dest, supported, err := claude.ConversationSessionPath(art, head, "codex")
	require.NoError(t, err)
	require.True(t, supported)
	require.NoFileExists(t, dest)

	orch, err := NewOrchestrator(Config{
		Dir:           root,
		Adapters:      []adapter.Adapter{claude},
		Store:         store,
		LocalDeviceID: "receiving-device",
	})
	require.NoError(t, err)
	defer orch.Close()

	require.Eventually(t, func() bool {
		_, statErr := os.Lstat(dest)
		return statErr == nil
	}, 3*time.Second, 20*time.Millisecond)
}
