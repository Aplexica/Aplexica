package nativebackup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/aplexica/aplexica/internal/privatefs"
	"github.com/aplexica/aplexica/internal/safepath"
	"github.com/fxamacker/cbor/v2"
)

const (
	CloudArchiveAlgorithm           = "AES-256-GCM-HKDF-SHA256"
	cloudArchiveMagicV2             = "APLEXICA-NATIVE-BACKUP-CLOUD-2\n"
	cloudArchiveMagicV1             = "APLEXICA-NATIVE-BACKUP-CLOUD-1\n"
	cloudArchiveAlgorithmV1         = "aplexica-native-backup-chunked-aes-256-gcm-v1"
	cloudArchiveChunkSize           = 1024 * 1024
	cloudArchiveMaxHeaderBytes      = 64 * 1024
	cloudBackupKeySize              = 32
	cloudBackupKeyLoadAttempts      = 64
	cloudBackupKeyLoadRetryDelay    = 2 * time.Millisecond
	cloudKeyringMaxBytes            = 32 * 1024
	cloudKeyringMaxRecords          = 256
	cloudArchiveNonceSize           = 12
	cloudArchiveDefaultMaxEncrypted = int64(64 << 30)
	cloudArchiveDefaultMaxPlain     = int64(64 << 30)
	cloudArchiveDefaultMaxChunks    = uint64(1 << 20)
	cloudArchiveMaxEntries          = 1_000_000
	cloudArchiveMaxPathBytes        = 4096
	cloudArchiveMaxDepth            = 128
)

var canonicalCBOR cbor.EncMode
var strictCBOR cbor.DecMode

func init() {
	var err error
	canonicalCBOR, err = cbor.CanonicalEncOptions().EncMode()
	if err != nil {
		panic(err)
	}
	strictCBOR, err = cbor.DecOptions{
		DupMapKey:         cbor.DupMapKeyEnforcedAPF,
		IndefLength:       cbor.IndefLengthForbidden,
		TagsMd:            cbor.TagsForbidden,
		MaxArrayElements:  1024,
		MaxMapPairs:       1024,
		ExtraReturnErrors: cbor.ExtraDecErrorUnknownField,
	}.DecMode()
	if err != nil {
		panic(err)
	}
}

type CloudArchiveMeta struct {
	Algorithm      string `json:"algorithm"`
	KeyID          string `json:"keyId"`
	PlainBytes     int64  `json:"plainBytes"`
	PlainSHA256    string `json:"plainSha256"`
	EncryptedBytes int64  `json:"encryptedBytes"`
	CipherSHA256   string `json:"cipherSha256"`
}

type CloudArchiveHeaderV2 struct {
	Version       uint16   `cbor:"version"`
	Algorithm     string   `cbor:"algorithm"`
	MasterKeyID   [32]byte `cbor:"masterKeyId"`
	ArchiveSalt   [32]byte `cbor:"archiveSalt"`
	ChunkSize     uint32   `cbor:"chunkSize"`
	CreatedAtUnix int64    `cbor:"createdAtUnix"`
}

type cloudArchiveHeaderV1 struct {
	Version     int       `json:"version"`
	Algorithm   string    `json:"algorithm"`
	KeyID       string    `json:"keyId"`
	ChunkSize   int       `json:"chunkSize"`
	NoncePrefix []byte    `json:"noncePrefix"`
	CreatedAt   time.Time `json:"createdAt"`
}

type CloudBackupKeyRecordV2 struct {
	KeyID         [32]byte `cbor:"keyId"`
	CreatedAtUnix int64    `cbor:"createdAtUnix"`
	State         string   `cbor:"state"`
	Key           [32]byte `cbor:"key"`
}

type CloudBackupKeyRingV2 struct {
	Version      uint16                   `cbor:"version"`
	Generation   uint64                   `cbor:"generation"`
	CurrentKeyID [32]byte                 `cbor:"currentKeyId"`
	Keys         []CloudBackupKeyRecordV2 `cbor:"keys"`
	Checksum     [32]byte                 `cbor:"checksum"`
}

type CloudDecryptLimits struct {
	MaxEncryptedBytes int64
	MaxPlainBytes     int64
	MaxChunks         uint64
}

type CloudBackupKeyStore struct{ Path string }

func defaultCloudLimits() CloudDecryptLimits {
	return CloudDecryptLimits{cloudArchiveDefaultMaxEncrypted, cloudArchiveDefaultMaxPlain, cloudArchiveDefaultMaxChunks}
}

func keyIDFor(key [32]byte) ([32]byte, error) {
	b, err := canonicalCBOR.Marshal([]any{"aplexica/native-cloud/master-key-id/v2", key[:]})
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(b), nil
}

func keyringChecksum(r CloudBackupKeyRingV2) ([32]byte, error) {
	r.Checksum = [32]byte{}
	b, err := canonicalCBOR.Marshal([]any{"aplexica/native-cloud/keyring/v2", r})
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(b), nil
}

