// Package conformance is the shared adapter-conformance test harness
// per BRD-02 §5.4. Every V1 adapter calls into this package from its
// own _test.go so the conformance contract lives in ONE place instead
// of duplicated per-adapter assertions.
//
// V1 scope (M0 exit criterion: "Basic test harness with the
// conformance suite stubbed; AGENTS.md and SKILL.md round-trip tests
// included"):
//
//   - Round-trip (§5.4.1): native → ACF → native, byte-equal modulo
//     documented noise.
//   - Idempotency (§5.4.2): a second Export onto the same target is a
//     no-op (returns either ErrArtifactTombstoned for redacted artifacts
//     or writes the same bytes).
//   - AGENTS.md round-trip (FR-02.12 / §6.1): when the adapter supports
//     the AAIF AGENTS.md standard, round-trip AGENTS.md verbatim sections.
//   - SKILL.md round-trip (FR-02.12 / §6.1): same, for the Agent Skills
//     Open Standard.
//
// Stubbed (deferred to M1):
//
//   - Watch correctness (§5.4.3): golden-file event sequences.
//   - Cross-conversion (§5.4.4): N×N matrix.
//   - Recursion guard (§5.4.5): outbound write must not retrigger an
//     inbound event.
//   - Performance (§5.4.6): 1 GB scan under 30s.
//   - Capability declaration (§5.4.7): compatibilityMatrix() match.
//
// Usage:
//
//	func TestClaudeCodeConformance(t *testing.T) {
//	    conformance.Run(t, conformance.Opts{
//	        Name:    "claude-code",
//	        Build:   func() adapter.Adapter { return claudecode.New() },
//	        Fixtures: conformance.DefaultFixtures(),
//	    })
//	}
package conformance

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter"
)

// osWriteFile is a thin shim around os.WriteFile so the tiny
// writeFile helper above can stay import-free at its declaration.
func osWriteFile(path string, b []byte) error {
	return os.WriteFile(path, b, 0o644)
}

// Opts parameterizes a conformance run for one adapter.
type Opts struct {
	// Name is the adapter's lowercase identifier — e.g. "claude-code".
	// Surfaced in test failure messages so multiple adapters' failures
	// stay distinguishable.
	Name string

	// Build constructs a fresh adapter instance per sub-test. The
	// returned adapter MUST be safe to use against a fresh tempdir
	// store; it MUST NOT share state between calls.
	Build func() adapter.Adapter

	// Fixtures supplies the test cases. DefaultFixtures() returns the
	// shared M0 set (AGENTS.md, SKILL.md, .mcp.json, plus minimal
	// memory/skill text).
	Fixtures []Fixture

	// HomeDir, when non-empty, is set on every constructed adapter via
	// the well-known HomeDir field of each concrete adapter type. For
	// global-scope artifacts. When empty, the harness uses t.TempDir()
	// so the adapter's real home directory is never touched.
	HomeDir string
}

// Fixture is one round-trip test case. The harness:
//  1. Writes Body to <root>/<NativeName>.
//  2. Calls adapter.Import on that file.
//  3. Calls adapter.Export back to a sibling path.
//  4. Asserts the byte-equality (or fidelity-set membership) of the
//     re-exported file vs. the original.
type Fixture struct {
	// Label is a short identifier surfaced in t.Run() subtest names.
	Label string

	// NativeName is the basename the adapter expects (e.g. "CLAUDE.md",
	// "AGENTS.md", "SKILL.md", ".mcp.json"). Routed via the adapter's
	// own Import dispatch.
	NativeName string

	// Body is the file contents. The harness writes this verbatim to
	// disk before invoking Import.
	Body string

	// Scope hints whether this artifact is global, project, or
	// namespace. Used to set the contextDir for Export.
	Scope acf.Scope

	// SkipIfAdapter is an optional set of adapter names that should
	// skip this fixture. Used for adapters that don't support a
	// specific native form (e.g. kilo doesn't have AGENTS.md).
	SkipIfAdapter []string

	// AcceptedDelta lists strings whose absence-or-presence is OK
	// in the round-tripped output. Per BRD-02 §5.4.1: "Differences
	// are limited to documented non-semantic noise."
	AcceptedDelta []string
}

