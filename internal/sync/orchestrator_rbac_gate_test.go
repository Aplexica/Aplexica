package syncd

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/rbac"
	"github.com/stretchr/testify/require"
)

// stubWriteAuthorizer is a test double for the orchestrator's WriteAuthorizer
// seam. It is NOT the unit under test — the unit under test is the
// orchestrator's guarded-commit chokepoint and the REAL acf.Store. The double
// only models the desync-safe tri-state the production *daemon.RoleService
// produces: a definitive deny, or a proceed (nil).
type stubWriteAuthorizer struct {
	err   error
	calls int
}

func (s *stubWriteAuthorizer) Authorize(_ context.Context, _ string, _ rbac.Operation) error {
	s.calls++
	return s.err
}

// seedNamespaceArtifact creates a namespace-scoped artifact in a REAL store
// with one genesis event, so HeadEventHash is known. Returns the kind + id.
func seedNamespaceArtifact(t *testing.T, store *acf.Store) (acf.Kind, string) {
	t.Helper()
	id := acf.NewID()
	art := acf.Artifact{
		AcfSchemaVersion: "0.1",
		ArtifactID:       id,
		Kind:             acf.KindMemory,
		Scope:            acf.ScopeNamespace,
		NamespaceID:      acf.NewID(),
		Name:             "team-note",
		CreatedAt:        time.Now().UTC(),
		UpdatedAt:        time.Now().UTC(),
	}
	require.NoError(t, store.WriteArtifact(art))
	genesis := acf.Event{
		EventID:    acf.NewID(),
		ArtifactID: id,
		Type:       acf.EventTypeCreate,
		Timestamp:  time.Now().UTC(),
		Payload:    json.RawMessage(`{"text":"seed"}`),
		ParentHash: "",
	}
	require.NoError(t, store.AppendEvent(acf.KindMemory, genesis))
	return acf.KindMemory, id
}

// readEventsFileBytes returns the raw on-disk JSONL bytes for an artifact's
// event log. It locates the file by walking the store's events tree for the
// "<id>.jsonl" leaf, so the test asserts on the REAL bytes the store wrote
// without depending on any unexported path helper.
func readEventsFileBytes(t *testing.T, store *acf.Store, _ acf.Kind, id string) []byte {
	t.Helper()
	var found string
	leaf := id + ".jsonl"
	err := filepath.Walk(filepath.Join(store.Root, "events"), func(p string, info os.FileInfo, werr error) error {
		if werr != nil {
			return werr
		}
		if !info.IsDir() && filepath.Base(p) == leaf {
			found = p
		}
		return nil
	})
	require.NoError(t, err)
	require.NotEmpty(t, found, "events file for artifact %s not found", id)
	b, rerr := os.ReadFile(found)
	require.NoError(t, rerr)
	return b
}

