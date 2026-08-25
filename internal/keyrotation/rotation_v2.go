package keyrotation

import (
	"context"
	"crypto/ed25519"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"io"
	"math"
	"sort"
	"time"

	"github.com/aplexica/aplexica/internal/identity"
	"github.com/fxamacker/cbor/v2"
	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/curve25519"
)

const MaxNamespaceKeyVersion = uint64(math.MaxInt64)

type NamespaceKeySnapshot struct {
	NamespaceID       string
	Version           uint64
	Key               [32]byte
	AccessGeneration  uint64
	AccessSetHash     [32]byte
	IssuedRosterEpoch uint64
	IssuedRosterHash  [32]byte
	ManifestHash      [32]byte
	Finalized         bool
}

type NamespaceKeyProvider interface {
	Current(ctx context.Context, namespaceID string) (NamespaceKeySnapshot, error)
	ByVersion(ctx context.Context, namespaceID string, version uint64) (NamespaceKeySnapshot, error)
}

type WrapContextV2 struct {
	NamespaceID        string   `cbor:"namespaceId"`
	KeyVersion         uint64   `cbor:"keyVersion"`
	StatementHash      [32]byte `cbor:"statementHash"`
	RecipientType      string   `cbor:"recipientType"`
	RecipientID        string   `cbor:"recipientId"`
	RecipientWrapKeyID [32]byte `cbor:"recipientWrapKeyId"`
	AccessGeneration   uint64   `cbor:"accessGeneration"`
	AccessSetHash      [32]byte `cbor:"accessSetHash"`
}

type NamespaceWrappedKeyV2 struct {
	EphemeralPublic [32]byte `cbor:"ephemeralPublic"`
	Nonce           [24]byte `cbor:"nonce"`
	Ciphertext      []byte   `cbor:"ciphertext"`
}

type WrappedKeyEntryV2 struct {
	RecipientType      string                `cbor:"recipientType"`
	RecipientID        string                `cbor:"recipientId"`
	RecipientWrapKeyID [32]byte              `cbor:"recipientWrapKeyId"`
	Wrapped            NamespaceWrappedKeyV2 `cbor:"wrapped"`
}

type RotationStatementUnsignedV1 struct {
	Version                  uint16   `cbor:"version"`
	NamespaceID              string   `cbor:"namespaceId"`
	PreviousVersion          uint64   `cbor:"previousVersion"`
	NewVersion               uint64   `cbor:"newVersion"`
	PreviousRosterHash       [32]byte `cbor:"previousRosterHash"`
	NewRosterEpoch           uint64   `cbor:"newRosterEpoch"`
	NewRosterHash            [32]byte `cbor:"newRosterHash"`
	PreviousAccessGeneration uint64   `cbor:"previousAccessGeneration"`
	PreviousAccessSetHash    [32]byte `cbor:"previousAccessSetHash"`
	NewAccessGeneration      uint64   `cbor:"newAccessGeneration"`
	NewAccessSetHash         [32]byte `cbor:"newAccessSetHash"`
	RosterTransitionHash     [32]byte `cbor:"rosterTransitionHash"`
	AuthorityStateHash       [32]byte `cbor:"authorityStateHash"`
	AuthorityEpoch           uint64   `cbor:"authorityEpoch"`
	RemovedDeviceIDs         []string `cbor:"removedDeviceIds"`
	AddedDeviceIDs           []string `cbor:"addedDeviceIds"`
	ChangedDeviceIDs         []string `cbor:"changedDeviceIds"`
	IssuedAtUnix             int64    `cbor:"issuedAtUnix"`
	ExpiresAtUnix            int64    `cbor:"expiresAtUnix"`
	Nonce                    [32]byte `cbor:"nonce"`
}

type SignedRotationStatementV1 struct {
	Statement    RotationStatementUnsignedV1 `cbor:"statement"`
	SignerKeyIDs [][32]byte                  `cbor:"signerKeyIds"`
	Signatures   [][64]byte                  `cbor:"signatures"`
}

