// magiclint is the Aplexica CI lint that enforces FR-10.6 (BRD-10 §10.5):
// every tunable parameter MUST originate in a configuration layer, not a
// magic numeric literal in source.
//
// Usage:
//
//	go run ./tools/magiclint [--init-allowlist] [path...]
//
// Default scan roots: ./cmd/... and ./internal/.... Test files (*_test.go
// and anything under /testdata/) are skipped per §10.4 exceptions. The
// permitted-exception list (also from §10.4) is encoded inline:
//
//   - Literals in `const` declarations.
//   - Literals 0, 1, -1, 2 (universally trivial; not "tunables").
//   - Literals used as file-mode struct-field values
//     (Mode/Perm/FileMode — e.g. 0o644 in atomicfile.WriteFile).
//   - Literals used as exit codes in os.Exit(N).
//   - Literals that define a fixed array type length (for example,
//     `[32]byte`). Array lengths are part of Go's static type identity,
//     not runtime-tunable parameters.
//   - Literals used as array/slice positions (`b[8:10]`) or bit-field
//     operations (`v>>4`, `v&0x0f`). These encode data layout rather than
//     a runtime tuning choice.
//   - Literals on the right-hand side of `iota` patterns (handled by
//     the const-decl exception above).
//
// An external `.magiclint-allow` file lists known existing violations
// (one `path:value:count` per line — count is how many occurrences of
// that literal are tolerated in that file). Lines starting with `#` are
// comments. CI fails when a magic number appears that is neither in the
// permitted-exception list nor covered by the allowlist — i.e. NEW
// violations fail the build. The intentionally-large initial allowlist
// reflects the FR-10.6 cleanup arc that is the natural follow-on to
// v0.69.0.
//
// History: the allowlist was originally keyed `path:line`. Line numbers
// shift whenever an allowlisted file is edited above an entry, so the
// list drifted on unrelated edits and each drift forced a wholesale
// --init-allowlist regeneration — which silently absorbed genuinely-new
// violations without review. The `path:value:count` key is edit-stable
// (lines can move freely) while still catching an N+1-th occurrence of
// an allowlisted value.
package main

