package acf

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/aplexica/aplexica/internal/privatefs"
)

// Key-file format (single line, ASCII):
//   acf-priv v1 <hex 64-byte ed25519 private key>
//   acf-pub  v1 <hex 32-byte ed25519 public key>
//
// Signature-file format (single line, ASCII):
//   acf-sig v1 <hex 32-byte ed25519 public key> <hex 64-byte signature> <hex 32-byte sha256(bundle)>
//
// Verification: re-hash the bundle and compare; verify Ed25519 sig over
// the SHA-256 hash using the supplied pubkey. We also assert the pubkey
// embedded in the signature matches the supplied --pubkey, so an attacker
// can't substitute their own pubkey alongside a forged signature.

const (
	keyPrivPrefix = "acf-priv v1 "
	keyPubPrefix  = "acf-pub  v1 "
	sigPrefix     = "acf-sig v1 "
)

// GenerateKeyPairFiles writes a fresh Ed25519 keypair to privPath / pubPath.
// Permissions: 0o600 for private, 0o600 for public.
func GenerateKeyPairFiles(privPath, pubPath string) error {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return fmt.Errorf("acf: ed25519 keygen: %w", err)
	}
	privLine := keyPrivPrefix + hex.EncodeToString(priv) + "\n"
	pubLine := keyPubPrefix + hex.EncodeToString(pub) + "\n"
	parent := filepath.Dir(privPath)
	if filepath.Clean(parent) != filepath.Clean(filepath.Dir(pubPath)) {
		return fmt.Errorf("acf: keypair files must share one directory")
	}
	if err := privatefs.EnsureDir(parent, privatefs.DirPolicy{Access: privatefs.AccessPrivate, RepairOwned: true, AllowExisting: true}); err != nil {
		return err
	}
	root, err := privatefs.OpenRoot(parent, privatefs.DirPolicy{Access: privatefs.AccessPrivate, RepairOwned: true})
	if err != nil {
		return err
	}
	defer root.Close()
	if err := root.WriteFile(filepath.Base(privPath), []byte(privLine), privatefs.FilePolicy{RequirePrivateParent: true, RejectWritableByOthers: true}); err != nil {
		return fmt.Errorf("acf: write priv key: %w", err)
	}
	if err := root.WriteFile(filepath.Base(pubPath), []byte(pubLine), privatefs.FilePolicy{RequirePrivateParent: true, RejectWritableByOthers: true}); err != nil {
		return fmt.Errorf("acf: write pub key: %w", err)
	}
	return nil
}

