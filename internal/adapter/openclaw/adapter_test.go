package openclaw

import (
	"path/filepath"
	"testing"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/stretchr/testify/require"
)

func TestAdapter_NameVersion(t *testing.T) {
	a := New()
	require.Equal(t, "openclaw", a.Name())
	require.Equal(t, "0.3.1", a.Version())
}

func TestAdapter_InferScope_Global(t *testing.T) {
	a := &Adapter{HomeDir: "/home/u", DeviceID: "dev"}
	require.Equal(t, acf.ScopeGlobal, a.inferScope(filepath.Join("/home/u", ".openclaw", "workspace", "MEMORY.md")))
}

func TestAdapter_InferScope_Project(t *testing.T) {
	a := &Adapter{HomeDir: "/home/u", DeviceID: "dev"}
	require.Equal(t, acf.ScopeProject, a.inferScope("/tmp/some-project/MEMORY.md"))
}

func TestAdapter_InferScope_NoHomeDir(t *testing.T) {
	a := &Adapter{HomeDir: "", DeviceID: "dev"}
	require.Equal(t, acf.ScopeProject, a.inferScope("/anywhere/MEMORY.md"))
}
