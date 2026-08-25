package adapter

import (
	"context"
	"errors"

	"github.com/aplexica/aplexica/internal/acf"
)

// ErrNotHandled is the typed sentinel an adapter's Import returns when the file
// is NOT one this adapter owns or recognizes (a benign dispatch probe-miss),
// as distinct from a genuine parse/import failure on a file it DOES claim.
//
// The sync orchestrator's extension/legacy import fallback probes every adapter;
// it uses errors.Is(err, ErrNotHandled) to avoid counting a probe-miss toward
// adapter quarantine (FR-03.15) — only a real failure on an owned file should
// quarantine. Adapters MUST wrap this (fmt.Errorf("...: %w", ErrNotHandled))
// from their dispatch when the filename matches none of their patterns.
var ErrNotHandled = errors.New("adapter: file not handled by this adapter")

// ToolKind enumerates the BRD-02 §4.4 tool-artifact subkinds an
// adapter may natively support. Adapters declare their per-kind
// support via Capabilities().Tools.
type ToolKind string

const (
	ToolKindMCPServer    ToolKind = "mcp-server"
	ToolKindSubagent     ToolKind = "subagent"
	ToolKindHook         ToolKind = "hook"
	ToolKindSlashCommand ToolKind = "slash-command"
	ToolKindPlugin       ToolKind = "plugin"
)

// AllToolKinds is the V1 enumeration. Adapters' Capabilities().Tools
// lists are subsets of this set.
var AllToolKinds = []ToolKind{
	ToolKindMCPServer, ToolKindSubagent, ToolKindHook,
	ToolKindSlashCommand, ToolKindPlugin,
}

// Surface identifies a user-facing runtime that consumes an adapter's native
// storage. Multiple surfaces may share one logical adapter and storage root;
// for example, Codex CLI and Codex Desktop both use CODEX_HOME. Keeping that
// relationship in capabilities avoids registering two adapters that would
// race over the same files.
type Surface string

const (
	SurfaceCLI     Surface = "cli"
	SurfaceDesktop Surface = "desktop"
)

// ArtifactSupport describes whether an adapter can Import + Export a
// given artifact kind end-to-end.
type ArtifactSupport struct {
	Memory       bool `json:"memory"`
	Skill        bool `json:"skill"`
	Tool         bool `json:"tool"`
	Conversation bool `json:"conversation"`
}

// Capabilities is the BRD-02 §4.5 compatibility matrix an adapter
// publishes describing its native support. Returned by Adapter's
// Capabilities() method.
//
// Per BRD-02 §5.4 #7 (conformance: capability declaration MUST match
// the adapter's actual behavior), the conformance harness verifies
// that the values returned here match what Import / Export / etc.
// actually do.
type Capabilities struct {
	// Name mirrors Adapter.Name() so the struct stands alone in
	// JSON / Markdown reports.
	Name string `json:"name"`

	// Surfaces lists the user-facing runtimes backed by this logical adapter.
	// It is optional for backwards compatibility with third-party adapters that
	// predate surface declarations.
	Surfaces []Surface `json:"surfaces,omitempty"`

	// Artifacts reports per-kind support — every adapter SHOULD
	// support memory + skill (per BRD-02 §6.1); tool and conversation
	// vary.
	Artifacts ArtifactSupport `json:"artifacts"`

	// Tools lists the tool-kind subtypes the adapter natively handles.
	// MCP servers are universal across V1 (BRD-02 §4.4); the other
	// kinds vary per agent.
	Tools []ToolKind `json:"tools"`

	// NativeBasenames is the set of native filenames the adapter's
	// Import dispatch recognizes. Used by the conformance harness to
	// cross-check that every advertised basename round-trips, and by
	// `aplexica adapters check` to surface "this adapter understands
	// these files" to users.
	NativeBasenames []string `json:"nativeBasenames"`

	// BasenameToKind maps each entry in NativeBasenames to the
	// artifact kind the Import dispatch routes that basename to. The
	// orchestrator's source-picker (BRD-02 §5.4 #5 recursion-guard
	// correctness) consults this to probe NativePath WITHOUT calling
	// Import — Import has side effects, so calling it on every
	// adapter in the picker loop violates the recursion guard for
	// shared filenames like AGENTS.md. Entries MUST be subset of
	// NativeBasenames.
	BasenameToKind map[string]acf.Kind `json:"basenameToKind,omitempty"`

	// NotesURL optionally points to the per-agent spec doc (FR-02.3).
	NotesURL string `json:"notesUrl,omitempty"`
}

// AdapterDispatch is an optional helper bundling Capabilities()-style
// metadata in a form that's easy for the orchestrator to consult
// without allocating. Returned by helper functions in this package
// rather than methods on the interface.
type AdapterDispatch struct {
	Basenames map[string]acf.Kind
}

