package nativebackup

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/aplexica/aplexica/internal/privatefs"
)

type ManifestAuth struct {
	Algorithm string `json:"algorithm"`
	KeyID     string `json:"keyId"`
	MAC       string `json:"mac"`
}

const (
	manifestAuthAlgorithm = "HMAC-SHA256"
	manifestAuthKeyBytes  = sha256.Size
)

type manifestProjection struct {
	SchemaVersion   int             `json:"schemaVersion"`
	CreatedAt       string          `json:"createdAt"`
	AplexicaVersion string          `json:"aplexicaVersion"`
	Agents          []AgentManifest `json:"agents"`
}

func canonicalManifestProjection(m Manifest) ([]byte, error) {
	agents := append([]AgentManifest(nil), m.Agents...)
	for i := range agents {
		agents[i].SourceRoots = append([]string(nil), agents[i].SourceRoots...)
		agents[i].Roots = append([]FileEntry(nil), agents[i].Roots...)
		agents[i].Skipped = append([]SkippedFile(nil), agents[i].Skipped...)
		sort.Strings(agents[i].SourceRoots)
		sort.Slice(agents[i].Roots, func(a, b int) bool { return agents[i].Roots[a].Path < agents[i].Roots[b].Path })
		sort.Slice(agents[i].Skipped, func(a, b int) bool { return agents[i].Skipped[a].Path < agents[i].Skipped[b].Path })
	}
	sort.Slice(agents, func(i, j int) bool { return agents[i].Name < agents[j].Name })
	return json.Marshal(manifestProjection{SchemaVersion: 2, CreatedAt: m.CreatedAt.UTC().Format(time.RFC3339Nano), AplexicaVersion: m.AplexicaVersion, Agents: agents})
}

func manifestKeyPathForBackupDir(backupDir string) string {
	return filepath.Join(filepath.Dir(filepath.Dir(filepath.Clean(backupDir))), "keys", "native-manifest-hmac-v2")
}

func loadManifestKey(path string, create bool) ([32]byte, error) {
	var out [32]byte
	abs, err := filepath.Abs(path)
	if err != nil {
		return out, err
	}
	parent, base := filepath.Dir(abs), filepath.Base(abs)
	if err = privatefs.EnsureDir(parent, privatefs.DirPolicy{Access: privatefs.AccessPrivate, RepairOwned: true, AllowExisting: create}); err != nil {
		return out, err
	}
	root, err := privatefs.OpenRoot(parent, privatefs.DirPolicy{Access: privatefs.AccessPrivate, RepairOwned: true})
	if err != nil {
		return out, err
	}
	defer root.Close()
	read := func() error {
		f, e := root.OpenReadRegular(base)
		if e != nil {
			return e
		}
		b, e := io.ReadAll(io.LimitReader(f, 33))
		ce := f.Close()
		if e == nil {
			e = ce
		}
		if e != nil || len(b) != 32 {
			return fmt.Errorf("nativebackup: invalid manifest authentication key")
		}
		copy(out[:], b)
		return nil
	}
	if err = read(); err == nil {
		return out, nil
	} else if !create || !errors.Is(err, os.ErrNotExist) {
		return out, err
	}
	if _, err = io.ReadFull(rand.Reader, out[:]); err != nil {
		return [32]byte{}, err
	}
	f, temp, err := root.CreateTemp(".", ".native-manifest-key-")
	if err != nil {
		return [32]byte{}, err
	}
	if _, err = f.Write(out[:]); err == nil {
		err = f.Sync()
	}
	if ce := f.Close(); err == nil {
		err = ce
	}
	if err == nil {
		err = root.InstallNoReplace(temp, base)
	}
	if err != nil && !errors.Is(err, os.ErrExist) {
		_ = root.RemoveRegular(temp)
		return [32]byte{}, err
	}
	if errors.Is(err, os.ErrExist) {
		_ = root.RemoveRegular(temp)
	}
	out = [32]byte{}
	return out, read()
}

func signManifest(m *Manifest, key [32]byte) error {
	if m.SchemaVersion != 2 {
		return fmt.Errorf("nativebackup: manifest v2 required")
	}
	b, err := canonicalManifestProjection(*m)
	if err != nil {
		return err
	}
	mac := hmac.New(sha256.New, key[:])
	_, _ = mac.Write([]byte("aplexica/native-manifest/v2\x00"))
	_, _ = mac.Write(b)
	id := sha256.Sum256(key[:])
	m.Auth = ManifestAuth{Algorithm: manifestAuthAlgorithm, KeyID: hex.EncodeToString(id[:]), MAC: hex.EncodeToString(mac.Sum(nil))}
	return nil
}
func SignDefaultManifest(m *Manifest, backupDir string) error {
	k, err := loadManifestKey(manifestKeyPathForBackupDir(backupDir), true)
	if err != nil {
		return err
	}
	return signManifest(m, k)
}
func signManifestWithKeyPath(m *Manifest, keyPath string) error {
	k, err := loadManifestKey(keyPath, true)
	if err != nil {
		return err
	}
	return signManifest(m, k)
}
func VerifyDefaultManifest(m Manifest, backupDir string) error {
	return VerifyManifestWithKeyPath(m, manifestKeyPathForBackupDir(backupDir))
}
func VerifyManifestWithKeyPath(m Manifest, keyPath string) error {
	k, err := loadManifestKey(keyPath, false)
	if err != nil {
		return err
	}
	return verifyManifest(m, k)
}

func verifyManifest(m Manifest, k [manifestAuthKeyBytes]byte) error {
	if m.SchemaVersion != 2 || m.Auth.Algorithm != manifestAuthAlgorithm {
		return fmt.Errorf("nativebackup: authenticated manifest v2 required")
	}
	id := sha256.Sum256(k[:])
	if m.Auth.KeyID != hex.EncodeToString(id[:]) {
		return fmt.Errorf("nativebackup: manifest key id mismatch")
	}
	b, err := canonicalManifestProjection(m)
	if err != nil {
		return err
	}
	mac := hmac.New(sha256.New, k[:])
	_, _ = mac.Write([]byte("aplexica/native-manifest/v2\x00"))
	_, _ = mac.Write(b)
	got, err := hex.DecodeString(m.Auth.MAC)
	if err != nil || !hmac.Equal(got, mac.Sum(nil)) {
		return fmt.Errorf("nativebackup: manifest authentication failed")
	}
	return nil
}
