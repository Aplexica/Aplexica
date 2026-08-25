package main

import (
	"context"

	"github.com/aplexica/aplexica/internal/nativebackup"
	syncd "github.com/aplexica/aplexica/internal/sync"
)

type orchestratorNativeRestoreCoordinator struct{ orch *syncd.Orchestrator }

func (c orchestratorNativeRestoreCoordinator) AcquireRestoreLease(ctx context.Context, _ []nativebackup.NativeTarget) (nativebackup.NativeRestoreLease, error) {
	return c.orch.AcquireNativeRestore(ctx)
}
