package main

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

type policyFinding struct {
	path    string
	line    int
	message string
}

// hostedRunnerLabel matches a GitHub-hosted runner label and nothing else:
// `ubuntu-latest`, `macos-15`, `windows-2022`, `macos-14-large`,
// `ubuntu-24.04-arm`. Anything carrying a `${{` expression, a bare
// `self-hosted`, or a custom runner label fails it, which is the point — see
// runnerIsHostedLiteral.
//
// The VERSION SEGMENT IS MANDATORY, and that is the whole strength of the
// pattern. Every hosted image label is `<os>-<latest|version>[-variant]`;
// nothing GitHub hosts is named bare `macos`, `ubuntu` or `windows`, and
// nothing hosted has a word where the version belongs. Meanwhile GitHub
// auto-labels every self-hosted runner with `self-hosted` PLUS its OS
// (`macOS`/`Windows`/`Linux`) PLUS its arch, and label matching is
// case-insensitive — so `runs-on: macos` can route to a persistent runner
// without the string `self-hosted` ever appearing. With an
// optional suffix group that scanned clean; `macos`, `macos-mini`,
// `windows-box` and `ubuntu-wsl` are all rejected now.
var hostedRunnerLabel = regexp.MustCompile(`^(ubuntu|macos|windows)-(latest|[0-9]+(\.[0-9]+)?)(-[0-9a-z]+)*$`)

// untrustedContext matches the expression contexts an outsider can influence.
// `github.event.*` carries PR titles, branch names and commit messages;
// `github.head_ref` is a fork's branch name; `github.actor` is a login. Any of
// them interpolated into a `run:` block is pasted into the script before bash
// ever sees a quote — the classic GitHub Actions script-injection RCE.
//
// The word boundaries carry weight. `github.event_name` is an enumerated value
// and must not match; `github.actor_id` is numeric and must not match either.
// Only `github.event.` with its trailing dot, and the three whole-word
// contexts, do.
var untrustedContext = regexp.MustCompile(`\bgithub\.(?:event\.|head_ref\b|actor\b|triggering_actor\b)`)

// bracketDeref matches the index form of a property access. The Actions
// expression language treats `github['event'].head_commit.message` as exactly
// equivalent to the dot form, so a deny-list that only knows dots has a
// one-character bypass. normalizeDeref rewrites the index form to the dot form
// before untrustedContext ever sees it.
var bracketDeref = regexp.MustCompile(`\s*\[\s*['"]([A-Za-z_][A-Za-z0-9_-]*)['"]\s*\]`)

func normalizeDeref(expression string) string {
	// Repeat to collapse chains like `github['event']['head_commit']`: the
	// first pass turns the trailing index into `.head_commit`, and only then
	// does the leading one become `.event`. One pass rewrites both here, but
	// looping keeps the function total for nested forms regexp cannot chew in
	// a single sweep.
	for {
		next := bracketDeref.ReplaceAllString(expression, ".$1")
		if next == expression {
			return expression
		}
		expression = next
	}
}

// expressionPattern isolates each `${{ ... }}`, so a deny-list hit stays caught
// inside a shell comment or a quoted literal, while prose in a heredoc that
// merely mentions github.actor outside an expression does not fire.
var expressionPattern = regexp.MustCompile(`\$\{\{[^}]*(?:\}[^}][^}]*)*\}\}`)

func mapValue(node *yaml.Node, key string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	return nil
}

func hasTrigger(on *yaml.Node, name string) bool {
	if on == nil {
		return false
	}
	if on.Kind == yaml.ScalarNode {
		return on.Value == name
	}
	if on.Kind == yaml.SequenceNode {
		for _, value := range on.Content {
			if value.Value == name {
				return true
			}
		}
	}
	return mapValue(on, name) != nil
}

// hasTagFilter reports whether the `push` trigger is narrowed to tags. A tag
// filter is what distinguishes a release workflow from an ordinary branch-push
// CI workflow: only the former runs at a `refs/tags/vX.Y.Z` ref, and only that
// ref can satisfy a release role's exact OIDC trust policy.
// `tags-ignore` counts too — it still means "run on tag pushes", just not all
// of them. A bare `push:` or `push: branches:` is not tag-shaped, so test.yml
// and security.yml stay outside this rule family.
func hasTagFilter(on *yaml.Node) bool {
	push := mapValue(on, "push")
	if push == nil {
		return false
	}
	return mapValue(push, "tags") != nil || mapValue(push, "tags-ignore") != nil
}

