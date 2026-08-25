package daemon

import (
	"context"
	"errors"
	"testing"

	"github.com/aplexica/aplexica/internal/plugin/proto"
)

// With no live proxy, the key-rotation delegating wrappers must report the
// transient reconnecting signal (same contract as Publish/Fetch/etc.) so
// the rotator retries rather than crashing.
func TestRemoteRunner_KeyRotationWrappers_ReconnectingWhenNoProxy(t *testing.T) {
	r := &RemoteRunner{}
	ctx := context.Background()

	if _, err := r.ListNamespaceDevices(ctx, "ns-1"); !errors.Is(err, ErrRemoteReconnecting) {
		t.Errorf("ListNamespaceDevices err = %v, want ErrRemoteReconnecting", err)
	}
	if _, err := r.PutNamespaceKey(ctx, proto.RemotePutNamespaceKeyParams{NamespaceID: "ns-1", KeyVersion: 1}); !errors.Is(err, ErrRemoteReconnecting) {
		t.Errorf("PutNamespaceKey err = %v, want ErrRemoteReconnecting", err)
	}
	if _, err := r.GetNamespaceKey(ctx, proto.RemoteGetNamespaceKeyParams{NamespaceID: "ns-1", KeyVersion: 1}); !errors.Is(err, ErrRemoteReconnecting) {
		t.Errorf("GetNamespaceKey err = %v, want ErrRemoteReconnecting", err)
	}
	if err := r.BroadcastNamespaceKey(ctx, proto.RemoteBroadcastNamespaceKeyParams{NamespaceID: "ns-1", KeyVersion: 1}); !errors.Is(err, ErrRemoteReconnecting) {
		t.Errorf("BroadcastNamespaceKey err = %v, want ErrRemoteReconnecting", err)
	}
}
