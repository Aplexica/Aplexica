package syncd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter"
	"github.com/aplexica/aplexica/internal/adapter/claudecode"
	"github.com/aplexica/aplexica/internal/secrets"
	"github.com/stretchr/testify/require"
)

// A file larger than the configured max-artifact-size cap must be REFUSED on
// the inbound path (BRD-03 §4.3): it must not be parsed/canonical-encoded
// (adapter.Import never runs) so a hostile/huge payload dropped into a watched
// root can't blow up the store, and the refusal must surface a user-visible
// warning. A file under the cap imports normally.
func TestOrchestrator_OversizeArtifact_RefusedAndWarned(t *testing.T) {
	tmp := realTempDir(t)
	storeRoot := filepath.Join(tmp, "store")
	secRoot := filepath.Join(tmp, "sec")
	watchDir := filepath.Join(tmp, "proj")
	require.NoError(t, os.MkdirAll(watchDir, 0o755))

	// One small file (under the cap) and one oversized file (over the cap),
	// both basenames an adapter claims so the ONLY thing that can stop the
	// big one from importing is the size gate.
	const cap = 1024 // bytes; tiny so the test stays fast
	small := filepath.Join(watchDir, "CLAUDE.md")
	require.NoError(t, os.WriteFile(small, []byte("memory body"), 0o644))
	big := filepath.Join(watchDir, "AGENTS.md")
	require.NoError(t, os.WriteFile(big, []byte(strings.Repeat("x", cap*4)), 0o644))

	var imports int32
	store := &acf.Store{Root: storeRoot}
	require.NoError(t, store.Init())
	ss := &secrets.Store{Root: secRoot}
	require.NoError(t, ss.Init())
	cc := claudecode.New()
	cc.HomeDir = tmp
	cc.SecretsStore = ss

	orch, err := NewOrchestrator(Config{
		Dir:              watchDir,
		Adapters:         []adapter.Adapter{importCountingAdapter{Adapter: cc, imports: &imports}},
		Store:            store,
		QuietPeriod:      50 * time.Millisecond,
		GuardWindow:      time.Second,
		MaxArtifactBytes: cap,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = orch.Close() })

	require.NoError(t, orch.InitialScan(context.Background()))

	// The under-cap file imported; the over-cap file did NOT (Import is the
	// expensive parse+encode step the gate exists to skip). Exactly one import.
	require.Equal(t, int32(1), atomic.LoadInt32(&imports),
		"only the under-cap file may be imported; the oversized file must be refused before Import")

	// The oversized file is NOT in the canonical store.
	heads, err := store.ListArtifacts(acf.KindMemory)
	require.NoError(t, err)
	for _, h := range heads {
		require.NotEqual(t, "AGENTS.md", h.Name,
			"the oversized artifact must never enter the canonical store")
	}

	// The refusal surfaced a user-visible warning (FR-03.5 status channel).
	warnings := orch.AdapterLastErrors()
	require.NotEmpty(t, warnings, "an oversized refusal must record a user-visible warning")
}
