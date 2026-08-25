package claudecode

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter"
)

// Claude Code exposes memory through TWO surfaces: the hand-authored
// ~/.claude/CLAUDE.md instruction file AND the auto-managed memory directory the
// `/memory` tool writes at ~/.claude/projects/<encoded-cwd>/memory/ — an index
// (MEMORY.md) plus one markdown file per topic. Each topic carries a
// `metadata.type` of "user" (personal/global) or "project" (folder-scoped):
//
//	---
//	name: dogs
//	metadata:
//	  type: user
//	---
//
//	Example User's dogs are Comet and Nova.
//
// Type-aware routing (v0.7.0): the BODY of each topic (the text after the
// frontmatter fence) is routed by its type. `type:user` bodies fold into the
// single GLOBAL ~/.claude/CLAUDE.md-keyed artifact, gathered across EVERY
// project's auto-memory dir, so a personal fact fans out to the other agents.
// `type:project` bodies fold into the cwd's REGISTERED project memory
// (<projectPath>/CLAUDE.md) and stay project-scoped — never global. A topic's
// folding is via adapter.ComposeAppendedMemory; the export side strips the same
// bodies back out before writing the destination CLAUDE.md
// (ExportMemory) so the hand-authored file stays pristine. Compose and strip are
// inverses, which keeps both round-trips (global and per-project) byte-stable.
//
// The MEMORY.md index is intentionally NOT folded in: it only holds wikilinks to
// its sibling topic files, which would be dead links in another agent's memory.
//
// A `type:project` topic only reaches a real artifact when its auto-memory dir's
// encoded cwd maps back to a REGISTERED local project (projectForEnc). With no
// registry, type:project bodies are silently dropped from both surfaces — they
// can't be attributed to a project, and they must never leak into global.

const (
	autoMemoryIndexBasename = "MEMORY.md"
	autoMemoryDirBasename   = "memory"
	memoryTypeUser          = "user"
	memoryTypeProject       = "project"
	fenceMarker             = "---"
)

func (a *Adapter) globalClaudeRoot() string { return filepath.Join(a.HomeDir, ".claude") }
func (a *Adapter) globalClaudePath() string { return filepath.Join(a.globalClaudeRoot(), "CLAUDE.md") }

// projectsRoot returns ~/.claude/projects, the parent of every per-cwd
// auto-memory directory.
func (a *Adapter) projectsRoot() string {
	return filepath.Join(a.globalClaudeRoot(), "projects")
}

// isGlobalClaudePath reports whether p is THIS adapter's global
// ~/.claude/CLAUDE.md (the destination that gets the type:user strip + compose
// treatment).
func (a *Adapter) isGlobalClaudePath(p string) bool {
	if a.HomeDir == "" {
		return false
	}
	ap, err := filepath.Abs(p)
	if err != nil {
		return false
	}
	return filepath.Clean(ap) == filepath.Clean(a.globalClaudePath())
}

// autoMemoryEnc reports whether p is a markdown file directly inside SOME
// per-cwd auto-memory directory (<projectsRoot>/<enc>/memory/). When it is, it
// returns the <enc> segment (the lossy-encoded cwd) so the caller can map it
// back to a registered project.
func (a *Adapter) autoMemoryEnc(p string) (enc string, ok bool) {
	if a.HomeDir == "" || filepath.Ext(p) != ".md" {
		return "", false
	}
	ap, err := filepath.Abs(p)
	if err != nil {
		return "", false
	}
	memDir := filepath.Dir(filepath.Clean(ap)) // <projectsRoot>/<enc>/memory
	if filepath.Base(memDir) != autoMemoryDirBasename {
		return "", false
	}
	encDir := filepath.Dir(memDir) // <projectsRoot>/<enc>
	if filepath.Clean(filepath.Dir(encDir)) != filepath.Clean(a.projectsRoot()) {
		return "", false
	}
	return filepath.Base(encDir), true
}

// projectForEnc returns the registered LOCAL project whose encoded path equals
// enc. nil registry → ("", false). Used to map a type:project auto-memory dir
// back to the project that owns it.
//
// LOCAL-only gate (I1): only "local"-scope projects match. type:project memory
// must NEVER become global — a project registered scope:"global" would make
// importProjectMemory stamp the artifact ScopeGlobal (via ResolveRegistered
// scope), fanning the project fact out to every agent. So global-scope projects
// are skipped here: their type:project topics are simply not imported yet
// (global-project memory is a deferred plan), and critically are NOT globalized.
//
// Lossy-encoding assumption (M2): encodeProjectDir is non-injective
// (/, ., _, space → -), so this returns the FIRST matching registry entry and
// relies on encoded paths being effectively unique across the (typically few)
// registered projects — acceptable in practice.
func (a *Adapter) projectForEnc(enc string) (projectPath string, ok bool) {
	if a.Registry == nil {
		return "", false
	}
	for _, e := range a.Registry.List() {
		if encodeProjectDir(e.Path) == enc && e.EffectiveScope() == "local" {
			return e.Path, true
		}
	}
	return "", false
}

// registeredProjectForClaudePath reports whether p is the <Path>/CLAUDE.md of a
// REGISTERED LOCAL project, returning that project's Path. Used by Import to
// route a registered project's own memory file through importProjectMemory (so
// its type:project auto-memory composes in) instead of the plain importer.
//
// LOCAL-only gate (I1): like projectForEnc, only "local"-scope projects match,
// so a project's type:project topics can never be stamped global. Global-scope
// project memory is a deferred plan.
func (a *Adapter) registeredProjectForClaudePath(p string) (projectPath string, ok bool) {
	if a.Registry == nil {
		return "", false
	}
	ap, err := filepath.Abs(p)
	if err != nil {
		return "", false
	}
	clean := filepath.Clean(ap)
	for _, e := range a.Registry.List() {
		if filepath.Clean(filepath.Join(e.Path, "CLAUDE.md")) == clean && e.EffectiveScope() == "local" {
			return e.Path, true
		}
	}
	return "", false
}

