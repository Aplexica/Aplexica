package web

import (
	"fmt"
	"net"
)

// portMin / portMax bound the legal TCP port range used by NewListener's
// up-front validation. 0 is the well-known sentinel for "let the OS
// choose an ephemeral port" and is permitted; negative values and values
// above portMax are rejected before reaching the OS so callers get a
// clear, actionable error.
const (
	portMin = 0
	portMax = 65535
)

// NewListener creates the loopback TCP listeners for the local web UI.
// It returns separate v4 and v6 listeners on the same port; LAN binds
// (anything other than 127.0.0.1 or ::1) are refused with an error
// that mentions "loopback" so users get a clear hint about the V1
// scope decision (LAN access deferred to V2 with mDNS + passkey + cert
// provisioning).
//
// port = 0 selects a random ephemeral port. When both listeners are
// created, they share the port chosen by the first bind so
// portinfo.json can record a single port number.
//
// Either v4 or v6 may be nil on success when the host doesn't have
// loopback available for that family — typically v6 is missing on
// IPv4-only CI images. The other listener carries the contract; the
// caller wraps both with http.Server.Serve in goroutines and serves on
// whichever ones are non-nil.
func NewListener(bind string, port int) (v4, v6 net.Listener, err error) {
	if port < portMin || port > portMax {
		return nil, nil, fmt.Errorf("web: port %d out of range [%d, %d]", port, portMin, portMax)
	}
	switch bind {
	case "127.0.0.1":
		v4, err = net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			return nil, nil, fmt.Errorf("web: bind 127.0.0.1:%d: %w", port, err)
		}
		chosen := v4.Addr().(*net.TCPAddr).Port
		// v6 is best-effort: bind ::1 at the same port for browsers
		// that resolve "localhost" to ::1 first. On dual-stack hosts
		// this is a separate socket from the v4 one; on IPv4-only
		// hosts the bind fails and we proceed with v4 alone.
		v6Conn, v6Err := net.Listen("tcp6", fmt.Sprintf("[::1]:%d", chosen))
		if v6Err == nil {
			v6 = v6Conn
		}
		return v4, v6, nil

	case "::1":
		v6, err = net.Listen("tcp6", fmt.Sprintf("[::1]:%d", port))
		if err != nil {
			return nil, nil, fmt.Errorf("web: bind [::1]:%d: %w", port, err)
		}
		chosen := v6.Addr().(*net.TCPAddr).Port
		// Mirror the v6-best-effort treatment in reverse: try the v4
		// loopback at the same port so the listener still answers
		// clients that resolve "localhost" to 127.0.0.1 first.
		v4Conn, v4Err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", chosen))
		if v4Err == nil {
			v4 = v4Conn
		}
		return v4, v6, nil

	default:
		return nil, nil, fmt.Errorf(
			"web: bind %q: only loopback (127.0.0.1 or ::1) is supported in V1; "+
				"LAN access deferred to V2 with mDNS + passkey + cert provisioning",
			bind)
	}
}
