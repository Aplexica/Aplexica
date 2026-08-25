package syncd

import (
	"testing"

	"github.com/aplexica/aplexica/internal/adapter"
	"github.com/stretchr/testify/require"
)

// chain_unspanned is the honest name for a file that holds a conversational row
// its own resume walk did not visit AND is provably not forked. It must be
// enumerated everywhere forked_mirror is, because inheriting an unreviewed class
// is how a decline reason stops meaning anything.
//
// It does NOT get a different REMEDY, though, and that distinction is the whole
// lesson of this class: the repair router (rebuildDivergedClaudeMirror,
// repairDivergedNativeSession) keys on "the walk did not span the file", so this
// build repairs a containment-provable chain_unspanned exactly as it repairs a
// fork. Only the SHAPE description differs, so the operator is not sent looking
// for a branch that is not there.
func TestChainUnspanned_IsRoutedAsItsOwnStructuralClass(t *testing.T) {
	require.Equal(t, ConversationRetryStructural,
		conversationRetryClassFor(adapter.SessionDeclineChainUnspanned),
		"re-reading the same bytes reaches the same conclusion")

	explanation := conversationDeclineExplanation(adapter.SessionDeclineChainUnspanned)
	require.NotEqual(t, conversationDeclineExplanation(adapter.SessionDeclineForkedMirror), explanation)
	require.NotEqual(t, conversationDeclineExplanation(adapter.SessionDeclineUnspecified), explanation,
		"an enumerated reason must never fall through to the default")
	require.Contains(t, explanation, "not forked")

	for _, surface := range []materializationSurface{
		{},
		{mirrorRepairSupported: true},
		{mirrorRepairSupported: true, mirrorRepairEnabled: true},
	} {
		explain := escalatedMaterializationExplain(adapter.SessionDeclineChainUnspanned, "", surface)
		require.NotEmpty(t, explain)
		require.Contains(t, explain, "not forked",
			"the operator must not be sent looking for a branch that is not there")
	}
	// A target with no rebuild is offered nothing, exactly as before.
	require.Empty(t, escalatedMaterializationRemedy(
		"codex", "019e0000", adapter.SessionDeclineChainUnspanned, "", materializationSurface{}))
}
