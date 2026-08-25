// SPDX-License-Identifier: AGPL-3.0-or-later
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/plugin/host"
	"github.com/aplexica/aplexica/internal/plugin/proto"
)

// pluginName / pluginVersion are the identity this plugin reports during
// the initialize handshake. They MUST agree with plugin.json (the daemon
// cross-checks the manifest against the initialize result).
const (
	pluginName    = "example-memory"
	pluginVersion = "0.1.0"
	// nativeBasename is the single native file this example translates.
	// Anything else is reported as unrecognized so the orchestrator falls
	// through to the next adapter.
	nativeBasename = "MEMORY.md"
	// memoryFormat is the ACF memory format this plugin round-trips.
	memoryFormat = "markdown"
)

// Handler is a complete, correct PURE-TRANSLATOR adapter plugin for a
// single markdown memory file (MEMORY.md). It never touches a store: the
// daemon owns all canonical IO. Import returns the parsed payload and the
// daemon-side reconciler mints the artifact + event; Export receives the
// replayed payload and writes it back verbatim.
//
// It embeds host.BaseCapabilitiesHandler only for its struct shape; the
// Capabilities method is overridden below to advertise the real (memory-
// only) surface.
type Handler struct {
	host.BaseCapabilitiesHandler
}

// Initialize reports the plugin's identity, the ABI it was built against,
// and the kinds/formats it claims. The daemon refuses the plugin unless
// ABIVersion matches exactly.
func (h *Handler) Initialize(_ context.Context, _ proto.InitializeParams) (proto.InitializeResult, error) {
	return proto.InitializeResult{
		PluginName:    pluginName,
		PluginVersion: pluginVersion,
		ABIVersion:    proto.ABIVersion,
		Kinds:         []acf.Kind{acf.KindMemory},
		Formats:       map[acf.Kind][]string{acf.KindMemory: {memoryFormat}},
	}, nil
}

// Import reads the native MEMORY.md and returns its content as a single
// memory ImportedItem. It performs NO store IO — the daemon-side
// reconciler persists the artifact and appends the event.
//
// If the file is not this plugin's (basename != MEMORY.md) it returns a
// CodeUnrecognizedNativeFile RPC error so the sync orchestrator falls
// through to the next adapter instead of treating the file as ours.
func (h *Handler) Import(_ context.Context, params proto.ImportParams) (proto.ImportResult, error) {
	base := filepath.Base(params.NativePath)
	if base != nativeBasename {
		return proto.ImportResult{}, &proto.RPCError{
			Code:    proto.CodeUnrecognizedNativeFile,
			Message: fmt.Sprintf("%s does not handle %q", pluginName, base),
		}
	}
	content, err := os.ReadFile(params.NativePath)
	if err != nil {
		return proto.ImportResult{}, &proto.RPCError{
			Code:    proto.CodeIOError,
			Message: fmt.Sprintf("read %s: %v", params.NativePath, err),
		}
	}
	payload, err := acf.EncodePayload(acf.MemoryPayload{
		Format:  memoryFormat,
		Content: string(content),
	})
	if err != nil {
		return proto.ImportResult{}, &proto.RPCError{
			Code:    proto.CodeInternal,
			Message: fmt.Sprintf("encode payload: %v", err),
		}
	}
	return proto.ImportResult{
		Imports: []proto.ImportedItem{{
			Kind:       acf.KindMemory,
			Scope:      acf.ScopeGlobal,
			Name:       base,
			SourcePath: params.NativePath,
			Payload:    json.RawMessage(payload),
		}},
	}, nil
}

// Export renders the current memory payload (already replayed by the
// daemon) to DestPath verbatim.
func (h *Handler) Export(_ context.Context, params proto.ExportParams) (proto.ExportResult, error) {
	var mp acf.MemoryPayload
	if err := json.Unmarshal(params.Payload, &mp); err != nil {
		return proto.ExportResult{}, &proto.RPCError{
			Code:    proto.CodeFormatUnsupported,
			Message: fmt.Sprintf("decode memory payload: %v", err),
		}
	}
	if err := os.WriteFile(params.DestPath, []byte(mp.Content), 0o644); err != nil {
		return proto.ExportResult{}, &proto.RPCError{
			Code:    proto.CodeIOError,
			Message: fmt.Sprintf("write %s: %v", params.DestPath, err),
		}
	}
	return proto.ExportResult{Written: true}, nil
}

// NativePath maps a memory artifact to its native on-disk location inside
// contextDir. Only KindMemory is supported; everything else reports
// Supports=false so the orchestrator skips this plugin for that artifact.
func (h *Handler) NativePath(_ context.Context, params proto.NativePathParams) (proto.NativePathResult, error) {
	if params.Artifact.Kind != acf.KindMemory {
		return proto.NativePathResult{}, nil
	}
	return proto.NativePathResult{
		Path:     filepath.Join(params.ContextDir, params.Artifact.Name),
		Supports: true,
	}, nil
}

// HandlesFormat reports whether this plugin can render (kind, format).
// True only for memory/markdown.
func (h *Handler) HandlesFormat(_ context.Context, params proto.HandlesFormatParams) (proto.HandlesFormatResult, error) {
	return proto.HandlesFormatResult{
		Handles: params.Kind == acf.KindMemory && params.Format == memoryFormat,
	}, nil
}

// Capabilities advertises the real surface: memory only, the single native
// basename, and the basename->kind map the orchestrator uses to route a
// discovered file to this plugin.
func (h *Handler) Capabilities(_ context.Context, _ proto.CapabilitiesParams) (proto.CapabilitiesResult, error) {
	return proto.CapabilitiesResult{
		Name: pluginName,
		Artifacts: proto.ArtifactSupport{
			Memory:       true,
			Skill:        false,
			Tool:         false,
			Conversation: false,
		},
		NativeBasenames: []string{nativeBasename},
		BasenameToKind:  map[string]acf.Kind{nativeBasename: acf.KindMemory},
	}, nil
}

// Shutdown is a no-op: this plugin holds no resources to release. The
// daemon closes our stdin (EOF) to terminate the serve loop.
func (h *Handler) Shutdown(_ context.Context, _ proto.ShutdownParams) (proto.ShutdownResult, error) {
	return proto.ShutdownResult{}, nil
}

// Compile-time check: Handler satisfies the plugin author interface.
var _ host.Handler = (*Handler)(nil)