func containsSelfHosted(node *yaml.Node) bool {
	if node == nil {
		return false
	}
	if node.Kind == yaml.ScalarNode {
		return strings.Contains(strings.ToLower(node.Value), "self-hosted")
	}
	for _, child := range node.Content {
		if containsSelfHosted(child) {
			return true
		}
	}
	return false
}

// runnerIsHostedLiteral is an allow-list, deliberately. Rejecting the literal
// string "self-hosted" is not enough on its own: `runs-on: ${{ matrix.os }}`
// can carry any label a matrix names, and GitHub's runner-group form
// `runs-on: {group: persistent-macos, labels: [macos]}` targets a self-hosted
// runner group without ever spelling the label. Both sail straight through a
// deny-list; this allow-list is what keeps an unnamed privileged job from
// landing on an arbitrary runner. The one permitted indirection is a matrix
// expression that resolves to hosted literals only — see
// runnerIsHostedMatrixExpression.
func runnerIsHostedLiteral(node *yaml.Node) bool {
	if node == nil {
		return false
	}
	switch node.Kind {
	case yaml.ScalarNode:
		return hostedRunnerLabel.MatchString(strings.ToLower(strings.TrimSpace(node.Value)))
	case yaml.SequenceNode:
		// An empty sequence names no runner at all: unpinned, not vacuously ok.
		if len(node.Content) == 0 {
			return false
		}
		for _, child := range node.Content {
			if child.Kind != yaml.ScalarNode {
				return false
			}
			if !hostedRunnerLabel.MatchString(strings.ToLower(strings.TrimSpace(child.Value))) {
				return false
			}
		}
		return true
	default:
		// A MappingNode is the `group:` form. There is no hosted runner group.
		return false
	}
}

// requiredReleaseRunner returns the exact runs-on literal a named release.yml
// job must pin. Build and publish need macOS (the darwin targets require cgo,
// and publish repeats build's rebuild); every other job is ubuntu. These are
// literal GitHub-hosted labels — never an expression, a custom label, or a
// runner group. Unknown job names return "" and fall through to the
// hosted-literal allow-list so a new privileged job cannot silently take an
// unreviewed runner.
func requiredReleaseRunner(jobName string) string {
	switch jobName {
	case "build", "publish":
		return "macos-latest"
	case "guard", "sign", "verify", "tap":
		return "ubuntu-latest"
	default:
		return ""
	}
}

// runnerIsHostedMatrixExpression resolves the one permitted indirection:
// `runs-on: ${{ matrix.os }}` backed by a strategy matrix whose every `os`
// entry is a GitHub-hosted literal. Anything else — a different expression, a
// matrix with a custom label, an `include:` that could smuggle another os
// value into the expansion, or a missing matrix — is not hosted-resolvable
// and fails the allow-list.
func runnerIsHostedMatrixExpression(job *yaml.Node, runner *yaml.Node) bool {
	if runner == nil || runner.Kind != yaml.ScalarNode {
		return false
	}
	if strings.TrimSpace(runner.Value) != "${{ matrix.os }}" {
		return false
	}
	strategy := mapValue(job, "strategy")
	matrix := mapValue(strategy, "matrix")
	if matrix == nil {
		return false
	}
	if mapValue(matrix, "include") != nil {
		return false
	}
	return runnerIsHostedLiteral(mapValue(matrix, "os"))
}

func walkScalars(node *yaml.Node, fn func(*yaml.Node)) {
	if node == nil {
		return
	}
	if node.Kind == yaml.ScalarNode {
		fn(node)
	}
	for _, child := range node.Content {
		walkScalars(child, fn)
	}
}

// stepRuns returns the `run:` scalar of every step in a job. Only `run:` is a
// shell; `if:` and `with:` are evaluated by the Actions expression engine and
// never reach bash, which is why cla.yml's `github.event.pull_request != null`
// job guard is correctly out of scope for the injection rule.
func stepRuns(job *yaml.Node) []*yaml.Node {
	steps := mapValue(job, "steps")
	if steps == nil || steps.Kind != yaml.SequenceNode {
		return nil
	}
	var out []*yaml.Node
	for _, step := range steps.Content {
		if run := mapValue(step, "run"); run != nil && run.Kind == yaml.ScalarNode {
			out = append(out, run)
		}
	}
	return out
}

func interpolatesUntrusted(script string) bool {
	for _, expression := range expressionPattern.FindAllString(script, -1) {
		if untrustedContext.MatchString(normalizeDeref(expression)) {
			return true
		}
	}
	return false
}