// Test 4: a DEFINITIVE deny leaves the canonical store byte-for-byte unchanged
// — no appended event line, no hash-chain extension — and returns an error
// wrapping rbac.ErrForbidden. This is the desync-safety proof: the gate fires
// strictly BEFORE Store.AppendEvent, so a refused namespace write cannot
// desync the local chain from peers.
func TestOrchestrator_GuardedCommit_DenyLeavesChainUnchanged(t *testing.T) {
	root := realTempDir(t)
	store := &acf.Store{Root: filepath.Join(root, "store")}
	require.NoError(t, store.Init())
	kind, id := seedNamespaceArtifact(t, store)

	preBytes := readEventsFileBytes(t, store, kind, id)
	preArt, err := store.ReadArtifact(kind, id)
	require.NoError(t, err)
	preHead := preArt.HeadEventHash
	require.NotEmpty(t, preHead)

	auth := &stubWriteAuthorizer{err: errForbiddenSample()}
	o := &Orchestrator{cfg: Config{Store: store, WriteAuthorizer: auth}}

	next := acf.Event{
		EventID:    acf.NewID(),
		ArtifactID: id,
		Type:       acf.EventTypeUpdate,
		Timestamp:  time.Now().UTC(),
		Payload:    json.RawMessage(`{"text":"edit-by-reader"}`),
		ParentHash: preHead,
	}
	gerr := o.commitNamespaceEvent(context.Background(), "ns-1", rbac.OpEditArtifact, kind, next)
	if gerr == nil {
		t.Fatalf("commitNamespaceEvent under a definitive deny = nil, want a permission error")
	}
	if !errors.Is(gerr, rbac.ErrForbidden) {
		t.Errorf("commitNamespaceEvent error = %v, want it to wrap rbac.ErrForbidden", gerr)
	}

	// The on-disk events file must be byte-identical: no event line appended.
	postBytes := readEventsFileBytes(t, store, kind, id)
	if string(postBytes) != string(preBytes) {
		t.Errorf("events file changed after a deny:\npre  = %q\npost = %q", preBytes, postBytes)
	}
	// HeadEventHash must be unchanged: no hash-chain extension.
	postArt, err := store.ReadArtifact(kind, id)
	require.NoError(t, err)
	if postArt.HeadEventHash != preHead {
		t.Errorf("HeadEventHash advanced after a deny: pre=%q post=%q", preHead, postArt.HeadEventHash)
	}
	// And exactly one event remains (the genesis).
	events, err := store.ReadEvents(kind, id)
	require.NoError(t, err)
	if len(events) != 1 {
		t.Errorf("event count = %d after a deny, want 1 (genesis only)", len(events))
	}
}

// Test 5: a PROCEEDING authorizer (contributor+/unknown/offline => nil) commits
// the event and advances HeadEventHash exactly as the ungated path. A nil
// WriteAuthorizer (today's default — OSS / un-paired daemon) must ALSO commit,
// proving the seam is backward-compatible.
func TestOrchestrator_GuardedCommit_ProceedAndNilAuthorizerCommit(t *testing.T) {
	for _, tc := range []struct {
		name string
		auth WriteAuthorizer
	}{
		{name: "proceed-authorizer", auth: &stubWriteAuthorizer{err: nil}},
		{name: "nil-authorizer", auth: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := realTempDir(t)
			store := &acf.Store{Root: filepath.Join(root, "store")}
			require.NoError(t, store.Init())
			kind, id := seedNamespaceArtifact(t, store)

			preArt, err := store.ReadArtifact(kind, id)
			require.NoError(t, err)
			preHead := preArt.HeadEventHash

			cfg := Config{Store: store}
			if tc.auth != nil {
				cfg.WriteAuthorizer = tc.auth
			}
			o := &Orchestrator{cfg: cfg}

			next := acf.Event{
				EventID:    acf.NewID(),
				ArtifactID: id,
				Type:       acf.EventTypeUpdate,
				Timestamp:  time.Now().UTC(),
				Payload:    json.RawMessage(`{"text":"edit-by-contributor"}`),
				ParentHash: preHead,
			}
			if err := o.commitNamespaceEvent(context.Background(), "ns-1", rbac.OpEditArtifact, kind, next); err != nil {
				t.Fatalf("commitNamespaceEvent (proceed) = %v, want nil", err)
			}

			postArt, err := store.ReadArtifact(kind, id)
			require.NoError(t, err)
			if postArt.HeadEventHash == preHead {
				t.Errorf("HeadEventHash did not advance after a proceed (event not committed)")
			}
			events, err := store.ReadEvents(kind, id)
			require.NoError(t, err)
			if len(events) != 2 {
				t.Errorf("event count = %d after a proceed, want 2 (genesis + update)", len(events))
			}
		})
	}
}

// errForbiddenSample mirrors the shape a real *daemon.RoleService.Authorize
// returns on a definitive deny: an error wrapping rbac.ErrForbidden.
func errForbiddenSample() error {
	return rbac.Authorize(rbac.RoleReader, rbac.OpEditArtifact)
}
