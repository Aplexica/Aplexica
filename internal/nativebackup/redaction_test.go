package nativebackup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func writeRedactionTestFile(t *testing.T, path string, data []byte) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, data, 0o600))
}

func redactionTestSHA256(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func TestRedactOpenClawConfigPreservesConfigurationAndClearsCredentials(t *testing.T) {
	raw := []byte(`{
  "channels": {
    "telegram": {
      "enabled": true,
      "botToken": "telegram-secret",
      "allowedChats": [123, 456]
    }
  },
  "models": {
    "primary": "openai/gpt-5",
    "temperature": 0.25,
    "api_key": "model-secret"
  },
  "agents": {
    "default": {
      "workspace": "/srv/openclaw",
      "credentials": {
        "password": {"value": "object-secret", "source": "local"},
        "refresh-token": "refresh-secret"
      }
    }
  },
  "mcp": {
    "servers": {
      "filesystem": {
        "command": "npx",
        "args": ["-y", "@modelcontextprotocol/server-filesystem", "/data"],
        "env": {
          "API_TOKEN": "env-secret",
          "EMPTY": "",
          "PORT": 4312,
          "DEBUG": false
        }
      }
    }
  },
  "gateway": {
    "port": 18789,
    "auth": {"mode": "token", "token": "gateway-secret"}
  },
  "webhook-secret": "hook-secret",
  "nonSecrets": {
    "tokenLimit": 8192,
    "secretSauce": "must survive",
    "description": "user-authored setting"
  }
}`)
	original := append([]byte(nil), raw...)

	got, err := redactOpenClawConfig(raw)
	require.NoError(t, err)
	require.Equal(t, original, raw, "redaction must never mutate the source bytes")
	require.JSONEq(t, `{
  "channels": {
    "telegram": {
      "enabled": true,
      "botToken": "",
      "allowedChats": [123, 456]
    }
  },
  "models": {
    "primary": "openai/gpt-5",
    "temperature": 0.25,
    "api_key": ""
  },
  "agents": {
    "default": {
      "workspace": "/srv/openclaw",
      "credentials": {}
    }
  },
  "mcp": {
    "servers": {
      "filesystem": {
        "command": "npx",
        "args": ["-y", "@modelcontextprotocol/server-filesystem", "/data"],
        "env": {
          "API_TOKEN": "",
          "EMPTY": "",
          "PORT": 4312,
          "DEBUG": false
        }
      }
    }
  },
  "gateway": {
    "port": 18789,
    "auth": {"mode": "token", "token": ""}
  },
  "webhook-secret": "",
  "nonSecrets": {
    "tokenLimit": 8192,
    "secretSauce": "must survive",
    "description": "user-authored setting"
  }
}`, string(got))
}

func TestSnapshotRedactFilesWritesRedactedBytesAndMatchingManifest(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".openclaw")
	configPath := filepath.Join(root, "openclaw.json")
	raw := []byte(`{
  "channels": {"discord": {"enabled": true, "token": "channel-secret"}},
  "models": {"primary": "anthropic/claude", "apiKey": "model-secret"},
  "agents": {"worker": {"workspace": "/workspace", "maxTurns": 12}},
  "mcp": {"servers": {"git": {"command": "uvx", "env": {"GITHUB_TOKEN": "env-secret", "RETRIES": 3, "ENABLED": true}}}},
  "gateway": {"auth": {"mode": "token", "token": "gateway-secret"}}
}`)
	writeRedactionTestFile(t, configPath, raw)
	dest := filepath.Join(t.TempDir(), "manual")

	man, err := Snapshot([]AgentRoots{{
		Name:  "openclaw",
		Roots: []string{root},
		RedactFiles: []FileRedaction{{
			Path: configPath,
			Kind: FileRedactionOpenClawConfig,
		}},
	}}, dest)
	require.NoError(t, err)

	sourceAfter, err := os.ReadFile(configPath)
	require.NoError(t, err)
	require.Equal(t, raw, sourceAfter, "snapshot redaction must leave the live OpenClaw config untouched")
	require.Len(t, man.Agents, 1)
	require.Len(t, man.Agents[0].Roots, 1)
	entry := man.Agents[0].Roots[0]
	copied, err := os.ReadFile(filepath.Join(dest, filepath.FromSlash(entry.Path)))
	require.NoError(t, err)
	require.NotEqual(t, raw, copied)
	require.NotContains(t, string(copied), "channel-secret")
	require.NotContains(t, string(copied), "model-secret")
	require.NotContains(t, string(copied), "env-secret")
	require.NotContains(t, string(copied), "gateway-secret")
	require.JSONEq(t, `{
  "channels": {"discord": {"enabled": true, "token": ""}},
  "models": {"primary": "anthropic/claude", "apiKey": ""},
  "agents": {"worker": {"workspace": "/workspace", "maxTurns": 12}},
  "mcp": {"servers": {"git": {"command": "uvx", "env": {"GITHUB_TOKEN": "", "RETRIES": 3, "ENABLED": true}}}},
  "gateway": {"auth": {"mode": "token", "token": ""}}
}`, string(copied))
	require.Equal(t, int64(len(copied)), entry.Bytes)
	require.Equal(t, redactionTestSHA256(copied), entry.SHA256)
	require.NoError(t, VerifySnapshotFilesContext(context.Background(), dest, man))
}

