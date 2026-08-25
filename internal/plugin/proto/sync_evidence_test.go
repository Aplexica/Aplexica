package proto

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestRemoteSyncEvidenceV1RoundTripsWithoutContentOrCursorTokens(t *testing.T) {
	in := RemoteStatusResult{SyncEvidence: &RemoteSyncEvidenceV1{
		SchemaVersion: 1,
		SelectedMode:  "delta_preferred",
		Complete:      true,
		CollectedAt:   time.Date(2026, 7, 20, 18, 0, 0, 0, time.UTC),
		Streams: []RemoteSyncStreamEvidenceV1{{
			StreamID: "scope-digest", StreamEpoch: "epoch-1", ServerMode: "delta_preferred",
			ServerTipPosition: 9, ServerTipCursorDigest: strings.Repeat("a", 64),
			ServerDevicePosition: 9, LocalCursorPresent: true, LocalCursorPosition: 9,
			LocalCursorDigest: strings.Repeat("a", 64), CursorAndHeadConverged: true,
			CheckpointPolicies: 1, CheckpointAnchors: 1, CheckpointReady: 1,
			CheckpointReadinessComplete: true,
		}},
		Outbound: RemoteOutboundEvidenceV1{
			DeltaCommitted: 4, RetainedSuppressed: 3, CheckpointCommitted: 1,
			UpdatedAt: time.Date(2026, 7, 20, 17, 59, 0, 0, time.UTC),
		},
	}}
	body, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbiddenKey := range []string{`"cursor"`, `"namespace_id"`, `"artifact_id"`, `"branch_id"`, `"path"`, `"prompt"`} {
		if strings.Contains(string(body), forbiddenKey) {
			t.Fatalf("sync evidence exposed forbidden key %s: %s", forbiddenKey, body)
		}
	}
	var out RemoteStatusResult
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(out, in) {
		t.Fatalf("sync evidence round trip mismatch:\n got: %+v\nwant: %+v", out, in)
	}
}
