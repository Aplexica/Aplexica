package proto

import (
	"crypto/sha256"
	"encoding/json"
	"reflect"
	"testing"
)

func TestGenerationActivationWireRoundTrip(t *testing.T) {
	if MethodRemoteGetSyncGenerationStatus != "remote.get_sync_generation_status" {
		t.Fatalf("generation status method = %q", MethodRemoteGetSyncGenerationStatus)
	}
	if MethodRemoteSubmitAtomicAuthorityRosterTransition != "remote.submit_atomic_authority_roster_transition" {
		t.Fatalf("atomic authority/roster method = %q", MethodRemoteSubmitAtomicAuthorityRosterTransition)
	}
	objectHash := sha256.Sum256([]byte("anchor"))
	request := RemoteSubmitSignedObjectParams{Object: RemoteOpaqueSignedObject{
		ScopeType: "account", ScopeID: "account-a", Kind: "trust-anchor", Sequence: 1,
		Hash: objectHash, Blob: []byte("anchor"),
	}}
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	var decodedRequest RemoteSubmitSignedObjectParams
	if err := json.Unmarshal(raw, &decodedRequest); err != nil || decodedRequest.Object.Hash != objectHash {
		t.Fatalf("trust anchor request = %+v, err=%v", decodedRequest, err)
	}
	atomicHash := sha256.Sum256([]byte("canonical-atomic-package"))
	atomic := RemoteSubmitSignedObjectParams{Object: RemoteOpaqueSignedObject{
		ScopeType: "account", ScopeID: "scope-a", Kind: "atomic-authority-roster-transition", Sequence: 2,
		PreviousHash: objectHash, Hash: atomicHash, Blob: []byte("canonical-atomic-package"),
	}}
	raw, err = json.Marshal(atomic)
	if err != nil {
		t.Fatal(err)
	}
	decodedRequest = RemoteSubmitSignedObjectParams{}
	if err := json.Unmarshal(raw, &decodedRequest); err != nil || !reflect.DeepEqual(decodedRequest.Object, atomic.Object) {
		t.Fatalf("atomic package request = %+v, err=%v", decodedRequest, err)
	}

	activation := RemoteActivateSyncGenerationParams{AttestationBlob: []byte("canonical-attestation")}
	raw, err = json.Marshal(activation)
	if err != nil {
		t.Fatal(err)
	}
	var decodedActivation RemoteActivateSyncGenerationParams
	if err := json.Unmarshal(raw, &decodedActivation); err != nil || string(decodedActivation.AttestationBlob) != "canonical-attestation" {
		t.Fatalf("activation request = %+v, err=%v", decodedActivation, err)
	}
	result := RemoteActivateSyncGenerationResult{AuthorityDigest: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", Revision: 3, Duplicate: true}
	raw, err = json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var decodedResult RemoteActivateSyncGenerationResult
	if err := json.Unmarshal(raw, &decodedResult); err != nil || decodedResult != result {
		t.Fatalf("activation result = %+v, err=%v", decodedResult, err)
	}
	status := RemoteGetSyncGenerationStatusResult{Status: "committed", AuthorityDigest: result.AuthorityDigest, Revision: 3, Duplicate: true}
	raw, err = json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	var decodedStatus RemoteGetSyncGenerationStatusResult
	if err := json.Unmarshal(raw, &decodedStatus); err != nil || decodedStatus != status {
		t.Fatalf("activation status = %+v, err=%v", decodedStatus, err)
	}
}
