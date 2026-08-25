package daemon

import (
	"context"
	"errors"
	"testing"

	"github.com/aplexica/aplexica/internal/plugin/proto"
)

// With no live proxy, GetNamespaceRole must report the transient
// reconnecting signal (same contract as Publish/Fetch/the key-rotation
// wrappers) so the caller retries rather than crashing.
func TestRemoteRunner_GetNamespaceRole_ReconnectingWhenNilProxy(t *testing.T) {
	r := &RemoteRunner{}
	if _, err := r.GetNamespaceRole(context.Background(), proto.RemoteGetNamespaceRoleParams{NamespaceID: "ns-1"}); !errors.Is(err, ErrRemoteReconnecting) {
		t.Errorf("GetNamespaceRole err = %v, want ErrRemoteReconnecting", err)
	}
}

// The RemoteRunner must satisfy the RBAC transport's caller slice.
var _ rbacRoleCaller = (*RemoteRunner)(nil)
