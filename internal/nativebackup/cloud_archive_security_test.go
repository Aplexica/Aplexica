package nativebackup

import (
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestCloudArchiveV2RoundTripWithoutPlaintextSpool(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	mustMkdirPrivate(t, source)
	if err := os.WriteFile(filepath.Join(source, "manifest.json"), []byte(`{"version":2}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "payload"), []byte("secret payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	outDir := filepath.Join(root, "out")
	mustMkdirPrivate(t, outDir)
	keyDir := filepath.Join(root, "keys")
	mustMkdirPrivate(t, keyDir)
	archive := filepath.Join(outDir, "snapshot.enc")
	keyring := filepath.Join(keyDir, "native-cloud-keyring-v2.cbor")
	meta, err := EncryptSnapshotDir(source, archive, keyring)
	if err != nil {
		t.Fatal(err)
	}
	if meta.Algorithm != CloudArchiveAlgorithm {
		t.Fatalf("algorithm = %q", meta.Algorithm)
	}
	b, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	if string(b[:len(cloudArchiveMagicV2)]) != cloudArchiveMagicV2 {
		t.Fatal("writer did not emit v2")
	}
	restored := filepath.Join(root, "restored")
	if _, err := DecryptSnapshotArchive(archive, restored, keyring); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(restored, "payload"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "secret payload" {
		t.Fatalf("payload = %q", got)
	}
	matches, err := filepath.Glob(filepath.Join(outDir, "*.tar.gz"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("plaintext spools: %v", matches)
	}
}

func TestCloudArchiveRejectsHugeChunkBeforeAllocation(t *testing.T) {
	root := t.TempDir()
	keyDir := filepath.Join(root, "keys")
	mustMkdirPrivate(t, keyDir)
	ring, err := (CloudBackupKeyStore{Path: filepath.Join(keyDir, "ring.cbor")}).LoadOrCreate()
	if err != nil {
		t.Fatal(err)
	}
	k, err := currentKey(ring)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytesBuffer
	ew, err := newCloudEncryptWriter(&buf, k)
	if err != nil {
		t.Fatal(err)
	}
	if err := ew.Close(); err != nil {
		t.Fatal(err)
	}
	b := buf.b
	headerEnd := len(cloudArchiveMagicV2) + 4 + int(binary.BigEndian.Uint32(b[len(cloudArchiveMagicV2):]))
	binary.BigEndian.PutUint32(b[headerEnd:headerEnd+4], 0xffffffff)
	cr, _, err := newCloudDecryptReader(&sliceReader{b: b}, ring, defaultCloudLimits())
	if err != nil {
		t.Fatal(err)
	}
	var one [1]byte
	if _, err = cr.Read(one[:]); err == nil {
		t.Fatal("huge chunk accepted")
	}
}

func TestCloudKeyringConcurrentFirstUseConverges(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "keys")
	mustMkdirPrivate(t, dir)
	path := filepath.Join(dir, "ring.cbor")
	const workers = 32
	results := make(chan CloudBackupKeyRingV2, workers)
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, e := (CloudBackupKeyStore{Path: path}).LoadOrCreate()
			if e != nil {
				errs <- e
				return
			}
			results <- r
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for e := range errs {
		t.Fatal(e)
	}
	var id [32]byte
	for r := range results {
		if id == ([32]byte{}) {
			id = r.CurrentKeyID
		} else if r.CurrentKeyID != id {
			t.Fatal("creators selected different keys")
		}
	}
	if _, err := (CloudBackupKeyStore{Path: path}).load(); err != nil {
		t.Fatal(err)
	}
}

func mustMkdirPrivate(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o700); err != nil {
		t.Fatal(err)
	}
}

type bytesBuffer struct{ b []byte }

func (b *bytesBuffer) Write(p []byte) (int, error) { b.b = append(b.b, p...); return len(p), nil }

type sliceReader struct{ b []byte }

func (r *sliceReader) Read(p []byte) (int, error) {
	if len(r.b) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.b)
	r.b = r.b[n:]
	return n, nil
}