import (
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

var (
	initAllowlist = flag.Bool("init-allowlist", false,
		"write the current set of violations to .magiclint-allow and exit 0")
	allowlistPath = flag.String("allowlist", ".magiclint-allow",
		"path to the allowlist file (one path:value:count per line)")
)

// trivial literals that aren't "tunables" — index sentinels, comparison
// guards, etc.
var trivialNumerics = map[string]struct{}{
	"0": {}, "1": {}, "2": {}, "-1": {},
}

// modeFieldNames flags struct-field assignments where the RHS literal
// is an octal file mode. These are protocol-shaped (FR-10.4 exception:
// magic protocol values).
var modeFieldNames = map[string]struct{}{
	"Mode": {}, "Perm": {}, "FileMode": {},
}

func main() {
	flag.Parse()
	roots := flag.Args()
	if len(roots) == 0 {
		roots = []string{"./cmd/...", "./internal/..."}
	}

	// Load the allowlist only when checking — --init-allowlist overwrites
	// its entries (while preserving leading comments) and must not choke on
	// a malformed/legacy-format entry (that would make migration impossible).
	var allow map[string]int
	if !*initAllowlist {
		var err error
		allow, err = loadAllowlist(*allowlistPath)
		if err != nil {
			fail("load allowlist: %v", err)
		}
	}

	var findings []finding
	for _, root := range roots {
		root = strings.TrimSuffix(root, "/...")
		if err := walkGoFiles(root, func(path string) error {
			fs, err := scanFile(path)
			if err != nil {
				return err
			}
			findings = append(findings, fs...)
			return nil
		}); err != nil {
			fail("walk %s: %v", root, err)
		}
	}

	// Sort for deterministic output.
	sort.Slice(findings, func(i, j int) bool {
		if findings[i].Path != findings[j].Path {
			return findings[i].Path < findings[j].Path
		}
		return findings[i].Line < findings[j].Line
	})

	if *initAllowlist {
		entries, err := writeAllowlist(*allowlistPath, findings)
		if err != nil {
			fail("write allowlist: %v", err)
		}
		fmt.Fprintf(os.Stdout, "magiclint: wrote %d path:value:count entries (%d findings) to %s\n",
			entries, len(findings), *allowlistPath)
		return
	}

	// Group by (path, value) and compare against the allowlist counts.
	violations := newViolationGroups(findings, allow)
	if len(violations) == 0 {
		fmt.Fprintf(os.Stdout,
			"magiclint: ok (%d allowlisted findings, 0 new violations)\n",
			len(findings))
		return
	}
	total := 0
	for _, v := range violations {
		total += v.delta
		fmt.Fprintf(os.Stderr,
			"%s: magic number %q ×%d (%d allowed) at line(s) %s (FR-10.6): move to defaults.toml\n",
			v.path, v.literal, len(v.lines), v.allowed, joinInts(v.lines))
	}
	fail("%d new magic-number violation(s); bump the path:value:count entry in %s "+
		"if intentional, otherwise migrate to defaults.toml + internal/config.",
		total, *allowlistPath)
}

type finding struct {
	Path    string
	Line    int
	Literal string
}

// groupKey is the edit-stable allowlist key: file + literal value, no
// line number (see the package-doc history note).
func (f finding) groupKey() string {
	return f.Path + ":" + f.Literal
}

// violationGroup is one (file, literal) whose occurrence count exceeds
// its allowance.
type violationGroup struct {
	path    string
	literal string
	lines   []int // every occurrence, so the developer can locate the new one
	allowed int
	delta   int // occurrences beyond the allowance
}

// newViolationGroups groups findings by (path, literal) and returns the
// groups whose occurrence count exceeds the allowlisted count. findings
// must be sorted (path, line) for deterministic output; group order
// follows first occurrence.
func newViolationGroups(findings []finding, allow map[string]int) []violationGroup {
	type acc struct {
		path, literal string
		lines         []int
	}
	groups := map[string]*acc{}
	var order []string
	for _, f := range findings {
		k := f.groupKey()
		g, ok := groups[k]
		if !ok {
			g = &acc{path: f.Path, literal: f.Literal}
			groups[k] = g
			order = append(order, k)
		}
		g.lines = append(g.lines, f.Line)
	}
	var out []violationGroup
	for _, k := range order {
		g := groups[k]
		allowed := allow[k]
		if len(g.lines) <= allowed {
			continue
		}
		out = append(out, violationGroup{
			path:    g.path,
			literal: g.literal,
			lines:   g.lines,
			allowed: allowed,
			delta:   len(g.lines) - allowed,
		})
	}
	return out
}

func joinInts(ns []int) string {
	parts := make([]string, len(ns))
	for i, n := range ns {
		parts[i] = strconv.Itoa(n)
	}
	return strings.Join(parts, ",")
}

func walkGoFiles(root string, visit func(path string) error) error {
	info, err := os.Stat(root)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return visit(root)
	}
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Skip testdata trees per §10.4 exception (4).
			if d.Name() == "testdata" {
				return filepath.SkipDir
			}
			// Skip /gen/ directories: code-generation tools whose
			// literals describe sprite dimensions, palette values,
			// and other build-time fixtures (§10.4 exception 3 —
			// "build metadata populated at compile time"). The
			// generated outputs become const declarations elsewhere.
			if d.Name() == "gen" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		if strings.HasSuffix(path, "_test.go") {
			return nil
		}
		// File-level exempt: schema-spec files where every literal is
		// authoritative algorithmic content.
		for prefix := range fileLevelExempt {
			if strings.HasSuffix(path, prefix) || strings.Contains(path, "/"+prefix) {
				return nil
			}
		}
		return visit(path)
	})
}