func validateKeyring(r CloudBackupKeyRingV2) error {
	if r.Version != 2 || r.Generation == 0 || len(r.Keys) == 0 || len(r.Keys) > cloudKeyringMaxRecords {
		return fmt.Errorf("nativebackup: invalid cloud keyring shape")
	}
	current := 0
	for i := range r.Keys {
		if i > 0 && bytes.Compare(r.Keys[i-1].KeyID[:], r.Keys[i].KeyID[:]) >= 0 {
			return fmt.Errorf("nativebackup: cloud keyring IDs are not unique and sorted")
		}
		id, err := keyIDFor(r.Keys[i].Key)
		if err != nil || id != r.Keys[i].KeyID {
			return fmt.Errorf("nativebackup: cloud keyring key ID mismatch")
		}
		if r.Keys[i].CreatedAtUnix <= 0 || r.Keys[i].CreatedAtUnix > time.Now().Add(24*time.Hour).Unix() {
			return fmt.Errorf("nativebackup: invalid cloud key timestamp")
		}
		switch r.Keys[i].State {
		case "current":
			current++
			if r.Keys[i].KeyID != r.CurrentKeyID {
				return fmt.Errorf("nativebackup: current cloud key mismatch")
			}
		case "retired":
		default:
			return fmt.Errorf("nativebackup: invalid cloud key state")
		}
	}
	if current != 1 {
		return fmt.Errorf("nativebackup: cloud keyring must have exactly one current key")
	}
	want, err := keyringChecksum(r)
	if err != nil || want != r.Checksum {
		return fmt.Errorf("nativebackup: cloud keyring checksum mismatch")
	}
	return nil
}

func encodeKeyring(r CloudBackupKeyRingV2) ([]byte, error) {
	sum, err := keyringChecksum(r)
	if err != nil {
		return nil, err
	}
	r.Checksum = sum
	return canonicalCBOR.Marshal(r)
}

func decodeKeyring(data []byte) (CloudBackupKeyRingV2, error) {
	if len(data) == 0 || len(data) > cloudKeyringMaxBytes {
		return CloudBackupKeyRingV2{}, fmt.Errorf("nativebackup: invalid cloud keyring size")
	}
	var r CloudBackupKeyRingV2
	if err := strictCBOR.Unmarshal(data, &r); err != nil {
		return r, fmt.Errorf("nativebackup: decode cloud keyring: %w", err)
	}
	if err := validateKeyring(r); err != nil {
		return r, err
	}
	return r, nil
}

func (s CloudBackupKeyStore) load() (CloudBackupKeyRingV2, error) {
	data, err := os.ReadFile(s.Path)
	if err != nil {
		return CloudBackupKeyRingV2{}, err
	}
	return decodeKeyring(data)
}

// loadWithRetry tolerates the short sharing/lock violations Windows can
// expose while another creator finishes installing the keyring or endpoint
// protection finishes inspecting it. Missing files are retried only after an
// install collision; every other error (including malformed keyring bytes and
// permission failures) remains fail-fast.
func (s CloudBackupKeyStore) loadWithRetry(retryMissing bool) (CloudBackupKeyRingV2, error) {
	var loadErr error
	for attempt := range cloudBackupKeyLoadAttempts {
		if existing, err := s.load(); err == nil {
			return existing, nil
		} else {
			loadErr = err
		}
		if !transientCloudKeyringLoadError(loadErr) &&
			!(retryMissing && errors.Is(loadErr, os.ErrNotExist)) {
			break
		}
		if attempt+1 < cloudBackupKeyLoadAttempts {
			time.Sleep(cloudBackupKeyLoadRetryDelay)
		}
	}
	return CloudBackupKeyRingV2{}, loadErr
}

func (s CloudBackupKeyStore) LoadOrCreate() (CloudBackupKeyRingV2, error) {
	if r, err := s.loadWithRetry(false); err == nil {
		return r, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return CloudBackupKeyRingV2{}, err
	}
	parent, base := filepath.Dir(s.Path), filepath.Base(s.Path)
	if err := privatefs.EnsureDir(parent, privatefs.DirPolicy{Access: privatefs.AccessPrivate, RepairOwned: true, AllowExisting: true}); err != nil {
		return CloudBackupKeyRingV2{}, err
	}
	root, err := privatefs.OpenRoot(parent, privatefs.DirPolicy{Access: privatefs.AccessPrivate, RepairOwned: true, AllowExisting: true})
	if err != nil {
		return CloudBackupKeyRingV2{}, err
	}
	defer root.Close()
	var key [32]byte
	if _, err := io.ReadFull(rand.Reader, key[:]); err != nil {
		return CloudBackupKeyRingV2{}, fmt.Errorf("nativebackup: generate cloud key: %w", err)
	}
	id, err := keyIDFor(key)
	if err != nil {
		return CloudBackupKeyRingV2{}, err
	}
	r := CloudBackupKeyRingV2{Version: 2, Generation: 1, CurrentKeyID: id, Keys: []CloudBackupKeyRecordV2{{KeyID: id, CreatedAtUnix: time.Now().Unix(), State: "current", Key: key}}}
	b, err := encodeKeyring(r)
	if err != nil {
		return CloudBackupKeyRingV2{}, err
	}
	tmp, tempRel, err := root.CreateTemp(".", ".cloud-keyring-")
	if err != nil {
		return CloudBackupKeyRingV2{}, err
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = tmp.Close()
			_ = root.RemoveRegular(tempRel)
		}
	}()
	if _, err = tmp.Write(b); err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return CloudBackupKeyRingV2{}, err
	}
	if err = root.InstallNoReplace(tempRel, base); err != nil {
		if errors.Is(err, os.ErrExist) {
			if existing, loadErr := s.loadWithRetry(true); loadErr == nil {
				return existing, nil
			} else {
				return CloudBackupKeyRingV2{}, fmt.Errorf("nativebackup: load concurrently installed cloud keyring: %w", loadErr)
			}
		}
		return CloudBackupKeyRingV2{}, err
	}
	cleanup = false
	return s.loadWithRetry(false)
}

