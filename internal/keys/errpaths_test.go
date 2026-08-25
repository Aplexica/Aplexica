package keys

// Error-branch coverage for the cryptographic paths in this package, per
// NFR-09.6 (100% branch coverage of crypto code). These are white-box tests
// (package keys) because several target branches live in unexported helpers
// (decode/create) or need hand-built wire blobs that only the in-package
// constants describe.
//
// The rand-failure branches (e.g. NewContentKey's rand.Read error, the
// ephemeral/nonce rand errors in WrapContentKey, NewDeviceKey's rand error)
// and the post-length-check newGCM/HKDF/X25519-derive defensive errors are
// NOT exercised here: crypto/rand.Read does not fail on supported platforms,
// curve25519.X25519 never errors on a 32-byte scalar, and newGCM cannot fail
// once the 32-byte key length is already validated. Forcing them would require
// dead production hooks, which NFR-09.6 explicitly disallows. They are
// documented as defensively-unreachable in the task notes.

import (
	"bytes"
	"encoding/base64"
	"io/fs"
	"testing"

	"golang.org/x/crypto/curve25519"
)

// Test-only constants (test files are exempt from magiclint's no-bare-literal
// rule; kept named here for readability anyway).
const (
	gcmTagSize  = 16 // AES-GCM authentication tag length.
	seedNonZero = 9  // arbitrary non-zero seed byte for a deterministic ephemeral scalar.
)

func encodeBase64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

// notExist is an error that reports true for errors.Is(_, os.ErrNotExist) so a
// fake secrets backend can simulate a clean miss (the create() path), without
// importing os just for the sentinel.
type notExist struct{}

func (notExist) Error() string        { return "not found" }
func (notExist) Is(target error) bool { return target == fs.ErrNotExist }
func (notExist) Unwrap() error        { return fs.ErrNotExist }

// failingSecrets is a SecretsStore whose Get/Put can be made to fail or to
// return a chosen stored value, so the device-key decode/create error branches
// become reachable without touching the on-disk store.
type failingSecrets struct {
	getVal string
	getErr error
	putErr error
	put    map[string]string
}

func (f *failingSecrets) Get(artifactID, key string) (string, error) {
	return f.getVal, f.getErr
}

func (f *failingSecrets) Put(artifactID, key, value string) error {
	if f.putErr != nil {
		return f.putErr
	}
	if f.put == nil {
		f.put = map[string]string{}
	}
	f.put[artifactID+"/"+key] = value
	return nil
}

func (f *failingSecrets) GetOrCreate(artifactID, key string, generate func() (string, error), validate func(string) (string, error)) (string, bool, error) {
	if f.getErr == nil {
		v, err := validate(f.getVal)
		return v, false, err
	}
	if _, ok := f.getErr.(notExist); !ok {
		return "", false, f.getErr
	}
	v, err := generate()
	if err != nil {
		return "", false, err
	}
	if f.putErr != nil {
		return "", false, f.putErr
	}
	v, err = validate(v)
	if err != nil {
		return "", false, err
	}
	return v, true, nil
}

// errSentinel is a non-os.ErrNotExist error used to drive the "default" arm of
// LoadOrCreate's switch (a real backend failure that is neither a hit nor a
// clean miss).
type errSentinel struct{}

func (errSentinel) Error() string { return "backend exploded" }

// --- UnwrapContentKey error branches ---------------------------------------

// A wrapped blob whose ephemeral-pub region is an all-zero (low-order) point
// drives the X25519(devicePriv, ephemeralPub) error branch (wrap.go ~line
// 153): curve25519 rejects the low-order point before any GCM work. This is
// the reachable sibling of the documented-unreachable all-zero shared-secret
// guard a few lines below it.
func TestUnwrap_LowOrderEphemeralPub_Fails(t *testing.T) {
	priv, _, err := NewDeviceKey()
	if err != nil {
		t.Fatalf("NewDeviceKey: %v", err)
	}
	// Full-length blob (>= X25519KeySize+nonceSize+ContentKeySize) so it
	// passes the length gate and reaches the ECDH step, but with an all-zero
	// ephemeral pub.
	wrapped := make([]byte, X25519KeySize+nonceSize+ContentKeySize+gcmTagSize)
	if _, err := UnwrapContentKey(wrapped, priv); err != ErrUnwrapAuthenticationFailed {
		t.Fatalf("expected ErrUnwrapAuthenticationFailed for low-order ephemeral pub, got %v", err)
	}
}