// scanCtx is the parent-context state threaded through the recursive
// scanner. The defer-in-ast.Inspect pattern doesn't work for scoped
// counters (defer fires on function return, not on subtree exit), so
// we do an explicit recursive descent that pushes/pops on entry/exit.
type scanCtx struct {
	inConstDecl bool
	inModeField bool
	inOsExit    bool
	// inArrayLen is true only while descending through ast.ArrayType.Len.
	// A fixed array's length is part of its Go type identity (for example,
	// a SHA-256 digest is [32]byte), so it cannot originate in runtime
	// configuration. Slice capacities and make/new arguments remain scanned.
	inArrayLen bool
	// inIndexPosition and inBitOperation cover only syntax that describes a
	// byte/bit layout. These contexts deliberately do not cover allocation
	// sizes, loop bounds, or ordinary arithmetic.
	inIndexPosition bool
	inBitOperation  bool
	// inFileModeArg flips to true for the specific argument position of
	// os.WriteFile/os.MkdirAll/os.Chmod/os.OpenFile that carries the
	// mode bits. Like inModeField, these are §10.4-exempt protocol
	// shapes (0o600 / 0o644 / 0o700 / 0o755) and aren't tunables.
	inFileModeArg bool
}

// fileModeArgFuncs is the set of stdlib calls whose mode argument is
// exempt. Map value is the 1-indexed argument position (1 = first arg).
var fileModeArgFuncs = map[string]int{
	"os.WriteFile":         3,
	"os.MkdirAll":          2,
	"os.Mkdir":             2,
	"os.Chmod":             2,
	"os.OpenFile":          3, // (name, flag, perm)
	"atomicfile.WriteFile": 3,
	// cobra arg-count constructors take a literal count — these are
	// CLI-shape constants, not tunables. Cobra usage is widespread in
	// cmd/aplexica/cmd_*.go and treating the count argument as a magic
	// number would force the linter to ignore the rest of the file.
	"cobra.ExactArgs":    1,
	"cobra.MinimumNArgs": 1,
	"cobra.MaximumNArgs": 1,
	"cobra.RangeArgs":    1, // (min, max) — first arg
}

// fileLevelExempt is the set of source files whose every literal is
// allowed. Used for schema/spec files where the literals ARE the
// configuration (BRD-10 §10.4 #2 — algorithmic constants forming the
// spec the configuration system uses). Adding a file here MUST come
// with a one-line comment explaining the rationale; this is not a
// general escape hatch.
var fileLevelExempt = map[string]string{
	// internal/config/schema.go contains the schema entries that
	// describe every tunable in defaults.toml. Migrating these to
	// defaults.toml would be circular (the schema is layer-0; it
	// describes layer-1). Range bounds and bounds-of-bounds are the
	// authoritative source.
	"internal/config/schema.go": "schema describing tunables in defaults.toml",

	// internal/conformance/conformance.go is the BRD-02 §5.4 spec
	// translated to executable form. Its literals are spec budgets
	// (1 GiB / 30s perf target, 5s CI slack, 8 KiB per-file size,
	// etc.) — not tunables to migrate. They live in code because
	// they ARE the spec the rest of the codebase is benchmarked
	// against.
	"internal/conformance/conformance.go": "BRD-02 §5.4 spec-defined conformance budgets",
}

func scanFile(path string) ([]finding, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}

	var out []finding
	scanNode(fset, file, scanCtx{}, &out)
	return out, nil
}