// MigrateLegacyCloudBackupKey converts the former raw 32-byte installation
// key into the fixed v2 keyring without changing the key material, so existing
// v1 archives remain decryptable. It is safe to call repeatedly and removes
// the legacy duplicate only after reopening and validating the installed ring.
func MigrateLegacyCloudBackupKey(legacyPath, keyringPath string) error {
	store := CloudBackupKeyStore{Path: keyringPath}
	if _, err := store.load(); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	legacyParent, legacyBase := filepath.Dir(legacyPath), filepath.Base(legacyPath)
	legacyRoot, err := privatefs.OpenRoot(legacyParent, privatefs.DirPolicy{Access: privatefs.AccessPrivate})
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer legacyRoot.Close()
	f, err := legacyRoot.OpenReadRegular(legacyBase)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	raw, err := io.ReadAll(io.LimitReader(f, cloudBackupKeySize+1))
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if len(raw) != cloudBackupKeySize {
		return fmt.Errorf("nativebackup: invalid legacy cloud backup key length")
	}
	var key [32]byte
	copy(key[:], raw)
	id, err := keyIDFor(key)
	if err != nil {
		return err
	}
	ring := CloudBackupKeyRingV2{Version: 2, Generation: 1, CurrentKeyID: id, Keys: []CloudBackupKeyRecordV2{{KeyID: id, CreatedAtUnix: time.Now().Unix(), State: "current", Key: key}}}
	b, err := encodeKeyring(ring)
	if err != nil {
		return err
	}
	parent, base := filepath.Dir(keyringPath), filepath.Base(keyringPath)
	root, err := privatefs.OpenRoot(parent, privatefs.DirPolicy{Access: privatefs.AccessPrivate, RepairOwned: true, AllowExisting: true})
	if err != nil {
		return err
	}
	defer root.Close()
	tmp, rel, err := root.CreateTemp(".", ".cloud-keyring-migrate-")
	if err != nil {
		return err
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = tmp.Close()
			_ = root.RemoveRegular(rel)
		}
	}()
	if _, err = tmp.Write(b); err == nil {
		err = tmp.Sync()
	}
	if e := tmp.Close(); err == nil {
		err = e
	}
	if err != nil {
		return err
	}
	if err := root.InstallNoReplace(rel, base); err != nil {
		if _, loadErr := store.load(); loadErr != nil {
			return err
		}
	} else {
		cleanup = false
	}
	if _, err := store.load(); err != nil {
		return err
	}
	return legacyRoot.RemoveRegular(legacyBase)
}

func currentKey(r CloudBackupKeyRingV2) (CloudBackupKeyRecordV2, error) {
	for _, k := range r.Keys {
		if k.KeyID == r.CurrentKeyID {
			return k, nil
		}
	}
	return CloudBackupKeyRecordV2{}, fmt.Errorf("nativebackup: current cloud key absent")
}
func keyByID(r CloudBackupKeyRingV2, id [32]byte) (CloudBackupKeyRecordV2, error) {
	for _, k := range r.Keys {
		if k.KeyID == id {
			return k, nil
		}
	}
	return CloudBackupKeyRecordV2{}, fmt.Errorf("nativebackup: archive cloud key is unavailable")
}

func EncryptSnapshotDir(snapshotDir, encryptedPath, keyPath string) (CloudArchiveMeta, error) {
	return EncryptSnapshotDirContext(context.Background(), snapshotDir, encryptedPath, keyPath)
}