// A blob that decrypts cleanly but to a plaintext whose length is not
// ContentKeySize drives the final len(plaintext) != ContentKeySize guard
// (wrap.go ~line 185). We build it by sealing a 16-byte plaintext by hand
// (total wire length 76 bytes passes the >=76 length gate, yet the recovered
// plaintext is 16 bytes).
func TestUnwrap_DecryptsToWrongLength_Fails(t *testing.T) {
	priv, pub, err := NewDeviceKey()
	if err != nil {
		t.Fatalf("NewDeviceKey: %v", err)
	}
	var ephemeral [X25519KeySize]byte
	ephemeral[0] = seedNonZero
	ephemeral[0] &= clampFirstByteMask
	ephemeral[scalarHighByteIdx] = (ephemeral[scalarHighByteIdx] & clampLastByteMask) | clampLastByteSet
	ephemeralPub, err := curve25519.X25519(ephemeral[:], curve25519.Basepoint)
	if err != nil {
		t.Fatalf("ephemeral pub: %v", err)
	}
	shared, err := curve25519.X25519(ephemeral[:], pub[:])
	if err != nil {
		t.Fatalf("ecdh: %v", err)
	}
	wrapKey, err := deriveWrapKey(shared, ephemeralPub, pub[:])
	if err != nil {
		t.Fatalf("deriveWrapKey: %v", err)
	}
	gcm, err := newGCM(wrapKey)
	if err != nil {
		t.Fatalf("newGCM: %v", err)
	}
	nonce := make([]byte, nonceSize)
	// 16-byte plaintext -> 32-byte ct -> 76-byte wire, length-gate-passing.
	ct := gcm.Seal(nil, nonce, make([]byte, ContentKeySize/2), ephemeralPub)
	wrapped := make([]byte, 0, X25519KeySize+nonceSize+len(ct))
	wrapped = append(wrapped, ephemeralPub...)
	wrapped = append(wrapped, nonce...)
	wrapped = append(wrapped, ct...)
	if len(wrapped) != X25519KeySize+nonceSize+ContentKeySize {
		t.Fatalf("precondition: wire len = %d, want %d (must pass the length gate)", len(wrapped), X25519KeySize+nonceSize+ContentKeySize)
	}
	if _, err := UnwrapContentKey(wrapped, priv); err != ErrUnwrapAuthenticationFailed {
		t.Fatalf("expected ErrUnwrapAuthenticationFailed for wrong-length plaintext, got %v", err)
	}
}

// --- WrapContentKey error branches -----------------------------------------

// A recipient devicePub that is the all-zero (low-order) X25519 point drives
// the ECDH error branch in WrapContentKey (wrap.go ~line 117): the ephemeral
// pub derivation against the basepoint succeeds, but the shared-secret
// derivation against the low-order recipient point is rejected by
// curve25519.X25519 before any GCM work. This models a corrupt or hostile
// pubkey registration. It is the wrap-side sibling of the unwrap low-order
// guard already covered by TestUnwrap_LowOrderEphemeralPub_Fails.
func TestWrap_LowOrderDevicePub_Fails(t *testing.T) {
	ck, err := NewContentKey()
	if err != nil {
		t.Fatalf("NewContentKey: %v", err)
	}
	var zeroPub [X25519KeySize]byte // all-zero low-order point
	if _, err := WrapContentKey(ck, zeroPub); err == nil {
		t.Fatal("expected WrapContentKey to fail for an all-zero (low-order) device pub")
	}
}

// --- newGCM error branch ---------------------------------------------------