// LoadOrCreateBackupSigningKey installs one complete dedicated signing key by
// no-replace rename. Concurrent creators all reopen and return the same final
// key. A missing public sidecar is deterministically repaired from that final.
func LoadOrCreateBackupSigningKey(privPath string) (ed25519.PrivateKey, ed25519.PublicKey, [32]byte, error) {
	parent, base := filepath.Dir(privPath), filepath.Base(privPath)
	if err := privatefs.EnsureDir(parent, privatefs.DirPolicy{Access: privatefs.AccessPrivate, RepairOwned: true, AllowExisting: true}); err != nil {
		return nil, nil, [32]byte{}, err
	}
	root, err := privatefs.OpenRoot(parent, privatefs.DirPolicy{Access: privatefs.AccessPrivate, RepairOwned: true})
	if err != nil {
		return nil, nil, [32]byte{}, err
	}
	defer root.Close()
	parse := func(rel string) (ed25519.PrivateKey, error) {
		f, e := root.OpenReadRegular(rel)
		if e != nil {
			return nil, e
		}
		data, e := io.ReadAll(io.LimitReader(f, 1025))
		ce := f.Close()
		if e == nil {
			e = ce
		}
		if e != nil || len(data) > 1024 {
			return nil, fmt.Errorf("acf: read backup signing key: %w", e)
		}
		return parsePrivKeyRecord(data)
	}
	priv, err := parse(base)
	if errors.Is(err, os.ErrNotExist) {
		_, generated, genErr := ed25519.GenerateKey(rand.Reader)
		if genErr != nil {
			return nil, nil, [32]byte{}, genErr
		}
		line := []byte(keyPrivPrefix + hex.EncodeToString(generated) + "\n")
		f, temp, e := root.CreateTemp(".", ".backup-signing-")
		if e != nil {
			return nil, nil, [32]byte{}, e
		}
		if _, e = f.Write(line); e == nil {
			e = f.Sync()
		}
		if ce := f.Close(); e == nil {
			e = ce
		}
		if e == nil {
			e = root.InstallNoReplace(temp, base)
		}
		if e != nil && !errors.Is(e, os.ErrExist) {
			_ = root.RemoveRegular(temp)
			return nil, nil, [32]byte{}, e
		}
		if errors.Is(e, os.ErrExist) {
			_ = root.RemoveRegular(temp)
		}
		priv, err = parse(base)
	}
	if err != nil {
		return nil, nil, [32]byte{}, err
	}
	pub := append(ed25519.PublicKey(nil), priv.Public().(ed25519.PublicKey)...)
	id := sha256.Sum256(pub)
	pubName := base + ".pub"
	pubLine := []byte(keyPubPrefix + hex.EncodeToString(pub) + "\n")
	if f, e := root.OpenReadRegular(pubName); e == nil {
		data, re := io.ReadAll(io.LimitReader(f, 1025))
		_ = f.Close()
		if re != nil || string(data) != string(pubLine) {
			return nil, nil, [32]byte{}, fmt.Errorf("acf: backup signing public sidecar mismatch")
		}
	} else if errors.Is(e, os.ErrNotExist) {
		f, temp, ce := root.CreateTemp(".", ".backup-signing-pub-")
		if ce != nil {
			return nil, nil, [32]byte{}, ce
		}
		if _, ce = f.Write(pubLine); ce == nil {
			ce = f.Sync()
		}
		if x := f.Close(); ce == nil {
			ce = x
		}
		if ce == nil {
			ce = root.InstallNoReplace(temp, pubName)
		}
		if ce != nil && !errors.Is(ce, os.ErrExist) {
			_ = root.RemoveRegular(temp)
			return nil, nil, [32]byte{}, ce
		}
		if errors.Is(ce, os.ErrExist) {
			_ = root.RemoveRegular(temp)
		}
	} else {
		return nil, nil, [32]byte{}, e
	}
	return append(ed25519.PrivateKey(nil), priv...), pub, id, nil
}

func parsePrivKeyRecord(data []byte) (ed25519.PrivateKey, error) {
	if len(data) == 0 || data[len(data)-1] != '\n' || strings.Count(string(data), "\n") != 1 || !strings.HasPrefix(string(data), keyPrivPrefix) {
		return nil, fmt.Errorf("acf: invalid private key record")
	}
	raw, err := hex.DecodeString(strings.TrimSuffix(strings.TrimPrefix(string(data), keyPrivPrefix), "\n"))
	if err != nil || len(raw) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("acf: invalid private key")
	}
	priv := ed25519.PrivateKey(raw)
	if !priv.Equal(ed25519.NewKeyFromSeed(priv.Seed())) {
		return nil, fmt.Errorf("acf: inconsistent private key")
	}
	return append(ed25519.PrivateKey(nil), priv...), nil
}

func SignDigest(priv ed25519.PrivateKey, digest [32]byte) ([]byte, error) {
	if len(priv) != ed25519.PrivateKeySize || !priv.Equal(ed25519.NewKeyFromSeed(priv.Seed())) {
		return nil, fmt.Errorf("acf: invalid signing key")
	}
	pub := priv.Public().(ed25519.PublicKey)
	sig := ed25519.Sign(priv, digest[:])
	return []byte(sigPrefix + hex.EncodeToString(pub) + " " + hex.EncodeToString(sig) + " " + hex.EncodeToString(digest[:]) + "\n"), nil
}