func TestSnapshotRedactFilesSupportsAmbientSingleFileRoot(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "openclaw.json")
	raw := []byte(`{"profile":"single-file","gateway":{"token":"machine-secret"}}`)
	writeRedactionTestFile(t, configPath, raw)
	dest := filepath.Join(t.TempDir(), "manual")

	man, err := Snapshot([]AgentRoots{{
		Name:        "openclaw",
		Roots:       []string{configPath},
		RedactFiles: []FileRedaction{{Path: configPath, Kind: FileRedactionOpenClawConfig}},
	}}, dest)
	require.NoError(t, err)
	require.Len(t, man.Agents[0].Roots, 1)
	require.Empty(t, man.Agents[0].Skipped)
	copied, err := os.ReadFile(filepath.Join(dest, filepath.FromSlash(man.Agents[0].Roots[0].Path)))
	require.NoError(t, err)
	require.Contains(t, string(copied), "single-file")
	require.NotContains(t, string(copied), "machine-secret")
	sourceAfter, err := os.ReadFile(configPath)
	require.NoError(t, err)
	require.Equal(t, raw, sourceAfter)
}

func TestSnapshotRedactFilesInvalidJSONIsSkippedWithoutCopyingRawSecret(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".openclaw")
	configPath := filepath.Join(root, "openclaw.json")
	raw := []byte(`{"gateway":{"token":"TOP-SECRET"},`)
	writeRedactionTestFile(t, configPath, raw)
	writeRedactionTestFile(t, filepath.Join(root, "settings.txt"), []byte("safe-setting"))
	dest := filepath.Join(t.TempDir(), "manual")

	man, err := Snapshot([]AgentRoots{{
		Name:        "openclaw",
		Roots:       []string{root},
		RedactFiles: []FileRedaction{{Path: configPath, Kind: FileRedactionOpenClawConfig}},
	}}, dest)
	require.NoError(t, err)
	require.Len(t, man.Agents, 1)
	require.Len(t, man.Agents[0].Roots, 1, "the unrelated safe file must still be copied")
	require.Equal(t, "settings.txt", filepath.Base(man.Agents[0].Roots[0].Path))
	require.Len(t, man.Agents[0].Skipped, 1)
	require.Equal(t, "openclaw.json", filepath.Base(man.Agents[0].Skipped[0].Path))
	require.Contains(t, man.Agents[0].Skipped[0].Reason, "parse OpenClaw config")

	_, err = os.Stat(filepath.Join(dest, "openclaw", relativize(root), "openclaw.json"))
	require.ErrorIs(t, err, os.ErrNotExist, "invalid config must fail closed instead of copying raw credentials")
	sourceAfter, err := os.ReadFile(configPath)
	require.NoError(t, err)
	require.Equal(t, raw, sourceAfter)
}

