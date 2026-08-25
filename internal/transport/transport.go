// Package transport carries the type definitions for the
// /api/transport surface (spec §6.8). The OSS V1 daemon supports
// exactly one transport — "local" (filesystem watchers + adapter
// callbacks; no network). The endpoint surface exists so a remote-transport
// plugin can add MQTT-via-EMQX and BYO relay support
// without changing this package's contract.
//
// V1 OSS behavior:
//   - GetTransport() returns Info{Mode: "local", Available: ["local"]}
//   - SetTransport("local") is a no-op success
//   - SetTransport(<any other value>) returns ErrModeUnsupported
//   - SetBYORelay() returns ErrBYONotInOSS (Cloud-only feature)
package transport

import "errors"

// Mode names the transport family.
//
//	ModeLocal    — filesystem watchers + adapter callbacks (V1 OSS default)
//	ModeBYORelay — self-hosted EMQX endpoint when a supporting plugin is loaded
//	ModeHosted   — Aplexica-hosted relay when a supporting plugin is loaded
type Mode string

const (
	ModeLocal    Mode = "local"
	ModeBYORelay Mode = "byo-relay"
	ModeHosted   Mode = "hosted"
)

// Info is the wire shape returned by GET /api/transport. Mirrors
// spec §6.8.
type Info struct {
	// Mode is the currently active transport.
	Mode Mode `json:"mode"`

	// Available enumerates every transport this daemon edition can
	// switch to via PUT /api/transport. On V1 OSS this is just
	// [ModeLocal]; the Cloud plugin extends it.
	Available []Mode `json:"available"`

	// BYO describes the configured BYO relay endpoint when Mode ==
	// ModeBYORelay, nil otherwise. The Cloud plugin populates this;
	// V1 OSS always reports nil.
	BYO *BYORelayOpts `json:"byo,omitempty"`
}

// BYORelayOpts describes a self-hosted EMQX endpoint. Surfaced by POST
// /api/transport/byo on Cloud editions; the V1 OSS endpoint returns
// ErrBYONotInOSS.
type BYORelayOpts struct {
	// URL is the broker endpoint (e.g. "mqtts://emqx.example.com:8883").
	URL string `json:"url"`

	// MTLSCertPath is the absolute path to the client certificate
	// PEM. The daemon reads this lazily on each connect.
	MTLSCertPath string `json:"mtlsCertPath,omitempty"`

	// MTLSKeyPath is the absolute path to the client private key PEM.
	MTLSKeyPath string `json:"mtlsKeyPath,omitempty"`

	// CACertPath is the absolute path to the CA certificate PEM the
	// daemon should pin against the broker's server cert. Empty
	// means the system CA store is used.
	CACertPath string `json:"caCertPath,omitempty"`

	// Namespaces, when non-empty, restricts which namespaces the
	// daemon will publish/subscribe via this relay. Empty = all.
	Namespaces []string `json:"namespaces,omitempty"`
}

// ErrModeUnsupported is returned by SetTransport when the requested
// mode isn't in Info.Available.
var ErrModeUnsupported = errors.New("transport: mode not supported in this edition")

// ErrBYONotInOSS is returned by SetBYORelay on V1 OSS where BYO relay
// is a Cloud-plugin feature.
var ErrBYONotInOSS = errors.New("transport: BYO relay is a Cloud-edition feature")

// LocalOnly is the V1 OSS Info value — used by the daemon's default
// Backend implementation and by tests.
var LocalOnly = Info{
	Mode:      ModeLocal,
	Available: []Mode{ModeLocal},
}
