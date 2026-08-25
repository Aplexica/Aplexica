package acf

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"filippo.io/age"
	"github.com/aplexica/aplexica/internal/audit"
	"github.com/aplexica/aplexica/internal/filelock"
	"github.com/aplexica/aplexica/internal/privatefs"
	"github.com/aplexica/aplexica/internal/securityerr"
)

type RestoreOptions struct {
	Limits            BundleLimits
	UnsignedOK        bool
	TrustedPubKey     ed25519.PublicKey
	ExpectedKeyID     [32]byte
	Signature         []byte
	DecryptIdentities []age.Identity
	Audit             audit.Recorder
}

type BundleTarget struct {
	Store       *Store
	SecretsRoot string
}

type PlannedBundleEntry struct {
	Path   string
	Size   int64
	SHA256 [32]byte
	Dir    bool
}

type ValidatedBundle struct {
	Meta    BundleMeta
	Entries []PlannedBundleEntry

	target       BundleTarget
	opts         RestoreOptions
	workDir      string
	workRoot     *privatefs.Root
	spool        *os.File
	semanticDone bool
	recovered    bool
	committed    bool
	closed       bool
	lock         *filelock.Lock
	artifactN    int
	eventN       int
}

func effectiveBundleLimits(in BundleLimits) (BundleLimits, error) {
	hard := DefaultBundleLimits()
	if in == (BundleLimits{}) {
		return hard, nil
	}
	vals := []struct{ got, max int64 }{
		{in.MaxCompressedBytes, hard.MaxCompressedBytes}, {in.MaxSignatureBytes, hard.MaxSignatureBytes},
		{in.MaxTrustedKeyBytes, hard.MaxTrustedKeyBytes}, {in.MaxIdentityFileBytes, hard.MaxIdentityFileBytes},
		{in.MaxMetaBytes, hard.MaxMetaBytes}, {in.MaxEntryBytes, hard.MaxEntryBytes},
		{in.MaxTotalBytes, hard.MaxTotalBytes}, {in.MaxSecretBytes, hard.MaxSecretBytes}, {in.MaxBlobBytes, hard.MaxBlobBytes},
	}
	for _, v := range vals {
		if v.got <= 0 || v.got > v.max {
			return BundleLimits{}, fmt.Errorf("acf: invalid bundle limit: %w", securityerr.ErrLimitExceeded)
		}
	}
	if in.MaxIdentities <= 0 || in.MaxIdentities > hard.MaxIdentities || in.MaxPathBytes <= 0 || in.MaxPathBytes > hard.MaxPathBytes || in.MaxEntries <= 0 || in.MaxEntries > hard.MaxEntries {
		return BundleLimits{}, fmt.Errorf("acf: invalid bundle count limit: %w", securityerr.ErrLimitExceeded)
	}
	return in, nil
}

func verifyBundleSignature(rawHash [32]byte, opts RestoreOptions) error {
	if len(opts.Signature) == 0 {
		if opts.UnsignedOK {
			return nil
		}
		return securityerr.ErrUnsignedInput
	}
	if int64(len(opts.Signature)) > opts.Limits.MaxSignatureBytes || len(opts.TrustedPubKey) != ed25519.PublicKeySize || opts.ExpectedKeyID == ([32]byte{}) {
		return fmt.Errorf("acf: incomplete bundle trust policy: %w", securityerr.ErrInvalidSignature)
	}
	if sha256.Sum256(opts.TrustedPubKey) != opts.ExpectedKeyID {
		return fmt.Errorf("acf: trusted key ID mismatch: %w", securityerr.ErrInvalidSignature)
	}
	if len(opts.Signature) == 0 || opts.Signature[len(opts.Signature)-1] != '\n' || strings.Count(string(opts.Signature), "\n") != 1 {
		return fmt.Errorf("acf: signature must be exactly one terminated record: %w", securityerr.ErrInvalidSignature)
	}
	line := strings.TrimSuffix(string(opts.Signature), "\n")
	if !strings.HasPrefix(line, sigPrefix) {
		return fmt.Errorf("acf: invalid signature prefix: %w", securityerr.ErrInvalidSignature)
	}
	parts := strings.Split(strings.TrimPrefix(line, sigPrefix), " ")
	if len(parts) != 3 || len(parts[0]) != 64 || len(parts[1]) != 128 || len(parts[2]) != 64 {
		return fmt.Errorf("acf: invalid signature shape: %w", securityerr.ErrInvalidSignature)
	}
	pub, e1 := hex.DecodeString(parts[0])
	sig, e2 := hex.DecodeString(parts[1])
	digest, e3 := hex.DecodeString(parts[2])
	if e1 != nil || e2 != nil || e3 != nil || !ed25519.PublicKey(pub).Equal(opts.TrustedPubKey) || !bytesEqual(digest, rawHash[:]) || !ed25519.Verify(opts.TrustedPubKey, rawHash[:], sig) {
		return fmt.Errorf("acf: bundle signature verification failed: %w", securityerr.ErrInvalidSignature)
	}
	return nil
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	var x byte
	for i := range a {
		x |= a[i] ^ b[i]
	}
	return x == 0
}