// DispatchOf returns the basename→kind map for `a`, derived from
// Capabilities(). Convenience wrapper used by the sync orchestrator.
func DispatchOf(a Adapter) AdapterDispatch {
	c := a.Capabilities()
	m := make(map[string]acf.Kind, len(c.BasenameToKind))
	for k, v := range c.BasenameToKind {
		m[k] = v
	}
	return AdapterDispatch{Basenames: m}
}

// ConversationDocTarget is an OPTIONAL adapter capability (consulted via type
// assertion, so it does not widen the Adapter interface or the out-of-process
// plugin protocol). An adapter implements it to declare WHERE Aplexica should
// drop a human-readable, rendered transcript of a conversation that originated
// in a DIFFERENT agent.
//
// Conversations are not losslessly portable across agents — each agent's
// session schema differs structurally, so a Codex rollout cannot be written
// into Claude Code's session store. Instead, when a conversation artifact fans
// out, the orchestrator renders the canonical conversation to markdown and
// writes it under this directory, so the user can READ another agent's
// conversation from within this agent's storage. Adapters that don't implement
// this interface simply don't receive transcript documents.
type ConversationDocTarget interface {
	// ConversationDocDir returns the directory under which rendered transcript
	// documents are written for global-scope conversations (e.g.
	// ~/.claude/aplexica/conversations). supports=false opts out.
	ConversationDocDir() (dir string, supports bool)
}

// ConversationSessionTarget is an OPTIONAL adapter capability (type-asserted,
// so it doesn't widen the Adapter interface). An adapter implements it to
// transcode a foreign conversation into its OWN native session store so the
// conversation shows up in that agent's native session list — e.g. Claude Code
// writes ~/.claude/projects/<cwd>/<id>.jsonl so a synced Codex conversation
// appears in `claude` /resume and is resumable.
//
// This is the higher-fidelity sibling of ConversationDocTarget (which only
// drops a read-only markdown file). The orchestrator prefers this when an
// adapter implements it.
type ConversationSessionTarget interface {
	// MaterializeConversationSession transcodes the conversation artifact's
	// head event into this agent's native session format and writes it.
	// Returns the path written (so the orchestrator can guard it), supports=
	// false to opt out (e.g. for a non-canonical payload it can't transcode),
	// or an error.
	MaterializeConversationSession(art acf.Artifact, head acf.Event, sourceAgent string) (path string, supports bool, err error)
}

// ConversationSessionPathTarget is an OPTIONAL refinement for
// ConversationSessionTarget. Native session stores that write deterministic
// files implement it so the orchestrator can recursion-guard the exact path
// BEFORE MaterializeConversationSession writes. That closes the tiny watcher
// race where a remotely materialized conversation file could be seen as a
// fresh local edit before the post-write guard mark landed.
type ConversationSessionPathTarget interface {
	// ConversationSessionPath returns the path MaterializeConversationSession
	// would write for this artifact/head pair. supports=false with an EMPTY
	// path means the same permanent opt-out as MaterializeConversationSession:
	// no native session should be materialized for this payload. supports=false
	// with a NON-EMPTY path is a temporary native-safety deferral: the stable
	// destination is known but must remain untouched until a later retry.
	ConversationSessionPath(art acf.Artifact, head acf.Event, sourceAgent string) (path string, supports bool, err error)
}

// NativeMirrorTarget is an OPTIONAL adapter capability for one logical agent
// that exposes multiple native filesystem surfaces. The primary NativePath
// remains the canonical destination; mirrors are additional places that the
// same engine currently reads, such as Claude Code Desktop's active automatic
// Git worktrees. The orchestrator applies the same read-before-clobber,
// recursion-guard, error, and destination-hash handling to every returned
// path. Adapters MUST return only absolute, validated paths and MUST NOT return
// the primary path itself.
type NativeMirrorTarget interface {
	NativeMirrorPaths(artifact acf.Artifact, contextDir, primaryPath string) ([]string, error)
}

// NativeMirrorTopologySource reports a stable, opaque token for the set of
// app-managed native mirrors currently present. The orchestrator polls this
// alongside its native-root safety scan and re-fans existing artifacts when
// the token changes, so a newly-created Desktop worktree receives context even
// when the canonical artifact itself has not changed since the worktree was
// created. Tokens must depend on topology only, not session activity times.
type NativeMirrorTopologySource interface {
	NativeMirrorTopologyToken() string
}

// RuntimeDiscoverable marks a built-in adapter whose underlying CLI/Desktop
// surfaces may be installed after the daemon has started. The daemon keeps
// these adapters configured even when startup discovery is negative, polls the
// candidate native roots, and the orchestrator re-checks Discover before each
// outbound materialization. This prevents Aplexica from creating files for an
// absent agent while allowing a later installation to become live without a
// daemon restart.
type RuntimeDiscoverable interface {
	CandidateDiscovery() Discovery
}