func EncryptSnapshotDirContext(ctx context.Context, snapshotDir, encryptedPath, keyPath string) (CloudArchiveMeta, error) {
	if snapshotDir == "" || encryptedPath == "" || keyPath == "" {
		return CloudArchiveMeta{}, fmt.Errorf("nativebackup: snapshotDir, encryptedPath, and keyPath are required")
	}
	if err := ctx.Err(); err != nil {
		return CloudArchiveMeta{}, err
	}
	r, err := (CloudBackupKeyStore{Path: keyPath}).LoadOrCreate()
	if err != nil {
		return CloudArchiveMeta{}, err
	}
	k, err := currentKey(r)
	if err != nil {
		return CloudArchiveMeta{}, err
	}
	parent := filepath.Dir(encryptedPath)
	if err := privatefs.EnsureDir(parent, privatefs.DirPolicy{Access: privatefs.AccessPrivate, RepairOwned: true, AllowExisting: true}); err != nil {
		return CloudArchiveMeta{}, err
	}
	root, err := privatefs.OpenRoot(parent, privatefs.DirPolicy{Access: privatefs.AccessPrivate, RepairOwned: true, AllowExisting: true})
	if err != nil {
		return CloudArchiveMeta{}, err
	}
	defer root.Close()
	out, tempRel, err := root.CreateTemp(".", ".aplexica-cloud-archive-")
	if err != nil {
		return CloudArchiveMeta{}, err
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = out.Close()
			_ = root.RemoveRegular(tempRel)
		}
	}()
	h := sha256.New()
	cw := &countingWriter{w: io.MultiWriter(out, h)}
	ew, err := newCloudEncryptWriter(cw, k)
	if err != nil {
		return CloudArchiveMeta{}, err
	}
	plainBytes, plainHash, err := writeSnapshotTarGzContext(ctx, snapshotDir, ew)
	if err == nil {
		err = ew.Close()
	}
	if err == nil {
		err = out.Sync()
	}
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return CloudArchiveMeta{}, err
	}
	final := filepath.Base(encryptedPath)
	if err = root.Replace(tempRel, final, ""); err != nil {
		return CloudArchiveMeta{}, err
	}
	cleanup = false
	return CloudArchiveMeta{Algorithm: CloudArchiveAlgorithm, KeyID: hex.EncodeToString(k.KeyID[:]), PlainBytes: plainBytes, PlainSHA256: plainHash, EncryptedBytes: cw.n, CipherSHA256: hex.EncodeToString(h.Sum(nil))}, nil
}

func DecryptSnapshotArchive(encryptedPath, destDir, keyPath string) (CloudArchiveMeta, error) {
	limits := defaultCloudLimits()
	st, err := os.Stat(encryptedPath)
	if err != nil {
		return CloudArchiveMeta{}, err
	}
	if !st.Mode().IsRegular() || st.Size() <= 0 || st.Size() > limits.MaxEncryptedBytes {
		return CloudArchiveMeta{}, fmt.Errorf("nativebackup: encrypted archive exceeds limit")
	}
	r, err := (CloudBackupKeyStore{Path: keyPath}).load()
	if err != nil {
		return CloudArchiveMeta{}, err
	}
	in, err := os.Open(encryptedPath)
	if err != nil {
		return CloudArchiveMeta{}, err
	}
	defer in.Close()
	cipherHash := sha256.New()
	counted := &countingReader{r: io.TeeReader(io.LimitReader(in, limits.MaxEncryptedBytes+1), cipherHash)}
	cr, algorithm, keyID, err := newCloudDecryptReaderAny(counted, r, limits)
	if err != nil {
		return CloudArchiveMeta{}, err
	}
	if err := privatefs.EnsureDir(destDir, privatefs.DirPolicy{Access: privatefs.AccessPrivate, RepairOwned: true, AllowExisting: true}); err != nil {
		return CloudArchiveMeta{}, err
	}
	plainHash := sha256.New()
	plainCount := &countingReader{r: io.TeeReader(cr, plainHash)}
	if err := extractSnapshotTarGzReader(plainCount, destDir, limits.MaxPlainBytes); err != nil {
		return CloudArchiveMeta{}, err
	}
	if err := cr.VerifyComplete(); err != nil {
		return CloudArchiveMeta{}, err
	}
	return CloudArchiveMeta{Algorithm: algorithm, KeyID: keyID, PlainBytes: plainCount.n, PlainSHA256: hex.EncodeToString(plainHash.Sum(nil)), EncryptedBytes: counted.n, CipherSHA256: hex.EncodeToString(cipherHash.Sum(nil))}, nil
}

type countingWriter struct {
	n int64
	w io.Writer
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, e := c.w.Write(p)
	c.n += int64(n)
	return n, e
}

type countingReader struct {
	n int64
	r io.Reader
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, e := c.r.Read(p)
	c.n += int64(n)
	return n, e
}

type cloudEncryptWriter struct {
	w          io.Writer
	gcm        cipher.AEAD
	headerHash [32]byte
	index      uint64
	buf        []byte
	n          int
	total      uint64
	digest     hash.Hash
	closed     bool
}

func deriveArchiveKey(master [32]byte, salt [32]byte, id [32]byte) ([]byte, error) {
	info, err := canonicalCBOR.Marshal([]any{"aplexica/native-cloud/archive-key/v2", id[:]})
	if err != nil {
		return nil, err
	}
	return hkdf.Key(sha256.New, master[:], salt[:], string(info), 32)
}
func nonceFor(i uint64) []byte { var n [12]byte; binary.BigEndian.PutUint64(n[4:], i); return n[:] }
func aadFor(domain string, hh [32]byte, i uint64) ([]byte, error) {
	return canonicalCBOR.Marshal([]any{domain, hh[:], i})
}