func OpenValidatedBundle(r io.Reader, target BundleTarget, opts RestoreOptions) (*ValidatedBundle, error) {
	if r == nil || target.Store == nil || target.Store.Root == "" {
		return nil, fmt.Errorf("acf: bundle target is required")
	}
	limits, err := effectiveBundleLimits(opts.Limits)
	if err != nil {
		return nil, err
	}
	opts.Limits = limits
	if opts.Audit == nil {
		opts.Audit = &audit.FileRecorder{Root: filepath.Join(target.Store.Root, ".audit")}
	}
	work, err := os.MkdirTemp("", "aplexica-bundle-validate-")
	if err != nil {
		return nil, err
	}
	_ = os.Chmod(work, 0o700)
	root, err := privatefs.OpenRoot(work, privatefs.DirPolicy{Access: privatefs.AccessPrivate, RepairOwned: true})
	if err != nil {
		_ = os.RemoveAll(work)
		return nil, err
	}
	b := &ValidatedBundle{target: target, opts: opts, workDir: work, workRoot: root}
	f, err := root.CreateExclusive("bundle.raw", privatefs.FilePolicy{RequirePrivateParent: true, RejectWritableByOthers: true})
	if err != nil {
		b.Close()
		return nil, err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(r, limits.MaxCompressedBytes+1))
	if err == nil && n > limits.MaxCompressedBytes {
		err = securityerr.ErrLimitExceeded
	}
	if err == nil {
		err = f.Sync()
	}
	if err != nil {
		_ = f.Close()
		b.Close()
		return nil, fmt.Errorf("acf: spool bundle: %w", err)
	}
	var rawHash [32]byte
	copy(rawHash[:], h.Sum(nil))
	if err := verifyBundleSignature(rawHash, opts); err != nil {
		_ = f.Close()
		b.Close()
		return nil, err
	}
	if err := privatefs.EnsureDir(target.Store.Root, privatefs.DirPolicy{Access: privatefs.AccessPrivate, RepairOwned: true, AllowExisting: true}); err != nil {
		_ = f.Close()
		b.Close()
		return nil, err
	}
	b.lock, err = filelock.Acquire(filepath.Join(target.Store.Root, ".restore.lock"), 10*time.Second)
	if err != nil {
		_ = f.Close()
		b.Close()
		return nil, fmt.Errorf("acf: acquire restore lock: %w", err)
	}
	if err := f.Close(); err != nil {
		b.Close()
		return nil, err
	}
	b.spool, err = root.OpenReadRegular("bundle.raw")
	if err != nil {
		b.Close()
		return nil, err
	}
	if err := b.parseToStage(); err != nil {
		b.Close()
		return nil, err
	}
	return b, nil
}

func OpenValidatedBundleFile(bundlePath, signaturePath string, target BundleTarget, opts RestoreOptions) (*ValidatedBundle, error) {
	if signaturePath != "" {
		data, err := readBoundedRegular(signaturePath, opts.Limits.MaxSignatureBytes, false)
		if err != nil {
			return nil, err
		}
		opts.Signature = data
	}
	f, err := os.Open(bundlePath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return OpenValidatedBundle(f, target, opts)
}

func readBoundedRegular(name string, max int64, private bool) ([]byte, error) {
	if max <= 0 {
		max = DefaultBundleLimits().MaxSignatureBytes
	}
	parent, base := filepath.Dir(name), filepath.Base(name)
	access := privatefs.AccessIntegrityOnly
	if private {
		access = privatefs.AccessPrivate
	}
	root, err := privatefs.OpenRoot(parent, privatefs.DirPolicy{Access: access})
	if err != nil {
		return nil, err
	}
	defer root.Close()
	var f *os.File
	if private {
		f, err = root.OpenReadRegular(base)
	} else {
		f, err = root.OpenReadRegularIntegrity(base)
	}
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(f, max+1))
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, securityerr.ErrLimitExceeded
	}
	return data, nil
}

