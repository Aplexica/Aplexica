package proto

import (
	"fmt"

	"github.com/aplexica/aplexica/internal/acf"
)

// Manifest is the on-disk plugin.json read by the daemon BEFORE spawning
// the plugin process. The daemon also receives the equivalent of this
// (minus install-time fields) in the initialize response and verifies
// they agree.
//
// Plugin kind:
//
// The `kind` field distinguishes adapter plugins from remote-transport
// plugins. When absent or empty, the loader treats the plugin as an adapter
// for backward compatibility with older plugin.json files. Set kind="remote" to enable the remote
// JSON-RPC surface declared in remote_messages.go.
//
// Adapter plugins MUST populate the Kinds array (which ACF kinds they
// translate). Remote plugins MUST leave Kinds empty — the remote ABI
// is kind-agnostic by design (the daemon ships canonical bytes, not
// per-kind payloads). The Validate method enforces this invariant.
type Manifest struct {
	ManifestVersion int    `json:"manifest_version"`
	Name            string `json:"name"`
	Version         string `json:"version"`
	ABIVersion      string `json:"abi_version"`
	Executable      string `json:"executable"`

	// PluginKind selects the protocol surface: "" (default) and
	// "adapter" both use the adapter method set; "remote" uses the
	// remote method set declared in remote_messages.go.
	PluginKind string `json:"kind,omitempty"`

	Kinds        []acf.Kind            `json:"kinds,omitempty"`
	Formats      map[acf.Kind][]string `json:"formats,omitempty"`
	Homepage     string                `json:"homepage,omitempty"`
	Author       string                `json:"author,omitempty"`
	License      string                `json:"license,omitempty"`
	Capabilities []string              `json:"capabilities,omitempty"`
}

// IsRemote reports whether this manifest declares a remote-transport
// plugin (PluginKind=="remote"). Used by the daemon's plugin loader
// to pick which JSON-RPC dispatch table to wire up.
func (m Manifest) IsRemote() bool {
	return m.PluginKind == RemoteKind
}

// IsAdapter reports whether this manifest declares an adapter plugin.
// Treats the empty string as "adapter" so older plugin.json files still
// load without modification.
func (m Manifest) IsAdapter() bool {
	return m.PluginKind == "" || m.PluginKind == "adapter"
}

// Validate returns nil if the manifest is well-formed. The daemon
// refuses plugins whose manifest fails Validate.
func (m Manifest) Validate() error {
	if m.ManifestVersion != 1 {
		return fmt.Errorf("plugin/proto: manifest_version must be 1, got %d", m.ManifestVersion)
	}
	if m.Name == "" {
		return fmt.Errorf("plugin/proto: manifest.name is required")
	}
	if m.Version == "" {
		return fmt.Errorf("plugin/proto: manifest.version is required")
	}
	if m.ABIVersion != ABIVersion {
		return fmt.Errorf("plugin/proto: manifest.abi_version = %q, daemon supports %q", m.ABIVersion, ABIVersion)
	}
	if m.Executable == "" {
		return fmt.Errorf("plugin/proto: manifest.executable is required")
	}
	// Per-kind enforcement: adapter plugins must declare at least one
	// ACF kind they translate; remote plugins are kind-agnostic and
	// must leave the field empty.
	switch {
	case m.IsRemote():
		if len(m.Kinds) != 0 {
			return fmt.Errorf("plugin/proto: remote plugins must NOT declare kinds (got %d)", len(m.Kinds))
		}
	case m.IsAdapter():
		if len(m.Kinds) == 0 {
			return fmt.Errorf("plugin/proto: adapter plugin manifest.kinds must list at least one kind")
		}
	default:
		return fmt.Errorf("plugin/proto: manifest.kind=%q is not supported (use %q or %q or omit)",
			m.PluginKind, "adapter", RemoteKind)
	}
	return nil
}
