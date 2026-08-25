package proxy

import (
	"context"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/plugin/host"
	"github.com/aplexica/aplexica/internal/plugin/proto"
)

// rbacStub extends the base remoteStub with the client-side RBAC surface: it
// canned-returns a role for a namespace and records the namespace asked.
type rbacStub struct {
	*remoteStub
	role      proto.RemoteGetNamespaceRoleResult
	roleErr   error
	askedNS   string
	askedCall bool
}

func (s *rbacStub) GetNamespaceRole(_ context.Context, p proto.RemoteGetNamespaceRoleParams) (proto.RemoteGetNamespaceRoleResult, error) {
	s.krMu()
	s.askedNS = p.NamespaceID
	s.askedCall = true
	s.krUnlock()
	return s.role, s.roleErr
}

// krMu/krUnlock guard the recorded fields against the read pump goroutine.
func (s *rbacStub) krMu()     { s.remoteStub.mu.Lock() }
func (s *rbacStub) krUnlock() { s.remoteStub.mu.Unlock() }

// End-to-end through the real proxy<->host pair over an in-memory pipe (real
// transport, fake handler): the proxy's GetNamespaceRole returns the role the
// host handler resolved.
func TestRemoteProxy_GetNamespaceRole_RoundTrip(t *testing.T) {
	proxySide, hostSide := newPipePair()
	stub := &rbacStub{
		remoteStub: &remoteStub{},
		role:       proto.RemoteGetNamespaceRoleResult{Role: "editor", Found: true},
	}

	hostDone := make(chan error, 1)
	go func() { hostDone <- host.ServeRemote(context.Background(), stub, hostSide, hostSide) }()

	rp, err := OpenRemote(context.Background(), proxySide, "dev-a", "v0.0.0-test")
	if err != nil {
		t.Fatalf("OpenRemote: %v", err)
	}

	res, err := rp.GetNamespaceRole(context.Background(), proto.RemoteGetNamespaceRoleParams{NamespaceID: "ns-7"})
	if err != nil {
		t.Fatalf("GetNamespaceRole: %v", err)
	}
	if res.Role != "editor" || !res.Found {
		t.Fatalf("result = %+v, want role=editor found=true", res)
	}

	stub.remoteStub.mu.Lock()
	asked := stub.askedNS
	called := stub.askedCall
	stub.remoteStub.mu.Unlock()
	if !called || asked != "ns-7" {
		t.Errorf("host asked namespace = %q (called=%v), want ns-7", asked, called)
	}

	// A not-a-member result round-trips with Found=false.
	stub.remoteStub.mu.Lock()
	stub.role = proto.RemoteGetNamespaceRoleResult{Found: false}
	stub.remoteStub.mu.Unlock()
	res2, err := rp.GetNamespaceRole(context.Background(), proto.RemoteGetNamespaceRoleParams{NamespaceID: "ns-8"})
	if err != nil {
		t.Fatalf("GetNamespaceRole(2): %v", err)
	}
	if res2.Found || res2.Role != "" {
		t.Errorf("not-found result = %+v", res2)
	}

	if err := rp.Shutdown(context.Background()); err != nil {
		t.Errorf("Shutdown: %v", err)
	}
	select {
	case <-hostDone:
	case <-time.After(2 * time.Second):
		t.Error("host did not return")
	}
}

// A handler with no team concept (BaseRemoteHandler default) surfaces a clear
// "unsupported" error rather than a silent empty role.
func TestRemoteProxy_GetNamespaceRole_UnsupportedDefault(t *testing.T) {
	proxySide, hostSide := newPipePair()
	stub := &remoteStub{} // embeds BaseRemoteHandler -> default GetNamespaceRole

	hostDone := make(chan error, 1)
	go func() { hostDone <- host.ServeRemote(context.Background(), stub, hostSide, hostSide) }()

	rp, err := OpenRemote(context.Background(), proxySide, "dev-a", "v0.0.0-test")
	if err != nil {
		t.Fatalf("OpenRemote: %v", err)
	}

	if _, err := rp.GetNamespaceRole(context.Background(), proto.RemoteGetNamespaceRoleParams{NamespaceID: "ns-1"}); err == nil {
		t.Error("expected an error from the unsupported default")
	}

	_ = rp.Shutdown(context.Background())
	select {
	case <-hostDone:
	case <-time.After(2 * time.Second):
		t.Error("host did not return")
	}
}
