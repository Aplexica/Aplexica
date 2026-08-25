// Package web hosts the loopback HTTP listener and SPA-serving stack
// for the OSS daemon's local web UI.
//
// Subpackages:
//   - middleware/ — Host-allowlist, CSP, CSRF, session enforcement (W2.4-W2.5, W3.4)
//   - auth/       — bootstrap-token + session store + /api/auth/* handlers (W3)
//   - api/        — REST handlers under /api/* (W4)
//   - sse/        — Server-Sent Events stream at /events (W5)
//   - embed/      — go:embed of the dist-local SPA bundle (W8)
//
// V1 listener is loopback-only (127.0.0.1 + ::1). LAN access is
// explicitly out of V1 scope and deferred to V2 with mDNS + passkey +
// automated cert provisioning.
package web

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/aplexica/aplexica/internal/atomicfile"
)

// portInfoFileMode is the POSIX file mode for portinfo.json. Owner-only
// (0600) prevents other local users on multi-user systems from reading
// the daemon's listener port — a defense-in-depth measure layered on
// top of the existing UDS socket permissions for the daemon's RPC
// surface. On Windows, the underlying file ACLs derive from the
// %USERPROFILE% directory's defaults and the mode bits are advisory.
const portInfoFileMode os.FileMode = 0o600

// PortInfo is the on-disk metadata written by the daemon at startup so
// tooling can discover the local-web-UI listener's bind address +
// chosen port without polling daemon stdout.
//
// File path convention: <state-dir>/portinfo.json. Mode 0600 on POSIX.
// Re-written on every daemon start; never edited at runtime.
//
// Readers: cmd/aplexica/cmd_web_port.go (`aplexica web port`),
// cmd/aplexica/cmd_web_open.go (`aplexica web open`), cmd/aplexicatray
// (the tray's Open-Aplexica menu item).
type PortInfo struct {
	InstanceID string `json:"instance_id"`
	Origin     string `json:"origin"`
	// Port is the TCP port the daemon's HTTP listener bound to. Always
	// > 0 once written.
	Port int `json:"port"`

	// Bind is the loopback address ("127.0.0.1" or "::1"). Mirrors the
	// effective cfg.Web.Bind after WebBind's default-fallback applies.
	Bind string `json:"bind"`

	// StartedAt is the UTC timestamp the daemon's web server bound the
	// listener. Caller may pre-set this; if zero on write, WritePortInfo
	// stamps with time.Now().UTC().
	StartedAt time.Time `json:"started_at"`

	// Version is the daemon binary's version string at startup. Used by
	// tooling to detect daemon/portal version mismatches at upgrade
	// time. Mirrors daemon.Version().
	Version string `json:"version"`
}

// WritePortInfo serializes info as indented JSON and writes it
// atomically to path with portInfoFileMode permissions. If
// info.StartedAt is the zero value, the writer stamps it with
// time.Now().UTC() before serializing.
//
// The write goes through atomicfile so a daemon crash mid-write cannot
// leave the file partially-written or corrupted — a torn portinfo.json
// would manifest as "tray can't find the listener" at restart, which
// would surprise users worse than a clean fallback.
func WritePortInfo(path string, info PortInfo) error {
	if info.StartedAt.IsZero() {
		info.StartedAt = time.Now().UTC()
	}
	data, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return fmt.Errorf("web: marshal portinfo: %w", err)
	}
	if err := atomicfile.WriteFile(path, data, portInfoFileMode); err != nil {
		return fmt.Errorf("web: write portinfo to %s: %w", path, err)
	}
	return nil
}

// ReadPortInfo loads and parses portinfo.json from path. Returns a
// distinguishable error for the "file does not exist" case so callers
// (`aplexica web port` etc.) can render a helpful "daemon not running"
// message instead of a generic I/O error.
func ReadPortInfo(path string) (PortInfo, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return PortInfo{}, fmt.Errorf("web: read portinfo from %s: %w", path, err)
	}
	var info PortInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return PortInfo{}, fmt.Errorf("web: parse portinfo at %s: %w", path, err)
	}
	return info, nil
}