func scanNode(fset *token.FileSet, n ast.Node, ctx scanCtx, out *[]finding) {
	if n == nil {
		return
	}

	// Apply parent-context flips before descending into children.
	switch v := n.(type) {
	case *ast.GenDecl:
		if v.Tok == token.CONST {
			ctx.inConstDecl = true
		}
	case *ast.KeyValueExpr:
		if ident, ok := v.Key.(*ast.Ident); ok {
			if _, ok := modeFieldNames[ident.Name]; ok {
				ctx.inModeField = true
			}
		}
	case *ast.CallExpr:
		if isOsExit(v) {
			ctx.inOsExit = true
		}
		// For known file-mode-bearing functions, descend with each
		// argument carrying its own flag-state. This lets the
		// permission bits in `os.WriteFile(path, body, 0o644)` be
		// exempt without exempting all literals in the call.
		if name, argIdx, ok := isFileModeFunc(v); ok {
			for i, arg := range v.Args {
				childCtx := ctx
				if i == argIdx-1 {
					childCtx.inFileModeArg = true
				}
				scanNode(fset, arg, childCtx, out)
			}
			_ = name
			// Also descend into the call's Fun (the selector expr) with
			// the parent context so e.g. `pkg.Foo` identifiers are not
			// mis-scanned.
			scanNode(fset, v.Fun, ctx, out)
			return
		}
	case *ast.ArrayType:
		// Descend into the array length under a narrowly scoped structural
		// exemption. The element type is scanned under the parent context so
		// any unrelated expression embedded there cannot inherit it.
		if v.Len != nil {
			lengthCtx := ctx
			lengthCtx.inArrayLen = true
			scanNode(fset, v.Len, lengthCtx, out)
		}
		scanNode(fset, v.Elt, ctx, out)
		return
	case *ast.IndexExpr:
		scanNode(fset, v.X, ctx, out)
		indexCtx := ctx
		indexCtx.inIndexPosition = true
		scanNode(fset, v.Index, indexCtx, out)
		return
	case *ast.SliceExpr:
		scanNode(fset, v.X, ctx, out)
		indexCtx := ctx
		indexCtx.inIndexPosition = true
		scanNode(fset, v.Low, indexCtx, out)
		scanNode(fset, v.High, indexCtx, out)
		scanNode(fset, v.Max, indexCtx, out)
		return
	case *ast.BinaryExpr:
		if isBitOperation(v.Op) {
			bitCtx := ctx
			bitCtx.inBitOperation = true
			scanNode(fset, v.X, bitCtx, out)
			scanNode(fset, v.Y, bitCtx, out)
			return
		}
	case *ast.UnaryExpr:
		if v.Op == token.XOR {
			bitCtx := ctx
			bitCtx.inBitOperation = true
			scanNode(fset, v.X, bitCtx, out)
			return
		}
	case *ast.BasicLit:
		if v.Kind == token.INT || v.Kind == token.FLOAT {
			if !ctx.inConstDecl && !ctx.inModeField && !ctx.inOsExit && !ctx.inArrayLen && !ctx.inIndexPosition && !ctx.inBitOperation && !ctx.inFileModeArg {
				lit := v.Value
				if _, trivial := trivialNumerics[lit]; !trivial {
					pos := fset.Position(v.Pos())
					*out = append(*out, finding{
						Path:    pos.Filename,
						Line:    pos.Line,
						Literal: lit,
					})
				}
			}
		}
		// BasicLit has no relevant children.
		return
	}

	// Visit children with the (possibly enriched) context.
	for _, child := range childrenOf(n) {
		scanNode(fset, child, ctx, out)
	}
}

func isBitOperation(op token.Token) bool {
	switch op {
	case token.AND, token.OR, token.XOR, token.SHL, token.SHR, token.AND_NOT:
		return true
	default:
		return false
	}
}

// childrenOf returns the AST children of n we care about for the lint.
// Using ast.Walk would also work but provides only an upward Visitor
// signal; explicit listing keeps the recursion simple and the cases
// auditable.
func childrenOf(n ast.Node) []ast.Node {
	var kids []ast.Node
	ast.Inspect(n, func(c ast.Node) bool {
		if c == n {
			return true // descend into n itself
		}
		if c == nil {
			return false
		}
		kids = append(kids, c)
		return false // do NOT descend — let scanNode recurse
	})
	return kids
}

