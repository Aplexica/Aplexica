package acf

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/aplexica/aplexica/internal/securityerr"
	"github.com/stretchr/testify/require"
)

func TestBundlePathValidatorRejectsTraversalLinksDuplicatesAndOversize(t *testing.T) {
	limits := DefaultBundleLimits()
	validID := "0197f000-aaaa-7000-8000-000000000001"
	cases := []struct {
		name string
		hdrs []*tar.Header
		want error
	}{
		{"traversal", []*tar.Header{{Name: "../outside", Typeflag: tar.TypeReg}}, securityerr.ErrPathEscape},
		{"absolute", []*tar.Header{{Name: "/outside", Typeflag: tar.TypeReg}}, securityerr.ErrPathEscape},
		{"symlink", []*tar.Header{{Name: "acf/memories/" + validID + ".json", Typeflag: tar.TypeSymlink, Linkname: "../../outside"}}, securityerr.ErrUnsafeFilesystemNode},
		{"hardlink", []*tar.Header{{Name: "acf/memories/" + validID + ".json", Typeflag: tar.TypeLink, Linkname: "meta.json"}}, securityerr.ErrUnsafeFilesystemNode},
		{"unknown root", []*tar.Header{{Name: "unknown/file", Typeflag: tar.TypeReg}}, securityerr.ErrUnsafeIdentifier},
		{"duplicate", []*tar.Header{{Name: "meta.json", Typeflag: tar.TypeReg}, {Name: "meta.json", Typeflag: tar.TypeReg}}, securityerr.ErrUnsafeIdentifier},
		{"file prefix", []*tar.Header{{Name: "secrets/a", Typeflag: tar.TypeReg}, {Name: "secrets/a/b", Typeflag: tar.TypeReg}}, securityerr.ErrUnsafeIdentifier},
		{"oversize meta", []*tar.Header{{Name: "meta.json", Typeflag: tar.TypeReg, Size: limits.MaxMetaBytes + 1}}, securityerr.ErrLimitExceeded},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			validator := newBundlePathValidator(limits)
			var got error
			for _, hdr := range tc.hdrs {
				if got = validator.validateHeader(hdr); got != nil {
					break
				}
			}
			require.Error(t, got)
			require.True(t, errors.Is(got, tc.want), "%v", got)
			require.NotContains(t, got.Error(), "outside")
		})
	}
}

func TestBundlePathValidatorAcceptsCanonicalInventory(t *testing.T) {
	validID := "0197f000-aaaa-7000-8000-000000000001"
	hash := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	headers := []*tar.Header{
		{Name: "meta.json", Typeflag: tar.TypeReg, Size: 10},
		{Name: "acf/memories/" + validID + ".json", Typeflag: tar.TypeReg, Size: 10},
		{Name: "events/memories/" + validID + ".jsonl", Typeflag: tar.TypeReg, Size: 10},
		{Name: "events/.compacted/memories/" + validID + ".jsonl.gz", Typeflag: tar.TypeReg, Size: 10},
		{Name: "eventTags/memories/" + validID + ".json", Typeflag: tar.TypeReg, Size: 10},
		{Name: "branches/memories/" + validID + ".json", Typeflag: tar.TypeReg, Size: 10},
		{Name: "blobs/01/23/" + hash, Typeflag: tar.TypeReg, Size: 10},
		{Name: "secrets/nested/config.json", Typeflag: tar.TypeReg, Size: 10},
	}
	validator := newBundlePathValidator(DefaultBundleLimits())
	for _, hdr := range headers {
		require.NoError(t, validator.validateHeader(hdr), hdr.Name)
	}
}

func TestRestoreRejectsTraversalBeforeOutsideMutation(t *testing.T) {
	parent := t.TempDir()
	storeRoot := filepath.Join(parent, "store")
	store := &Store{Root: storeRoot}
	require.NoError(t, store.Init())
	canary := filepath.Join(parent, "outside")
	require.NoError(t, os.WriteFile(canary, []byte("unchanged"), 0o600))

	bundle := testBundleWithEntries(t, []testTarEntry{
		{name: "../outside", body: []byte("overwritten"), typeflag: tar.TypeReg, mode: 0o777},
	})
	err := store.RestoreWithOptions(bytes.NewReader(bundle), "", RestoreOptions{UnsignedOK: true})
	require.ErrorIs(t, err, securityerr.ErrPathEscape)
	got, readErr := os.ReadFile(canary)
	require.NoError(t, readErr)
	require.Equal(t, []byte("unchanged"), got)
}