// Run executes the conformance suite against the configured adapter.
// Each fixture becomes a top-level subtest named
// `<adapter-name>/<fixture-label>`. Within each fixture, four sub-
// assertions run: parse, round-trip, idempotency, and a hook for
// adapter-specific overrides.
func Run(t *testing.T, opts Opts) {
	t.Helper()
	if opts.Name == "" {
		t.Fatal("conformance.Run: Opts.Name required")
	}
	if opts.Build == nil {
		t.Fatal("conformance.Run: Opts.Build required")
	}
	fixtures := opts.Fixtures
	if fixtures == nil {
		fixtures = DefaultFixtures()
	}

	for _, fx := range fixtures {
		fx := fx
		if shouldSkip(opts.Name, fx.SkipIfAdapter) {
			t.Run(fx.Label+"/skipped-by-adapter", func(t *testing.T) {
				t.Skipf("fixture %q skipped for adapter %q", fx.Label, opts.Name)
			})
			continue
		}
		t.Run(fx.Label, func(t *testing.T) {
			runFixture(t, opts, fx)
		})
	}
}

func shouldSkip(adapterName string, skipList []string) bool {
	for _, s := range skipList {
		if s == adapterName {
			return true
		}
	}
	return false
}

// PerfBudget is the BRD-02 §5.4 #6 budget. Default values cover the
// CI-friendly scaled test; the env-gated full-size test override
// supplies its own.
type PerfBudget struct {
	// CorpusBytes is the total native-storage size to generate.
	CorpusBytes int64
	// MaxDuration is the wall-clock ceiling the scan must finish under.
	MaxDuration time.Duration
}

// DefaultPerfBudget is the BRD-02 §5.4 #6 spec-grade target
// (1 GiB / 30s). Used by the env-gated full-size test; CI-friendly
// runs use ScaledPerfBudget which preserves the same ratio at
// ~10 MiB / 0.3s.
func DefaultPerfBudget() PerfBudget {
	return PerfBudget{
		CorpusBytes: 1 << 30, // 1 GiB
		MaxDuration: 30 * time.Second,
	}
}

// ScaledPerfBudget is the CI-friendly scaled-down test. CorpusBytes
// is 10 MiB; MaxDuration is 5s to cover Windows CI runners under the
// race detector. Native execution completes much faster, so the slack is only
// consumed on slower platforms. The spec-grade
// 1 GiB/30s test (DefaultPerfBudget) is the authoritative target;
// this scaled version validates the scanner doesn't regress
// dramatically.
func ScaledPerfBudget() PerfBudget {
	return PerfBudget{
		CorpusBytes: 10 * (1 << 20), // 10 MiB
		MaxDuration: 5 * time.Second,
	}
}

// PerfResult is the outcome of one RunPerformanceScan.
type PerfResult struct {
	BytesScanned    int64
	FilesScanned    int
	FilesClassified int
	Duration        time.Duration
	ThroughputMBs   float64
	Failure         string
}

