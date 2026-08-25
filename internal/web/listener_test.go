package web

import (
	"net"
	"strings"
	"testing"
)

// TestNewListenerRefusesNonLoopback enforces the V1 invariant: only
// loopback binds are accepted. LAN binds (V2 territory: requires mDNS,
// passkey auth, automated cert provisioning) return a clear error
// referencing "loopback" so users see a hint rather than a generic
// "invalid address" failure.
func TestNewListenerRefusesNonLoopback(t *testing.T) {
	for _, bind := range []string{"0.0.0.0", "192.168.1.1", "::", "10.0.0.1", "8.8.8.8"} {
		v4, v6, err := NewListener(bind, 0)
		if err == nil {
			if v4 != nil {
				v4.Close()
			}
			if v6 != nil {
				v6.Close()
			}
			t.Errorf("NewListener(%q) returned nil error; should refuse non-loopback", bind)
			continue
		}
		if !strings.Contains(err.Error(), "loopback") {
			t.Errorf("NewListener(%q) error = %v; want one mentioning 'loopback'", bind, err)
		}
		if v4 != nil || v6 != nil {
			t.Errorf("NewListener(%q) returned a listener on error path; should be nil", bind)
			if v4 != nil {
				v4.Close()
			}
			if v6 != nil {
				v6.Close()
			}
		}
	}
}

// TestNewListenerBindsV4Loopback confirms 127.0.0.1 always works on
// platforms with an IPv4 loopback (i.e., every supported platform).
func TestNewListenerBindsV4Loopback(t *testing.T) {
	v4, v6, err := NewListener("127.0.0.1", 0)
	if err != nil {
		t.Fatalf("NewListener(127.0.0.1, 0): %v", err)
	}
	defer v4.Close()
	if v6 != nil {
		defer v6.Close()
	}
	if v4 == nil {
		t.Fatal("v4 listener is nil for bind=127.0.0.1")
	}
	port := v4.Addr().(*net.TCPAddr).Port
	if port < 1 || port > 65535 {
		t.Errorf("port = %d out of range", port)
	}
}

// TestNewListenerBindsV6Loopback confirms ::1 works on platforms with
// IPv6 loopback available (skipped in IPv4-only CI environments).
func TestNewListenerBindsV6Loopback(t *testing.T) {
	// Probe whether ::1 is available before asserting it must work.
	probe, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skipf("::1 loopback not available on this host: %v", err)
	}
	probe.Close()

	v4, v6, err := NewListener("::1", 0)
	if err != nil {
		t.Fatalf("NewListener(::1, 0): %v", err)
	}
	if v4 != nil {
		defer v4.Close()
	}
	defer v6.Close()
	if v6 == nil {
		t.Fatal("v6 listener is nil for bind=::1")
	}
	port := v6.Addr().(*net.TCPAddr).Port
	if port < 1 || port > 65535 {
		t.Errorf("port = %d out of range", port)
	}
}

// TestNewListenerSharesPortAcrossV4V6 confirms that when both listeners
// are created, they bind to the same port. This is the contract
// portinfo.json relies on — one Port field, not two.
func TestNewListenerSharesPortAcrossV4V6(t *testing.T) {
	v4, v6, err := NewListener("127.0.0.1", 0)
	if err != nil {
		t.Fatalf("NewListener: %v", err)
	}
	defer v4.Close()
	if v6 == nil {
		t.Skip("v6 loopback not available on this host; cannot test shared-port invariant")
	}
	defer v6.Close()
	p4 := v4.Addr().(*net.TCPAddr).Port
	p6 := v6.Addr().(*net.TCPAddr).Port
	if p4 != p6 {
		t.Errorf("v4 port %d != v6 port %d; they must share a port", p4, p6)
	}
}

// TestNewListenerHonorsExplicitPort confirms a caller-specified port is
// used. Uses port 0 first to find an ephemeral port, then closes and
// retries with that port to avoid hard-coding a port that might be in
// use on the test host.
func TestNewListenerHonorsExplicitPort(t *testing.T) {
	scout, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("scout listen: %v", err)
	}
	wantPort := scout.Addr().(*net.TCPAddr).Port
	scout.Close()

	v4, v6, err := NewListener("127.0.0.1", wantPort)
	if err != nil {
		t.Fatalf("NewListener(127.0.0.1, %d): %v", wantPort, err)
	}
	defer v4.Close()
	if v6 != nil {
		defer v6.Close()
	}
	if got := v4.Addr().(*net.TCPAddr).Port; got != wantPort {
		t.Errorf("v4 port = %d, want %d", got, wantPort)
	}
}

// TestNewListenerRefusesInvalidPortRange refuses obviously-invalid port
// values (negative, > 65535) before reaching the OS layer so callers
// get a clear error rather than an inscrutable syscall failure.
func TestNewListenerRefusesInvalidPort(t *testing.T) {
	for _, port := range []int{-1, -100, 65536, 100000} {
		v4, v6, err := NewListener("127.0.0.1", port)
		if err == nil {
			if v4 != nil {
				v4.Close()
			}
			if v6 != nil {
				v6.Close()
			}
			t.Errorf("NewListener(127.0.0.1, %d) returned nil error; should refuse out-of-range port", port)
		}
	}
}
