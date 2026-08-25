package main

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter"
	"github.com/stretchr/testify/require"
)

// fieldDroppingAdapter wraps a real adapter but mangles the SKILL.md
// export by stripping the YAML frontmatter — a deterministic,
// round-trip-breaking-but-idempotent exporter. `adapters check` must
// catch this via the native->ACF->native round-trip assertion, not pass
// it on idempotency alone.
type fieldDroppingAdapter struct {
	adapter.Adapter
}

func (a fieldDroppingAdapter) Export(ctx context.Context, store *acf.Store, id, dest string) error {
	if err := a.Adapter.Export(ctx, store, id, dest); err != nil {
		return err
	}
	if strings.HasSuffix(dest, "SKILL.md") {
		b, err := os.ReadFile(dest)
		if err != nil {
			return err
		}
		s := string(b)
		// Drop a leading YAML frontmatter block (deterministic, so the
		// second export drops it too — idempotency still holds).
		if strings.HasPrefix(s, "---\n") {
			if end := strings.Index(s[4:], "\n---\n"); end >= 0 {
				s = s[4+end+len("\n---\n"):]
				s = strings.TrimLeft(s, "\n")
			}
		}
		return os.WriteFile(dest, []byte(s), 0o644)
	}
	return nil
}

// TestCheckAdapterBasename_CatchesRoundTripFieldDrop is the core fix: a
// deterministically-wrong exporter (drops SKILL.md frontmatter) used to
// PASS `adapters check` because only idempotency was asserted. The
// round-trip assertion must now flag it.
func TestCheckAdapterBasename_CatchesRoundTripFieldDrop(t *testing.T) {
	base, err := buildAdapter("claude-code", t.TempDir())
	require.NoError(t, err)
	faulty := fieldDroppingAdapter{Adapter: base}

	ctx := context.Background()

	// SKILL.md is canonical (its minimal body round-trips byte-equally),
	// so a frontmatter drop must FAIL.
	ok, msg := checkAdapterBasename(ctx, faulty, "SKILL.md")
	require.False(t, ok, "field-dropping SKILL.md export must fail round-trip; msg=%s", msg)
	require.Contains(t, msg, "round-trip")

	// A correct adapter still passes SKILL.md.
	ok, _ = checkAdapterBasename(ctx, base, "SKILL.md")
	require.True(t, ok)
}

// TestCheckAdapterBasename_ReformattedConfigStillPasses confirms the
// round-trip assertion does NOT false-fail adapters whose canonical
// export reformats a config body (hermes YAML mcp_servers, etc.) — those
// shared probe bodies are not byte-canonical, so only idempotency is
// enforced for them.
func TestCheckAdapterBasename_ReformattedConfigStillPasses(t *testing.T) {
	ad, err := buildAdapter("hermes", t.TempDir())
	require.NoError(t, err)
	ok, msg := checkAdapterBasename(context.Background(), ad, "config.yaml")
	require.True(t, ok, "reformatted-config round-trip must still pass; msg=%s", msg)
}