func newCloudEncryptWriter(w io.Writer, master CloudBackupKeyRecordV2) (*cloudEncryptWriter, error) {
	var salt [32]byte
	if _, err := io.ReadFull(rand.Reader, salt[:]); err != nil {
		return nil, err
	}
	if salt == ([32]byte{}) {
		return nil, fmt.Errorf("nativebackup: random archive salt is zero")
	}
	hdr := CloudArchiveHeaderV2{Version: 2, Algorithm: CloudArchiveAlgorithm, MasterKeyID: master.KeyID, ArchiveSalt: salt, ChunkSize: cloudArchiveChunkSize, CreatedAtUnix: time.Now().Unix()}
	hb, err := canonicalCBOR.Marshal(hdr)
	if err != nil {
		return nil, err
	}
	if len(hb) > cloudArchiveMaxHeaderBytes {
		return nil, fmt.Errorf("nativebackup: header too large")
	}
	key, err := deriveArchiveKey(master.Key, salt, master.KeyID)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if _, err = w.Write([]byte(cloudArchiveMagicV2)); err != nil {
		return nil, err
	}
	if err = binary.Write(w, binary.BigEndian, uint32(len(hb))); err != nil {
		return nil, err
	}
	if _, err = w.Write(hb); err != nil {
		return nil, err
	}
	return &cloudEncryptWriter{w: w, gcm: gcm, headerHash: sha256.Sum256(hb), buf: make([]byte, cloudArchiveChunkSize), digest: sha256.New()}, nil
}
func (e *cloudEncryptWriter) Write(p []byte) (int, error) {
	if e.closed {
		return 0, fmt.Errorf("nativebackup: archive writer closed")
	}
	written := 0
	for len(p) > 0 {
		n := copy(e.buf[e.n:], p)
		e.n += n
		p = p[n:]
		written += n
		if e.total > math.MaxUint64-uint64(n) {
			return written, fmt.Errorf("nativebackup: plaintext size overflow")
		}
		e.total += uint64(n)
		_, _ = e.digest.Write(e.buf[e.n-n : e.n])
		if e.n == len(e.buf) {
			if err := e.flush(); err != nil {
				return written, err
			}
		}
	}
	return written, nil
}
func (e *cloudEncryptWriter) flush() error {
	if e.index == math.MaxUint64 {
		return fmt.Errorf("nativebackup: nonce exhausted")
	}
	aad, err := aadFor("aplexica/native-cloud/chunk/v2", e.headerHash, e.index)
	if err != nil {
		return err
	}
	ct := e.gcm.Seal(nil, nonceFor(e.index), e.buf[:e.n], aad)
	if err = binary.Write(e.w, binary.BigEndian, uint32(len(ct))); err != nil {
		return err
	}
	if _, err = e.w.Write(ct); err != nil {
		return err
	}
	e.index++
	e.n = 0
	return nil
}

type cloudFinalV2 struct {
	ChunkCount  uint64   `cbor:"chunkCount"`
	PlainBytes  uint64   `cbor:"plainBytes"`
	PlainSHA256 [32]byte `cbor:"plainSha256"`
}

func (e *cloudEncryptWriter) Close() error {
	if e.closed {
		return nil
	}
	e.closed = true
	if e.n > 0 {
		if err := e.flush(); err != nil {
			return err
		}
	}
	if e.index == math.MaxUint64 {
		return fmt.Errorf("nativebackup: nonce exhausted")
	}
	if err := binary.Write(e.w, binary.BigEndian, uint32(0)); err != nil {
		return err
	}
	var d [32]byte
	copy(d[:], e.digest.Sum(nil))
	tr, err := canonicalCBOR.Marshal(cloudFinalV2{e.index, e.total, d})
	if err != nil {
		return err
	}
	aad, err := aadFor("aplexica/native-cloud/final/v2", e.headerHash, e.index)
	if err != nil {
		return err
	}
	ct := e.gcm.Seal(nil, nonceFor(e.index), tr, aad)
	if err := binary.Write(e.w, binary.BigEndian, uint32(len(ct))); err != nil {
		return err
	}
	_, err = e.w.Write(ct)
	return err
}

type cloudDecryptReader struct {
	r          io.Reader
	gcm        cipher.AEAD
	headerHash [32]byte
	limits     CloudDecryptLimits
	index      uint64
	total      uint64
	digest     hash.Hash
	plain      []byte
	off        int
	final      bool
	verified   bool
}

type cloudPlainReader interface {
	io.Reader
	VerifyComplete() error
}

func newCloudDecryptReaderAny(r io.Reader, ring CloudBackupKeyRingV2, limits CloudDecryptLimits) (cloudPlainReader, string, string, error) {
	prefix := make([]byte, len(cloudArchiveMagicV2))
	if _, err := io.ReadFull(r, prefix); err != nil {
		return nil, "", "", err
	}
	replay := io.MultiReader(bytes.NewReader(prefix), r)
	switch string(prefix) {
	case cloudArchiveMagicV2:
		d, h, err := newCloudDecryptReader(replay, ring, limits)
		return d, h.Algorithm, hex.EncodeToString(h.MasterKeyID[:]), err
	case cloudArchiveMagicV1:
		d, h, err := newCloudDecryptReaderV1(replay, ring, limits)
		return d, h.Algorithm, h.KeyID, err
	default:
		return nil, "", "", fmt.Errorf("nativebackup: unsupported cloud backup archive")
	}
}

