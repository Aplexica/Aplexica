package nativebackup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/aplexica/aplexica/internal/privatefs"
)

const backupRedactionMaxInputBytes = int64(16 << 20)

func validFileRedactionKind(kind FileRedactionKind) bool {
	return kind == FileRedactionOpenClawConfig
}

func redactBackupFile(kind FileRedactionKind, data []byte) ([]byte, error) {
	switch kind {
	case FileRedactionOpenClawConfig:
		return redactOpenClawConfig(data)
	default:
		return nil, fmt.Errorf("nativebackup: unsupported file redaction %q", kind)
	}
}

// redactOpenClawConfig preserves the complete user-authored configuration but
// clears machine credential values. OpenClaw's root config mixes channels,
// models, agents, MCP servers, and gateway settings with tokens, so dropping
// the whole file loses irreplaceable configuration while copying it verbatim
// duplicates credentials. String-valued env entries follow Aplexica's existing
// MCP secret model and are cleared; numeric/bool environment settings survive.
func redactOpenClawConfig(data []byte) ([]byte, error) {
	root, err := decodeOpenClawConfig(data)
	if err != nil {
		return nil, err
	}
	changed := redactOpenClawValue(root, false)
	if !changed {
		return append([]byte(nil), data...), nil
	}
	return marshalOpenClawConfig(root)
}

func decodeOpenClawConfig(data []byte) (map[string]any, error) {
	if int64(len(data)) > backupRedactionMaxInputBytes {
		return nil, fmt.Errorf("nativebackup: OpenClaw config exceeds redaction limit")
	}
	if err := rejectDuplicateJSONKeys(data); err != nil {
		return nil, fmt.Errorf("nativebackup: parse OpenClaw config: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var decoded any
	if err := dec.Decode(&decoded); err != nil {
		return nil, fmt.Errorf("nativebackup: parse OpenClaw config: %w", err)
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return nil, fmt.Errorf("nativebackup: trailing OpenClaw config data")
	}
	root, ok := decoded.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("nativebackup: OpenClaw config root is not an object")
	}
	return root, nil
}

func marshalOpenClawConfig(root map[string]any) ([]byte, error) {
	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

// mergeRedactedBackupFile overlays only machine-local values from the current
// live configuration onto a redacted backup copy. Restoring a mixed config can
// therefore recover its user settings without either importing old secrets or
// blanking credentials that belong to the destination machine.
func mergeRedactedBackupFile(kind FileRedactionKind, backup, live []byte) ([]byte, error) {
	switch kind {
	case FileRedactionOpenClawConfig:
		backupRoot, err := decodeOpenClawConfig(backup)
		if err != nil {
			return nil, err
		}
		liveRoot, err := decodeOpenClawConfig(live)
		if err != nil {
			return nil, fmt.Errorf("nativebackup: parse live OpenClaw config before restore: %w", err)
		}
		mergeOpenClawMachineValues(backupRoot, liveRoot)
		return marshalOpenClawConfig(backupRoot)
	default:
		return nil, fmt.Errorf("nativebackup: unsupported file redaction %q", kind)
	}
}

func mergeOpenClawMachineValues(backup, live any) any {
	switch backupTyped := backup.(type) {
	case map[string]any:
		liveTyped, ok := live.(map[string]any)
		if !ok {
			if containsOpenClawMachineValue(live) {
				return live
			}
			return backup
		}
		for key, liveChild := range liveTyped {
			if secretOpenClawKey(key) {
				backupTyped[key] = liveChild
				continue
			}
			normalized := normalizedSecretKey(key)
			if openClawMachineContainerKey(normalized) {
				// Header and credential containers are intentionally absent from a
				// backup. Preserve the complete destination-machine value on restore;
				// field-by-field matching is not a stable credential identity and can
				// assign a secret to the wrong account after a reorder.
				backupTyped[key] = liveChild
				continue
			}
			if normalized == "env" {
				liveEnv, liveOK := liveChild.(map[string]any)
				if !liveOK {
					// An unexpected scalar/array env representation is still
					// machine-local. It was removed from the backup, so retain the
					// destination's complete value instead of overwriting it.
					backupTyped[key] = liveChild
					continue
				}
				backupEnv, backupOK := backupTyped[key].(map[string]any)
				if !backupOK {
					backupEnv = make(map[string]any)
					backupTyped[key] = backupEnv
				}
				for envKey, liveValue := range liveEnv {
					_, existedInBackup := backupEnv[envKey]
					_, liveIsString := liveValue.(string)
					if !existedInBackup || liveIsString {
						backupEnv[envKey] = liveValue
					}
				}
				continue
			}
			backupChild, exists := backupTyped[key]
			if !exists {
				if containsOpenClawMachineValue(liveChild) {
					backupTyped[key] = liveChild
				}
				continue
			}
			backupTyped[key] = mergeOpenClawMachineValues(backupChild, liveChild)
		}
		return backupTyped
	case []any:
		liveTyped, ok := live.([]any)
		if !ok {
			if containsOpenClawMachineValue(live) {
				return live
			}
			return backup
		}
		// Array position is not a stable credential identity. If either side has
		// machine values, preserve the complete live array so a reordered token is
		// never assigned to a different account/server/channel. Backup arrays with
		// no machine values remain ordinary restorable user configuration.
		if containsOpenClawMachineValue(backupTyped) || containsOpenClawMachineValue(liveTyped) {
			return liveTyped
		}
		return backupTyped
	}
	if containsOpenClawMachineValue(live) {
		return live
	}
	return backup
}

func containsOpenClawMachineValue(value any) bool {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if secretOpenClawKey(key) {
				return true
			}
			normalized := normalizedSecretKey(key)
			if openClawMachineContainerKey(normalized) {
				return true
			}
			if normalized == "env" {
				if env, ok := child.(map[string]any); ok {
					for _, envValue := range env {
						switch envValue.(type) {
						case string, map[string]any, []any:
							return true
						}
					}
				} else {
					// Non-object env shapes cannot be classified field by field.
					// Treat them as machine-local so restore never overwrites them
					// from an older redacted snapshot.
					return true
				}
			}
			if containsOpenClawMachineValue(child) {
				return true
			}
		}
	case []any:
		for _, child := range typed {
			if containsOpenClawMachineValue(child) {
				return true
			}
		}
	}
	return false
}