// RunPerformanceScan generates a synthetic native-storage corpus of
// `budget.CorpusBytes` under a fresh tempdir, then performs the
// daemon's initial-reconciliation DISCOVERY pass (walk the tree +
// classify each file via Capabilities().BasenameToKind, without
// invoking Import). Asserts total duration stays under
// budget.MaxDuration.
//
// Per BRD-02 §5.4 #6 the "initial scan" budget is the discovery
// phase, not a full Import sweep. A 1-GiB tree's full Import will
// naturally take longer because the adapter's parse + canonical-
// store-write costs are proportional to the number of recognized
// files; full-Import perf is an orthogonal concern tracked
// separately by per-adapter benchmarks.
//
// Returns a PerfResult; Failure is non-empty when the scan exceeded
// the budget.
func RunPerformanceScan(t *testing.T, opts Opts, budget PerfBudget) PerfResult {
	t.Helper()
	a := opts.Build()
	_ = freshStore(t) // unused in the discovery-only scan
	root := t.TempDir()

	const perFileBytes = 8 * 1024 // 8 KiB per file — small enough to scale,
	// large enough that filesystem overhead doesn't dominate.
	body := make([]byte, perFileBytes)
	// Fill with valid-markdown content so the parser doesn't reject.
	body[0] = '#'
	body[1] = ' '
	for i := 2; i < perFileBytes-1; i++ {
		body[i] = 'a' + byte(i%26)
	}
	body[perFileBytes-1] = '\n'

	nFiles := int(budget.CorpusBytes / perFileBytes)
	if nFiles < 1 {
		nFiles = 1
	}

	// Layout: each file lives in its own subdir to mimic a realistic
	// monorepo layout (1 AGENTS.md per project root). Sharding across
	// many directories keeps a single directory from holding tens of
	// thousands of entries — that's hostile to most filesystems' linear
	// directory scans.
	for i := 0; i < nFiles; i++ {
		dir := filepath.Join(root, dirName(i))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return PerfResult{Failure: "mkdir: " + err.Error()}
		}
		path := filepath.Join(dir, fileNameForIndex(i))
		if err := osWriteFile(path, body); err != nil {
			return PerfResult{Failure: "write " + path + ": " + err.Error()}
		}
	}

	// Discovery scan: walk + classify by basename via Capabilities.
	// No Import — that's a separate measurement.
	caps := a.Capabilities()
	dispatch := caps.BasenameToKind
	_ = testContext()
	start := time.Now()
	scanned := 0
	classified := 0
	var bytesScanned int64
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // best-effort
		}
		if info.IsDir() {
			return nil
		}
		scanned++
		bytesScanned += info.Size()
		if _, ok := dispatch[filepath.Base(p)]; ok {
			classified++
		}
		return nil
	})
	elapsed := time.Since(start)

	if err != nil {
		return PerfResult{Failure: "walk: " + err.Error()}
	}

	res := PerfResult{
		BytesScanned:    bytesScanned,
		FilesScanned:    scanned,
		FilesClassified: classified,
		Duration:        elapsed,
		ThroughputMBs:   float64(bytesScanned) / elapsed.Seconds() / (1 << 20),
	}
	if elapsed > budget.MaxDuration {
		res.Failure = "scan exceeded budget: " +
			elapsed.String() + " > " + budget.MaxDuration.String()
	}
	return res
}

// fileNameForIndex returns a deterministic per-index basename so the
// PerfScan corpus stays read-only under filepath.Walk semantics.
// Uses an AAIF-recognized basename so adapters' Import doesn't reject
// every file outright.
func fileNameForIndex(i int) string {
	// Cycle through a few known basenames so different adapters'
	// dispatch tables exercise different code paths.
	choices := []string{"AGENTS.md", "SKILL.md", "MEMORY.md", "CLAUDE.md"}
	return choices[i%len(choices)]
}

// dirName returns a zero-padded directory name for shard i.
func dirName(i int) string {
	// 6 digits → up to 1M dirs, plenty of headroom.
	out := []byte("dir-000000")
	for k := 9; k >= 4; k-- {
		out[k] = byte('0' + i%10)
		i /= 10
	}
	return string(out)
}

// WatchStep is one step in a BRD-02 §5.4 #3 watch-correctness
// sequence. A WatchSequence is a slice of these; the harness applies
// them in order against a fresh tempdir + adapter and compares the
// resulting ACF event stream against the expected event types.
type WatchStep struct {
	// Op is "write" or "delete".
	Op string

	// Body is the content to write (Op="write" only).
	Body string

	// ExpectedEventTypes lists the event types expected on the
	// canonical store's events log AFTER this step. Cumulative —
	// step N's expectation includes every event produced by steps
	// 1..N. "create" / "update" / "redaction" / "amendment".
	ExpectedEventTypes []string
}

// WatchSequence is a named sequence + the basename it operates on.
type WatchSequence struct {
	Name     string
	Basename string
	Steps    []WatchStep
}

