package keyrotation

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/filelock"
	"github.com/aplexica/aplexica/internal/identity"
	"github.com/aplexica/aplexica/internal/privatefs"
	"github.com/aplexica/aplexica/internal/securityerr"
	"github.com/fxamacker/cbor/v2"
)

type namespaceKeyRecordV1 struct {
	Version      uint16                       `cbor:"version"`
	Snapshot     NamespaceKeySnapshot         `cbor:"snapshot"`
	Statement    SignedRotationStatementV1    `cbor:"statement"`
	Manifest     SignedNamespaceKeyManifestV1 `cbor:"manifest"`
	RecordDigest [32]byte                     `cbor:"recordDigest"`
}

type NamespaceKeyStore struct {
	Root string
}

func (s *NamespaceKeyStore) namespaceRoot(namespaceID string) (*privatefs.Root, error) {
	if err := acf.ValidateWireUUIDv7(namespaceID); err != nil || s == nil || s.Root == "" {
		return nil, securityerr.ErrUnsafeIdentifier
	}
	path := filepath.Join(s.Root, namespaceID)
	if err := privatefs.EnsureDir(path, privatefs.DirPolicy{Access: privatefs.AccessPrivate, RepairOwned: true, AllowExisting: true}); err != nil {
		return nil, err
	}
	return privatefs.OpenRoot(path, privatefs.DirPolicy{Access: privatefs.AccessPrivate, RepairOwned: true})
}

func recordDigest(record namespaceKeyRecordV1) ([32]byte, error) {
	record.RecordDigest = [32]byte{}
	b, err := rotationEnc.Marshal([]any{"aplexica/namespace-key-record/v1", record})
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(b), nil
}

func versionName(version uint64) string { return fmt.Sprintf("version-%020d.cbor", version) }

func (s *NamespaceKeyStore) InstallVerified(ctx context.Context, previous, next identity.VerifiedRoster, statement SignedRotationStatementV1, manifest SignedNamespaceKeyManifestV1, recipientType, recipientID string, recipientPrivate [32]byte) (NamespaceKeySnapshot, error) {
	return s.InstallVerifiedAt(ctx, previous, next, statement, manifest, recipientType, recipientID, recipientPrivate, time.Now())
}

