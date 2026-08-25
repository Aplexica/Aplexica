package adapter

import (
	"errors"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"
)

// fakeSecretWriter records Put and AddUsedByTool calls so WriteToolSecrets can
// be exercised without importing internal/secrets (mirroring fakeSecretStore in
// tool_dedup_test.go).
type fakeSecretWriter struct {
	put       map[string]string // name -> value
	links     map[string]string // name -> artifactID
	deleted   []string
	unlinked  []string
	listErr   error
	deleteErr error
	unlinkErr error
	putErr    error
	linkErr   error
}

func newFakeSecretWriter() *fakeSecretWriter {
	return &fakeSecretWriter{put: map[string]string{}, links: map[string]string{}}
}

func (f *fakeSecretWriter) Put(artifactID, key, value string) error {
	if f.putErr != nil {
		return f.putErr
	}
	f.put[key] = value
	return nil
}

func (f *fakeSecretWriter) ListForArtifact(string) ([]string, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	keys := make([]string, 0, len(f.put))
	for key := range f.put {
		keys = append(keys, key)
	}
	return keys, nil
}

func (f *fakeSecretWriter) Delete(_ string, key string) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	delete(f.put, key)
	f.deleted = append(f.deleted, key)
	return nil
}

func (f *fakeSecretWriter) AddUsedByTool(name, artifactID string) error {
	if f.linkErr != nil {
		return f.linkErr
	}
	f.links[name] = artifactID
	return nil
}

func (f *fakeSecretWriter) UnlinkToolSecret(name, artifactID string) error {
	if f.unlinkErr != nil {
		return f.unlinkErr
	}
	delete(f.links, name)
	f.unlinked = append(f.unlinked, name)
	return nil
}

func TestWriteToolSecrets_PutsAndLinks(t *testing.T) {
	f := newFakeSecretWriter()
	extracted := map[string]string{
		"github.TOKEN": "tok",
		"db.PASSWORD":  "pw",
	}
	require.NoError(t, WriteToolSecrets(f, "art-1", extracted))

	require.Equal(t, extracted, f.put, "every secret value must be stored")

	var linkedNames []string
	for name, id := range f.links {
		require.Equal(t, "art-1", id, "each secret must be linked to the importing artifact")
		linkedNames = append(linkedNames, name)
	}
	sort.Strings(linkedNames)
	require.Equal(t, []string{"db.PASSWORD", "github.TOKEN"}, linkedNames,
		"every extracted secret must be recorded in usedByTools")
}

func TestWriteToolSecrets_PrunesStaleSecrets(t *testing.T) {
	f := newFakeSecretWriter()
	f.put["old.TOKEN"] = "old"
	f.links["old.TOKEN"] = "art-1"

	require.NoError(t, WriteToolSecrets(f, "art-1", map[string]string{"new.TOKEN": "new"}))

	require.Equal(t, map[string]string{"new.TOKEN": "new"}, f.put)
	require.Equal(t, map[string]string{"new.TOKEN": "art-1"}, f.links)
	require.Equal(t, []string{"old.TOKEN"}, f.deleted)
	require.Equal(t, []string{"old.TOKEN"}, f.unlinked)
}

func TestWriteToolSecrets_PutErrorWrappedWithName(t *testing.T) {
	f := newFakeSecretWriter()
	f.putErr = errors.New("disk full")
	err := WriteToolSecrets(f, "art-1", map[string]string{"github.TOKEN": "tok"})
	require.ErrorContains(t, err, "store secret github.TOKEN")
}

func TestWriteToolSecrets_LinkErrorWrappedWithName(t *testing.T) {
	f := newFakeSecretWriter()
	f.linkErr = errors.New("meta write failed")
	err := WriteToolSecrets(f, "art-1", map[string]string{"github.TOKEN": "tok"})
	require.ErrorContains(t, err, "link secret github.TOKEN")
}