func redactOpenClawValue(value any, envValues bool) bool {
	changed := false
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if secretOpenClawKey(key) {
				// A recognized credential key is secret-bearing regardless of its
				// JSON type. Replacing objects/arrays/numbers as well as strings
				// prevents an unexpected typed credential envelope from bypassing
				// redaction. The one idempotent representation is the empty string.
				if current, ok := child.(string); !ok || current != "" {
					typed[key] = ""
					changed = true
				}
				continue
			}
			normalized := normalizedSecretKey(key)
			if openClawMachineContainerKey(normalized) {
				// Headers and generic credential envelopes can carry secrets under
				// arbitrary provider-specific names. An allowlist cannot prove the
				// remaining values are safe, so omit the entire container.
				if container, ok := child.(map[string]any); !ok || len(container) != 0 {
					typed[key] = map[string]any{}
					changed = true
				}
				continue
			}
			childEnv := normalized == "env"
			if childEnv {
				if env, ok := child.(map[string]any); ok {
					for envKey, envValue := range env {
						switch current := envValue.(type) {
						case string:
							if current != "" {
								env[envKey] = ""
								changed = true
							}
						case json.Number, bool, nil:
							// Numeric and boolean process settings are not credentials.
						default:
							// Objects/arrays are not a supported env value shape and may
							// contain arbitrarily named secrets. Fail closed.
							env[envKey] = ""
							changed = true
						}
					}
					continue
				}
				// A scalar/array env field may itself be a serialized credential
				// envelope. Replace it with the one safe typed representation.
				typed[key] = map[string]any{}
				changed = true
				continue
			}
			if redactOpenClawValue(child, childEnv) {
				changed = true
			}
		}
	case []any:
		for _, child := range typed {
			if redactOpenClawValue(child, envValues) {
				changed = true
			}
		}
	}
	return changed
}

func normalizedSecretKey(key string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '-', '_', '.', ' ':
			return -1
		default:
			return r
		}
	}, strings.ToLower(key))
}

func secretOpenClawKey(key string) bool {
	switch normalizedSecretKey(key) {
	case "token", "authtoken", "bottoken", "accesstoken", "refreshtoken",
		"sessiontoken", "idtoken", "bearer", "bearertoken",
		"apikey", "secret", "secretkey", "clientsecret", "password", "passphrase",
		"privatekey", "signingkey", "webhooksecret", "accesskey", "accesskeyid",
		"secretaccesskey", "authorization", "proxyauthorization", "cookie", "setcookie":
		return true
	default:
		return false
	}
}

// openClawMachineContainerKey identifies objects whose child key names are
// provider-defined and therefore cannot be safely allowlisted. The caller
// passes a normalized key to make that contract explicit.
func openClawMachineContainerKey(normalized string) bool {
	switch normalized {
	case "headers", "credentials", "credential":
		return true
	default:
		return false
	}
}

func copyRedactedFileContext(ctx context.Context, src, dst, manifestRoot string, kind FileRedactionKind) (FileEntry, *SkippedFile, error) {
	if err := ctx.Err(); err != nil {
		return FileEntry{}, nil, err
	}
	pathBefore, err := os.Lstat(src)
	if err != nil {
		if os.IsNotExist(err) {
			return FileEntry{}, nil, nil
		}
		return FileEntry{}, skipReason(src, dst, manifestRoot, fmt.Sprintf("inspect for redaction: %v", err)), nil
	}
	if pathBefore.Mode()&os.ModeSymlink != 0 || !pathBefore.Mode().IsRegular() {
		return FileEntry{}, skipReason(src, dst, manifestRoot, "unsafe non-regular redaction source"), nil
	}
	if hooks := snapshotCopyHooksFromContext(ctx); hooks != nil && hooks.afterSourceInfo != nil {
		hooks.afterSourceInfo(src)
	}
	sourceRoot, err := privatefs.OpenNativeRoot(filepath.Dir(src), privatefs.DirPolicy{Access: privatefs.AccessIntegrityOnly})
	if err != nil {
		if os.IsNotExist(err) {
			return FileEntry{}, nil, nil
		}
		return FileEntry{}, skipReason(src, dst, manifestRoot, fmt.Sprintf("open redaction parent: %v", err)), nil
	}
	defer sourceRoot.Close()
	return copyRetainedRedactedFileContextAfterInfo(ctx, sourceRoot, filepath.Base(src), pathBefore, src, dst, manifestRoot, kind)
}