// NativeMirrorFirstContactGuard lets a multi-surface adapter decide whether an
// existing mirror file is safe to overwrite before the orchestrator has a
// recorded destination fingerprint. This closes the first-contact hole where
// an app may already have edited its isolated worktree. Missing mirror files
// are safe without consulting this interface; existing files fail closed when
// the adapter does not implement it.
type NativeMirrorFirstContactGuard interface {
	NativeMirrorFirstContactSafe(store *acf.Store, artifact acf.Artifact, mirrorPath string) (bool, error)
}

type nativeMirrorContextKey struct{}

// WithNativeMirror marks an Export call as an additional same-agent surface
// write. Skill exporters use it to retain the canonical artifact marker even
// when the source and target agent names match.
func WithNativeMirror(ctx context.Context) context.Context {
	return context.WithValue(ctx, nativeMirrorContextKey{}, true)
}

// IsNativeMirror reports whether ctx was marked by WithNativeMirror.
func IsNativeMirror(ctx context.Context) bool {
	v, _ := ctx.Value(nativeMirrorContextKey{}).(bool)
	return v
}

// Adapter translates between one agent's native storage and ACF.
// Each adapter is a Go package
// compiled into the same binary as the daemon and CLI.
type Adapter interface {
	// Name returns the lowercase agent identifier, e.g. "claude-code".
	Name() string

	// Version returns the adapter's semver; recorded in event provenance.
	Version() string

	// Import reads native state from `nativePath` and writes ACF artifacts +
	// events to `store`. It returns the artifact IDs it wrote (so the caller
	// can print them or chain to other operations).
	Import(ctx context.Context, store *acf.Store, nativePath string) ([]string, error)

	// Export reads the ACF artifact identified by `artifactID` from `store`
	// and writes the corresponding native representation to `destPath`.
	Export(ctx context.Context, store *acf.Store, artifactID, destPath string) error

	// NativePath returns the absolute filesystem path where THIS adapter
	// would write the given artifact when materializing it into contextDir.
	// supports is false when this adapter does not natively support the
	// artifact's kind (e.g., kilo returns supports=false for conversation
	// because its DB-backed sessions are not mapped for lossless fan-out yet).
	//
	// Used by the sync orchestrator (internal/sync) to fan out events: when
	// adapter A imports artifact X at contextDir D, the orchestrator asks
	// every other installed adapter B for NativePath(X, D); if B says
	// supports=true, the orchestrator exports X to that path via B.
	//
	// contextDir should be an absolute directory path. Global artifacts
	// (Scope == ScopeGlobal) MAY ignore contextDir and return paths under
	// the user's home directory.
	NativePath(artifact acf.Artifact, contextDir string) (path string, supports bool, err error)

	// HandlesFormat returns true when this adapter can materialize an
	// artifact of the given kind with the given payload Format. Used by
	// the sync orchestrator to gate fan-out exports — an adapter that
	// says NativePath supports=true but doesn't HandlesFormat the
	// artifact's current format will be skipped without invoking Export.
	//
	// Adapters MUST return true for all formats their Import can produce,
	// and SHOULD return true for shared interop formats (e.g. "markdown"
	// for memory, "skill.md" for skill, "acf.mcp.v1" for tool) so
	// cross-adapter fan-out continues to work for those kinds.
	//
	// Conversation formats are typically per-agent (one adapter only).
	HandlesFormat(kind acf.Kind, format string) bool

	// Capabilities returns the adapter's static compatibility matrix
	// (BRD-02 §4.5 / FR-02.14 / FR-02.18 `aplexica tool capabilities`).
	// Cheap; called from `aplexica adapters check`, conformance tests,
	// and the daemon's startup banner.
	Capabilities() Capabilities

	// Discover reports whether this agent is installed on the local machine
	// and, if so, the native global-storage roots the daemon should watch.
	// Per BRD-03 FR-03.3 ("the daemon MUST detect installed agents at
	// startup using each adapter's discover() method") and BRD-02 §4.13 /
	// FR-02.36. Project-scope storage is discovered dynamically elsewhere
	// (project-detection scan) and is NOT returned here. Implementations
	// MUST be cheap and side-effect-free (stat only); the daemon calls this
	// once per adapter at startup and may re-probe on demand.
	Discover() (Discovery, error)
}

// DeviceIDSetter is implemented by adapters whose Import stamps a device
// identity into event provenance. Pairing — and RE-pairing, which retires the
// previous cloud device id — rotates that identity while the daemon is
// running; the daemon pushes the new id through this so subsequent imports do
// not attribute events to a retired (or hostname-default) identity.
type DeviceIDSetter interface {
	SetDeviceID(id string)
}
