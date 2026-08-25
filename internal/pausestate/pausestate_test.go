package pausestate

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestStore_DefaultsToNotPaused(t *testing.T) {
	s := &Store{Path: filepath.Join(t.TempDir(), "p.json")}
	paused, _ := s.IsPaused("claude-code", time.Now().UTC())
	require.False(t, paused)
}

func TestStore_PauseGlobal_TakesEffectImmediately(t *testing.T) {
	s := &Store{Path: filepath.Join(t.TempDir(), "p.json")}
	require.NoError(t, s.PauseGlobal(0))

	paused, scope := s.IsPaused("any-adapter", time.Now().UTC())
	require.True(t, paused)
	require.Equal(t, "global", scope)
}

func TestStore_PauseGlobal_AutoExpires(t *testing.T) {
	s := &Store{Path: filepath.Join(t.TempDir(), "p.json")}
	require.NoError(t, s.PauseGlobal(time.Millisecond))
	time.Sleep(5 * time.Millisecond)

	paused, _ := s.IsPaused("any", time.Now().UTC())
	require.False(t, paused, "expired pause must report as not-paused")
}

func TestStore_ResumeGlobal(t *testing.T) {
	s := &Store{Path: filepath.Join(t.TempDir(), "p.json")}
	require.NoError(t, s.PauseGlobal(0))
	require.NoError(t, s.ResumeGlobal())

	paused, _ := s.IsPaused("any", time.Now().UTC())
	require.False(t, paused)
}

func TestStore_PauseAdapter_OnlyAffectsNamedAdapter(t *testing.T) {
	s := &Store{Path: filepath.Join(t.TempDir(), "p.json")}
	require.NoError(t, s.PauseAdapter("codex", 0))

	paused, scope := s.IsPaused("codex", time.Now().UTC())
	require.True(t, paused)
	require.Equal(t, "adapter", scope)

	paused, _ = s.IsPaused("claude-code", time.Now().UTC())
	require.False(t, paused)
}

func TestStore_ResumeAdapter(t *testing.T) {
	s := &Store{Path: filepath.Join(t.TempDir(), "p.json")}
	require.NoError(t, s.PauseAdapter("codex", 0))
	require.NoError(t, s.ResumeAdapter("codex"))

	paused, _ := s.IsPaused("codex", time.Now().UTC())
	require.False(t, paused)
}

func TestStore_GlobalBeatsAdapter(t *testing.T) {
	// If both global and adapter pause are set, the report says "global"
	// because the global scope is the wider statement.
	s := &Store{Path: filepath.Join(t.TempDir(), "p.json")}
	require.NoError(t, s.PauseAdapter("codex", 0))
	require.NoError(t, s.PauseGlobal(0))

	paused, scope := s.IsPaused("codex", time.Now().UTC())
	require.True(t, paused)
	require.Equal(t, "global", scope)
}

func TestStore_PausePersistsAcrossLoads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "p.json")
	s1 := &Store{Path: path}
	require.NoError(t, s1.PauseAdapter("hermes", 0))

	s2 := &Store{Path: path}
	paused, scope := s2.IsPaused("hermes", time.Now().UTC())
	require.True(t, paused)
	require.Equal(t, "adapter", scope)
}

func TestStore_CleanExpired(t *testing.T) {
	s := &Store{Path: filepath.Join(t.TempDir(), "p.json")}
	require.NoError(t, s.PauseGlobal(time.Millisecond))
	require.NoError(t, s.PauseAdapter("codex", time.Millisecond))
	require.NoError(t, s.PauseAdapter("hermes", time.Hour)) // long-lived

	time.Sleep(5 * time.Millisecond)
	require.NoError(t, s.CleanExpired(time.Now().UTC()))

	st, err := s.Load()
	require.NoError(t, err)
	require.False(t, st.Global.Paused, "expired global pause must be cleaned")
	_, exists := st.Adapters["codex"]
	require.False(t, exists, "expired adapter pause must be cleaned")
	_, exists = st.Adapters["hermes"]
	require.True(t, exists, "long-lived adapter pause must remain")
}

func TestStore_ResumeGlobalDoesNotTouchAdapterPauses(t *testing.T) {
	s := &Store{Path: filepath.Join(t.TempDir(), "p.json")}
	require.NoError(t, s.PauseAdapter("codex", 0))
	require.NoError(t, s.PauseGlobal(0))
	require.NoError(t, s.ResumeGlobal())

	paused, scope := s.IsPaused("codex", time.Now().UTC())
	require.True(t, paused, "adapter pause survives global resume")
	require.Equal(t, "adapter", scope)
}