// copyRetainedRedactedFileContext transforms a file that was discovered below
// an already-retained native root. Source content must stay anchored to that
// root: reopening src through the ambient namespace would let a concurrent
// root or parent replacement redirect only redacted files to a different tree.
func copyRetainedRedactedFileContext(ctx context.Context, sourceRoot *privatefs.Root, rel string, pathBefore os.FileInfo, src, dst, manifestRoot string, kind FileRedactionKind) (FileEntry, *SkippedFile, error) {
	if hooks := snapshotCopyHooksFromContext(ctx); hooks != nil && hooks.afterSourceInfo != nil {
		hooks.afterSourceInfo(src)
	}
	return copyRetainedRedactedFileContextAfterInfo(ctx, sourceRoot, rel, pathBefore, src, dst, manifestRoot, kind)
}

func copyRetainedRedactedFileContextAfterInfo(ctx context.Context, sourceRoot *privatefs.Root, rel string, pathBefore os.FileInfo, src, dst, manifestRoot string, kind FileRedactionKind) (FileEntry, *SkippedFile, error) {
	if err := ctx.Err(); err != nil {
		return FileEntry{}, nil, err
	}
	limiter, _ := ctx.Value(throughputLimitContextKey{}).(*throughputLimiter)
	if err := limiter.waitFile(ctx); err != nil {
		return FileEntry{}, nil, err
	}
	in, err := sourceRoot.OpenReadRegularIntegrity(rel)
	if err != nil {
		if os.IsNotExist(err) {
			return FileEntry{}, nil, nil
		}
		return FileEntry{}, skipReason(src, dst, manifestRoot, fmt.Sprintf("open for redaction: %v", err)), nil
	}
	defer in.Close()
	before, err := in.Stat()
	if err != nil || !sameStableSnapshotFile(pathBefore, before) || before.Size() > backupRedactionMaxInputBytes {
		return FileEntry{}, skipReason(src, dst, manifestRoot, "unsafe or oversized redaction source"), nil
	}
	if hooks := snapshotCopyHooksFromContext(ctx); hooks != nil && hooks.afterSourceOpen != nil {
		hooks.afterSourceOpen(src, in)
	}
	raw, err := io.ReadAll(io.LimitReader(in, backupRedactionMaxInputBytes+1))
	if err != nil {
		return FileEntry{}, skipReason(src, dst, manifestRoot, fmt.Sprintf("read for redaction: %v", err)), nil
	}
	after, statErr := in.Stat()
	if statErr != nil || !sameStableSnapshotFile(before, after) || int64(len(raw)) != before.Size() {
		return FileEntry{}, skipReason(src, dst, manifestRoot, "redaction source changed while reading"), nil
	}
	redacted, err := redactBackupFile(kind, raw)
	if err != nil {
		return FileEntry{}, skipReason(src, dst, manifestRoot, err.Error()), nil
	}
	return writeRedactedSnapshotFile(ctx, redacted, dst, manifestRoot, limiter)
}

func writeRedactedSnapshotFile(ctx context.Context, data []byte, dst, manifestRoot string, limiter *throughputLimiter) (FileEntry, *SkippedFile, error) {
	if err := os.MkdirAll(filepath.Dir(dst), dirPerm); err != nil {
		return FileEntry{}, nil, fmt.Errorf("mkdir parent of %s: %w", dst, err)
	}
	out, err := os.CreateTemp(filepath.Dir(dst), ".nativebackup-redacted-*.tmp")
	if err != nil {
		return FileEntry{}, nil, err
	}
	tmpName := out.Name()
	committed := false
	defer func() {
		if !committed {
			_ = out.Close()
			_ = os.Remove(tmpName)
		}
	}()
	h := sha256.New()
	n, err := copyWithContext(ctx, io.MultiWriter(out, h), bytes.NewReader(data))
	if err == nil && (limiter == nil || !limiter.skipFileSync) {
		err = out.Sync()
	}
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Chmod(tmpName, filePerm)
	}
	if err == nil {
		err = os.Rename(tmpName, dst)
	}
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return FileEntry{}, nil, err
		}
		return FileEntry{}, nil, err
	}
	committed = true
	rel, err := filepath.Rel(manifestRoot, dst)
	if err != nil {
		return FileEntry{}, nil, err
	}
	return FileEntry{Path: filepath.ToSlash(rel), Bytes: n, SHA256: hex.EncodeToString(h.Sum(nil))}, nil, nil
}