func LoadTrustedPublicKey(path string, expected [32]byte, limits BundleLimits) (ed25519.PublicKey, [32]byte, error) {
	if limits == (BundleLimits{}) {
		limits = DefaultBundleLimits()
	}
	data, err := readBoundedRegular(path, limits.MaxTrustedKeyBytes, false)
	if err != nil {
		return nil, [32]byte{}, err
	}
	if len(data) == 0 || data[len(data)-1] != '\n' || strings.Count(string(data), "\n") != 1 {
		return nil, [32]byte{}, fmt.Errorf("acf: public key must be one terminated record")
	}
	line := strings.TrimSuffix(string(data), "\n")
	if !strings.HasPrefix(line, keyPubPrefix) {
		return nil, [32]byte{}, fmt.Errorf("acf: invalid public key record")
	}
	rawHex := strings.TrimPrefix(line, keyPubPrefix)
	if len(rawHex) != ed25519.PublicKeySize*2 {
		return nil, [32]byte{}, fmt.Errorf("acf: invalid public key length")
	}
	raw, err := hex.DecodeString(rawHex)
	if err != nil {
		return nil, [32]byte{}, err
	}
	key := append(ed25519.PublicKey(nil), raw...)
	id := sha256.Sum256(key)
	if expected == ([32]byte{}) || id != expected {
		return nil, id, fmt.Errorf("acf: public key ID is not the pinned key: %w", securityerr.ErrInvalidSignature)
	}
	return key, id, nil
}

func LoadAgeIdentitiesBounded(path string, limits BundleLimits) ([]age.Identity, error) {
	if limits == (BundleLimits{}) {
		limits = DefaultBundleLimits()
	}
	data, err := readBoundedRegular(path, limits.MaxIdentityFileBytes, true)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(string(data), "\n")
	records := 0
	for _, line := range lines {
		if len(line) > 4096 {
			return nil, securityerr.ErrLimitExceeded
		}
		trimmed := strings.TrimSpace(line)
		if trimmed != "" && !strings.HasPrefix(trimmed, "#") {
			records++
		}
	}
	if records == 0 || records > limits.MaxIdentities {
		return nil, securityerr.ErrLimitExceeded
	}
	ids, err := age.ParseIdentities(strings.NewReader(string(data)))
	if err != nil || len(ids) != records || len(ids) > limits.MaxIdentities {
		return nil, fmt.Errorf("acf: parse age identities: %w", err)
	}
	return ids, nil
}

