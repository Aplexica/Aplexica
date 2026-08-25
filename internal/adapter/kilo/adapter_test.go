package kilo

import (
	"testing"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/stretchr/testify/require"
)

func TestAdapter_Identity(t *testing.T) {
	a := New()
	require.Equal(t, "kilo", a.Name())
	require.NotEmpty(t, a.Version())
}

func TestAdapter_InferScope_GlobalConfigAndProjectFiles(t *testing.T) {
	a := &Adapter{HomeDir: "/home/u"}
	require.Equal(t, acf.ScopeGlobal, a.inferScope("/home/u/.config/kilo/AGENTS.md"))
	require.Equal(t, acf.ScopeGlobal, a.inferScope("/home/u/.kilo/skills/api/SKILL.md"))
	require.Equal(t, acf.ScopeProject, a.inferScope("/home/u/projects/foo/AGENTS.md"))
	require.Equal(t, acf.ScopeProject, a.inferScope("/anywhere/else"))
}
