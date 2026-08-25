package keyrotation

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"

	"github.com/aplexica/aplexica/internal/keys"
)

// secretsBackend is the slice of internal/secrets.Store the content-key
// store needs. Declared as an interface so tests can substitute, and so
// this package doesn't hard-depend on the concrete store type.
type secretsBackend interface {
	Get(artifactID, key string) (string, error)
	Put(artifactID, key, value string) error
}

// SecretsContentKeyStore persists namespace content keys in the local
// secrets store (0o600, never hashed into the event chain — per ADR-0027).
// Keys are scoped by namespace id and version so every rotation version is
// retained, preserving forward-erasure read access to older artifacts.
type SecretsContentKeyStore struct {
	secrets secretsBackend
}

// NewSecretsContentKeyStore builds a store over the given secrets backend.
func NewSecretsContentKeyStore(s secretsBackend) *SecretsContentKeyStore {
	return &SecretsContentKeyStore{secrets: s}
}

func contentKeySecretName(version int) string {
	return fmt.Sprintf("nskey_v%d", version)
}

// GetContentKey returns the stored content key for (namespace, version).
// ok is false (with nil error) when no key is stored yet.
func (s *SecretsContentKeyStore) GetContentKey(namespaceID string, version int) ([]byte, bool, error) {
	raw, err := s.secrets.Get(namespaceID, contentKeySecretName(version))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("keyrotation: read content key %s v%d: %w", namespaceID, version, err)
	}
	decoded, derr := base64.StdEncoding.DecodeString(raw)
	if derr != nil {
		return nil, false, fmt.Errorf("keyrotation: decode content key %s v%d: %w", namespaceID, version, derr)
	}
	// Reject a truncated/corrupt stored key here rather than letting a
	// wrong-length scalar flow into SealBody/OpenBody/WrapContentKey, where it
	// surfaces as a generic crypto error far from the source (mirrors the
	// devicekey decode length check in devicekey.go).
	if len(decoded) != keys.ContentKeySize {
		return nil, false, fmt.Errorf("keyrotation: stored content key %s v%d is %d bytes, want %d", namespaceID, version, len(decoded), keys.ContentKeySize)
	}
	return decoded, true, nil
}

// PutContentKey persists the content key for (namespace, version).
func (s *SecretsContentKeyStore) PutContentKey(namespaceID string, version int, key []byte) error {
	enc := base64.StdEncoding.EncodeToString(key)
	if err := s.secrets.Put(namespaceID, contentKeySecretName(version), enc); err != nil {
		return fmt.Errorf("keyrotation: persist content key %s v%d: %w", namespaceID, version, err)
	}
	return nil
}
