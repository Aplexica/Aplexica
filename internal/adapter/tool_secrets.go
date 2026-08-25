package adapter

import (
	"errors"
	"fmt"
	"os"
)

// SecretsWriter is the write side of the secrets store the tool imports use to
// persist extracted env-block secrets and link them back to the importing tool
// artifact. *secrets.Store satisfies it. Kept as a narrow interface so this
// package need not import internal/secrets.
type SecretsWriter interface {
	// ListForArtifact returns all current per-artifact secret keys.
	ListForArtifact(artifactID string) ([]string, error)
	// Put writes a per-artifact secret value (<artifact-id>/<key>).
	Put(artifactID, key, value string) error
	// Delete removes one per-artifact secret value.
	Delete(artifactID, key string) error
	// AddUsedByTool records a tool artifact's reference to a named secret in
	// the secret's sidecar (usedByTools). Idempotent.
	AddUsedByTool(name, artifactID string) error
	// UnlinkToolSecret drops a tool artifact's reference from a secret's
	// sidecar when a previously imported secret is no longer present.
	UnlinkToolSecret(name, artifactID string) error
}

// SecretsUnlinker is the rollback side: it removes per-artifact secret values
// AND the sidecar usedByTools references that WriteToolSecrets created.
// *secrets.Store satisfies it.
type SecretsUnlinker interface {
	// DeleteForArtifact removes the entire <artifact-id> secrets dir. Idempotent.
	DeleteForArtifact(artifactID string) error
	// UnlinkToolSecret drops a tool artifact's reference from a secret's
	// sidecar (usedByTools), pruning the sidecar if that leaves it empty and
	// unbacked. Idempotent.
	UnlinkToolSecret(name, artifactID string) error
}

// WriteToolSecrets replaces the per-artifact secret set with exactly extracted,
// then records the tool artifact's reference in each secret's sidecar. It is the
// single chokepoint the per-adapter ImportTool paths share so stale secrets from
// a previous config revision do not keep making an unchanged tool import look
// dirty forever.
//
// On any Put error the secret name is included so the caller can wrap it with
// its adapter prefix. AddUsedByTool failures surface too — the link is part of
// the import's contract, not best-effort. The create-path rollback in each
// adapter's deferred cleanup removes the per-artifact secrets via
// DeleteForArtifact; the orphaned sidecar usedByTools entries are pruned lazily
// when the secret is next listed/used, consistent with how the sidecar already
// tolerates stale references.
func WriteToolSecrets(sec SecretsWriter, artifactID string, extracted map[string]string) error {
	if err := pruneStaleToolSecrets(sec, artifactID, extracted); err != nil {
		return err
	}
	for name, value := range extracted {
		if err := sec.Put(artifactID, name, value); err != nil {
			return fmt.Errorf("store secret %s: %w", name, err)
		}
		if err := sec.AddUsedByTool(name, artifactID); err != nil {
			return fmt.Errorf("link secret %s: %w", name, err)
		}
	}
	return nil
}

type toolSecretPruner interface {
	ListForArtifact(artifactID string) ([]string, error)
	Delete(artifactID, key string) error
	UnlinkToolSecret(name, artifactID string) error
}

func pruneStaleToolSecrets(sec toolSecretPruner, artifactID string, want map[string]string) error {
	keys, err := sec.ListForArtifact(artifactID)
	if err != nil {
		return err
	}
	for _, key := range keys {
		if _, keep := want[key]; keep {
			continue
		}
		if err := sec.Delete(artifactID, key); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("delete stale secret %s: %w", key, err)
		}
		if err := sec.UnlinkToolSecret(key, artifactID); err != nil {
			return fmt.Errorf("unlink stale secret %s: %w", key, err)
		}
	}
	return nil
}

// RollbackToolSecrets undoes WriteToolSecrets for the create path: it removes
// the per-artifact secret dir and every usedByTools reference that this import
// added to a secret's sidecar. Best-effort — errors are ignored, mirroring the
// deferred cleanup it runs inside. extracted is the same map passed to
// WriteToolSecrets so only the links this import created are removed.
func RollbackToolSecrets(sec SecretsUnlinker, artifactID string, extracted map[string]string) {
	_ = sec.DeleteForArtifact(artifactID)
	for name := range extracted {
		_ = sec.UnlinkToolSecret(name, artifactID)
	}
}