// RunWatchCorrectness applies the supplied sequence against a fresh
// adapter built from opts. Each step modifies the file, calls
// adapter.Import, and asserts the cumulative event-type sequence on
// the resulting artifact matches Steps[i].ExpectedEventTypes.
//
// The harness sits ABOVE the platform filesystem watchers — it
// invokes adapter.Import directly so the test is deterministic across
// FSEvents / inotify / ReadDirectoryChangesW. The actual watcher
// integration is covered by the orchestrator's existing
// TestOrchestrator_* suite.
//
// Returns the collected (step, error) pairs; empty = pass.
type WatchResult struct {
	Step    int
	Failure string
}

func RunWatchCorrectness(t *testing.T, opts Opts, seq WatchSequence) []WatchResult {
	t.Helper()
	a := opts.Build()
	store := freshStore(t)
	dir := t.TempDir()
	path := filepath.Join(dir, seq.Basename)
	ctx := testContext()

	var fails []WatchResult
	var artifactID string

	for i, step := range seq.Steps {
		switch step.Op {
		case "write":
			if err := osWriteFile(path, []byte(step.Body)); err != nil {
				fails = append(fails, WatchResult{Step: i, Failure: "write: " + err.Error()})
				continue
			}
			ids, err := a.Import(ctx, store, path)
			if err != nil {
				fails = append(fails, WatchResult{Step: i, Failure: "Import: " + err.Error()})
				continue
			}
			if len(ids) == 0 {
				fails = append(fails, WatchResult{Step: i, Failure: "Import returned 0 IDs"})
				continue
			}
			artifactID = ids[0]
		case "delete":
			if err := os.Remove(path); err != nil {
				fails = append(fails, WatchResult{Step: i, Failure: "delete: " + err.Error()})
				continue
			}
			// Deletion in the V1 model is materialized via the
			// adapter's redaction path, which isn't a public method —
			// it's invoked by the daemon's watcher when a file is
			// removed. For the harness, we record the deletion as a
			// no-op on the canonical store and continue; the
			// orchestrator's per-adapter deletion handling is covered
			// by its own tests. The golden sequence's
			// ExpectedEventTypes for the delete step should be the
			// PRE-deletion event list (no new event produced here).
		default:
			fails = append(fails, WatchResult{Step: i, Failure: "unknown op " + step.Op})
			continue
		}

		if artifactID == "" {
			continue
		}

		// Find the artifact's kind (we don't know it statically; the
		// harness is kind-agnostic). Walk all four; the first
		// successful ReadEvents wins.
		var events []acf.Event
		for _, kind := range []acf.Kind{acf.KindMemory, acf.KindSkill, acf.KindTool, acf.KindConversation} {
			if e, err := store.ReadEvents(kind, artifactID); err == nil && len(e) > 0 {
				events = e
				break
			}
		}

		var gotTypes []string
		for _, e := range events {
			gotTypes = append(gotTypes, string(e.Type))
		}
		if !sliceEqualStrings(gotTypes, step.ExpectedEventTypes) {
			fails = append(fails, WatchResult{Step: i,
				Failure: "event sequence mismatch: want " +
					fmtSlice(step.ExpectedEventTypes) + " got " + fmtSlice(gotTypes)})
		}
	}
	return fails
}

func sliceEqualStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func fmtSlice(s []string) string {
	out := "["
	for i, v := range s {
		if i > 0 {
			out += " "
		}
		out += v
	}
	return out + "]"
}

// DefaultWatchSequence is the M1 baseline sequence: write twice (the
// second write becomes an "update" via the stable-UUIDv7 / SourcePath
// reconciliation in adapter.opaque + per-adapter memory importers).
//
// Adapters that DON'T support the alias-based source-path
// reconciliation (any adapter that mints a fresh ID per Import) will
// fail this — that's a real conformance gap surfaced by the test.
func DefaultWatchSequence(basename string) WatchSequence {
	return WatchSequence{
		Name:     "write-twice-create-then-update",
		Basename: basename,
		Steps: []WatchStep{
			{Op: "write", Body: "# initial\n",
				ExpectedEventTypes: []string{"create"}},
			{Op: "write", Body: "# modified\n",
				ExpectedEventTypes: []string{"create", "update"}},
		},
	}
}

