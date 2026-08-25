package acf

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"filippo.io/age"

	"github.com/aplexica/aplexica/internal/atomicfile"
	"github.com/aplexica/aplexica/internal/privatefs"
)

// GenerateAgeIdentity creates a fresh X25519 age identity (private +
// public key pair).
func GenerateAgeIdentity() (*age.X25519Identity, error) {
	id, err := age.GenerateX25519Identity()
	if err != nil {
		return nil, fmt.Errorf("acf: generate age identity: %w", err)
	}
	return id, nil
}

// EncryptBundle reads plaintext bytes and writes them ciphertext-wrapped to
// out using the given recipients. Any number of recipients can be supplied;
// the resulting ciphertext decrypts under ANY one of their identities.
func EncryptBundle(plaintext io.Reader, ciphertext io.Writer, recipients []age.Recipient) error {
	w, err := age.Encrypt(ciphertext, recipients...)
	if err != nil {
		return fmt.Errorf("acf: age encrypt header: %w", err)
	}
	if _, err := io.Copy(w, plaintext); err != nil {
		return fmt.Errorf("acf: age encrypt body: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("acf: age encrypt close: %w", err)
	}
	return nil
}

// DecryptBundle reads ciphertext from in and writes plaintext to out using
// any of the supplied identities. Returns an error if no identity matches.
func DecryptBundle(ciphertext io.Reader, plaintext io.Writer, identities []age.Identity) error {
	r, err := age.Decrypt(ciphertext, identities...)
	if err != nil {
		return fmt.Errorf("acf: age decrypt: %w", err)
	}
	if _, err := io.Copy(plaintext, r); err != nil {
		return fmt.Errorf("acf: age read plaintext: %w", err)
	}
	return nil
}

// SaveAgeIdentity writes id's secret key to path with 0o600 perms (via
// atomicfile). The file is one line in age's standard text format
// ("AGE-SECRET-KEY-1...").
func SaveAgeIdentity(path string, id *age.X25519Identity) error {
	line := id.String() + "\n"
	return atomicfile.WriteFile(path, []byte(line), 0o600)
}

// LoadAgeIdentity reads an X25519 identity (AGE-SECRET-KEY-...) from path.
func LoadAgeIdentity(path string) (*age.X25519Identity, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("acf: read age identity: %w", err)
	}
	line := strings.TrimSpace(string(data))
	id, err := age.ParseX25519Identity(line)
	if err != nil {
		return nil, fmt.Errorf("acf: parse age identity: %w", err)
	}
	return id, nil
}

// SaveAgeRecipient writes the public recipient line ("age1...") to path
// with 0o600 perms. Safe to share.
func SaveAgeRecipient(path string, id *age.X25519Identity) error {
	line := id.Recipient().String() + "\n"
	return atomicfile.WriteFile(path, []byte(line), 0o600)
}

// LoadAgeRecipient reads an "age1..." recipient line from path.
func LoadAgeRecipient(path string) (age.Recipient, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	root, err := privatefs.OpenRoot(filepath.Dir(abs), privatefs.DirPolicy{Access: privatefs.AccessIntegrityOnly})
	if err != nil {
		return nil, fmt.Errorf("acf: open age recipient parent: %w", err)
	}
	defer root.Close()
	f, err := root.OpenReadRegularIntegrity(filepath.Base(abs))
	if err != nil {
		return nil, fmt.Errorf("acf: read age recipient: %w", err)
	}
	data, err := io.ReadAll(io.LimitReader(f, 4097))
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil || len(data) > 4096 {
		return nil, fmt.Errorf("acf: read age recipient: bounded input required")
	}
	line := strings.TrimSpace(string(data))
	if strings.Count(string(data), "\n") > 1 || strings.HasPrefix(line, "AGE-SECRET-KEY-") {
		return nil, fmt.Errorf("acf: recipient file must contain one public age recipient")
	}
	return age.ParseX25519Recipient(line)
}
