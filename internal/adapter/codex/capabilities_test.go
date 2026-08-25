package codex

import (
	"testing"

	"github.com/aplexica/aplexica/internal/adapter"
	"github.com/stretchr/testify/require"
)

func TestCapabilities_AdvertisesSharedCLIDesktopSurfaces(t *testing.T) {
	caps := New().Capabilities()
	require.Equal(t, []adapter.Surface{adapter.SurfaceCLI, adapter.SurfaceDesktop}, caps.Surfaces)
	require.Equal(t, []adapter.ToolKind{adapter.ToolKindMCPServer}, caps.Tools,
		"capabilities must advertise only implemented tool mappings")
}