func (b *ValidatedBundle) parseToStage() error {
	if len(b.opts.DecryptIdentities) > b.opts.Limits.MaxIdentities {
		return securityerr.ErrLimitExceeded
	}
	var source io.Reader = b.spool
	if len(b.opts.DecryptIdentities) > 0 {
		decrypted, err := age.Decrypt(b.spool, b.opts.DecryptIdentities...)
		if err != nil {
			return fmt.Errorf("acf: decrypt authenticated bundle: %w", err)
		}
		source = decrypted
	}
	gz, err := gzip.NewReader(source)
	if err != nil {
		return fmt.Errorf("acf: bundle gzip header: %w", err)
	}
	defer gz.Close()
	if err := b.workRoot.EnsureDir("stage", privatefs.DirPolicy{Access: privatefs.AccessPrivate, AllowExisting: true}); err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	validator := newBundlePathValidator(b.opts.Limits)
	metaSeen := false
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("acf: bundle tar: %w", err)
		}
		if err := validator.validateHeader(hdr); err != nil {
			return err
		}
		name := strings.TrimSuffix(hdr.Name, "/")
		if !metaSeen && name != "meta.json" {
			return fmt.Errorf("acf: meta.json must be first: %w", securityerr.ErrUnsafeIdentifier)
		}
		stageRel := filepath.Join("stage", filepath.FromSlash(name))
		if hdr.Typeflag == tar.TypeDir {
			if err := b.workRoot.EnsureDir(stageRel, privatefs.DirPolicy{Access: privatefs.AccessPrivate, AllowExisting: true}); err != nil {
				return err
			}
			b.Entries = append(b.Entries, PlannedBundleEntry{Path: name, Dir: true})
			continue
		}
		parent := filepath.Dir(stageRel)
		if err := b.workRoot.EnsureDir(parent, privatefs.DirPolicy{Access: privatefs.AccessPrivate, AllowExisting: true}); err != nil {
			return err
		}
		n, digest, err := b.workRoot.WriteReader(stageRel, tr, hdr.Size, privatefs.FilePolicy{RequirePrivateParent: true, RejectWritableByOthers: true})
		if err != nil || n != hdr.Size {
			if err == nil {
				err = io.ErrUnexpectedEOF
			}
			return fmt.Errorf("acf: stage bundle entry: %w", err)
		}
		if name == "meta.json" {
			f, err := b.workRoot.OpenReadRegular(stageRel)
			if err != nil {
				return err
			}
			dec := json.NewDecoder(io.LimitReader(f, b.opts.Limits.MaxMetaBytes+1))
			dec.DisallowUnknownFields()
			err = dec.Decode(&b.Meta)
			if err == nil {
				err = requireJSONEOF(dec)
			}
			_ = f.Close()
			if err != nil {
				return fmt.Errorf("acf: invalid bundle metadata: %w", err)
			}
			if !versionCompatible(b.Meta.BundleVersion) {
				return fmt.Errorf("acf: bundle version %q is newer than supported %s", b.Meta.BundleVersion, BundleVersion)
			}
			metaSeen = true
		}
		b.Entries = append(b.Entries, PlannedBundleEntry{Path: name, Size: n, SHA256: digest})
	}
	if !metaSeen {
		return fmt.Errorf("acf: bundle missing meta.json")
	}
	if extra, err := io.Copy(io.Discard, gz); err != nil || extra != 0 {
		if err == nil {
			err = fmt.Errorf("trailing uncompressed bundle data")
		}
		return fmt.Errorf("acf: finish bundle stream: %w", err)
	}
	if err := gz.Close(); err != nil {
		return fmt.Errorf("acf: finish gzip: %w", err)
	}
	if err := b.validateManifestHashes(); err != nil {
		return err
	}
	return nil
}

func requireJSONEOF(dec *json.Decoder) error {
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("trailing JSON value")
		}
		return err
	}
	return nil
}

func (b *ValidatedBundle) validateManifestHashes() error {
	legacy := len(b.Meta.Hashes) == 0 && b.Meta.BundleVersion == "1.0"
	var total int64
	seen := make(map[string]bool)
	for _, e := range b.Entries {
		if e.Dir || e.Path == "meta.json" {
			continue
		}
		total += e.Size
		if legacy {
			continue
		}
		want, ok := b.Meta.Hashes[e.Path]
		if !ok || want != hex.EncodeToString(e.SHA256[:]) {
			return fmt.Errorf("acf: bundle entry digest mismatch: %w", securityerr.ErrInvalidSignature)
		}
		seen[e.Path] = true
	}
	if !legacy {
		if len(seen) != len(b.Meta.Hashes) || total != b.Meta.TotalBytes {
			return fmt.Errorf("acf: bundle manifest totals mismatch: %w", securityerr.ErrMetadataMismatch)
		}
		for name := range b.Meta.Hashes {
			if !seen[name] {
				return fmt.Errorf("acf: bundle manifest names an absent entry: %w", securityerr.ErrMetadataMismatch)
			}
		}
	}
	return nil
}

func (b *ValidatedBundle) stagePath(name string) string {
	return filepath.Join(b.workDir, "stage", filepath.FromSlash(name))
}

func (b *ValidatedBundle) VerifySemantic() error {
	return b.verifySemantic(true)
}