// SignBundle reads the bundle file at bundlePath, SHA-256-hashes it, signs
// the hash with the Ed25519 private key at privPath, and returns the
// signature line bytes (ready to write to <bundle>.sig).
func SignBundle(privPath, bundlePath string) ([]byte, error) {
	priv, err := loadPrivKey(privPath)
	if err != nil {
		return nil, err
	}
	hash, err := sha256File(bundlePath)
	if err != nil {
		return nil, err
	}
	sig := ed25519.Sign(priv, hash)
	pub := priv.Public().(ed25519.PublicKey)
	line := sigPrefix + hex.EncodeToString(pub) + " " + hex.EncodeToString(sig) + " " + hex.EncodeToString(hash) + "\n"
	return []byte(line), nil
}

// VerifyBundle parses sigData, re-hashes bundlePath, and verifies the
// signature with the Ed25519 public key at pubPath. Also checks the
// pubkey in the signature matches the supplied pubkey (so an attacker
// can't substitute their own pubkey).
func VerifyBundle(pubPath, bundlePath string, sigData []byte) error {
	pub, err := loadPubKey(pubPath)
	if err != nil {
		return err
	}
	line := strings.TrimSpace(string(sigData))
	if !strings.HasPrefix(line, sigPrefix) {
		return fmt.Errorf("acf: signature missing %q prefix", strings.TrimSpace(sigPrefix))
	}
	body := strings.TrimPrefix(line, sigPrefix)
	parts := strings.Fields(body)
	if len(parts) != 3 {
		return fmt.Errorf("acf: signature line expects 3 hex fields, got %d", len(parts))
	}
	sigPub, err := hex.DecodeString(parts[0])
	if err != nil {
		return fmt.Errorf("acf: signature pubkey hex: %w", err)
	}
	if string(sigPub) != string(pub) {
		return fmt.Errorf("acf: signature pubkey does not match supplied --pubkey")
	}
	sig, err := hex.DecodeString(parts[1])
	if err != nil {
		return fmt.Errorf("acf: signature hex: %w", err)
	}
	expectedHash, err := hex.DecodeString(parts[2])
	if err != nil {
		return fmt.Errorf("acf: signature hash hex: %w", err)
	}
	actualHash, err := sha256File(bundlePath)
	if err != nil {
		return err
	}
	if string(actualHash) != string(expectedHash) {
		return fmt.Errorf("acf: bundle hash mismatch: file has been modified after signing")
	}
	if !ed25519.Verify(pub, actualHash, sig) {
		return fmt.Errorf("acf: signature does not verify against pubkey")
	}
	return nil
}

func loadPrivKey(path string) (ed25519.PrivateKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("acf: read priv key: %w", err)
	}
	line := strings.TrimSpace(string(data))
	if !strings.HasPrefix(line, keyPrivPrefix) {
		return nil, fmt.Errorf("acf: priv key missing %q prefix", strings.TrimSpace(keyPrivPrefix))
	}
	body := strings.TrimPrefix(line, keyPrivPrefix)
	body = strings.TrimSpace(body)
	raw, err := hex.DecodeString(body)
	if err != nil {
		return nil, fmt.Errorf("acf: priv key hex: %w", err)
	}
	if len(raw) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("acf: priv key length %d, expected %d", len(raw), ed25519.PrivateKeySize)
	}
	return ed25519.PrivateKey(raw), nil
}

func loadPubKey(path string) (ed25519.PublicKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("acf: read pub key: %w", err)
	}
	line := strings.TrimSpace(string(data))
	if !strings.HasPrefix(line, keyPubPrefix) {
		return nil, fmt.Errorf("acf: pub key missing %q prefix", strings.TrimSpace(keyPubPrefix))
	}
	body := strings.TrimPrefix(line, keyPubPrefix)
	body = strings.TrimSpace(body)
	raw, err := hex.DecodeString(body)
	if err != nil {
		return nil, fmt.Errorf("acf: pub key hex: %w", err)
	}
	if len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("acf: pub key length %d, expected %d", len(raw), ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(raw), nil
}

func sha256File(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("acf: open bundle: %w", err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return nil, fmt.Errorf("acf: hash bundle: %w", err)
	}
	return h.Sum(nil), nil
}
