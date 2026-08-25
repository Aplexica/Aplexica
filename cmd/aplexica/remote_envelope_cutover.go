package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/aplexica/aplexica/internal/daemon"
)

// remoteEnvelopeV2CutoverRequired implements the account migration gate.
//
// A pre-existing account that has never established a signed roster remains on
// the encrypted v1 overlap protocol. The first durable identity/security-epoch
// artifact closes that overlap permanently: a partial, corrupt, or stale v2
// state must fail closed instead of silently downgrading after restart.
func remoteEnvelopeV2CutoverRequired(ctx context.Context, identityRoot string, provider *daemon.VerifiedRosterProvider) bool {
	if provider != nil {
		if _, err := provider.Current(ctx, "account", ""); err == nil {
			return true
		}
	}

	entries, err := os.ReadDir(filepath.Join(identityRoot, "account"))
	if err == nil {
		return len(entries) != 0
	}
	// A genuinely new account may not have an account directory yet. Every
	// other filesystem failure is ambiguous and therefore stays fail-closed.
	if !errors.Is(err, os.ErrNotExist) {
		return true
	}
	info, rootErr := os.Lstat(identityRoot)
	switch {
	case rootErr == nil:
		return !info.IsDir()
	case errors.Is(rootErr, os.ErrNotExist):
		return false
	default:
		return true
	}
}