func (b *ValidatedBundle) verifySemantic(checkCollisions bool) error {
	if b == nil || b.closed {
		return fmt.Errorf("acf: validated bundle is closed")
	}
	if !b.recovered {
		if err := recoverRestoreJournals(b.target, b.opts.Audit); err != nil {
			return err
		}
		b.recovered = true
	}
	counts := map[string]int{}
	b.artifactN, b.eventN = 0, 0
	artifacts := map[string]Artifact{}
	for _, e := range b.Entries {
		if e.Dir || e.Path == "meta.json" {
			continue
		}
		if k, id, ok := acfArtifactRef(e.Path); ok {
			f, err := os.Open(b.stagePath(e.Path))
			if err != nil {
				return err
			}
			var a Artifact
			dec := json.NewDecoder(io.LimitReader(f, b.opts.Limits.MaxEntryBytes+1))
			dec.DisallowUnknownFields()
			err = dec.Decode(&a)
			if err == nil {
				err = requireJSONEOF(dec)
			}
			_ = f.Close()
			if err != nil || a.Kind != k || a.ArtifactID != id {
				return fmt.Errorf("acf: invalid artifact entry %s: %w", e.Path, err)
			}
			artifacts[string(k)+"\x00"+id] = a
			counts[string(k)]++
			b.artifactN++
			continue
		}
		if strings.HasPrefix(e.Path, blobsDirName+"/") {
			parts := strings.Split(e.Path, "/")
			if parts[len(parts)-1] != hex.EncodeToString(e.SHA256[:]) {
				return fmt.Errorf("acf: blob digest mismatch: %w", securityerr.ErrMetadataMismatch)
			}
		}
	}
	for kind, want := range b.Meta.ArtifactCounts {
		if counts[kind] != want {
			return fmt.Errorf("acf: artifact count mismatch: %w", securityerr.ErrMetadataMismatch)
		}
	}
	for key, art := range artifacts {
		_ = key
		eventsName := path.Join("events", kindDir(art.Kind), art.ArtifactID+".jsonl")
		_, statErr := os.Stat(b.stagePath(eventsName))
		if statErr != nil && !(errors.Is(statErr, os.ErrNotExist) && art.HeadEventHash == "") {
			return fmt.Errorf("acf: artifact event log missing: %w", statErr)
		}
		if statErr == nil {
			events, err := readEventLogBounded(b.stagePath(eventsName), b.opts.Limits.MaxEntryBytes)
			if err != nil || VerifyChain(events) != nil {
				if err == nil {
					err = VerifyChain(events)
				}
				return fmt.Errorf("acf: invalid event chain: %w", err)
			}
			b.eventN += len(events)
			if len(events) > 0 && art.HeadEventHash != "" {
				last := events[len(events)-1]
				if last.Type != EventTypeBaseline && last.Hash != art.HeadEventHash {
					return fmt.Errorf("acf: artifact head mismatch: %w", securityerr.ErrMetadataMismatch)
				}
			}
		}
		if checkCollisions {
			if _, err := b.target.Store.ReadArtifact(art.Kind, art.ArtifactID); err == nil {
				return fmt.Errorf("acf: artifact %s/%s already exists", art.Kind, art.ArtifactID)
			} else if !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	b.semanticDone = true
	return nil
}

func (b *ValidatedBundle) SemanticStats() (artifacts, events int) {
	if b == nil {
		return 0, 0
	}
	return b.artifactN, b.eventN
}

func readEventLogBounded(filename string, max int64) ([]Event, error) {
	f, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	dec := json.NewDecoder(io.LimitReader(f, max+1))
	dec.DisallowUnknownFields()
	var out []Event
	for {
		var e Event
		err := dec.Decode(&e)
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
}

type restoreJournalEntry struct {
	Root      string   `json:"root"`
	Temp      string   `json:"temp"`
	Final     string   `json:"final"`
	Digest    [32]byte `json:"digest"`
	Installed bool     `json:"installed"`
}

type restoreJournal struct {
	Version            uint16                `json:"version"`
	Committed          bool                  `json:"committed"`
	AuditCommitted     bool                  `json:"auditCommitted"`
	AuditTransactionID string                `json:"auditTransactionId,omitempty"`
	Entries            []restoreJournalEntry `json:"entries"`
}

func journalBytes(j restoreJournal) ([]byte, error) {
	return json.Marshal(j)
}

func recoverRestoreJournals(target BundleTarget, recorder audit.Recorder) error {
	if err := privatefs.EnsureDir(target.Store.Root, privatefs.DirPolicy{Access: privatefs.AccessPrivate, RepairOwned: true, AllowExisting: true}); err != nil {
		return err
	}
	storeRoot, err := privatefs.OpenRoot(target.Store.Root, privatefs.DirPolicy{Access: privatefs.AccessPrivate, RepairOwned: true})
	if err != nil {
		return err
	}
	defer storeRoot.Close()
	entries, err := storeRoot.ReadDir(".")
	if err != nil {
		return err
	}
	allowedRoots := map[string]bool{filepath.Clean(target.Store.Root): true}
	if target.SecretsRoot != "" {
		allowedRoots[filepath.Clean(target.SecretsRoot)] = true
	}
	for _, de := range entries {
		if de.IsDir() || !strings.HasPrefix(de.Name(), ".restore-journal-") || !strings.HasSuffix(de.Name(), ".json") {
			continue
		}
		f, err := storeRoot.OpenReadRegular(de.Name())
		if err != nil {
			return err
		}
		dec := json.NewDecoder(io.LimitReader(f, 4<<20))
		dec.DisallowUnknownFields()
		var j restoreJournal
		err = dec.Decode(&j)
		if err == nil {
			err = requireJSONEOF(dec)
		}
		_ = f.Close()
		if err != nil || j.Version != 1 || len(j.Entries) > DefaultBundleLimits().MaxEntries {
			return fmt.Errorf("acf: invalid restore recovery journal")
		}
		roots := map[string]*privatefs.Root{filepath.Clean(target.Store.Root): storeRoot}
		for i := len(j.Entries) - 1; i >= 0; i-- {
			e := j.Entries[i]
			cleanRoot := filepath.Clean(e.Root)
			if !allowedRoots[cleanRoot] {
				return fmt.Errorf("acf: restore journal root mismatch: %w", securityerr.ErrPathEscape)
			}
			r := roots[cleanRoot]
			if r == nil {
				if err := privatefs.EnsureDir(cleanRoot, privatefs.DirPolicy{Access: privatefs.AccessPrivate, RepairOwned: true, AllowExisting: true}); err != nil {
					return err
				}
				r, err = privatefs.OpenRoot(cleanRoot, privatefs.DirPolicy{Access: privatefs.AccessPrivate, RepairOwned: true})
				if err != nil {
					return err
				}
				roots[cleanRoot] = r
			}
			if !j.Committed && e.Installed {
				if err := r.RemoveRegular(e.Final); err != nil && !errors.Is(err, os.ErrNotExist) {
					return err
				}
			}
			if err := r.RemoveRegular(e.Temp); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		for name, r := range roots {
			if name != filepath.Clean(target.Store.Root) {
				_ = r.Close()
			}
		}
		if j.AuditTransactionID != "" && !j.AuditCommitted {
			outcome := "rolled-back"
			if j.Committed {
				outcome = "success"
			}
			if recorder == nil {
				return fmt.Errorf("acf: restore audit recorder unavailable")
			}
			if err := recorder.CompleteTransaction(context.Background(), j.AuditTransactionID, outcome); err != nil {
				return err
			}
		}
		if err := storeRoot.RemoveRegular(de.Name()); err != nil {
			return err
		}
	}
	return nil
}

type preparedRestore struct {
	entry restoreJournalEntry
	root  *privatefs.Root
}

func (b *ValidatedBundle) Commit() error {
	if b == nil || b.closed || !b.semanticDone {
		return fmt.Errorf("acf: bundle must pass semantic verification before commit")
	}
	if b.committed {
		return nil
	}
	if err := b.target.Store.Init(); err != nil {
		return err
	}
	storeRoot, err := privatefs.OpenRoot(b.target.Store.Root, privatefs.DirPolicy{Access: privatefs.AccessPrivate, RepairOwned: true})
	if err != nil {
		return err
	}
	defer storeRoot.Close()
	roots := map[string]*privatefs.Root{filepath.Clean(b.target.Store.Root): storeRoot}
	if b.target.SecretsRoot != "" {
		if err := privatefs.EnsureDir(b.target.SecretsRoot, privatefs.DirPolicy{Access: privatefs.AccessPrivate, RepairOwned: true, AllowExisting: true}); err != nil {
			return err
		}
		r, err := privatefs.OpenRoot(b.target.SecretsRoot, privatefs.DirPolicy{Access: privatefs.AccessPrivate, RepairOwned: true})
		if err != nil {
			return err
		}
		defer r.Close()
		roots[filepath.Clean(b.target.SecretsRoot)] = r
	}
	files := make([]PlannedBundleEntry, 0, len(b.Entries))
	for _, e := range b.Entries {
		if !e.Dir && e.Path != "meta.json" {
			files = append(files, e)
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	prepared := make([]preparedRestore, 0, len(files))
	cleanupPrepared := func() {
		for _, p := range prepared {
			_ = p.root.RemoveRegular(p.entry.Temp)
		}
	}
	for _, e := range files {
		rootPath := filepath.Clean(b.target.Store.Root)
		final := filepath.FromSlash(e.Path)
		if strings.HasPrefix(e.Path, "secrets/") && b.target.SecretsRoot != "" {
			rootPath = filepath.Clean(b.target.SecretsRoot)
			final = filepath.FromSlash(strings.TrimPrefix(e.Path, "secrets/"))
		}
		r := roots[rootPath]
		parent := filepath.Dir(final)
		if err := r.EnsureDir(parent, privatefs.DirPolicy{Access: privatefs.AccessPrivate, AllowExisting: true}); err != nil {
			cleanupPrepared()
			return err
		}
		// Existing content-addressed blobs are safe only when byte-identical.
		if strings.HasPrefix(e.Path, blobsDirName+"/") {
			if existing, err := r.OpenReadRegular(final); err == nil {
				h := sha256.New()
				_, copyErr := io.Copy(h, existing)
				_ = existing.Close()
				var got [32]byte
				copy(got[:], h.Sum(nil))
				if copyErr != nil || got != e.SHA256 {
					cleanupPrepared()
					return fmt.Errorf("acf: existing blob mismatch")
				}
				continue
			} else if !errors.Is(err, os.ErrNotExist) {
				cleanupPrepared()
				return err
			}
		}
		tmp, tempRel, err := r.CreateTemp(parent, ".restore-prepared-")
		if err != nil {
			cleanupPrepared()
			return err
		}
		src, err := os.Open(b.stagePath(e.Path))
		if err != nil {
			_ = tmp.Close()
			_ = r.RemoveRegular(tempRel)
			cleanupPrepared()
			return err
		}
		h := sha256.New()
		n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(src, e.Size+1))
		_ = src.Close()
		if err == nil && n != e.Size {
			err = io.ErrUnexpectedEOF
		}
		if err == nil {
			err = tmp.Sync()
		}
		if closeErr := tmp.Close(); err == nil {
			err = closeErr
		}
		var got [32]byte
		copy(got[:], h.Sum(nil))
		if err != nil || got != e.SHA256 {
			_ = r.RemoveRegular(tempRel)
			cleanupPrepared()
			return fmt.Errorf("acf: destination preparation mismatch: %w", err)
		}
		prepared = append(prepared, preparedRestore{restoreJournalEntry{Root: rootPath, Temp: tempRel, Final: final, Digest: e.SHA256}, r})
	}
	txnID := NewID()
	artifactsField := audit.Count("artifact_count", uint64(b.artifactN))
	eventsField := audit.Count("event_count", uint64(b.eventN))
	if b.opts.Audit == nil {
		cleanupPrepared()
		return fmt.Errorf("acf: restore audit recorder unavailable")
	}
	if err := b.opts.Audit.BeginTransaction(context.Background(), txnID, audit.Event{Code: "bundle.restore_completed", Fields: []audit.Field{artifactsField, eventsField}}); err != nil {
		cleanupPrepared()
		return fmt.Errorf("acf: begin restore audit: %w", err)
	}
	journal := restoreJournal{Version: 1, AuditTransactionID: txnID, Entries: make([]restoreJournalEntry, len(prepared))}
	for i := range prepared {
		journal.Entries[i] = prepared[i].entry
	}
	journalName := ".restore-journal-" + randomBundleID() + ".json"
	writeJournal := func() error {
		data, err := journalBytes(journal)
		if err != nil {
			return err
		}
		return storeRoot.WriteFile(journalName, data, privatefs.FilePolicy{RequirePrivateParent: true, RejectWritableByOthers: true})
	}
	if err := writeJournal(); err != nil {
		cleanupPrepared()
		_ = b.opts.Audit.CompleteTransaction(context.Background(), txnID, "rolled-back")
		return err
	}
	rollback := func() {
		for i := len(prepared) - 1; i >= 0; i-- {
			if journal.Entries[i].Installed {
				_ = prepared[i].root.RemoveRegular(journal.Entries[i].Final)
			}
			_ = prepared[i].root.RemoveRegular(journal.Entries[i].Temp)
		}
	}
	for i := range prepared {
		if err := prepared[i].root.InstallNoReplace(prepared[i].entry.Temp, prepared[i].entry.Final); err != nil {
			rollback()
			_ = b.opts.Audit.CompleteTransaction(context.Background(), txnID, "rolled-back")
			_ = storeRoot.RemoveRegular(journalName)
			return err
		}
		journal.Entries[i].Installed = true
		if err := writeJournal(); err != nil {
			rollback()
			return err
		}
	}
	journal.Committed = true
	if err := writeJournal(); err != nil {
		return err
	}
	if err := b.opts.Audit.CompleteTransaction(context.Background(), txnID, "success"); err != nil {
		return fmt.Errorf("acf: complete restore audit: %w", err)
	}
	journal.AuditCommitted = true
	if err := writeJournal(); err != nil {
		return err
	}
	b.committed = true
	return storeRoot.RemoveRegular(journalName)
}

func randomBundleID() string {
	var id [16]byte
	if _, err := io.ReadFull(rand.Reader, id[:]); err != nil {
		panic("crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(id[:])
}

func (b *ValidatedBundle) Close() error {
	if b == nil || b.closed {
		return nil
	}
	b.closed = true
	var err error
	if b.spool != nil {
		err = b.spool.Close()
	}
	if b.workRoot != nil {
		if e := b.workRoot.Close(); err == nil {
			err = e
		}
	}
	if b.lock != nil {
		if e := b.lock.Close(); err == nil {
			err = e
		}
		b.lock = nil
	}
	if e := os.RemoveAll(b.workDir); err == nil {
		err = e
	}
	return err
}

func (s *Store) RestoreWithOptions(r io.Reader, secretsRoot string, opts RestoreOptions) error {
	b, err := OpenValidatedBundle(r, BundleTarget{Store: s, SecretsRoot: secretsRoot}, opts)
	if err != nil {
		return err
	}
	defer b.Close()
	if err := b.VerifySemantic(); err != nil {
		return err
	}
	return b.Commit()
}

// Restore intentionally has no implicit unsigned compatibility mode. Callers
// must select an authenticity policy through RestoreWithOptions.
func (s *Store) Restore(_ io.Reader, _ string) error { return securityerr.ErrUnsignedInput }

// RestoreDryRun likewise requires an explicit policy so inspection cannot
// accidentally parse attacker-controlled input before trust is decided.
func (s *Store) RestoreDryRun(_ io.Reader) (DryRunResult, error) {
	return DryRunResult{}, securityerr.ErrUnsignedInput
}

func (s *Store) RestoreDryRunWithOptions(r io.Reader, secretsRoot string, opts RestoreOptions) (DryRunResult, error) {
	result := DryRunResult{ByKind: map[Kind]*KindDryRun{}}
	b, err := OpenValidatedBundle(r, BundleTarget{Store: s, SecretsRoot: secretsRoot}, opts)
	if err != nil {
		return result, err
	}
	defer b.Close()
	return b.DryRun()
}

func (b *ValidatedBundle) DryRun() (DryRunResult, error) {
	result := DryRunResult{ByKind: map[Kind]*KindDryRun{}}
	if err := b.verifySemantic(false); err != nil {
		return result, err
	}
	for _, e := range b.Entries {
		k, id, ok := acfArtifactRef(e.Path)
		if !ok {
			continue
		}
		kd := result.ByKind[k]
		if kd == nil {
			kd = &KindDryRun{}
			result.ByKind[k] = kd
		}
		if _, err := b.target.Store.ReadArtifact(k, id); err == nil {
			kd.CollisionIDs = append(kd.CollisionIDs, id)
		} else if errors.Is(err, os.ErrNotExist) {
			kd.Adds++
		} else {
			return result, err
		}
	}
	for _, kd := range result.ByKind {
		sort.Strings(kd.CollisionIDs)
	}
	return result, nil
}

func PeekBundleMetaWithLimits(r io.Reader, limits BundleLimits) (BundleMeta, error) {
	targetDir, err := os.MkdirTemp("", "aplexica-peek-target-")
	if err != nil {
		return BundleMeta{}, err
	}
	defer os.RemoveAll(targetDir)
	target := &Store{Root: filepath.Join(targetDir, "store")}
	b, err := OpenValidatedBundle(r, BundleTarget{Store: target}, RestoreOptions{Limits: limits, UnsignedOK: true})
	if err != nil {
		return BundleMeta{}, err
	}
	defer b.Close()
	return b.Meta, nil
}
