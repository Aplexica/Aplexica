package identity

import (
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"fmt"

	"golang.org/x/crypto/curve25519"
)

func TranscriptHash(t PairingTranscriptV1) ([32]byte, error) {
	if t.Version != 1 || !validText(t.ServiceOrigin, 512) || !validText(t.AccountID, 256) || !validText(t.PendingID, 256) || !validText(t.CandidateDeviceID, 256) || !validText(t.ApproverDeviceID, 256) || !nonzero32(t.PairingNonce) || !nonzero32(t.CandidateEphemeralPublic) || !nonzero32(t.ApproverEphemeralPublic) || !sortedUniqueVersions(t.CandidateEnvelopeVersions) {
		return [32]byte{}, fmt.Errorf("identity: invalid pairing transcript")
	}
	return digest("aplexica/pairing-transcript/v1", t)
}

type PairingKeys struct {
	Master                  [32]byte
	CandidateToApproverAEAD [32]byte
	ApproverToCandidateAEAD [32]byte
	CandidateConfirm        [32]byte
	ApproverConfirm         [32]byte
	SASKey                  [32]byte
}

func DerivePairingKeys(localPrivate [32]byte, peerPublic [32]byte, t PairingTranscriptV1) (PairingKeys, error) {
	th, err := TranscriptHash(t)
	if err != nil {
		return PairingKeys{}, err
	}
	shared, err := curve25519.X25519(localPrivate[:], peerPublic[:])
	if err != nil || zeroBytes(shared) {
		return PairingKeys{}, fmt.Errorf("identity: invalid pairing shared secret")
	}
	master, err := hkdf.Key(sha256.New, shared, t.PairingNonce[:], "aplexica/pairing-master/v1"+string(th[:]), 32)
	if err != nil {
		return PairingKeys{}, err
	}
	var out PairingKeys
	copy(out.Master[:], master)
	derive := func(info string, dst *[32]byte) error {
		b, e := hkdf.Key(sha256.New, master, th[:], info, 32)
		if e == nil {
			copy(dst[:], b)
		}
		return e
	}
	for _, x := range []struct {
		i string
		d *[32]byte
	}{{"aplexica/pairing-aead/c2a/v1", &out.CandidateToApproverAEAD}, {"aplexica/pairing-aead/a2c/v1", &out.ApproverToCandidateAEAD}, {"aplexica/pairing-confirm/c2a/v1", &out.CandidateConfirm}, {"aplexica/pairing-confirm/a2c/v1", &out.ApproverConfirm}, {"aplexica/pairing-sas/v1", &out.SASKey}} {
		if err := derive(x.i, x.d); err != nil {
			return PairingKeys{}, err
		}
	}
	return out, nil
}
func PairingSAS(key [32]byte, transcriptHash [32]byte) (string, error) {
	const million = uint64(1_000_000)
	limit := uint64(1<<32) / million * million
	for i := uint32(0); i < 256; i++ {
		m := hmac.New(sha256.New, key[:])
		_, _ = m.Write([]byte("aplexica/pairing-sas/v1"))
		_, _ = m.Write(transcriptHash[:])
		var b [4]byte
		binary.BigEndian.PutUint32(b[:], i)
		_, _ = m.Write(b[:])
		v := uint64(binary.BigEndian.Uint32(m.Sum(nil)[:4]))
		if v < limit {
			return fmt.Sprintf("%06d", v%million), nil
		}
	}
	return "", fmt.Errorf("identity: SAS rejection sampling exhausted")
}
func zeroBytes(b []byte) bool {
	var v byte
	for _, x := range b {
		v |= x
	}
	return v == 0
}
