package hermes

import (
	"path/filepath"
	"testing"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/stretchr/testify/require"
)

func TestAdapter_Identity(t *testing.T) {
	a := New()
	require.Equal(t, "hermes", a.Name())
	require.NotEmpty(t, a.Version())
}

func TestAdapter_InferScope_GlobalUnderHermesDir(t *testing.T) {
	a := &Adapter{HomeDir: "/home/u"}
	abs := filepath.Join("/home/u/.hermes/memories", "MEMORY.md")
	require.Equal(t, acf.ScopeGlobal, a.inferScope(abs))
}

func TestAdapter_InferScope_ProjectOutsideHermesDir(t *testing.T) {
	a := &Adapter{HomeDir: "/home/u"}
	abs := filepath.Join("/home/u/projects/foo", "MEMORY.md")
	require.Equal(t, acf.ScopeProject, a.inferScope(abs))
}

func TestAdapter_InferScope_NoHomeDir_DefaultsProject(t *testing.T) {
	a := &Adapter{HomeDir: ""}
	require.Equal(t, acf.ScopeProject, a.inferScope("/anywhere/MEMORY.md"))
}