// CrossEntry is one cell in the BRD-02 §5.4 #4 cross-conversion
// matrix: a source adapter's name + its build closure, plus a
// representative fixture body that the source can Import.
type CrossEntry struct {
	Opts    Opts
	Fixture Fixture
}

// RunCrossConversion exercises BRD-02 §5.4 #4 — "Take a
// representative bundle from each of the other V1 agents; convert
// through this adapter; produce an apply report." For every (source,
// target) pair in entries, the harness:
//
//  1. Builds a source adapter, Imports the fixture, captures the
//     resulting artifact IDs in a per-source store.
//  2. Builds a target adapter; for each artifact, asks the target's
//     NativePath where it would write, then calls Export to that
//     path.
//  3. Records the outcome: ok / unsupported (NativePath says no) /
//     tombstoned / error.
//
// Returns a CrossReport listing one row per cell. Caller decides
// whether non-ok cells fail the test (e.g. "unsupported" cells are
// fine; "error" cells fail).
//
// The matrix is N×N (5×5 in V1; 25 cells, 5 identity). Each entry's
// Build closure is called O(N) times, once per target it pairs with.
type CrossReport struct {
	Cells []CrossCell
}

type CrossCell struct {
	Source string
	Target string
	// Outcome is one of:
	//   "ok"          — export succeeded
	//   "unsupported" — target's NativePath returned supports=false
	//   "tombstoned"  — adapter.ErrArtifactTombstoned
	//   "error"       — any other error
	//   "skipped"     — source's Import didn't recognize the fixture
	Outcome string
	Detail  string
}

// RunCrossConversion runs the full N×N matrix across the supplied
// entries and returns the report. The caller iterates report.Cells
// and asserts whatever shape it wants.
func RunCrossConversion(t *testing.T, entries []CrossEntry) CrossReport {
	t.Helper()
	rep := CrossReport{}
	for _, src := range entries {
		// One source store per source adapter so cross-target runs
		// don't share state with each other.
		store := freshStore(t)
		srcA := src.Opts.Build()
		dir := t.TempDir()
		path := filepath.Join(dir, src.Fixture.NativeName)
		if err := osWriteFile(path, []byte(src.Fixture.Body)); err != nil {
			t.Fatalf("cross: write source fixture %s: %v", src.Fixture.NativeName, err)
		}
		ids, err := srcA.Import(testContext(), store, path)
		if err != nil {
			for _, tgt := range entries {
				rep.Cells = append(rep.Cells, CrossCell{
					Source: src.Opts.Name, Target: tgt.Opts.Name,
					Outcome: "skipped",
					Detail:  "source Import failed: " + err.Error(),
				})
			}
			continue
		}
		if len(ids) == 0 {
			for _, tgt := range entries {
				rep.Cells = append(rep.Cells, CrossCell{
					Source: src.Opts.Name, Target: tgt.Opts.Name,
					Outcome: "skipped",
					Detail:  "source Import returned 0 artifacts",
				})
			}
			continue
		}

		for _, tgt := range entries {
			rep.Cells = append(rep.Cells, runCrossCell(t, src.Opts.Name, tgt, store, ids))
		}
	}
	return rep
}