// newGCM must surface aes.NewCipher's error for a key whose length is not a
// valid AES key size (wrap.go ~line 209). In the production seal/unwrap paths
// the key length is always validated before newGCM is reached, so this branch
// is only reachable by calling the helper directly with a malformed key — the
// guard that keeps a future caller from feeding GCM a wrong-size key. The
// companion cipher.NewGCM error arm (wrap.go ~line 213) stays unreachable: an
// AES block is always 128-bit, the one block size cipher.NewGCM accepts.
func TestNewGCM_RejectsBadKeyLength(t *testing.T) {
	const notAnAESKeySize = 17 // AES accepts only 16, 24, or 32-byte keys.
	if _, err := newGCM(make([]byte, notAnAESKeySize)); err == nil {
		t.Fatal("expected newGCM to fail for a key that is not a valid AES key size")
	}
	// Sanity: a correctly-sized key still builds a GCM (cipher.NewGCM does not
	// error for an AES block), so the rejection above is specifically the
	// aes.NewCipher length check, not a blanket failure.
	if _, err := newGCM(make([]byte, ContentKeySize)); err != nil {
		t.Fatalf("newGCM(%d-byte key) unexpectedly failed: %v", ContentKeySize, err)
	}
}

// --- OpenBody error branches -----------------------------------------------

// OpenBody must reject a wrong-length key and a wrong-length nonce up front
// (body.go ~line 47) before touching GCM.
func TestOpenBody_RejectsBadKeyAndNonceLen(t *testing.T) {
	goodKey, _ := NewContentKey()
	goodNonce := make([]byte, BodyNonceSize)

	if _, err := OpenBody(make([]byte, ContentKeySize-1), goodNonce, nil); err != ErrBodyAuthenticationFailed {
		t.Fatalf("short key: expected ErrBodyAuthenticationFailed, got %v", err)
	}
	if _, err := OpenBody(goodKey, make([]byte, BodyNonceSize+1), nil); err != ErrBodyAuthenticationFailed {
		t.Fatalf("wrong nonce len: expected ErrBodyAuthenticationFailed, got %v", err)
	}
}

// --- DeviceKeyStore decode/create error branches ---------------------------

// A backend Get failure that is NOT os.ErrNotExist must surface as a wrapped
// load error (devicekey.go LoadOrCreate default arm, ~line 53).
func TestLoadOrCreate_BackendError_Propagates(t *testing.T) {
	st := NewDeviceKeyStore(&failingSecrets{getErr: errSentinel{}})
	if _, _, err := st.LoadOrCreate(); err == nil {
		t.Fatal("expected backend Get error to propagate from LoadOrCreate")
	}
}

// A stored value that is not valid base64 drives the decode error branch
// (devicekey.go decode, ~line 60).
func TestLoadOrCreate_UndecodableStoredKey_Fails(t *testing.T) {
	st := NewDeviceKeyStore(&failingSecrets{getVal: "!!!not base64!!!"})
	if _, _, err := st.LoadOrCreate(); err == nil {
		t.Fatal("expected base64 decode error for a corrupt stored device key")
	}
}

// A stored value that decodes to the wrong number of bytes drives the length
// guard (devicekey.go decode, ~line 63).
func TestLoadOrCreate_WrongLengthStoredKey_Fails(t *testing.T) {
	// 16 bytes, base64-encoded: decodes cleanly but is the wrong length.
	enc := encodeBase64(bytes.Repeat([]byte{0x01}, X25519KeySize/2))
	st := NewDeviceKeyStore(&failingSecrets{getVal: enc})
	if _, _, err := st.LoadOrCreate(); err == nil {
		t.Fatal("expected length error for a wrong-length stored device key")
	}
}

// A backend Put failure during first-use creation drives the persist error
// branch (devicekey.go create, ~line 81). getErr is os.ErrNotExist so
// LoadOrCreate takes the create() path, then Put fails.
func TestLoadOrCreate_PersistFails(t *testing.T) {
	st := NewDeviceKeyStore(&failingSecrets{getErr: notExist{}, putErr: errSentinel{}})
	if _, _, err := st.LoadOrCreate(); err == nil {
		t.Fatal("expected persist error when the backend Put fails on creation")
	}
}
