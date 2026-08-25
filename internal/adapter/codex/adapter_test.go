package codex

import (
	"path/filepath"
	"testing"

	"github.com/aplexica/aplexica/internal/adapter"
	"github.com/stretchr/testify/require"
)

// Compile-time assertion that Adapter satisfies the adapter.Adapter interface.
var _ adapter.Adapter = (*Adapter)(nil)

func TestAdapter_Name(t *testing.T) {
	a := New()
	require.Equal(t, "codex", a.Name())
}

func TestAdapter_Version(t *testing.T) {
	a := New()
	require.NotEmpty(t, a.Version())
}

func TestAdapter_InferScope_Global(t *testing.T) {
	// inferScope joins HomeDir + ".codex" with filepath.Separator, so the
	// input path must use the same separator the platform produces.
	home := filepath.FromSlash("/home/u")
	a := &Adapter{HomeDir: home}
	require.Equal(t, "global", string(a.inferScope(filepath.Join(home, ".codex", "AGENTS.md"))))
}

func TestAdapter_InferScope_Project(t *testing.T) {
	home := filepath.FromSlash("/home/u")
	a := &Adapter{HomeDir: home}
	require.Equal(t, "project", string(a.inferScope(filepath.Join(home, "proj", "AGENTS.md"))))
}

func TestAdapter_InferScope_EmptyHome(t *testing.T) {
	a := &Adapter{HomeDir: ""}
	require.Equal(t, "project", string(a.inferScope("/anywhere/AGENTS.md")))
}

func TestAdapter_InferScope_NoFalsePositiveOnPrefixMatch(t *testing.T) {
	a := &Adapter{HomeDir: "/home/u"}
	// "/home/u/.codex-backup/..." must NOT match "/home/u/.codex/"
	require.Equal(t, "project", string(a.inferScope(filepath.Join("/home/u/.codex-backup", "AGENTS.md"))))
}