// blanketWrite reports whether a permissions block is the scalar `write-all`.
// That is the maximal grant — every scope GitHub has, including `actions:`,
// `packages:` and `deployments:` — and nothing in this repository has a use for
// it. It is a categorically different thing from a scoped `contents: write`,
// which a release job legitimately needs, so it is checked separately and
// without regard to how the workflow is triggered.
func blanketWrite(node *yaml.Node) bool {
	return node != nil && node.Kind == yaml.ScalarNode && node.Value == "write-all"
}

// permissionWrites reports whether a fine-grained permissions mapping grants
// any write scope. Blanket `write-all` is handled by blanketWrite.
func permissionWrites(node *yaml.Node) bool {
	if node == nil || node.Kind != yaml.MappingNode {
		return false
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i+1].Value == "write" {
			return true
		}
	}
	return false
}

func scanWorkflow(path string, data []byte) ([]policyFinding, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	if len(doc.Content) == 0 {
		return nil, nil
	}
	root := doc.Content[0]
	on := mapValue(root, "on")
	untrusted := hasTrigger(on, "pull_request") || hasTrigger(on, "pull_request_target") || hasTrigger(on, "issue_comment")
	// A tag-triggered push (or a `release` event) is the other end of the risk
	// spectrum from an untrusted trigger: nothing contributor-controlled starts
	// it, but it is the only workflow allowed to request the OIDC token whose
	// workflow identity external signing policy and public provenance bind. Before this
	// clause the scanner returned any non-untrusted workflow unscanned, so the
	// single highest-privilege file in the repo got no policy review at all.
	privileged := (hasTrigger(on, "push") && hasTagFilter(on)) || hasTrigger(on, "release")
	if !untrusted && !privileged {
		return nil, nil
	}
	var out []policyFinding
	if p := mapValue(root, "permissions"); p != nil {
		switch {
		case blanketWrite(p):
			// Unconditional, and that is the whole point. release.yml is the one
			// file here that already holds contents/id-token write.
			// Escalating its root block to write-all hands every scope to every
			// job in it; while this rule was untrusted-only that edit scanned
			// clean, and packaging/scripts/test-installer-security.sh does not
			// notice it either — its `require_fixed contents: write` is satisfied
			// by the release job's own block.
			out = append(out, policyFinding{path, p.Line, "workflow declares blanket write-all permission"})
		case untrusted && permissionWrites(p):
			// Scoped writes stay untrusted-only: a release workflow legitimately
			// needs `contents: write`, and release.yml follows the house pattern
			// of root `contents: read` with the writes declared per job.
			//
			// This one is deliberately ROOT-ONLY, unlike blanketWrite, which is
			// checked at both levels. Do not "fix" the asymmetry: cla.yml is
			// pull_request_target- and issue_comment-triggered and legitimately
			// holds job-level `actions: write` / `pull-requests: write` /
			// `statuses: write` (cla.yml:32-36), so extending this arm to job
			// level would red the Security workflow on the next run.
			out = append(out, policyFinding{path, p.Line, "untrusted workflow has workflow-level write permission"})
		}
	}
	if privileged && (hasTrigger(on, "pull_request") || hasTrigger(on, "pull_request_target")) {
		out = append(out, policyFinding{path, on.Line, "tag-triggered release workflow also accepts an untrusted trigger"})
	}
	if privileged && hasTrigger(on, "workflow_dispatch") {
		// CANONICAL-CONTRACT.md §5 forbids this trigger on the release workflow
		// by name. A dispatched branch run cannot satisfy the exact tag/workflow
		// identity required by the release signing role and provenance policy.
		out = append(out, policyFinding{path, on.Line, "tag-triggered release workflow also accepts workflow_dispatch; a dispatched branch run cannot satisfy the release tag/workflow identity"})
	}
	jobs := mapValue(root, "jobs")
	if jobs == nil || jobs.Kind != yaml.MappingNode {
		return out, nil
	}
	for i := 0; i+1 < len(jobs.Content); i += 2 {
		// jobLine anchors a finding for a job that names no runner node at all;
		// there is nothing else with a line number to point at.
		jobLine := jobs.Content[i].Line
		job := jobs.Content[i+1]
		if p := mapValue(job, "permissions"); blanketWrite(p) {
			out = append(out, policyFinding{path, p.Line, "job declares blanket write-all permission"})
		}
		runner := mapValue(job, "runs-on")
		// Untrusted workflows are held to the same hosted allow-list as
		// privileged ones, with one extra permitted shape: this repository's
		// own house style of `runs-on: ${{ matrix.os }}` resolved against a
		// hosted-only matrix (test.yml, security.yml). A job-level `uses:`
		// has no runner of its own and is out of scope here; the privileged
		// arm below rejects delegation where it matters.
		if untrusted && mapValue(job, "uses") == nil {
			switch {
			case containsSelfHosted(runner):
				out = append(out, policyFinding{path, runner.Line, "untrusted trigger reaches self-hosted runner"})
			case !runnerIsHostedLiteral(runner) && !runnerIsHostedMatrixExpression(job, runner):
				runnerLine := jobLine
				if runner != nil {
					runnerLine = runner.Line
				}
				out = append(out, policyFinding{path, runnerLine, "untrusted workflow job does not pin a GitHub-hosted runner"})
			}
		}
		// Named release jobs must pin their exact hosted literal — never an
		// expression, a custom label, or a runner group.
		// The arms are exclusive so a single job never reports the same
		// node twice; later arms catch every other way of reaching an
		// unapproved runner — INCLUDING a job that declares no `runs-on`
		// at all, which used to be exempt outright.
		//
		// The first arm is the delegation escape. A job body of
		// `uses: Aplexica/Aplexica/.github/workflows/build.yml@<40hex>` has no
		// `runs-on`, and the callee — triggered by `workflow_call` — is neither
		// untrusted nor privileged, so scanWorkflow returns it unscanned. That
		// combination silently switched off this entire rule family for the one
		// job holding the OIDC token, and tools/actionpin accepts the pinned
		// call. It is rejected rather than followed because the split is fatal
		// on its own terms: external authorities bind the workflow path from
		// `job_workflow_ref`, i.e. the file the JOB is defined in, so it would
		// no longer match the repository's release policy or provenance.
		// CANONICAL-CONTRACT.md §3 names splitting by hand for that reason.
		if privileged {
			jobName := jobs.Content[i].Value
			want := requiredReleaseRunner(jobName)
			callee := mapValue(job, "uses")
			runnerLine := jobLine
			if runner != nil {
				runnerLine = runner.Line
			}
			switch {
			case callee != nil:
				out = append(out, policyFinding{path, callee.Line, "tag-triggered release job delegates to a reusable workflow; external signing policy and public provenance bind the callee, not .github/workflows/release.yml"})
			case want != "":
				if runner == nil || runner.Kind != yaml.ScalarNode || strings.TrimSpace(runner.Value) != want {
					out = append(out, policyFinding{path, runnerLine, "tag-triggered " + jobName + " job must pin " + want})
				}
			case containsSelfHosted(runner):
				out = append(out, policyFinding{path, runner.Line, "tag-triggered release workflow reaches self-hosted runner"})
			case !runnerIsHostedLiteral(runner):
				out = append(out, policyFinding{path, runnerLine, "tag-triggered release workflow does not pin a hosted runner label"})
			}
		}
		for _, run := range stepRuns(job) {
			if interpolatesUntrusted(run.Value) {
				out = append(out, policyFinding{path, run.Line, "run step interpolates an untrusted expression; pass it through env: instead"})
			}
		}
		if hasTrigger(on, "pull_request_target") {
			steps := mapValue(job, "steps")
			walkScalars(steps, func(value *yaml.Node) {
				if strings.HasPrefix(value.Value, "actions/checkout@") || strings.HasPrefix(value.Value, "./") {
					out = append(out, policyFinding{path, value.Line, "pull_request_target executes contributor-controlled checkout or local action"})
				}
			})
		}
		if hasTrigger(on, "pull_request") {
			walkScalars(job, func(value *yaml.Node) {
				if strings.Contains(value.Value, "secrets.") {
					out = append(out, policyFinding{path, value.Line, "pull_request job references a secret"})
				}
			})
		}
	}
	return out, nil
}

func scan(root string) ([]policyFinding, error) {
	var out []policyFinding
	err := filepath.WalkDir(filepath.Join(root, ".github", "workflows"), func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || filepath.Ext(path) != ".yml" && filepath.Ext(path) != ".yaml" {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		found, err := scanWorkflow(path, data)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		out = append(out, found...)
		return nil
	})
	if os.IsNotExist(err) {
		return nil, nil
	}
	return out, err
}

func main() {
	// Defaults to the working directory, which is how security.yml invokes it.
	// An explicit root lets the same code run from a test or a pre-commit hook.
	root := "."
	if len(os.Args) > 1 {
		root = os.Args[1]
	}
	findings, err := scan(root)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	for _, item := range findings {
		fmt.Fprintf(os.Stderr, "%s:%d: %s\n", item.path, item.line, item.message)
	}
	if len(findings) > 0 {
		os.Exit(1)
	}
}