type RosterTransitionBindingV1 struct {
	NamespaceID        string   `cbor:"namespaceId"`
	PreviousEpoch      uint64   `cbor:"previousEpoch"`
	PreviousRosterHash [32]byte `cbor:"previousRosterHash"`
	NewEpoch           uint64   `cbor:"newEpoch"`
	NewRosterHash      [32]byte `cbor:"newRosterHash"`
	AuthorityStateHash [32]byte `cbor:"authorityStateHash"`
}

type NamespaceKeyManifestUnsignedV1 struct {
	Version            uint16              `cbor:"version"`
	StatementHash      [32]byte            `cbor:"statementHash"`
	NamespaceID        string              `cbor:"namespaceId"`
	KeyVersion         uint64              `cbor:"keyVersion"`
	AccessGeneration   uint64              `cbor:"accessGeneration"`
	AccessSetHash      [32]byte            `cbor:"accessSetHash"`
	IssuedRosterEpoch  uint64              `cbor:"issuedRosterEpoch"`
	IssuedRosterHash   [32]byte            `cbor:"issuedRosterHash"`
	AuthorityStateHash [32]byte            `cbor:"authorityStateHash"`
	LeaderDeviceID     string              `cbor:"leaderDeviceId"`
	LeaderSigningKeyID [32]byte            `cbor:"leaderSigningKeyId"`
	Wrapped            []WrappedKeyEntryV2 `cbor:"wrapped"`
}

type SignedNamespaceKeyManifestV1 struct {
	Manifest  NamespaceKeyManifestUnsignedV1 `cbor:"manifest"`
	Signature [64]byte                       `cbor:"signature"`
}

var rotationEnc cbor.EncMode

func init() {
	var err error
	rotationEnc, err = cbor.CanonicalEncOptions().EncMode()
	if err != nil {
		panic(err)
	}
}

func rotationCanonical(domain string, value any) ([]byte, error) {
	return rotationEnc.Marshal([]any{domain, value})
}
func rotationDigest(domain string, values ...any) ([32]byte, error) {
	all := append([]any{domain}, values...)
	b, err := rotationEnc.Marshal(all)
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(b), nil
}

func WrapKeyV2(key [32]byte, recipientPub [32]byte, ctx WrapContextV2) (NamespaceWrappedKeyV2, error) {
	return wrapKeyV2WithReader(rand.Reader, key, recipientPub, ctx)
}

func wrapKeyV2WithReader(random io.Reader, key [32]byte, recipientPub [32]byte, ctx WrapContextV2) (NamespaceWrappedKeyV2, error) {
	if ctx.NamespaceID == "" || ctx.KeyVersion == 0 || ctx.StatementHash == ([32]byte{}) || ctx.AccessGeneration == 0 || ctx.AccessSetHash == ([32]byte{}) || ctx.RecipientWrapKeyID != sha256.Sum256(recipientPub[:]) || (ctx.RecipientType != "device" && ctx.RecipientType != "recovery") || ctx.RecipientID == "" || (ctx.RecipientType == "recovery" && ctx.RecipientID != "account-recovery") {
		return NamespaceWrappedKeyV2{}, fmt.Errorf("keyrotation: invalid wrap context")
	}
	if random == nil {
		return NamespaceWrappedKeyV2{}, fmt.Errorf("keyrotation: random source required")
	}
	aad, err := rotationCanonical("aplexica/namespace-key-wrap-context/v2", ctx)
	if err != nil {
		return NamespaceWrappedKeyV2{}, err
	}
	var ephemeralPrivate [32]byte
	if _, err := io.ReadFull(random, ephemeralPrivate[:]); err != nil {
		return NamespaceWrappedKeyV2{}, err
	}
	ephemeralPublic, err := curve25519.X25519(ephemeralPrivate[:], curve25519.Basepoint)
	if err != nil {
		return NamespaceWrappedKeyV2{}, err
	}
	shared, err := curve25519.X25519(ephemeralPrivate[:], recipientPub[:])
	if err != nil || allZero(shared) {
		return NamespaceWrappedKeyV2{}, fmt.Errorf("keyrotation: invalid recipient public key")
	}
	saltInput := append([]byte("aplexica/namespace-key-wrap-salt/v2"), ephemeralPublic...)
	saltInput = append(saltInput, recipientPub[:]...)
	salt := sha256.Sum256(saltInput)
	contextHash := sha256.Sum256(aad)
	info := append([]byte("aplexica/namespace-key-wrap/v2"), contextHash[:]...)
	derived, err := hkdf.Key(sha256.New, shared, salt[:], string(info), 32)
	if err != nil {
		return NamespaceWrappedKeyV2{}, err
	}
	aead, err := chacha20poly1305.NewX(derived)
	if err != nil {
		return NamespaceWrappedKeyV2{}, err
	}
	var out NamespaceWrappedKeyV2
	copy(out.EphemeralPublic[:], ephemeralPublic)
	if _, err := io.ReadFull(random, out.Nonce[:]); err != nil {
		return NamespaceWrappedKeyV2{}, err
	}
	out.Ciphertext = aead.Seal(nil, out.Nonce[:], key[:], aad)
	return out, nil
}

