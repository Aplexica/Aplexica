// SPDX-License-Identifier: AGPL-3.0-or-later
package adapterplugin

import (
	"context"
	"io"
	"os"

	"github.com/aplexica/aplexica/internal/plugin/host"
	"github.com/aplexica/aplexica/internal/plugin/proto"
)

// Handler is the interface a plugin author implements. Each method maps to
// one JSON-RPC method on the wire (initialize / import / export /
// native_path / handles_format / capabilities / shutdown). Methods may
// return a *RPCError to surface a specific error code; any other error is
// reported to the daemon as CodeInternal.
//
// This is a type alias for the internal host.Handler so third-party authors
// import only this stable, non-internal path.
type Handler = host.Handler

// BaseCapabilitiesHandler is an embeddable struct that provides a
// conservative default Capabilities response (all artifact kinds claimed,
// no tool kinds). Embed it in your Handler implementation so you only need
// to override Capabilities when you want to advertise a narrower surface.
//
// IMPORTANT: set its PluginName field. The default Capabilities response
// reports Name = PluginName, and per the plugin protocol (§4.6) the
// capabilities `name` MUST match plugin_name; embedding it without setting
// PluginName yields an empty capabilities name. For example:
//
//	Handler{ BaseCapabilitiesHandler: adapterplugin.BaseCapabilitiesHandler{PluginName: "my-plugin"} }
type BaseCapabilitiesHandler = host.BaseCapabilitiesHandler

// Serve reads JSON-RPC requests from r, dispatches each to h, and writes
// responses to w. It returns nil on a clean EOF (the daemon closed the
// transport) or after handling a successful Shutdown; it returns a non-nil
// error only for an unrecoverable transport failure.
//
// Most plugins call ServeStdio instead, which wires r/w to stdin/stdout.
func Serve(ctx context.Context, h Handler, r io.Reader, w io.Writer) error {
	return host.Serve(ctx, h, r, w)
}

// ServeStdio is the convenience entry point for a plugin's main: it runs
// Serve over os.Stdin / os.Stdout, which is exactly the transport the
// daemon wires up when it spawns the plugin subprocess.
//
// IMPORTANT: a plugin MUST write protocol frames to stdout ONLY. Any
// diagnostic logging must go to stderr (the daemon routes stderr to its
// own log). Writing logs to stdout corrupts the JSON-RPC frame stream and
// the daemon will drop the plugin.
func ServeStdio(ctx context.Context, h Handler) error {
	return host.Serve(ctx, h, os.Stdin, os.Stdout)
}

// ABIVersion is the plugin protocol ABI this SDK builds against. A plugin
// MUST echo this exact value from Initialize and declare it in its
// plugin.json; the daemon refuses any plugin whose abi_version does not
// match.
const ABIVersion = proto.ABIVersion

// Plugin protocol error codes. Return one of these wrapped in an *RPCError
// from a Handler method to control how the daemon (and the sync
// orchestrator) react.
const (
	// CodeUnrecognizedNativeFile signals that the plugin does not claim the
	// file passed to Import; the orchestrator falls through to the next
	// adapter. Return this — not a generic error — when a native path is
	// simply not yours.
	CodeUnrecognizedNativeFile = proto.CodeUnrecognizedNativeFile
	// CodeParseErrorPlugin signals the file IS this plugin's but is
	// malformed and could not be parsed.
	CodeParseErrorPlugin = proto.CodeParseErrorPlugin
	// CodeFormatUnsupported signals Export cannot render this payload even
	// though HandlesFormat said yes.
	CodeFormatUnsupported = proto.CodeFormatUnsupported
	// CodeIOError signals the plugin could not read or write a path.
	CodeIOError = proto.CodeIOError
	// CodeSecretExtractionFailed signals a tool import could not extract a
	// declared secret.
	CodeSecretExtractionFailed = proto.CodeSecretExtractionFailed
	// CodeInternal is the catch-all for plugin bugs.
	CodeInternal = proto.CodeInternal
)

// RPCError is the error type a Handler returns to surface a specific
// protocol error code. Construct it with one of the Code* constants:
//
//	return ImportResult{}, &adapterplugin.RPCError{
//	    Code:    adapterplugin.CodeUnrecognizedNativeFile,
//	    Message: "not my file",
//	}
type RPCError = proto.RPCError

// Request/result type aliases for every proto message a plugin author
// touches. Aliasing (not re-declaring) keeps the wire types identical to
// what the daemon expects.
type (
	// InitializeParams is sent by the daemon as the first call after spawn.
	InitializeParams = proto.InitializeParams
	// InitializeResult is the plugin's identity + declared kinds/formats.
	InitializeResult = proto.InitializeResult

	// ImportParams asks the plugin to translate a native file.
	ImportParams = proto.ImportParams
	// ImportResult is the PURE-TRANSLATOR output: a description of what was
	// parsed. The plugin never writes a store — the daemon reconciles and
	// persists every ImportedItem.
	ImportResult = proto.ImportResult
	// ImportedItem describes one artifact's parsed content.
	ImportedItem = proto.ImportedItem
	// NamedSecret is one secret the plugin extracted from a tool config.
	NamedSecret = proto.NamedSecret

	// ExportParams asks the plugin to render a payload to dest_path.
	ExportParams = proto.ExportParams
	// ExportResult confirms the file was written.
	ExportResult = proto.ExportResult

	// NativePathParams asks where an artifact lives natively.
	NativePathParams = proto.NativePathParams
	// NativePathResult is the computed native path + whether it is supported.
	NativePathResult = proto.NativePathResult

	// HandlesFormatParams asks whether the plugin handles a (kind, format).
	HandlesFormatParams = proto.HandlesFormatParams
	// HandlesFormatResult is the yes/no answer.
	HandlesFormatResult = proto.HandlesFormatResult

	// CapabilitiesParams is empty; the daemon calls capabilities to learn
	// the plugin's surface.
	CapabilitiesParams = proto.CapabilitiesParams
	// CapabilitiesResult mirrors adapter.Capabilities.
	CapabilitiesResult = proto.CapabilitiesResult
	// ArtifactSupport flags which ACF kinds the plugin can produce.
	ArtifactSupport = proto.ArtifactSupport

	// ShutdownParams / ShutdownResult are both empty.
	ShutdownParams = proto.ShutdownParams
	ShutdownResult = proto.ShutdownResult
)
