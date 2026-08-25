package retention

import (
	"testing"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/stretchr/testify/require"
)

// TestGCReport_GoldenMarkdownAndJSON pins the stable wire renderings of a
// GCReport (FR-03.22/23). Both MarshalMarkdown and MarshalJSON are
// byte-for-byte golden so dry-run output diffs cleanly and downstream
// consumers can rely on the shape.
func TestGCReport_GoldenMarkdownAndJSON(t *testing.T) {
	var r GCReport
	r.DryRun = true
	r.AddAction(GCAction{Kind: acf.KindMemory, ArtifactID: "art-1", Op: OpPruneEvents, Detail: "ev-aaa", BytesSaved: 0})
	r.AddAction(GCAction{Kind: acf.KindConversation, ArtifactID: "art-2", Op: OpEvictAttachment, Detail: "2 attachment(s)", BytesSaved: 1024})
	r.AddAction(GCAction{Kind: acf.KindConversation, ArtifactID: "", Op: OpGCBlob, Detail: "deadbeef", BytesSaved: 512})

	// AddAction accumulates BytesSaved.
	require.Equal(t, int64(1536), r.BytesSaved)

	wantMD := "| Kind | Artifact | Op | Detail | Bytes |\n" +
		"| --- | --- | --- | --- | --- |\n" +
		"| memory | art-1 | prune-events | ev-aaa | 0 |\n" +
		"| conversation | art-2 | evict-attachment | 2 attachment(s) | 1024 |\n" +
		"| conversation |  | gc-blob | deadbeef | 512 |\n" +
		"| **Total** | | | | 1536 |\n"
	require.Equal(t, wantMD, string(r.MarshalMarkdown()))

	gotJSON, err := r.MarshalJSON()
	require.NoError(t, err)
	wantJSON := `{"actions":[` +
		`{"kind":"memory","artifactId":"art-1","op":"prune-events","detail":"ev-aaa","bytesSaved":0},` +
		`{"kind":"conversation","artifactId":"art-2","op":"evict-attachment","detail":"2 attachment(s)","bytesSaved":1024},` +
		`{"kind":"conversation","artifactId":"","op":"gc-blob","detail":"deadbeef","bytesSaved":512}` +
		`],"bytesSaved":1536,"dryRun":true}`
	require.JSONEq(t, wantJSON, string(gotJSON))
	// Exact byte match too (stable field order), not just JSONEq.
	require.Equal(t, wantJSON, string(gotJSON))
}

// TestGCReport_EmptyActionsMarshalAsArray guards the wire contract: an
// action-less report serializes "actions" as [] (never null).
func TestGCReport_EmptyActionsMarshalAsArray(t *testing.T) {
	var r GCReport
	got, err := r.MarshalJSON()
	require.NoError(t, err)
	require.Equal(t, `{"actions":[],"bytesSaved":0,"dryRun":false}`, string(got))
}