func UnwrapKeyV2(blob NamespaceWrappedKeyV2, private [32]byte, ctx WrapContextV2) ([32]byte, error) {
	if len(blob.Ciphertext) != 48 {
		return [32]byte{}, fmt.Errorf("keyrotation: wrap authentication failed")
	}
	public, err := curve25519.X25519(private[:], curve25519.Basepoint)
	if err != nil || ctx.RecipientWrapKeyID != sha256.Sum256(public) {
		return [32]byte{}, fmt.Errorf("keyrotation: wrap authentication failed")
	}
	aad, err := rotationCanonical("aplexica/namespace-key-wrap-context/v2", ctx)
	if err != nil {
		return [32]byte{}, fmt.Errorf("keyrotation: wrap authentication failed")
	}
	shared, err := curve25519.X25519(private[:], blob.EphemeralPublic[:])
	if err != nil || allZero(shared) {
		return [32]byte{}, fmt.Errorf("keyrotation: wrap authentication failed")
	}
	saltInput := append([]byte("aplexica/namespace-key-wrap-salt/v2"), blob.EphemeralPublic[:]...)
	saltInput = append(saltInput, public...)
	salt := sha256.Sum256(saltInput)
	contextHash := sha256.Sum256(aad)
	info := append([]byte("aplexica/namespace-key-wrap/v2"), contextHash[:]...)
	derived, err := hkdf.Key(sha256.New, shared, salt[:], string(info), 32)
	if err != nil {
		return [32]byte{}, fmt.Errorf("keyrotation: wrap authentication failed")
	}
	aead, err := chacha20poly1305.NewX(derived)
	if err != nil {
		return [32]byte{}, fmt.Errorf("keyrotation: wrap authentication failed")
	}
	plaintext, err := aead.Open(nil, blob.Nonce[:], blob.Ciphertext, aad)
	if err != nil || len(plaintext) != 32 {
		return [32]byte{}, fmt.Errorf("keyrotation: wrap authentication failed")
	}
	var key [32]byte
	copy(key[:], plaintext)
	return key, nil
}

func allZero(value []byte) bool {
	var result byte
	for _, b := range value {
		result |= b
	}
	return result == 0
}

func RosterTransitionHash(previous, next identity.VerifiedRoster) ([32]byte, error) {
	p, n := previous.Manifest.Manifest, next.Manifest.Manifest
	if p.ScopeID != n.ScopeID || n.Epoch != p.Epoch+1 {
		return [32]byte{}, fmt.Errorf("keyrotation: invalid roster transition")
	}
	binding := RosterTransitionBindingV1{NamespaceID: n.ScopeID, PreviousEpoch: p.Epoch, PreviousRosterHash: [32]byte(previous.Hash), NewEpoch: n.Epoch, NewRosterHash: [32]byte(next.Hash), AuthorityStateHash: next.Authority.StateHash}
	return rotationDigest("aplexica/roster-transition-binding/v1", binding)
}