func validateHeader(h CloudArchiveHeaderV2) error {
	if h.Version != 2 || h.Algorithm != CloudArchiveAlgorithm || h.ArchiveSalt == ([32]byte{}) || h.MasterKeyID == ([32]byte{}) || h.ChunkSize != cloudArchiveChunkSize {
		return fmt.Errorf("nativebackup: invalid cloud archive header")
	}
	if h.CreatedAtUnix <= 0 || h.CreatedAtUnix > time.Now().Add(24*time.Hour).Unix() {
		return fmt.Errorf("nativebackup: invalid cloud archive timestamp")
	}
	return nil
}
func newCloudDecryptReader(r io.Reader, ring CloudBackupKeyRingV2, limits CloudDecryptLimits) (*cloudDecryptReader, CloudArchiveHeaderV2, error) {
	magic := make([]byte, len(cloudArchiveMagicV2))
	if _, err := io.ReadFull(r, magic); err != nil {
		return nil, CloudArchiveHeaderV2{}, err
	}
	if string(magic) != cloudArchiveMagicV2 {
		return nil, CloudArchiveHeaderV2{}, fmt.Errorf("nativebackup: unsupported cloud backup archive")
	}
	var hl uint32
	if err := binary.Read(r, binary.BigEndian, &hl); err != nil {
		return nil, CloudArchiveHeaderV2{}, err
	}
	if hl == 0 || hl > cloudArchiveMaxHeaderBytes {
		return nil, CloudArchiveHeaderV2{}, fmt.Errorf("nativebackup: invalid cloud header length")
	}
	hb := make([]byte, hl)
	if _, err := io.ReadFull(r, hb); err != nil {
		return nil, CloudArchiveHeaderV2{}, err
	}
	var h CloudArchiveHeaderV2
	if err := strictCBOR.Unmarshal(hb, &h); err != nil {
		return nil, h, err
	}
	if err := validateHeader(h); err != nil {
		return nil, h, err
	}
	k, err := keyByID(ring, h.MasterKeyID)
	if err != nil {
		return nil, h, err
	}
	key, err := deriveArchiveKey(k.Key, h.ArchiveSalt, h.MasterKeyID)
	if err != nil {
		return nil, h, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, h, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, h, err
	}
	return &cloudDecryptReader{r: r, gcm: gcm, headerHash: sha256.Sum256(hb), limits: limits, digest: sha256.New()}, h, nil
}
func (d *cloudDecryptReader) Read(p []byte) (int, error) {
	if d.off < len(d.plain) {
		n := copy(p, d.plain[d.off:])
		d.off += n
		return n, nil
	}
	if d.final {
		return 0, io.EOF
	}
	if d.index >= d.limits.MaxChunks {
		return 0, fmt.Errorf("nativebackup: cloud archive chunk limit exceeded")
	}
	var l uint32
	if err := binary.Read(d.r, binary.BigEndian, &l); err != nil {
		return 0, err
	}
	if l == 0 {
		if err := d.readFinal(); err != nil {
			return 0, err
		}
		d.final = true
		return 0, io.EOF
	}
	min := uint32(d.gcm.Overhead())
	max := uint32(cloudArchiveChunkSize + d.gcm.Overhead())
	if l < min || l > max {
		return 0, fmt.Errorf("nativebackup: invalid encrypted chunk length")
	}
	ct := make([]byte, max)
	if _, err := io.ReadFull(d.r, ct[:l]); err != nil {
		return 0, err
	}
	aad, err := aadFor("aplexica/native-cloud/chunk/v2", d.headerHash, d.index)
	if err != nil {
		return 0, err
	}
	plain, err := d.gcm.Open(ct[:0], nonceFor(d.index), ct[:l], aad)
	if err != nil {
		return 0, fmt.Errorf("nativebackup: authenticate cloud chunk: %w", err)
	}
	if uint64(len(plain)) > uint64(d.limits.MaxPlainBytes)-d.total {
		return 0, fmt.Errorf("nativebackup: cloud plaintext limit exceeded")
	}
	d.total += uint64(len(plain))
	_, _ = d.digest.Write(plain)
	d.index++
	d.plain = plain
	d.off = 0
	return d.Read(p)
}
func (d *cloudDecryptReader) readFinal() error {
	if d.index == math.MaxUint64 {
		return fmt.Errorf("nativebackup: nonce exhausted")
	}
	var l uint32
	if err := binary.Read(d.r, binary.BigEndian, &l); err != nil {
		return err
	}
	if l < uint32(d.gcm.Overhead()) || l > 4096 {
		return fmt.Errorf("nativebackup: invalid final record length")
	}
	buf := make([]byte, 4096)
	if _, err := io.ReadFull(d.r, buf[:l]); err != nil {
		return err
	}
	aad, err := aadFor("aplexica/native-cloud/final/v2", d.headerHash, d.index)
	if err != nil {
		return err
	}
	plain, err := d.gcm.Open(buf[:0], nonceFor(d.index), buf[:l], aad)
	if err != nil {
		return fmt.Errorf("nativebackup: authenticate final record: %w", err)
	}
	var f cloudFinalV2
	if err := strictCBOR.Unmarshal(plain, &f); err != nil {
		return err
	}
	var got [32]byte
	copy(got[:], d.digest.Sum(nil))
	if f.ChunkCount != d.index || f.PlainBytes != d.total || f.PlainSHA256 != got {
		return fmt.Errorf("nativebackup: cloud archive final record mismatch")
	}
	var extra [1]byte
	n, e := d.r.Read(extra[:])
	if n != 0 || e != io.EOF {
		return fmt.Errorf("nativebackup: trailing cloud archive data")
	}
	d.verified = true
	return nil
}
func (d *cloudDecryptReader) VerifyComplete() error {
	if !d.final || !d.verified {
		return fmt.Errorf("nativebackup: cloud archive was not completely authenticated")
	}
	return nil
}

