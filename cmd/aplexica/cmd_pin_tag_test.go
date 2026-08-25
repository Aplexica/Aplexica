package main

import (
	"testing"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/stretchr/testify/require"
)

// TestPin_TagFlag verifies BRD-03 §4.8.5 `aplexica pin <id> [--tag <name>]`:
// a non-default configured pin tag can be applied (pinTag() alone could only
// ever reach retention.pin_tags[0]), and an unconfigured tag is rejected so
// the retention engine never sees a tag it won't honor as an exemption.
func TestPin_TagFlag(t *testing.T) {
	t.Cleanup(func() { pinTagFlag = "" })
	root, id := seedPinStore(t)
	store := &acf.Store{Root: root}
	require.NoError(t, store.Init())

	// An unconfigured tag must be rejected.
	out, err := runPinCmd(t, "pin", root, id, "--tag", "definitely-not-a-pin-tag")
	require.Error(t, err, "unconfigured pin tag must be rejected; output:\n%s", out)

	// A configured tag (the last entry, e.g. "keep-forever") must be applied.
	cfg, cerr := resolveRetentionConfig()
	require.NoError(t, cerr)
	require.NotEmpty(t, cfg.PinTags)
	tag := cfg.PinTags[len(cfg.PinTags)-1]
	out, err = runPinCmd(t, "pin", root, id, "--tag", tag)
	require.NoError(t, err, "configured tag must be accepted; output:\n%s", out)
	art, err := store.ReadArtifact(acf.KindMemory, id)
	require.NoError(t, err)
	require.Contains(t, art.Tags, tag, "the requested --tag must be applied")
}