func StatementHash(statement SignedRotationStatementV1) ([32]byte, error) {
	return rotationDigest("aplexica/namespace-rotation-statement-hash/v1", statement)
}

func accessDiff(previous, next identity.VerifiedRoster) (removed, added, changed []string) {
	old := map[string]identity.DeviceCertificateUnsignedV1{}
	newSet := map[string]identity.DeviceCertificateUnsignedV1{}
	for _, cert := range previous.Manifest.Manifest.Devices {
		old[cert.Certificate.DeviceID] = cert.Certificate
	}
	for _, cert := range next.Manifest.Manifest.Devices {
		newSet[cert.Certificate.DeviceID] = cert.Certificate
	}
	for id, prior := range old {
		current, ok := newSet[id]
		if !ok {
			removed = append(removed, id)
			continue
		}
		if prior.KeyEpoch != current.KeyEpoch || prior.SigningKeyID != current.SigningKeyID || prior.SigningPublicKey != current.SigningPublicKey || prior.WrapKeyID != current.WrapKeyID || prior.WrapPublicKey != current.WrapPublicKey {
			changed = append(changed, id)
		}
	}
	for id := range newSet {
		if _, ok := old[id]; !ok {
			added = append(added, id)
		}
	}
	sort.Strings(removed)
	sort.Strings(added)
	sort.Strings(changed)
	return
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func VerifyRotationStatement(previous, next identity.VerifiedRoster, statement SignedRotationStatementV1, now time.Time) error {
	s := statement.Statement
	p, n := previous.Manifest.Manifest, next.Manifest.Manifest
	transitionHash, err := RosterTransitionHash(previous, next)
	if err != nil {
		return err
	}
	removed, added, changed := accessDiff(previous, next)
	if s.Version != 1 || s.NamespaceID != n.ScopeID || s.PreviousVersion == 0 || s.PreviousVersion >= MaxNamespaceKeyVersion || s.NewVersion != s.PreviousVersion+1 || s.PreviousRosterHash != [32]byte(previous.Hash) || s.NewRosterEpoch != n.Epoch || s.NewRosterHash != [32]byte(next.Hash) || s.PreviousAccessGeneration != p.AccessGeneration || s.PreviousAccessSetHash != p.AccessSetHash || s.NewAccessGeneration != n.AccessGeneration || s.NewAccessSetHash != n.AccessSetHash || n.AccessGeneration != p.AccessGeneration+1 || s.RosterTransitionHash != transitionHash || s.AuthorityStateHash != next.Authority.StateHash || s.AuthorityEpoch != next.Authority.AuthorityEpoch || !equalStrings(s.RemovedDeviceIDs, removed) || !equalStrings(s.AddedDeviceIDs, added) || !equalStrings(s.ChangedDeviceIDs, changed) || s.IssuedAtUnix > now.Add(5*time.Minute).Unix() || s.ExpiresAtUnix <= s.IssuedAtUnix || now.Unix() > s.ExpiresAtUnix || s.Nonce == ([32]byte{}) {
		return fmt.Errorf("keyrotation: rotation statement metadata mismatch")
	}
	if len(statement.SignerKeyIDs) != len(statement.Signatures) || len(statement.SignerKeyIDs) < int(previous.Authority.Threshold) || len(statement.SignerKeyIDs) > len(previous.Authority.Authorities) {
		return fmt.Errorf("keyrotation: insufficient rotation signatures")
	}
	preimage, err := rotationCanonical("aplexica/namespace-rotation-statement/v1", s)
	if err != nil {
		return err
	}
	for i, id := range statement.SignerKeyIDs {
		if i > 0 && string(statement.SignerKeyIDs[i-1][:]) >= string(id[:]) {
			return fmt.Errorf("keyrotation: noncanonical rotation signers")
		}
		authority, ok := previous.Authority.Authorities[identity.DeviceKeyID(id)]
		if !ok || !activePreviousAuthority(previous, authority, s.IssuedAtUnix) || !ed25519.Verify(authority.SigningPublicKey[:], preimage, statement.Signatures[i][:]) {
			return fmt.Errorf("keyrotation: invalid rotation signature")
		}
	}
	return nil
}

func activePreviousAuthority(previous identity.VerifiedRoster, authority identity.RosterAuthorityV1, at int64) bool {
	for _, signed := range previous.Manifest.Manifest.Devices {
		credential := signed.Certificate
		if credential.DeviceID == authority.DeviceID && credential.SigningKeyID == authority.SigningKeyID &&
			credential.SigningPublicKey == authority.SigningPublicKey && credential.NotBeforeUnix <= at && at < credential.NotAfterUnix {
			return true
		}
	}
	return false
}

func ManifestHash(manifest SignedNamespaceKeyManifestV1) ([32]byte, error) {
	return rotationDigest("aplexica/namespace-key-manifest-hash/v1", manifest)
}

func VerifyNamespaceKeyManifest(next identity.VerifiedRoster, statement SignedRotationStatementV1, manifest SignedNamespaceKeyManifestV1) error {
	s, m := statement.Statement, manifest.Manifest
	statementHash, err := StatementHash(statement)
	if err != nil {
		return err
	}
	if m.Version != 1 || m.StatementHash != statementHash || m.NamespaceID != s.NamespaceID || m.KeyVersion != s.NewVersion || m.AccessGeneration != s.NewAccessGeneration || m.AccessSetHash != s.NewAccessSetHash || m.IssuedRosterEpoch != s.NewRosterEpoch || m.IssuedRosterHash != s.NewRosterHash || m.AuthorityStateHash != s.AuthorityStateHash {
		return fmt.Errorf("keyrotation: namespace key manifest metadata mismatch")
	}
	var leader *identity.DeviceCertificateUnsignedV1
	for i := range next.Manifest.Manifest.Devices {
		c := &next.Manifest.Manifest.Devices[i].Certificate
		if c.DeviceID == m.LeaderDeviceID && c.SigningKeyID == m.LeaderSigningKeyID {
			leader = c
			break
		}
	}
	if leader == nil {
		return fmt.Errorf("keyrotation: manifest leader is not active")
	}
	preimage, err := rotationCanonical("aplexica/namespace-key-manifest/v1", m)
	if err != nil || !ed25519.Verify(leader.SigningPublicKey[:], preimage, manifest.Signature[:]) {
		return fmt.Errorf("keyrotation: invalid manifest leader signature")
	}
	expected := make(map[string][32]byte, len(next.Manifest.Manifest.Devices)+1)
	for _, cert := range next.Manifest.Manifest.Devices {
		expected["device\x00"+cert.Certificate.DeviceID] = cert.Certificate.WrapKeyID
	}
	expected["recovery\x00account-recovery"] = next.Authority.Anchor.Anchor.RecoveryWrapKeyID
	if len(m.Wrapped) != len(expected) {
		return fmt.Errorf("keyrotation: incomplete manifest recipients")
	}
	last := ""
	for _, entry := range m.Wrapped {
		key := entry.RecipientType + "\x00" + entry.RecipientID
		if key <= last || expected[key] != entry.RecipientWrapKeyID || len(entry.Wrapped.Ciphertext) != 48 {
			return fmt.Errorf("keyrotation: invalid manifest recipient set")
		}
		delete(expected, key)
		last = key
	}
	if len(expected) != 0 {
		return fmt.Errorf("keyrotation: incomplete manifest recipient set")
	}
	return nil
}
