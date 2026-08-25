package host

import (
	"context"

	"github.com/aplexica/aplexica/internal/plugin/proto"
)

// Handler is what a plugin author implements. Each method maps to one
// JSON-RPC method on the wire. Methods may return a *proto.RPCError to
// surface a specific error code; any other error is sent as
// proto.CodeInternal.
//
// Capabilities is v0.94.0+; older plugins that haven't implemented it
// can embed BaseCapabilitiesHandler for a conservative default
// (everything claimed; empty tool kinds).
type Handler interface {
	Initialize(ctx context.Context, params proto.InitializeParams) (proto.InitializeResult, error)
	Import(ctx context.Context, params proto.ImportParams) (proto.ImportResult, error)
	Export(ctx context.Context, params proto.ExportParams) (proto.ExportResult, error)
	NativePath(ctx context.Context, params proto.NativePathParams) (proto.NativePathResult, error)
	HandlesFormat(ctx context.Context, params proto.HandlesFormatParams) (proto.HandlesFormatResult, error)
	Capabilities(ctx context.Context, params proto.CapabilitiesParams) (proto.CapabilitiesResult, error)
	Shutdown(ctx context.Context, params proto.ShutdownParams) (proto.ShutdownResult, error)
}

// BaseCapabilitiesHandler is an embeddable struct providing a
// conservative default Capabilities response. Plugin authors who
// haven't yet declared their full surface can embed this in their
// Handler implementation to satisfy the interface; v0.94.0+ ABI
// requires the method but the default is safe (no Tools claimed).
type BaseCapabilitiesHandler struct {
	PluginName string
}

func (b BaseCapabilitiesHandler) Capabilities(_ context.Context, _ proto.CapabilitiesParams) (proto.CapabilitiesResult, error) {
	return proto.CapabilitiesResult{
		Name: b.PluginName,
		Artifacts: proto.ArtifactSupport{
			Memory: true, Skill: true, Tool: true, Conversation: true,
		},
		// No Tools / NativeBasenames / BasenameToKind declared by
		// default — plugins override to advertise their actual surface.
	}, nil
}