type cloudDecryptReaderV1 struct {
	r        io.Reader
	gcm      cipher.AEAD
	header   []byte
	prefix   [4]byte
	limits   CloudDecryptLimits
	index    uint64
	total    uint64
	plain    []byte
	off      int
	finished bool
}

func legacyKeyID(key [32]byte) string {
	sum := sha256.Sum256(key[:])
	return hex.EncodeToString(sum[:8])
}

func newCloudDecryptReaderV1(r io.Reader, ring CloudBackupKeyRingV2, limits CloudDecryptLimits) (*cloudDecryptReaderV1, cloudArchiveHeaderV1, error) {
	magic := make([]byte, len(cloudArchiveMagicV1))
	if _, err := io.ReadFull(r, magic); err != nil {
		return nil, cloudArchiveHeaderV1{}, err
	}
	if string(magic) != cloudArchiveMagicV1 {
		return nil, cloudArchiveHeaderV1{}, fmt.Errorf("nativebackup: invalid v1 cloud archive magic")
	}
	var hl uint32
	if err := binary.Read(r, binary.BigEndian, &hl); err != nil {
		return nil, cloudArchiveHeaderV1{}, err
	}
	if hl == 0 || hl > cloudArchiveMaxHeaderBytes {
		return nil, cloudArchiveHeaderV1{}, fmt.Errorf("nativebackup: invalid v1 cloud header length")
	}
	hb := make([]byte, hl)
	if _, err := io.ReadFull(r, hb); err != nil {
		return nil, cloudArchiveHeaderV1{}, err
	}
	var h cloudArchiveHeaderV1
	dec := json.NewDecoder(bytes.NewReader(hb))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&h); err != nil {
		return nil, h, fmt.Errorf("nativebackup: decode v1 cloud header: %w", err)
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF || h.Version != 1 || h.Algorithm != cloudArchiveAlgorithmV1 || h.ChunkSize != cloudArchiveChunkSize || len(h.NoncePrefix) != 4 || h.KeyID == "" {
		return nil, h, fmt.Errorf("nativebackup: invalid v1 cloud header")
	}
	var key *CloudBackupKeyRecordV2
	for i := range ring.Keys {
		if legacyKeyID(ring.Keys[i].Key) == h.KeyID {
			if key != nil {
				return nil, h, fmt.Errorf("nativebackup: ambiguous v1 cloud key id")
			}
			key = &ring.Keys[i]
		}
	}
	if key == nil {
		return nil, h, fmt.Errorf("nativebackup: v1 archive cloud key is unavailable")
	}
	block, err := aes.NewCipher(key.Key[:])
	if err != nil {
		return nil, h, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, h, err
	}
	d := &cloudDecryptReaderV1{r: r, gcm: gcm, header: hb, limits: limits, plain: make([]byte, cloudArchiveChunkSize+gcm.Overhead())}
	copy(d.prefix[:], h.NoncePrefix)
	return d, h, nil
}

func (d *cloudDecryptReaderV1) Read(p []byte) (int, error) {
	if d.off < len(d.plain) {
		n := copy(p, d.plain[d.off:])
		d.off += n
		return n, nil
	}
	if d.finished {
		return 0, io.EOF
	}
	if d.index >= d.limits.MaxChunks || d.index == math.MaxUint64 {
		return 0, fmt.Errorf("nativebackup: v1 cloud archive chunk limit exceeded")
	}
	var l uint32
	if err := binary.Read(d.r, binary.BigEndian, &l); err != nil {
		return 0, err
	}
	if l == 0 {
		var extra [1]byte
		n, err := d.r.Read(extra[:])
		if n != 0 || err != io.EOF {
			return 0, fmt.Errorf("nativebackup: trailing v1 cloud archive data")
		}
		d.finished = true
		return 0, io.EOF
	}
	if l < uint32(d.gcm.Overhead()) || l > uint32(cloudArchiveChunkSize+d.gcm.Overhead()) {
		return 0, fmt.Errorf("nativebackup: invalid v1 encrypted chunk length")
	}
	buf := d.plain[:cloudArchiveChunkSize+d.gcm.Overhead()]
	if _, err := io.ReadFull(d.r, buf[:l]); err != nil {
		return 0, err
	}
	var nonce [12]byte
	copy(nonce[:4], d.prefix[:])
	binary.BigEndian.PutUint64(nonce[4:], d.index)
	plain, err := d.gcm.Open(buf[:0], nonce[:], buf[:l], d.header)
	if err != nil {
		return 0, fmt.Errorf("nativebackup: authenticate v1 cloud chunk: %w", err)
	}
	if uint64(len(plain)) > uint64(d.limits.MaxPlainBytes)-d.total {
		return 0, fmt.Errorf("nativebackup: v1 cloud plaintext limit exceeded")
	}
	d.total += uint64(len(plain))
	d.index++
	d.plain = plain
	d.off = 0
	return d.Read(p)
}

