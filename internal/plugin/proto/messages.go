package proto

import (
	"encoding/json"

	"github.com/aplexica/aplexica/internal/acf"
)

// Method names (single source of truth — used by host dispatch and
// proxy callsites).
const (
	MethodInitialize    = "initialize"
	MethodImport        = "import"
	MethodExport        = "export"
	MethodNativePath    = "native_path"
	MethodHandlesFormat = "handles_format"
	MethodCapabilities  = "capabilities"
	MethodShutdown      = "shutdown"
)

// InitializeParams — sent by the daemon as the first call after spawn.
type InitializeParams struct {
	ABIVersion    string `json:"abi_version"`
	DaemonVersion string `json:"daemon_version"`
	DeviceID      string `json:"device_id"`
}

// InitializeResult — plugin replies with its identity and the kinds /
// formats it claims to handle. Daemon verifies abi_version matches.
type InitializeResult struct {
	PluginName    string                `json:"plugin_name"`
	PluginVersion string                `json:"plugin_version"`
	ABIVersion    string                `json:"abi_version"`
	Kinds         []acf.Kind            `json:"kinds"`
	Formats       map[acf.Kind][]string `json:"formats,omitempty"`
}

// ImportParams — daemon asks plugin to import a native file.
type ImportParams struct {
	NativePath string `json:"native_path"`
	ContextDir string `json:"context_dir"`
	CausedBy   string `json:"caused_by,omitempty"`
}

// ImportResult — plugin describes what it imported. The daemon does
// identity reconciliation, mints event IDs, computes parent_hash, and
// writes the artifact + event to the canonical store.
type ImportResult struct {
	Imports []ImportedItem `json:"imports"`
	Secrets []NamedSecret  `json:"secrets,omitempty"`
}

// ImportedItem describes one artifact's content as the plugin parsed
// it from the native file. Payload is the kind-typed payload JSON (e.g.,
// acf.MemoryPayload JSON for KindMemory).
type ImportedItem struct {
	Kind       acf.Kind        `json:"kind"`
	Scope      acf.Scope       `json:"scope"`
	Name       string          `json:"name"`
	SourcePath string          `json:"source_path"`
	Payload    json.RawMessage `json:"payload"`
}

// NamedSecret is one name+value pair the plugin extracted from a tool
// config. Daemon writes these to its secrets store atomically with the
// event append.
type NamedSecret struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// ExportParams — daemon asks plugin to render the current payload to
// dest_path. Daemon has already replayed the event chain; plugin sees
// only the current payload, not the raw events.
type ExportParams struct {
	Artifact acf.Artifact    `json:"artifact"`
	Payload  json.RawMessage `json:"payload"`
	DestPath string          `json:"dest_path"`
}

// ExportResult — plugin confirms the file was written.
type ExportResult struct {
	Written bool `json:"written"`
}

// NativePathParams / Result.
type NativePathParams struct {
	Artifact   acf.Artifact `json:"artifact"`
	ContextDir string       `json:"context_dir"`
}

type NativePathResult struct {
	Path     string `json:"path"`
	Supports bool   `json:"supports"`
}

// HandlesFormatParams / Result.
type HandlesFormatParams struct {
	Kind   acf.Kind `json:"kind"`
	Format string   `json:"format"`
}

type HandlesFormatResult struct {
	Handles bool `json:"handles"`
}

// CapabilitiesParams / Result — v0.94.0 plugin Capabilities RPC.
// Empty params; the result mirrors adapter.Capabilities so the proxy
// can return it as-is from its Capabilities() method.
type CapabilitiesParams struct{}

type CapabilitiesResult struct {
	Name            string              `json:"name"`
	Surfaces        []string            `json:"surfaces,omitempty"`
	Artifacts       ArtifactSupport     `json:"artifacts"`
	Tools           []string            `json:"tools"`
	NativeBasenames []string            `json:"native_basenames"`
	BasenameToKind  map[string]acf.Kind `json:"basename_to_kind,omitempty"`
	NotesURL        string              `json:"notes_url,omitempty"`
}

// ArtifactSupport mirrors adapter.ArtifactSupport (the proto package
// avoids importing internal/adapter to prevent an import cycle —
// proxy adapter consumes both).
type ArtifactSupport struct {
	Memory       bool `json:"memory"`
	Skill        bool `json:"skill"`
	Tool         bool `json:"tool"`
	Conversation bool `json:"conversation"`
}

// ShutdownParams / Result — both empty; defined as named types for
// signature symmetry with the other methods.
type ShutdownParams struct{}
type ShutdownResult struct{}