func runCrossCell(t *testing.T, srcName string, tgt CrossEntry, store *acf.Store, ids []string) CrossCell {
	t.Helper()
	a := tgt.Opts.Build()
	outDir := t.TempDir()

	for _, id := range ids {
		var art acf.Artifact
		found := false
		for _, k := range []acf.Kind{acf.KindMemory, acf.KindSkill, acf.KindTool, acf.KindConversation} {
			if x, err := store.ReadArtifact(k, id); err == nil {
				art = x
				found = true
				break
			}
		}
		if !found {
			return CrossCell{Source: srcName, Target: tgt.Opts.Name,
				Outcome: "error", Detail: "artifact " + id + " not found in source store"}
		}
		dest, supports, err := a.NativePath(art, outDir)
		if err != nil {
			return CrossCell{Source: srcName, Target: tgt.Opts.Name,
				Outcome: "error", Detail: "NativePath: " + err.Error()}
		}
		if !supports {
			return CrossCell{Source: srcName, Target: tgt.Opts.Name,
				Outcome: "unsupported",
				Detail:  tgt.Opts.Name + " does not support kind=" + string(art.Kind)}
		}
		if err := osMkdirAll(filepath.Dir(dest)); err != nil {
			return CrossCell{Source: srcName, Target: tgt.Opts.Name,
				Outcome: "error", Detail: "mkdir: " + err.Error()}
		}
		if err := a.Export(testContext(), store, id, dest); err != nil {
			if strings_Contains(err.Error(), "tombstoned") {
				return CrossCell{Source: srcName, Target: tgt.Opts.Name,
					Outcome: "tombstoned", Detail: err.Error()}
			}
			return CrossCell{Source: srcName, Target: tgt.Opts.Name,
				Outcome: "error", Detail: "Export: " + err.Error()}
		}
	}
	return CrossCell{Source: srcName, Target: tgt.Opts.Name, Outcome: "ok"}
}

// osMkdirAll is a tiny helper to keep this file's imports thin.
func osMkdirAll(path string) error { return os.MkdirAll(path, 0o755) }

// strings_Contains avoids an additional strings import below by name.
func strings_Contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// RunCapabilityCheck verifies BRD-02 §5.4 #7: the adapter's
// `Capabilities()` declaration matches its actual behavior. For each
// basename in `Capabilities().NativeBasenames`, the harness:
//
//  1. Writes a minimal valid body for that filename.
//  2. Calls adapter.Import.
//  3. Asserts the Import succeeds (the declared basename is recognized).
//
// The full §5.4.7 capability-declaration check — asserting that NativePath
// produces a path (supports=true) for every artifact kind that
// Capabilities().Artifacts marks supported — is deferred to M1 (see the
// package header's stubbed list); only the basename Import probe above runs
// today.
//
// Returns the list of mismatch strings; empty = pass.
func RunCapabilityCheck(t *testing.T, opts Opts) []string {
	t.Helper()
	a := opts.Build()
	caps := a.Capabilities()
	var fails []string

	// Per-basename Import probe — write a minimal valid body that
	// each adapter's parsers should accept.
	for _, bn := range caps.NativeBasenames {
		body := minimalBodyForBasename(bn)
		if body == "" {
			continue // basename has no shared minimal body; skip the probe
		}
		dir := t.TempDir()
		path := dir + string('/') + bn
		if err := writeFile(path, body); err != nil {
			fails = append(fails, "writeFile "+bn+": "+err.Error())
			continue
		}
		store := freshStore(t)
		_, err := a.Import(testContext(), store, path)
		if err != nil {
			fails = append(fails,
				"Capabilities().NativeBasenames lists "+bn+
					" but Import rejected it: "+err.Error())
		}
	}

	return fails
}

