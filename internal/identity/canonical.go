package identity

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/aplexica/aplexica/internal/securityerr"
	"github.com/fxamacker/cbor/v2"
)

var enc cbor.EncMode
var dec cbor.DecMode

func init() {
	var err error
	enc, err = cbor.CanonicalEncOptions().EncMode()
	if err != nil {
		panic(err)
	}
	dec, err = cbor.DecOptions{DupMapKey: cbor.DupMapKeyEnforcedAPF, IndefLength: cbor.IndefLengthForbidden, TagsMd: cbor.TagsForbidden, MaxNestedLevels: 32, MaxArrayElements: 1024, MaxMapPairs: 1024, ExtraReturnErrors: cbor.ExtraDecErrorUnknownField}.DecMode()
	if err != nil {
		panic(err)
	}
}
func canonical(domain string, v any) ([]byte, error) { return enc.Marshal([]any{domain, v}) }
func digest(domain string, values ...any) ([32]byte, error) {
	v := append([]any{domain}, values...)
	b, err := enc.Marshal(v)
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(b), nil
}
func verifySig(pub []byte, domain string, v any, sig []byte) error {
	if len(pub) != ed25519.PublicKeySize || len(sig) != ed25519.SignatureSize {
		return securityerr.ErrInvalidSignature
	}
	b, err := canonical(domain, v)
	if err != nil {
		return err
	}
	if !ed25519.Verify(ed25519.PublicKey(pub), b, sig) {
		return securityerr.ErrInvalidSignature
	}
	return nil
}
func validText(s string, max int) bool {
	if s == "" || len(s) > max || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}
func nonzero32(v [32]byte) bool { return v != ([32]byte{}) }
func sortedUniqueIDs(ids [][32]byte) bool {
	for i := range ids {
		if !nonzero32(ids[i]) || (i > 0 && strings.Compare(string(ids[i-1][:]), string(ids[i][:])) >= 0) {
			return false
		}
	}
	return true
}
func sortedUniqueVersions(v []uint16) bool {
	if len(v) == 0 {
		return false
	}
	for i, x := range v {
		if x == 0 || (i > 0 && v[i-1] >= x) {
			return false
		}
	}
	return true
}
func authoritiesValid(a []RosterAuthorityV1, threshold uint16) bool {
	if len(a) == 0 || len(a) > 255 || threshold == 0 || int(threshold) > len(a) {
		return false
	}
	seen := map[string]bool{}
	for i, x := range a {
		if !validText(x.DeviceID, 256) || seen[x.DeviceID] || !nonzero32(x.SigningKeyID) || !nonzero32(x.SigningPublicKey) || sha256.Sum256(x.SigningPublicKey[:]) != x.SigningKeyID {
			return false
		}
		seen[x.DeviceID] = true
		if i > 0 && strings.Compare(string(a[i-1].SigningKeyID[:]), string(x.SigningKeyID[:])) >= 0 {
			return false
		}
	}
	return true
}
func futureSane(unix int64) bool { return unix > 0 && unix <= time.Now().Add(5*time.Minute).Unix() }
func mapAuthorities(a []RosterAuthorityV1) map[DeviceKeyID]RosterAuthorityV1 {
	m := make(map[DeviceKeyID]RosterAuthorityV1, len(a))
	for _, x := range a {
		m[DeviceKeyID(x.SigningKeyID)] = x
	}
	return m
}
func verifyThreshold(ids [][32]byte, sigs [][64]byte, threshold uint16, authorities map[DeviceKeyID]RosterAuthorityV1, domain string, v any) error {
	if len(ids) != len(sigs) || len(ids) < int(threshold) || !sortedUniqueIDs(ids) {
		return securityerr.ErrInvalidSignature
	}
	for i, id := range ids {
		a, ok := authorities[DeviceKeyID(id)]
		if !ok {
			return securityerr.ErrInvalidSignature
		}
		if err := verifySig(a.SigningPublicKey[:], domain, v, sigs[i][:]); err != nil {
			return err
		}
	}
	return nil
}
func sortedCerts(c []DeviceCertificateV1) bool {
	for i := 1; i < len(c); i++ {
		if c[i-1].Certificate.DeviceID >= c[i].Certificate.DeviceID {
			return false
		}
	}
	return true
}
func sortedEnrollments(e []RecoveryEnrollmentV1) bool {
	for i := 1; i < len(e); i++ {
		if e[i-1].Enrollment.CandidateDeviceID >= e[i].Enrollment.CandidateDeviceID {
			return false
		}
	}
	return true
}
func CanonicalRosterBytes(unsigned RosterManifestUnsignedV1) ([]byte, error) {
	copyDevices := append([]DeviceCertificateV1(nil), unsigned.Devices...)
	sort.Slice(copyDevices, func(i, j int) bool { return copyDevices[i].Certificate.DeviceID < copyDevices[j].Certificate.DeviceID })
	unsigned.Devices = copyDevices
	return enc.Marshal(unsigned)
}

// DecodeCanonicalRosterUnsigned accepts only the exact deterministic CBOR
// representation used by roster signatures. Authority-exchange inputs are
// rejected before a local private key can be used when they contain unknown
// fields, duplicate keys, indefinite values, or a non-canonical device order.
func DecodeCanonicalRosterUnsigned(raw []byte) (RosterManifestUnsignedV1, error) {
	var unsigned RosterManifestUnsignedV1
	if len(raw) == 0 || len(raw) > chainStateMaxBytes || dec.Unmarshal(raw, &unsigned) != nil {
		return RosterManifestUnsignedV1{}, ErrInvalidFreshnessRenewal
	}
	canonical, err := CanonicalRosterBytes(unsigned)
	if err != nil || !bytes.Equal(canonical, raw) {
		return RosterManifestUnsignedV1{}, ErrInvalidFreshnessRenewal
	}
	return unsigned, nil
}
