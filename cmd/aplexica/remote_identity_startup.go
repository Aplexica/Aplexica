package main

import (
	"context"
	"fmt"

	"github.com/aplexica/aplexica/internal/devicetransition"
	"github.com/aplexica/aplexica/internal/identity"
	"github.com/aplexica/aplexica/internal/securityepoch"
)

// recoveredRemoteIdentity is created only after the existing-account genesis
// journal has either been proven absent or replayed completely. Keeping the
// shared coordinator on this token makes startup ordering explicit: callers
// cannot obtain the coordinator used by publisher/admission before recovery.
type recoveredRemoteIdentity struct {
	coordinator *securityepoch.Coordinator
}

func recoverRemoteIdentityStartup(ctx context.Context, identityRoot string) (*recoveredRemoteIdentity, bool, error) {
	coordinator := &securityepoch.Coordinator{Root: identityRoot}
	installer := &identity.ExistingAccountGenesisInstaller{IdentityRoot: identityRoot, Coordinator: coordinator}
	_, recovered, err := installer.Recover(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("recover existing-account identity transition: %w", err)
	}
	// Namespace device/rekey journals need the verified remote plugin to
	// complete their barrier phases, but their bytes and path binding must be
	// authenticated before any plugin process is allowed to execute.
	if _, err := devicetransition.ValidatePending(identityRoot); err != nil {
		return nil, false, fmt.Errorf("validate pending namespace device transition: %w", err)
	}
	return &recoveredRemoteIdentity{coordinator: coordinator}, recovered, nil
}