func (d *cloudDecryptReaderV1) VerifyComplete() error {
	if !d.finished {
		return fmt.Errorf("nativebackup: v1 cloud archive was not completely read")
	}
	return nil
}

func writeSnapshotTarGzContext(ctx context.Context, snapshotDir string, out io.Writer) (int64, string, error) {
	h := sha256.New()
	cw := &countingWriter{w: io.MultiWriter(out, h)}
	// BestSpeed is the balanced scheduled-backup setting: native roots contain
	// many gigabytes of session databases/logs, and the default DEFLATE level
	// kept the daemon at a full CPU core for hours. Encryption authenticates the
	// archive independently, so changing compression level has no security or
	// compatibility impact (gzip readers auto-detect it).
	gz, gzErr := gzip.NewWriterLevel(cw, gzip.BestSpeed)
	if gzErr != nil {
		return 0, "", gzErr
	}
	tw := tar.NewWriter(gz)
	err := filepath.WalkDir(snapshotDir, func(path string, de fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(snapshotDir, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		name, ok := safeTarName(filepath.ToSlash(rel))
		if !ok {
			return fmt.Errorf("nativebackup: unsafe archive path")
		}
		info, err := de.Info()
		if err != nil {
			return err
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("nativebackup: source links are not allowed")
		}
		if info.IsDir() {
			return tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeDir, Mode: int64(dirPerm), ModTime: info.ModTime()})
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("nativebackup: special source file rejected")
		}
		if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: int64(filePerm), Size: info.Size(), ModTime: info.ModTime()}); err != nil {
			return err
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		_, copyErr := copyWithContext(ctx, tw, in)
		closeErr := in.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	})
	if err == nil {
		err = tw.Close()
	}
	if err == nil {
		err = gz.Close()
	}
	if err != nil {
		return 0, "", err
	}
	return cw.n, hex.EncodeToString(h.Sum(nil)), nil
}

func extractSnapshotTarGzReader(in io.Reader, destDir string, maxPlain int64) error {
	root, err := privatefs.OpenNativeRoot(destDir, privatefs.DirPolicy{Access: privatefs.AccessPrivate, RepairOwned: true, AllowExisting: true})
	if err != nil {
		return err
	}
	defer root.Close()
	gz, err := gzip.NewReader(io.LimitReader(in, maxPlain+1))
	if err != nil {
		return fmt.Errorf("nativebackup: read gzip archive: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	entries := 0
	var total int64
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		entries++
		if entries > cloudArchiveMaxEntries {
			return fmt.Errorf("nativebackup: archive entry limit exceeded")
		}
		name, ok := safeTarName(hdr.Name)
		if !ok {
			return fmt.Errorf("nativebackup: unsafe restore path")
		}
		if len(name) > cloudArchiveMaxPathBytes || strings.Count(name, "/")+1 > cloudArchiveMaxDepth {
			return fmt.Errorf("nativebackup: archive path limit exceeded")
		}
		rel := filepath.FromSlash(name)
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := root.EnsureDir(rel, privatefs.DirPolicy{Access: privatefs.AccessPrivate, AllowExisting: true}); err != nil && !errors.Is(err, os.ErrExist) {
				return err
			}
		case tar.TypeReg:
			if hdr.Size < 0 || hdr.Size > maxPlain-total {
				return fmt.Errorf("nativebackup: archive size limit exceeded")
			}
			parent := filepath.Dir(rel)
			if parent != "." {
				if err := root.EnsureDir(parent, privatefs.DirPolicy{Access: privatefs.AccessPrivate, AllowExisting: true}); err != nil {
					return err
				}
			}
			f, err := root.CreateExclusive(rel, privatefs.FilePolicy{RequirePrivateParent: true, RejectWritableByOthers: true})
			if err != nil {
				return err
			}
			n, copyErr := io.CopyN(f, tr, hdr.Size)
			syncErr := f.Sync()
			closeErr := f.Close()
			if copyErr != nil {
				return copyErr
			}
			if n != hdr.Size {
				return io.ErrUnexpectedEOF
			}
			if syncErr != nil {
				return syncErr
			}
			if closeErr != nil {
				return closeErr
			}
			total += n
		default:
			return fmt.Errorf("nativebackup: unsupported archive entry type")
		}
	}
	return gz.Close()
}

func safeTarName(name string) (string, bool) {
	if len(name) == 0 || len(name) > cloudArchiveMaxPathBytes {
		return "", false
	}
	if err := safepath.ValidateNativeArchiveName(name); err != nil {
		return "", false
	}
	return strings.TrimSuffix(name, "/"), true
}

var _ = sort.Slice