func TestRedactOpenClawConfigFailsClosedForCredentialContainersAndMalformedEnv(t *testing.T) {
	raw := []byte(`{
  "transport": {
    "headers": {
      "Authorization": "Bearer header-secret",
      "Cookie": "session=cookie-secret",
      "X-Custom-Credential": "custom-header-secret",
      "Content-Type": "application/json"
    },
    "credentials": {
      "providerSpecificValue": "generic-credential-secret",
      "account": "machine-account"
    },
    "bearer": "bearer-secret",
    "accessKeyId": "access-key-secret",
    "secretAccessKey": "secret-key-secret",
    "env": "serialized-env-secret"
  },
  "ordinary": {"description": "must survive", "retries": 3}
}`)

	redacted, err := redactOpenClawConfig(raw)
	require.NoError(t, err)
	for _, secret := range []string{
		"header-secret", "cookie-secret", "custom-header-secret",
		"generic-credential-secret", "machine-account", "bearer-secret",
		"access-key-secret", "secret-key-secret", "serialized-env-secret",
	} {
		require.NotContains(t, string(redacted), secret)
	}
	require.JSONEq(t, `{
  "transport": {
    "headers": {},
    "credentials": {},
    "bearer": "",
    "accessKeyId": "",
    "secretAccessKey": "",
    "env": {}
  },
  "ordinary": {"description": "must survive", "retries": 3}
}`, string(redacted))
}

func TestMergeRedactedOpenClawConfigPreservesLiveCredentialContainers(t *testing.T) {
	backup := []byte(`{
  "transport": {
    "headers": {},
    "credentials": {},
    "env": {},
    "timeout": 10
  }
}`)
	live := []byte(`{
  "transport": {
    "headers": {"Authorization": "Bearer current-header-secret"},
    "credentials": {"arbitrary": "current-credential-secret"},
    "env": "current-serialized-env-secret",
    "timeout": 99
  }
}`)

	merged, err := mergeRedactedBackupFile(FileRedactionOpenClawConfig, backup, live)
	require.NoError(t, err)
	require.JSONEq(t, `{
  "transport": {
    "headers": {"Authorization": "Bearer current-header-secret"},
    "credentials": {"arbitrary": "current-credential-secret"},
    "env": "current-serialized-env-secret",
    "timeout": 10
  }
}`, string(merged))
}

func TestSnapshotRejectsInvalidRedactionPathsBeforeCreatingDestination(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".openclaw")
	configPath := filepath.Join(root, "openclaw.json")
	writeRedactionTestFile(t, configPath, []byte(`{"gateway":{"token":"secret"}}`))

	for _, tc := range []struct {
		name   string
		policy FileRedaction
	}{
		{name: "relative", policy: FileRedaction{Path: "openclaw.json", Kind: FileRedactionOpenClawConfig}},
		{name: "outside root", policy: FileRedaction{Path: filepath.Join(t.TempDir(), "openclaw.json"), Kind: FileRedactionOpenClawConfig}},
		{name: "unsupported kind", policy: FileRedaction{Path: configPath, Kind: FileRedactionKind("unknown")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dest := filepath.Join(t.TempDir(), "manual")
			_, err := Snapshot([]AgentRoots{{Name: "openclaw", Roots: []string{root}, RedactFiles: []FileRedaction{tc.policy}}}, dest)
			require.Error(t, err)
			_, statErr := os.Stat(dest)
			require.ErrorIs(t, statErr, os.ErrNotExist)
		})
	}
}