// parseAutoMemoryEntry splits a topic file into its declared type and its body.
// When content begins with a `---` fence, the frontmatter is scanned for a
// `type:` key — either top-level (`type: user`) or nested under `metadata:`
// (`  type: project`) — and the body is everything after the closing fence
// (trimmed). With no leading fence the whole content is the body and the type is
// "".
func parseAutoMemoryEntry(content string) (typeTag, body string) {
	lines := strings.Split(content, "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != fenceMarker {
		return "", strings.TrimSpace(content)
	}
	closeIdx := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == fenceMarker {
			closeIdx = i
			break
		}
	}
	if closeIdx == -1 {
		// Unterminated fence — treat the whole thing as body (no type).
		return "", strings.TrimSpace(content)
	}
	for _, ln := range lines[1:closeIdx] {
		key, val, found := strings.Cut(strings.TrimSpace(ln), ":")
		if !found {
			continue
		}
		if strings.TrimSpace(key) == "type" {
			if v := strings.TrimSpace(val); v != "" {
				typeTag = v
				break
			}
		}
	}
	body = strings.TrimSpace(strings.Join(lines[closeIdx+1:], "\n"))
	return typeTag, body
}

// autoMemoryBodiesByType reads every *.md in dir EXCEPT the MEMORY.md index, in
// sorted filename order, and returns the BODY of each topic whose declared type
// equals wantType. A missing/unreadable dir yields no bodies (best-effort).
func autoMemoryBodiesByType(dir, wantType string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".md" || e.Name() == autoMemoryIndexBasename {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	var out []string
	for _, n := range names {
		b, rerr := os.ReadFile(filepath.Join(dir, n))
		if rerr != nil {
			continue
		}
		typeTag, body := parseAutoMemoryEntry(string(b))
		if typeTag == wantType && body != "" {
			out = append(out, body)
		}
	}
	return out
}

// userMemoryBodies gathers the type:user topic bodies from EVERY per-cwd
// auto-memory directory under projectsRoot, in deterministic (dir-sorted, then
// filename-sorted) order. These compose into the single global CLAUDE.md.
func (a *Adapter) userMemoryBodies() []string {
	root := a.projectsRoot()
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	dirs := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, e.Name())
		}
	}
	sort.Strings(dirs)
	var out []string
	for _, d := range dirs {
		memDir := filepath.Join(root, d, autoMemoryDirBasename)
		out = append(out, autoMemoryBodiesByType(memDir, memoryTypeUser)...)
	}
	return out
}

// projectMemoryBodies returns the type:project topic bodies from the given
// project's auto-memory directory (<projectsRoot>/<encodeProjectDir(path)>/
// memory/). These compose into that project's <path>/CLAUDE.md.
func (a *Adapter) projectMemoryBodies(projectPath string) []string {
	memDir := filepath.Join(a.projectsRoot(), encodeProjectDir(projectPath), autoMemoryDirBasename)
	return autoMemoryBodiesByType(memDir, memoryTypeProject)
}

// importGlobalMemory composes ~/.claude/CLAUDE.md + ALL type:user topic bodies
// (gathered from every project's auto-memory dir) into the single
// CLAUDE.md-keyed GLOBAL memory artifact. It is invoked for a change to the
// global CLAUDE.md OR to any auto-memory file; either way the artifact's
// SourcePath is CLAUDE.md, so auto-memory edits update the one canonical global
// memory (attributed to claude-code) instead of minting a colliding second
// global-memory artifact. type:project bodies are NOT folded here — they route
// to their project (importProjectMemory).
func (a *Adapter) importGlobalMemory(ctx context.Context, store *acf.Store) ([]string, error) {
	claudePath := a.globalClaudePath()
	body, err := os.ReadFile(claudePath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("claudecode: read CLAUDE.md: %w", err)
	}
	composed := adapter.ComposeAppendedMemory(string(body), a.userMemoryBodies())
	return adapter.ImportOpaqueContent(
		ctx, store, acf.KindMemory, a.opaqueParams(), claudePath, []byte(composed), memoryEncode,
	)
}

// importProjectMemory composes <projectPath>/CLAUDE.md + that project's
// type:project topic bodies into the project's CLAUDE.md-keyed memory artifact.
// Because the SourcePath lives under a registered local project, the shared
// import pipeline (ResolveRegisteredScope) keeps the artifact PROJECT-scoped,
// keyed by the project — never global. A no-op compose (no type:project bodies)
// is safe: it just re-imports the project's existing CLAUDE.md.
func (a *Adapter) importProjectMemory(ctx context.Context, store *acf.Store, projectPath string) ([]string, error) {
	claudePath := filepath.Join(projectPath, "CLAUDE.md")
	body, err := os.ReadFile(claudePath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("claudecode: read project CLAUDE.md: %w", err)
	}
	composed := adapter.ComposeAppendedMemory(string(body), a.projectMemoryBodies(projectPath))
	return adapter.ImportOpaqueContent(
		ctx, store, acf.KindMemory, a.opaqueParams(), claudePath, []byte(composed), memoryEncode,
	)
}