// InstallVerifiedAt is used only by a locally journaled cutover to replay the
// exact already-authorized package after a crash. authorizedAt must be the
// durable local authorization instant covered by the signed statement.
func (s *NamespaceKeyStore) InstallVerifiedAt(ctx context.Context, previous, next identity.VerifiedRoster, statement SignedRotationStatementV1, manifest SignedNamespaceKeyManifestV1, recipientType, recipientID string, recipientPrivate [32]byte, authorizedAt time.Time) (NamespaceKeySnapshot, error) {
	if authorizedAt.IsZero() || authorizedAt.Unix() < statement.Statement.IssuedAtUnix || authorizedAt.Unix() > statement.Statement.ExpiresAtUnix {
		return NamespaceKeySnapshot{}, fmt.Errorf("keyrotation: invalid durable authorization instant")
	}
	if err := VerifyRotationStatement(previous, next, statement, authorizedAt); err != nil {
		return NamespaceKeySnapshot{}, err
	}
	if err := VerifyNamespaceKeyManifest(next, statement, manifest); err != nil {
		return NamespaceKeySnapshot{}, err
	}
	m := manifest.Manifest
	var entry *WrappedKeyEntryV2
	for i := range m.Wrapped {
		if m.Wrapped[i].RecipientType == recipientType && m.Wrapped[i].RecipientID == recipientID {
			entry = &m.Wrapped[i]
			break
		}
	}
	if entry == nil {
		return NamespaceKeySnapshot{}, fmt.Errorf("keyrotation: no exact recipient wrap")
	}
	wrapContext := WrapContextV2{NamespaceID: m.NamespaceID, KeyVersion: m.KeyVersion, StatementHash: m.StatementHash, RecipientType: entry.RecipientType, RecipientID: entry.RecipientID, RecipientWrapKeyID: entry.RecipientWrapKeyID, AccessGeneration: m.AccessGeneration, AccessSetHash: m.AccessSetHash}
	key, err := UnwrapKeyV2(entry.Wrapped, recipientPrivate, wrapContext)
	if err != nil {
		return NamespaceKeySnapshot{}, err
	}
	manifestHash, err := ManifestHash(manifest)
	if err != nil {
		return NamespaceKeySnapshot{}, err
	}
	snapshot := NamespaceKeySnapshot{NamespaceID: m.NamespaceID, Version: m.KeyVersion, Key: key, AccessGeneration: m.AccessGeneration, AccessSetHash: m.AccessSetHash, IssuedRosterEpoch: m.IssuedRosterEpoch, IssuedRosterHash: m.IssuedRosterHash, ManifestHash: manifestHash, Finalized: true}
	root, err := s.namespaceRoot(m.NamespaceID)
	if err != nil {
		return NamespaceKeySnapshot{}, err
	}
	defer root.Close()
	lock, err := filelock.Acquire(filepath.Join(s.Root, m.NamespaceID, ".namespace-key.lock"), 30*time.Second)
	if err != nil {
		return NamespaceKeySnapshot{}, err
	}
	defer lock.Close()
	if current, err := s.readRecord(root, "current.cbor"); err == nil {
		if current.Snapshot.Version > snapshot.Version || current.Snapshot.Version == snapshot.Version && current.Snapshot.ManifestHash != manifestHash {
			return NamespaceKeySnapshot{}, fmt.Errorf("keyrotation: namespace key rollback or equivocation")
		}
		if current.Snapshot.Version == snapshot.Version {
			return current.Snapshot, nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return NamespaceKeySnapshot{}, err
	}
	if existing, err := s.readRecord(root, versionName(m.KeyVersion)); err == nil {
		if existing.Snapshot.ManifestHash == manifestHash {
			encoded, encodeErr := rotationEnc.Marshal(existing)
			if encodeErr != nil {
				return NamespaceKeySnapshot{}, encodeErr
			}
			if err := root.WriteFile("current.cbor", encoded, privatefs.FilePolicy{RejectWritableByOthers: true}); err != nil {
				return NamespaceKeySnapshot{}, err
			}
			return existing.Snapshot, nil
		}
		return NamespaceKeySnapshot{}, fmt.Errorf("keyrotation: same-version manifest equivocation")
	} else if !errors.Is(err, os.ErrNotExist) {
		return NamespaceKeySnapshot{}, err
	}
	record := namespaceKeyRecordV1{Version: 1, Snapshot: snapshot, Statement: statement, Manifest: manifest}
	record.RecordDigest, err = recordDigest(record)
	if err != nil {
		return NamespaceKeySnapshot{}, err
	}
	b, err := rotationEnc.Marshal(record)
	if err != nil {
		return NamespaceKeySnapshot{}, err
	}
	if err := root.WriteFile(versionName(snapshot.Version), b, privatefs.FilePolicy{RejectWritableByOthers: true}); err != nil {
		return NamespaceKeySnapshot{}, err
	}
	if err := root.WriteFile("current.cbor", b, privatefs.FilePolicy{RejectWritableByOthers: true}); err != nil {
		return NamespaceKeySnapshot{}, err
	}
	if err := root.SyncDir("."); err != nil {
		return NamespaceKeySnapshot{}, err
	}
	_ = ctx
	return snapshot, nil
}

func (s *NamespaceKeyStore) readRecord(root *privatefs.Root, name string) (namespaceKeyRecordV1, error) {
	f, err := root.OpenReadRegular(name)
	if err != nil {
		return namespaceKeyRecordV1{}, err
	}
	b, err := io.ReadAll(io.LimitReader(f, 8<<20+1))
	closeErr := f.Close()
	if err != nil || closeErr != nil || len(b) > 8<<20 {
		return namespaceKeyRecordV1{}, securityerr.ErrLimitExceeded
	}
	var record namespaceKeyRecordV1
	dec, _ := cbor.DecOptions{DupMapKey: cbor.DupMapKeyEnforcedAPF, IndefLength: cbor.IndefLengthForbidden, TagsMd: cbor.TagsForbidden, MaxNestedLevels: 32, MaxArrayElements: 1024, MaxMapPairs: 1024, ExtraReturnErrors: cbor.ExtraDecErrorUnknownField}.DecMode()
	if err := dec.Unmarshal(b, &record); err != nil {
		return namespaceKeyRecordV1{}, err
	}
	want, err := recordDigest(record)
	if err != nil || record.Version != 1 || want != record.RecordDigest || !record.Snapshot.Finalized || record.Snapshot.Key == ([32]byte{}) {
		return namespaceKeyRecordV1{}, securityerr.ErrMetadataMismatch
	}
	return record, nil
}

func (s *NamespaceKeyStore) Current(_ context.Context, namespaceID string) (NamespaceKeySnapshot, error) {
	root, err := s.namespaceRoot(namespaceID)
	if err != nil {
		return NamespaceKeySnapshot{}, err
	}
	defer root.Close()
	record, err := s.readRecord(root, "current.cbor")
	return record.Snapshot, err
}

func (s *NamespaceKeyStore) ByVersion(_ context.Context, namespaceID string, version uint64) (NamespaceKeySnapshot, error) {
	if version == 0 || version > MaxNamespaceKeyVersion {
		return NamespaceKeySnapshot{}, securityerr.ErrMetadataMismatch
	}
	root, err := s.namespaceRoot(namespaceID)
	if err != nil {
		return NamespaceKeySnapshot{}, err
	}
	defer root.Close()
	record, err := s.readRecord(root, versionName(version))
	return record.Snapshot, err
}

var _ NamespaceKeyProvider = (*NamespaceKeyStore)(nil)