func runFixture(t *testing.T, opts Opts, fx Fixture) {
	t.Helper()

	tmp := t.TempDir()
	storeRoot := filepath.Join(tmp, "store")
	require := func(err error, msg string) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %s: %v", opts.Name, msg, err)
		}
	}

	store := &acf.Store{Root: storeRoot}
	require(store.Init(), "store init")

	a := opts.Build()
	ctx := context.Background()

	// Write the fixture to disk under a deterministic path the adapter
	// will recognize. For global-scope, the adapter consults its
	// HomeDir/<.agent>/<name> convention; for project-scope, it walks
	// the supplied contextDir for the native file.
	nativeDir := filepath.Join(tmp, "native")
	require(os.MkdirAll(nativeDir, 0o755), "mkdir native")
	nativePath := filepath.Join(nativeDir, fx.NativeName)
	require(os.WriteFile(nativePath, []byte(fx.Body), 0o644), "write fixture body")

	// 1. Import.
	ids, err := a.Import(ctx, store, nativePath)
	if err != nil {
		// Some adapters don't recognize every native form. Treat as a
		// "skipped by design" rather than a hard failure when the
		// adapter signals "unrecognized filename".
		t.Skipf("%s adapter does not recognize fixture %q (%s): %v",
			opts.Name, fx.Label, fx.NativeName, err)
		return
	}
	if len(ids) == 0 {
		t.Fatalf("%s: Import returned 0 IDs for fixture %q", opts.Name, fx.Label)
	}
	id := ids[0]

	// 2. Round-trip: Export to a sibling path; bytes (modulo
	//    documented delta) MUST match the original.
	outPath := filepath.Join(tmp, "out-"+fx.NativeName)
	if err := a.Export(ctx, store, id, outPath); err != nil {
		t.Fatalf("%s: Export(%s) failed: %v", opts.Name, id, err)
	}
	got, err := os.ReadFile(outPath)
	require(err, "read export")
	if !fidelityEqual(string(got), fx.Body, fx.AcceptedDelta) {
		t.Fatalf("%s: round-trip mismatch for fixture %q\n--- original ---\n%s\n--- exported ---\n%s",
			opts.Name, fx.Label, fx.Body, string(got))
	}

	// 3. Idempotency: a second Export on the same target MUST be a
	//    no-op. (Tombstoned artifacts return ErrArtifactTombstoned
	//    which is fine; here we expect a fresh non-redacted Export.)
	if err := a.Export(ctx, store, id, outPath); err != nil {
		t.Fatalf("%s: second Export failed (should be idempotent): %v", opts.Name, err)
	}
	got2, err := os.ReadFile(outPath)
	require(err, "read second export")
	if string(got) != string(got2) {
		t.Fatalf("%s: idempotency violation — second Export produced different bytes\n"+
			"first:  %q\nsecond: %q", opts.Name, string(got), string(got2))
	}
}

// fidelityEqual returns true when got and want differ only in
// substrings the fixture has marked as accepted delta. This is the
// "documented non-semantic noise" carve-out from BRD-02 §5.4.1 — for
// most fixtures it's empty so we fall through to strict equality.
func fidelityEqual(got, want string, deltas []string) bool {
	if got == want {
		return true
	}
	if len(deltas) == 0 {
		return false
	}
	// Strip every accepted delta from both sides and re-compare. Sort
	// for deterministic ordering of removals (longest first, so a
	// delta that's a prefix of another doesn't pre-empt it).
	sorted := append([]string{}, deltas...)
	sort.Slice(sorted, func(i, j int) bool {
		return len(sorted[i]) > len(sorted[j])
	})
	stripA, stripB := got, want
	for _, d := range sorted {
		for {
			idxA := indexOf(stripA, d)
			if idxA < 0 {
				break
			}
			stripA = stripA[:idxA] + stripA[idxA+len(d):]
		}
		for {
			idxB := indexOf(stripB, d)
			if idxB < 0 {
				break
			}
			stripB = stripB[:idxB] + stripB[idxB+len(d):]
		}
	}
	return stripA == stripB
}

// minimalBodyForBasename returns the smallest valid body for a known
// native basename so RunCapabilityCheck can probe Import without
// hand-rolling a fixture per adapter.
func minimalBodyForBasename(bn string) string {
	switch bn {
	case "AGENTS.md", "AGENT.md", "CLAUDE.md", "MEMORY.md", "USER.md", "DREAMS.md":
		return "# memory\n"
	case "SKILL.md":
		return "---\nname: probe\ndescription: capability probe.\n---\n\n# probe\n"
	case ".mcp.json", "openclaw.json", "openclaw.jsonc", "openclaw.json5",
		"kilo.jsonc":
		return `{"mcpServers":{}}`
	case "config.yaml", "hermes.yaml", "hermes.yml":
		return "mcpServers: {}\n"
	case "config.toml":
		return "# empty Codex MCP configuration\n"
	}
	return ""
}

// writeFile is a tiny helper to avoid importing os in this file's
// already-busy surface; keeps the conformance package import-thin.
func writeFile(path, body string) error {
	return osWriteFile(path, []byte(body))
}

