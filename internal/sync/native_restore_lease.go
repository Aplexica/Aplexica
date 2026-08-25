package syncd

import (
	"context"
	"fmt"
	"time"
)

type NativeRestoreGateLease struct {
	o          *Orchestrator
	generation uint64
	closed     bool
}

func (o *Orchestrator) AcquireNativeRestore(ctx context.Context) (*NativeRestoreGateLease, error) {
	if o == nil {
		return nil, fmt.Errorf("syncd: nil orchestrator")
	}
	for !o.nativeRestoreGate.TryLock() {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
	g := o.nativeRestoreGeneration.Add(1)
	return &NativeRestoreGateLease{o: o, generation: g}, nil
}
func (l *NativeRestoreGateLease) CheckCurrent() error {
	if l == nil || l.closed || l.o == nil || l.o.nativeRestoreGeneration.Load() != l.generation {
		return fmt.Errorf("syncd: native restore lease stale")
	}
	return nil
}
func (l *NativeRestoreGateLease) Close() error {
	if l == nil || l.closed {
		return nil
	}
	l.closed = true
	l.o.nativeRestoreGate.Unlock()
	return nil
}
