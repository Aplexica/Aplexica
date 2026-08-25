package main

import (
	"encoding/base64"
	"strings"
	"testing"
)

// The daemon owns the device X25519 wrap keypair; wrapPubKeyEnv emits the
// PUBLIC half (base64) for the plugin to forward at pairing. The private half
// never leaves the secrets store. The key is stable across calls (persisted).
func TestRemoteWebAccessor_WrapPubKeyEnv(t *testing.T) {
	acc := &remoteWebAccessor{deps: &webAPIDeps{secretsRoot: t.TempDir()}}

	env := acc.wrapPubKeyEnv()
	const prefix = "APLEXICA_WRAP_PUBKEY="
	if !strings.HasPrefix(env, prefix) {
		t.Fatalf("env = %q, want %s<base64>", env, prefix)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(env, prefix))
	if err != nil {
		t.Fatalf("decode pubkey: %v", err)
	}
	if len(raw) != 32 {
		t.Fatalf("wrap pubkey len = %d, want 32", len(raw))
	}
	if again := acc.wrapPubKeyEnv(); again != env {
		t.Error("wrapPubKeyEnv must be stable across calls (persisted keypair)")
	}
}

func TestRemoteWebAccessor_WrapPubKeyEnv_EmptyWhenNoSecretsRoot(t *testing.T) {
	acc := &remoteWebAccessor{deps: &webAPIDeps{}}
	if got := acc.wrapPubKeyEnv(); got != "" {
		t.Errorf("no secrets root should yield empty env, got %q", got)
	}
}