func isOsExit(c *ast.CallExpr) bool {
	sel, ok := c.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	ident, ok := sel.X.(*ast.Ident)
	if !ok {
		return false
	}
	return ident.Name == "os" && sel.Sel.Name == "Exit"
}

// isFileModeFunc reports whether c is one of the file-mode-bearing
// stdlib (or atomicfile) calls. Returns the full dotted name and the
// 1-indexed position of the mode argument.
func isFileModeFunc(c *ast.CallExpr) (string, int, bool) {
	sel, ok := c.Fun.(*ast.SelectorExpr)
	if !ok {
		return "", 0, false
	}
	ident, ok := sel.X.(*ast.Ident)
	if !ok {
		return "", 0, false
	}
	full := ident.Name + "." + sel.Sel.Name
	if pos, ok := fileModeArgFuncs[full]; ok {
		return full, pos, true
	}
	return "", 0, false
}

// loadAllowlist parses `path:value:count` lines into a map keyed
// `path:value` → tolerated occurrence count. Duplicate keys sum.
// Legacy 2-part `path:line` entries are rejected with a migration hint
// — silently treating a line number as a value would mis-allow.
func loadAllowlist(path string) (map[string]int, error) {
	out := map[string]int{}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return nil, err
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// Lines may have an optional trailing comment.
		if idx := strings.Index(line, " #"); idx > 0 {
			line = strings.TrimSpace(line[:idx])
		}
		// Split from the right: count, then value; the remainder is the
		// path (numeric literals never contain ':').
		i := strings.LastIndex(line, ":")
		if i <= 0 {
			return nil, fmt.Errorf("%s: malformed entry %q (want path:value:count)", path, line)
		}
		key, countStr := line[:i], line[i+1:]
		if !strings.Contains(key, ":") {
			return nil, fmt.Errorf("%s: entry %q looks like the legacy path:line format — "+
				"the allowlist is now keyed path:value:count; regenerate with --init-allowlist",
				path, line)
		}
		count, cerr := strconv.Atoi(countStr)
		if cerr != nil || count <= 0 {
			return nil, fmt.Errorf("%s: entry %q has a non-positive or non-numeric count", path, line)
		}
		out[key] += count
	}
	return out, nil
}

// writeAllowlist writes the grouped allowlist and returns the number of
// path:value:count entries written. An existing leading comment block is
// preserved verbatim so reviewed baseline rationale is not silently erased by
// a mechanical --init-allowlist refresh.
func writeAllowlist(path string, findings []finding) (int, error) {
	counts := map[string]int{}
	for _, f := range findings {
		counts[f.groupKey()]++
	}
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	b.WriteString(existingAllowlistHeader(path))
	for _, k := range keys {
		fmt.Fprintf(&b, "%s:%d\n", k, counts[k])
	}
	return len(keys), os.WriteFile(path, []byte(b.String()), 0o644)
}

func existingAllowlistHeader(path string) string {
	const fallback = `# Aplexica magiclint allowlist — auto-generated by
#   go run ./tools/magiclint --init-allowlist
#
# Each non-comment line is a path:value:count triple. The count is the reviewed
# occurrence ceiling; the first unreviewed increase fails CI.

`

	raw, err := os.ReadFile(path)
	if err != nil {
		return fallback
	}
	lines := strings.SplitAfter(string(raw), "\n")
	var header strings.Builder
	seenComment := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			seenComment = true
			header.WriteString(line)
			continue
		}
		if trimmed == "" && seenComment {
			header.WriteString(line)
			continue
		}
		break
	}
	if !seenComment {
		return fallback
	}
	value := header.String()
	if !strings.HasSuffix(value, "\n\n") {
		value = strings.TrimRight(value, "\n") + "\n\n"
	}
	return value
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "magiclint: "+format+"\n", args...)
	os.Exit(2)
}