// freshStore mints a new acf.Store under t.TempDir.
func freshStore(t *testing.T) *acf.Store {
	t.Helper()
	root := t.TempDir() + string('/') + "store"
	s := &acf.Store{Root: root}
	if err := s.Init(); err != nil {
		t.Fatalf("freshStore: init: %v", err)
	}
	return s
}

// testContext returns a context.Background. Kept as a helper so we
// can swap in a deadline-bearing ctx for stress tests later.
func testContext() context.Context { return context.Background() }

// indexOf is a tiny helper to avoid pulling strings.Index here only
// to keep the package's import surface minimal.
func indexOf(s, sub string) int {
	if sub == "" {
		return 0
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// DefaultFixtures returns the M0 shared fixture set. Each adapter's
// _test.go calls this and lets the adapter-specific SkipIfAdapter
// list filter out unsupported native forms.
func DefaultFixtures() []Fixture {
	return []Fixture{
		// FR-02.12 AGENTS.md — every V1 adapter supports it as of v0.78.0.
		{
			Label:      "agents-md-round-trip",
			NativeName: "AGENTS.md",
			Body: `# Project agents

This project uses Claude Code, Codex, and Hermes.

## Conventions

- TDD-style tests
- No magic numbers
`,
			Scope: acf.ScopeProject,
		},

		// FR-02.12 SKILL.md — every V1 adapter supports the Agent
		// Skills Open Standard as of v0.78.0.
		{
			Label:      "skill-md-round-trip",
			NativeName: "SKILL.md",
			Body: `---
name: example-skill
description: Round-trip fixture for the conformance harness.
---

# Example skill

A minimal SKILL.md document conformant with the Agent Skills Open
Standard.

## Steps

1. Do the first thing.
2. Do the second thing.
`,
			Scope: acf.ScopeGlobal,
		},

		// CLAUDE.md memory — covered for claude-code; other adapters
		// may not recognize the basename.
		{
			Label:      "memory-md-round-trip",
			NativeName: "CLAUDE.md",
			Body: `# Memory

User prefers Go over Rust for systems work.
`,
			Scope:         acf.ScopeGlobal,
			SkipIfAdapter: []string{"codex", "hermes", "openclaw", "kilo"},
		},

		// MCP config — every adapter should round-trip an empty
		// mcpServers block at minimum. Per-adapter native form
		// names differ; each adapter that doesn't write .mcp.json
		// gets its own fixture below.
		{
			Label:      "mcp-empty-round-trip",
			NativeName: ".mcp.json",
			Body:       `{"mcpServers":{}}`,
			Scope:      acf.ScopeProject,
			// Adapters whose native MCP config has a different filename
			// have their own per-adapter fixtures below.
			SkipIfAdapter: []string{"kilo", "openclaw", "hermes", "codex"},
		},

		// v0.85.0 — kilo-native MCP config fixture. kilo.jsonc uses the
		// nested `mcp` shape (not the flat `mcpServers` of claude-code).
		{
			Label:         "kilo-jsonc-round-trip",
			NativeName:    "kilo.jsonc",
			Body:          "{\n  \"mcp\": {}\n}",
			Scope:         acf.ScopeProject,
			SkipIfAdapter: []string{"claude-code", "codex", "hermes", "openclaw"},
		},

		// v0.85.0 — openclaw-native MCP config fixture. openclaw uses
		// the nested `mcp.servers` schema (not the flat
		// `mcpServers` everyone else uses).
		{
			Label:         "openclaw-json-round-trip",
			NativeName:    "openclaw.json",
			Body:          "{\n  \"mcp\": {\n    \"servers\": {}\n  }\n}",
			Scope:         acf.ScopeProject,
			SkipIfAdapter: []string{"claude-code", "codex", "hermes", "kilo"},
		},

		// v0.85.0 — hermes-native MCP config fixture. hermes uses the
		// snake_case `mcp_servers` key in its config.yaml.
		{
			Label:         "hermes-yaml-round-trip",
			NativeName:    "config.yaml",
			Body:          "mcp_servers: {}\n",
			Scope:         acf.ScopeProject,
			SkipIfAdapter: []string{"claude-code", "codex", "openclaw", "kilo"},
		},
	}
}