func TestRestoreRedactedFilePreservesLiveMachineCredentialsWhileRestoringSettings(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "native", ".openclaw")
	configPath := filepath.Join(root, "openclaw.json")
	backupConfig := []byte(`{
  "channels": {"telegram": {"enabled": false, "botToken": "backup-channel-secret"}},
  "models": {"primary": "openai/backup-model", "apiKey": "backup-model-secret"},
  "mcp": {"servers": {"git": {"command": "backup-command", "env": {"GITHUB_TOKEN": "backup-env-secret", "PORT": 3}}}},
  "gateway": {"port": 18000, "auth": {"mode": "token", "token": "backup-gateway-secret"}}
}`)
	writeRedactionTestFile(t, configPath, backupConfig)
	backupDir := filepath.Join(base, "backups", "manual-old-unredacted")
	_, err := SnapshotAuthenticated([]AgentRoots{{Name: "openclaw", Roots: []string{root}}}, backupDir)
	require.NoError(t, err, "the fixture models an authenticated snapshot from before typed redaction")

	liveConfig := []byte(`{
  "channels": {
    "telegram": {"enabled": true, "botToken": "current-channel-secret"},
    "discord": {"enabled": true, "token": "current-discord-secret"}
  },
  "models": {"primary": "openai/live-model", "apiKey": "current-model-secret"},
  "mcp": {"servers": {"git": {"command": "live-command", "env": {"GITHUB_TOKEN": "current-env-secret", "PORT": 9, "LIVE_ONLY": "current-only"}}}},
  "gateway": {"port": 19000, "auth": {"mode": "token", "token": "current-gateway-secret"}},
  "liveOnlyNonSecret": {"note": "newer setting may be rolled back"}
}`)
	require.NoError(t, os.WriteFile(configPath, liveConfig, 0o600))
	policy := AgentRoots{
		Name:        "openclaw",
		Roots:       []string{root},
		RedactFiles: []FileRedaction{{Path: configPath, Kind: FileRedactionOpenClawConfig}},
	}
	result, err := RestoreWithOptions(context.Background(), backupDir, NativeRestoreOptions{
		Agent:             "openclaw",
		CurrentAgentRoots: []AgentRoots{policy},
		Coordinator: LocalRestoreCoordinator{
			LockPath: filepath.Join(base, "state", "native-restore.lock"),
		},
	})
	require.NoError(t, err)
	require.Len(t, result.Files, 1)

	restored, err := os.ReadFile(configPath)
	require.NoError(t, err)
	require.NotContains(t, string(restored), "backup-channel-secret")
	require.NotContains(t, string(restored), "backup-model-secret")
	require.NotContains(t, string(restored), "backup-env-secret")
	require.NotContains(t, string(restored), "backup-gateway-secret")
	require.JSONEq(t, `{
  "channels": {
    "telegram": {"enabled": false, "botToken": "current-channel-secret"},
    "discord": {"enabled": true, "token": "current-discord-secret"}
  },
  "models": {"primary": "openai/backup-model", "apiKey": "current-model-secret"},
  "mcp": {"servers": {"git": {"command": "backup-command", "env": {"GITHUB_TOKEN": "current-env-secret", "PORT": 3, "LIVE_ONLY": "current-only"}}}},
  "gateway": {"port": 18000, "auth": {"mode": "token", "token": "current-gateway-secret"}}
}`, string(restored))

	preRestoreConfig := filepath.Join(result.PreRestoreDir, "openclaw", relativize(root), "openclaw.json")
	preRestoreBytes, err := os.ReadFile(preRestoreConfig)
	require.NoError(t, err)
	for _, secret := range []string{
		"current-channel-secret", "current-discord-secret", "current-model-secret", "current-env-secret", "current-gateway-secret", "current-only",
	} {
		require.NotContains(t, string(preRestoreBytes), secret,
			"the reversible pre-restore snapshot must not duplicate current machine credentials")
	}
}

func TestMergeRedactedOpenClawConfigPreservesWholeLiveCredentialArrayWhenReordered(t *testing.T) {
	backup := []byte(`{
  "accounts": [
    {"name": "alpha", "model": "backup-alpha", "token": ""},
    {"name": "beta", "model": "backup-beta", "token": ""}
  ],
  "ordinary": ["restore", "this"]
}`)
	live := []byte(`{
  "accounts": [
    {"name": "beta", "model": "live-beta", "token": "beta-current-secret"},
    {"name": "alpha", "model": "live-alpha", "token": "alpha-current-secret"}
  ],
  "ordinary": ["current", "value"]
}`)

	merged, err := mergeRedactedBackupFile(FileRedactionOpenClawConfig, backup, live)
	require.NoError(t, err)
	require.JSONEq(t, `{
  "accounts": [
    {"name": "beta", "model": "live-beta", "token": "beta-current-secret"},
    {"name": "alpha", "model": "live-alpha", "token": "alpha-current-secret"}
  ],
  "ordinary": ["restore", "this"]
}`, string(merged), "credential-bearing arrays must preserve live identity/order, while ordinary arrays restore normally")
}