func TestRestoreRejectsLinkBeforeTargetMutation(t *testing.T) {
	store := &Store{Root: filepath.Join(t.TempDir(), "store")}
	require.NoError(t, store.Init())
	bundle := testBundleWithEntries(t, []testTarEntry{
		{name: "secrets/token", typeflag: tar.TypeSymlink, linkname: "../../outside"},
	})
	err := store.RestoreWithOptions(bytes.NewReader(bundle), "", RestoreOptions{UnsignedOK: true})
	require.ErrorIs(t, err, securityerr.ErrUnsafeFilesystemNode)
	_, statErr := os.Lstat(filepath.Join(store.Root, "secrets", "token"))
	require.ErrorIs(t, statErr, os.ErrNotExist)
}

func TestRestoreIgnoresArchiveModes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows DACL assertions are covered by privatefs native tests")
	}
	root := t.TempDir()
	store := &Store{Root: filepath.Join(root, "store")}
	require.NoError(t, store.Init())
	secretsRoot := filepath.Join(root, "secrets")
	bundle := testBundleWithEntries(t, []testTarEntry{
		{name: "secrets/token", body: []byte("secret"), typeflag: tar.TypeReg, mode: 0o777},
	})
	require.NoError(t, store.RestoreWithOptions(bytes.NewReader(bundle), secretsRoot, RestoreOptions{UnsignedOK: true}))
	info, err := os.Stat(filepath.Join(secretsRoot, "token"))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	parent, err := os.Stat(secretsRoot)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o700), parent.Mode().Perm())
}

func TestValidatedBundleStagesSemanticsBeforeCommit(t *testing.T) {
	src := &Store{Root: filepath.Join(t.TempDir(), "src")}
	require.NoError(t, src.Init())
	a := newTestArtifact(NewID())
	require.NoError(t, src.WriteArtifact(a))
	payload, err := EncodePayload(MemoryPayload{Format: "markdown", Content: "safe"})
	require.NoError(t, err)
	require.NoError(t, src.AppendEvent(KindMemory, Event{
		EventID: NewID(), ArtifactID: a.ArtifactID, Type: EventTypeCreate,
		Provenance: Provenance{DeviceID: "dev", SourceAgent: "test"}, Payload: payload,
	}))
	var raw bytes.Buffer
	require.NoError(t, src.Bundle(&raw, BundleOpts{}))
	dst := &Store{Root: filepath.Join(t.TempDir(), "dst")}
	b, err := OpenValidatedBundle(bytes.NewReader(raw.Bytes()), BundleTarget{Store: dst}, RestoreOptions{UnsignedOK: true})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, b.Close()) })
	require.NoError(t, b.VerifySemantic())
	_, err = dst.ReadArtifact(KindMemory, a.ArtifactID)
	require.Error(t, err, "semantic verification must not mutate the target")
	require.NoError(t, b.Commit())
	_, err = dst.ReadArtifact(KindMemory, a.ArtifactID)
	require.NoError(t, err)
}

type testTarEntry struct {
	name     string
	body     []byte
	typeflag byte
	linkname string
	mode     int64
}

func testBundleWithEntries(t *testing.T, entries []testTarEntry) []byte {
	t.Helper()
	var out bytes.Buffer
	gz := gzip.NewWriter(&out)
	tw := tar.NewWriter(gz)
	meta, err := json.Marshal(BundleMeta{BundleVersion: "1.0"})
	require.NoError(t, err)
	require.NoError(t, tw.WriteHeader(&tar.Header{Name: "meta.json", Typeflag: tar.TypeReg, Mode: 0o600, Size: int64(len(meta))}))
	_, err = tw.Write(meta)
	require.NoError(t, err)
	for _, entry := range entries {
		hdr := &tar.Header{
			Name: entry.name, Typeflag: entry.typeflag, Linkname: entry.linkname,
			Mode: entry.mode, Size: int64(len(entry.body)),
		}
		require.NoError(t, tw.WriteHeader(hdr))
		if len(entry.body) != 0 {
			_, err := tw.Write(entry.body)
			require.NoError(t, err)
		}
	}
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())
	return out.Bytes()
}
