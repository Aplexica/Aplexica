package adapter

import (
	"encoding/json"

	"github.com/aplexica/aplexica/internal/acf"
)

// SecretsReader is the read side of the secrets store the tool imports consult
// to decide whether a re-import changed any secret value. *secrets.Store
// satisfies it. Kept as a narrow interface so this package need not import
// internal/secrets.
type SecretsReader interface {
	ListForArtifact(artifactID string) ([]string, error)
	Get(artifactID, name string) (string, error)
}

type SecretsReconciler interface {
	SecretsReader
	Delete(artifactID, key string) error
	UnlinkToolSecret(name, artifactID string) error
}

// SecretsUnchanged reports whether the secret set already stored for artifactID
// exactly matches want — same keys, same values, none added, none removed.
// Returns false on any difference (so the caller appends an event); read errors
// are surfaced rather than silently treated as "changed".
func SecretsUnchanged(store SecretsReader, artifactID string, want map[string]string) (bool, error) {
	keys, err := store.ListForArtifact(artifactID)
	if err != nil {
		return false, err
	}
	if len(keys) != len(want) {
		return false, nil
	}
	for _, k := range keys {
		got, gerr := store.Get(artifactID, k)
		if gerr != nil {
			return false, gerr
		}
		if w, ok := want[k]; !ok || w != got {
			return false, nil
		}
	}
	return true, nil
}

// ToolImportUnchanged reports whether re-importing a tool config is a true
// no-op: the redacted payload matches the artifact's head AND every stored
// secret matches the freshly extracted set. Tool imports use this instead of
// EventPayloadUnchanged alone because a tool's canonical payload is the
// REDACTED config — a secret rotation leaves the payload byte-identical yet
// must still append an event so the rotated value fans out to other agents.
//
// Returns false (caller should append) on any difference, and false with the
// underlying error if either the event log or the secrets store can't be read.
func ToolImportUnchanged(store *acf.Store, sec SecretsReconciler, artifactID string, payload json.RawMessage, wantSecrets map[string]string) (bool, error) {
	samePayload, err := EventPayloadUnchanged(store, acf.KindTool, artifactID, payload)
	if err != nil || !samePayload {
		return false, err
	}
	sameSecrets, err := SecretsUnchanged(sec, artifactID, wantSecrets)
	if err != nil || sameSecrets {
		return sameSecrets, err
	}
	if secretsOnlyHaveStaleExtras(sec, artifactID, wantSecrets) {
		if err := pruneStaleToolSecrets(sec, artifactID, wantSecrets); err != nil {
			return false, err
		}
		return true, nil
	}
	return false, nil
}

func secretsOnlyHaveStaleExtras(sec SecretsReader, artifactID string, want map[string]string) bool {
	keys, err := sec.ListForArtifact(artifactID)
	if err != nil {
		return false
	}
	seen := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		seen[key] = struct{}{}
		wantValue, ok := want[key]
		if !ok {
			continue
		}
		got, gerr := sec.Get(artifactID, key)
		if gerr != nil || got != wantValue {
			return false
		}
	}
	for key := range want {
		if _, ok := seen[key]; !ok {
			return false
		}
	}
	return true
}
