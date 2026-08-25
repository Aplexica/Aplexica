package adapter

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestUpsertProjectSection_AppendAndReplace(t *testing.T) {
	base := "# central memory\n- user likes Go\n"
	v1 := UpsertProjectSection(base, "local:abc:demo", "demo (/p/demo)", "- project rule one\n")
	require.Contains(t, v1, "## Project: demo (/p/demo)")
	require.Contains(t, v1, "- project rule one")
	require.True(t, strings.HasPrefix(v1, base), "global content stays untouched ahead of the section")

	v2 := UpsertProjectSection(v1, "local:abc:demo", "demo (/p/demo)", "- project rule two\n")
	require.NotContains(t, v2, "rule one", "section is replaced, not appended")
	require.Contains(t, v2, "rule two")
	require.Equal(t, 1, strings.Count(v2, "<!-- aplexica:project-memory:local:abc:demo -->"))
}

func TestUpsertProjectSection_TwoProjectsCoexist(t *testing.T) {
	out := UpsertProjectSection("", "p1", "one", "- a\n")
	out = UpsertProjectSection(out, "p2", "two", "- b\n")
	out = UpsertProjectSection(out, "p1", "one", "- a2\n")
	require.Contains(t, out, "- a2")
	require.Contains(t, out, "- b")
	require.NotContains(t, out, "- a\n<!--", "p1 replaced in place")
}

func TestStripProjectSections_RoundTripsGlobalContent(t *testing.T) {
	base := "# central memory\n- user likes Go\n"
	composed := UpsertProjectSection(base, "p1", "one", "- a\n")
	composed = UpsertProjectSection(composed, "p2", "two", "- b\n")
	require.Equal(t, base, StripProjectSections(composed),
		"stripping must reproduce the pristine global content")
	require.Equal(t, "", StripProjectSections(UpsertProjectSection("", "p1", "one", "- a\n")),
		"a file that was ONLY sections strips to empty")
}

func TestUpsertProjectSection_Idempotent(t *testing.T) {
	a := UpsertProjectSection("base\n", "p1", "one", "- a\n")
	b := UpsertProjectSection(a, "p1", "one", "- a\n")
	require.Equal(t, a, b)
}
