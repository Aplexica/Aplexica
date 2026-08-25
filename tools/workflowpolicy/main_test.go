package main

import (
	"os"
	"path/filepath"
	"testing"
)

// messages flattens findings so a test can assert on what fired rather than on
// how many things fired. A bare count cannot say which rules produced it, so a
// rule can be deleted outright and a count-only test stays green.
func messages(found []policyFinding) []string {
	out := make([]string, 0, len(found))
	for _, item := range found {
		out = append(out, item.message)
	}
	return out
}

func scanFixture(t *testing.T, workflow string) []policyFinding {
	t.Helper()
	found, err := scanWorkflow("fixture.yml", []byte(workflow))
	if err != nil {
		t.Fatal(err)
	}
	return found
}

func requireMessages(t *testing.T, found []policyFinding, want ...string) {
	t.Helper()
	got := messages(found)
	if len(got) != len(want) {
		t.Fatalf("findings = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("findings = %q, want %q", got, want)
		}
	}
}

func TestUntrustedPolicyRejectsRunnerSecretAndTargetCheckout(t *testing.T) {
	found := scanFixture(t, `on:
  pull_request_target:
  pull_request:
jobs:
  unsafe:
    runs-on: [self-hosted, mac]
    steps:
      - uses: actions/checkout@0123456789abcdef0123456789abcdef01234567
        env:
          TOKEN: ${{ secrets.FOO }}
`)
	requireMessages(t, found,
		"untrusted trigger reaches self-hosted runner",
		"pull_request_target executes contributor-controlled checkout or local action",
		"pull_request job references a secret",
	)
}

func TestTagTriggeredPolicyRejectsSelfHostedRunner(t *testing.T) {
	found := scanFixture(t, `on:
  push:
    tags: ['v[0-9]+.[0-9]+.[0-9]+']
jobs:
  release:
    runs-on: [self-hosted, macOS]
    steps:
      - run: goreleaser release
`)
	requireMessages(t, found, "tag-triggered release workflow reaches self-hosted runner")
}

// A matrix expression is this repository's own house style for `runs-on`
// (test.yml, security.yml), so a deny-list on the literal string "self-hosted"
// is defeated by moving the label into the matrix.
func TestTagTriggeredPolicyRejectsMatrixExpressionRunner(t *testing.T) {
	found := scanFixture(t, `on:
  push:
    tags: ['v[0-9]+.[0-9]+.[0-9]+']
jobs:
  release:
    strategy:
      matrix:
        os: [self-hosted]
    runs-on: ${{ matrix.os }}
    steps:
      - run: goreleaser release
`)
	requireMessages(t, found, "tag-triggered release workflow does not pin a hosted runner label")
}

// GitHub's runner-group form reaches a self-hosted runner without ever writing
// the label, which is why the privileged rule is an allow-list.
func TestTagTriggeredPolicyRejectsRunnerGroup(t *testing.T) {
	found := scanFixture(t, `on:
  push:
    tags: ['v[0-9]+.[0-9]+.[0-9]+']
jobs:
  release:
    runs-on:
      group: persistent-macos
      labels: [macos]
    steps:
      - run: goreleaser release
`)
	requireMessages(t, found, "tag-triggered release workflow does not pin a hosted runner label")
}

func TestTagTriggeredPolicyAcceptsHostedRunnerLabels(t *testing.T) {
	for _, runsOn := range []string{
		"macos-latest",
		"ubuntu-latest",
		"windows-2022",
		"ubuntu-24.04",
		"macos-14-large",
		"macos-13-xlarge",
		"ubuntu-24.04-arm",
		"windows-11-arm",
		"[ubuntu-latest]",
	} {
		t.Run(runsOn, func(t *testing.T) {
			found := scanFixture(t, `on:
  push:
    tags: ['v[0-9]+.[0-9]+.[0-9]+']
jobs:
  release:
    runs-on: `+runsOn+`
    steps:
      - run: gh release create
`)
			requireMessages(t, found)
		})
	}
}

const (
	testFleetSelectLinux = `${{ (github.repository == 'Aplexica/Aplexica' && github.event.repository.fork == false && github.event.repository.visibility == 'private') && 'aplexica-linux' || 'ubuntu-latest' }}`
	testFleetSelectMac   = `${{ (github.repository == 'Aplexica/Aplexica' && github.event.repository.fork == false && github.event.repository.visibility == 'private') && 'aplexica-mac' || 'macos-latest' }}`
)

func TestTagTriggeredPolicyRequiresNamedFleetSelect(t *testing.T) {
	type tc struct {
		job  string
		want string
	}
	cases := []tc{
		{job: "guard", want: testFleetSelectLinux},
		{job: "sign", want: testFleetSelectLinux},
		{job: "publish", want: testFleetSelectLinux},
		{job: "verify", want: testFleetSelectLinux},
		{job: "tap", want: testFleetSelectLinux},
		{job: "build", want: testFleetSelectMac},
	}
	bads := []string{
		"ubuntu-latest",
		"macos-latest",
		"aplexica-linux",
		"aplexica-mac",
		`${{ github.event.repository.visibility == 'public' && 'ubuntu-latest' || 'aplexica-linux' }}`,
		`${{ github.event.repository.visibility == 'public' && 'macos-latest' || 'aplexica-mac' }}`,
	}
	for _, c := range cases {
		t.Run(c.job+" accepted", func(t *testing.T) {
			found := scanFixture(t, `on:
  push:
    tags: ['v[0-9]+.[0-9]+.[0-9]+']
jobs:
  `+c.job+`:
    runs-on: `+c.want+`
    steps:
      - run: echo ok
`)
			requireMessages(t, found)
		})
		for _, bad := range bads {
			if bad == c.want {
				continue
			}
			t.Run(c.job+" rejected "+bad, func(t *testing.T) {
				found := scanFixture(t, `on:
  push:
    tags: ['v[0-9]+.[0-9]+.[0-9]+']
jobs:
  `+c.job+`:
    runs-on: `+bad+`
    steps:
      - run: echo ok
`)
				requireMessages(t, found, "tag-triggered "+c.job+" job does not use the private+canonical+not-fork runner select")
			})
		}
	}
}

func TestTagTriggeredPolicyRejectsHardFleetPinOnUnknownJob(t *testing.T) {
	found := scanFixture(t, `on:
  push:
    tags: ['v[0-9]+.[0-9]+.[0-9]+']
jobs:
  release:
    runs-on: aplexica-linux
    steps:
      - run: gh release create
`)
	requireMessages(t, found, "tag-triggered release workflow does not pin a hosted runner label")
}

// The counterpart to the accepting table, and the reason hostedRunnerLabel
// requires a version segment. None of these names a GitHub-hosted image, and
// every one of them resolves ONLY to a self-hosted runner: GitHub auto-labels
// each self-hosted runner `self-hosted` PLUS its OS PLUS its arch, and label
// matching is case-insensitive, so bare `macos` can land the signing job on a
// persistent runner while never writing the string `self-hosted`. Without
// this table the regex can silently loosen back to an OS-prefix match.
func TestTagTriggeredPolicyRejectsSelfHostedOnlyRunnerLabels(t *testing.T) {
	for _, runsOn := range []string{
		"macos",
		"ubuntu",
		"windows",
		"macos-mini",
		"macos-builders",
		"windows-box",
		"ubuntu-wsl",
		"[macos]",
		"[macos, arm64]",
	} {
		t.Run(runsOn, func(t *testing.T) {
			found := scanFixture(t, `on:
  push:
    tags: ['v[0-9]+.[0-9]+.[0-9]+']
permissions:
  contents: read
jobs:
  release:
    runs-on: `+runsOn+`
    permissions:
      id-token: write
    steps:
      - run: cosign sign-blob --yes --bundle dist/SHA256SUMS.sigstore.json dist/SHA256SUMS
`)
			requireMessages(t, found, "tag-triggered release workflow does not pin a hosted runner label")
		})
	}
}

// Splitting release.yml into a caller plus a `workflow_call` callee used to turn
// the entire privileged rule family off: the caller's job has no `runs-on`, and
// the callee is neither untrusted nor privileged, so it is returned unscanned.
// tools/actionpin accepts the 40-hex pin and test-installer-security.sh's
// unpinned-uses grep does not flag it either, so this is the only gate that can
// catch it. Release authorities bind the callee's path, so the split publishes
// under a `build.yml` identity no documented verify command can match.
func TestTagTriggeredPolicyRejectsReusableWorkflowDelegation(t *testing.T) {
	found := scanFixture(t, `on:
  push:
    tags: ['v[0-9]+.[0-9]+.[0-9]+']
permissions:
  contents: read
jobs:
  release:
    uses: Aplexica/Aplexica/.github/workflows/build.yml@df4cb1c069e1874edd31b4311f1884172cec0e10
    secrets: inherit
    permissions:
      contents: write
      id-token: write
`)
	requireMessages(t, found, "tag-triggered release job delegates to a reusable workflow; external signing policy and public provenance bind the callee, not .github/workflows/release.yml")
}

// A privileged job that simply omits `runs-on` is unpinned, not vacuously fine.
// The guard used to be `privileged && runner != nil`, which exempted it.
func TestTagTriggeredPolicyRejectsMissingRunner(t *testing.T) {
	found := scanFixture(t, `on:
  push:
    tags: ['v[0-9]+.[0-9]+.[0-9]+']
permissions:
  contents: read
jobs:
  release:
    permissions:
      id-token: write
    steps:
      - run: cosign sign-blob --yes --bundle dist/SHA256SUMS.sigstore.json dist/SHA256SUMS
`)
	requireMessages(t, found, "tag-triggered release workflow does not pin a hosted runner label")
}

// The callee half of a split. It carries every property the privileged rules
// exist to forbid, and it is only reachable because something called it — which
// is why the caller is rejected outright rather than followed.
func TestWorkflowCallCalleeIsNotItselfPrivileged(t *testing.T) {
	found := scanFixture(t, `on:
  workflow_call:
jobs:
  build:
    runs-on: [self-hosted, macOS]
    steps:
      - run: cosign sign-blob --yes --bundle dist/SHA256SUMS.sigstore.json dist/SHA256SUMS
`)
	requireMessages(t, found)
}

// The real .github/workflows/release.yml must scan clean: root `contents: read`
// with the release job holding both signing and publication authority and
// `gh release create` actually need. TestRepositoryWorkflowsScanClean asserts
// that against the real file; this one pins the shape.
func TestTagTriggeredPolicyAllowsHostedRunnerWithJobLevelWrites(t *testing.T) {
	found := scanFixture(t, `on:
  push:
    tags: ['v[0-9]+.[0-9]+.[0-9]+']
permissions:
  contents: read
jobs:
  release:
    runs-on: macos-latest
    permissions:
      contents: write
      id-token: write
      attestations: write
    steps:
      - run: gh release create
`)
	requireMessages(t, found)
}

// A scoped `contents: write` is legitimate for a release; the maximal grant
// never is, on any trigger. This is the escalation that used to scan clean.
func TestBlanketWriteAllIsRejectedOnPrivilegedWorkflow(t *testing.T) {
	found := scanFixture(t, `on:
  push:
    tags: ['v[0-9]+.[0-9]+.[0-9]+']
permissions: write-all
jobs:
  release:
    runs-on: macos-latest
    permissions:
      contents: write
      id-token: write
      attestations: write
    steps:
      - run: gh release create
`)
	requireMessages(t, found, "workflow declares blanket write-all permission")
}

func TestBlanketWriteAllIsRejectedOnUntrustedWorkflow(t *testing.T) {
	found := scanFixture(t, `on:
  pull_request:
permissions: write-all
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - run: go build ./...
`)
	requireMessages(t, found, "workflow declares blanket write-all permission")
}

// The root block is not the only place the maximal grant can hide.
func TestBlanketWriteAllIsRejectedAtJobLevel(t *testing.T) {
	found := scanFixture(t, `on:
  push:
    tags: ['v[0-9]+.[0-9]+.[0-9]+']
permissions:
  contents: read
jobs:
  release:
    runs-on: macos-latest
    permissions: write-all
    steps:
      - run: gh release create
`)
	requireMessages(t, found, "job declares blanket write-all permission")
}

func TestUntrustedWorkflowLevelScopedWriteIsRejected(t *testing.T) {
	found := scanFixture(t, `on:
  pull_request:
permissions:
  contents: write
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - run: go build ./...
`)
	requireMessages(t, found, "untrusted workflow has workflow-level write permission")
}

func TestTagTriggeredPolicyRejectsUntrustedTrigger(t *testing.T) {
	found := scanFixture(t, `on:
  push:
    tags: ['v[0-9]+.[0-9]+.[0-9]+']
  pull_request:
jobs:
  release:
    runs-on: ubuntu-latest
    steps:
      - run: gh release create
`)
	requireMessages(t, found, "tag-triggered release workflow also accepts an untrusted trigger")
}

// CANONICAL-CONTRACT.md §5: no workflow_dispatch on the release workflow. A
// dispatched branch run cannot satisfy the required tag/workflow identity.
func TestTagTriggeredPolicyRejectsWorkflowDispatch(t *testing.T) {
	found := scanFixture(t, `on:
  workflow_dispatch:
  push:
    tags: ['v[0-9]+.[0-9]+.[0-9]+']
permissions:
  contents: read
jobs:
  release:
    runs-on: macos-latest
    permissions:
      id-token: write
    steps:
      - run: cosign sign-blob --yes --bundle dist/SHA256SUMS.sigstore.json dist/SHA256SUMS
`)
	requireMessages(t, found, "tag-triggered release workflow also accepts workflow_dispatch; a dispatched branch run cannot satisfy the release tag/workflow identity")
}

// workflow_dispatch is only a problem on a release-shaped workflow.
// security.yml carries it legitimately (`.github/workflows/security.yml`, in
// its `on:` block alongside pull_request, push and schedule).
func TestWorkflowDispatchAloneIsNotPrivileged(t *testing.T) {
	found := scanFixture(t, `on:
  workflow_dispatch:
  push:
    branches: [main]
jobs:
  validate:
    runs-on: ubuntu-latest
    steps:
      - run: go build ./...
`)
	requireMessages(t, found)
}

// An attacker-authored commit message, PR title or branch name interpolated
// into a shell is the classic Actions RCE. It is equally wrong on a release
// workflow, where that shell holds the signing identity.
func TestRunStepInjectionIsRejected(t *testing.T) {
	for name, expression := range map[string]string{
		"event":            "${{ github.event.head_commit.message }}",
		"pull request":     "${{ github.event.pull_request.title }}",
		"head ref":         "${{ github.head_ref }}",
		"actor":            "${{ github.actor }}",
		"triggering actor": "${{ github.triggering_actor }}",
		// The Actions expression language treats index and dot dereference as
		// the same operation, so the deny-list has to normalise before matching
		// or `github['event']` is a one-character bypass of the whole rule.
		"bracket event":  "${{ github['event'].head_commit.message }}",
		"bracket chain":  "${{ github['event']['pull_request']['title'] }}",
		"bracket suffix": "${{ github.event['head_commit'].message }}",
		"bracket ref":    "${{ github['head_ref'] }}",
	} {
		t.Run(name, func(t *testing.T) {
			found := scanFixture(t, `on:
  push:
    tags: ['v[0-9]+.[0-9]+.[0-9]+']
jobs:
  release:
    runs-on: ubuntu-latest
    steps:
      - run: echo "`+expression+`" > notes.md
`)
			requireMessages(t, found, "run step interpolates an untrusted expression; pass it through env: instead")
		})
	}
}

func TestRunStepInjectionIsRejectedOnUntrustedTrigger(t *testing.T) {
	found := scanFixture(t, `on:
  pull_request:
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - run: echo "${{ github.event.pull_request.title }}"
`)
	requireMessages(t, found, "run step interpolates an untrusted expression; pass it through env: instead")
}

// False positives here break CI immediately. The matrix case is security.yml's
// govulncheck step today; the rest are near-misses the word boundaries in
// untrustedContext have to survive.
func TestRunStepInjectionAllowsTrustedExpressions(t *testing.T) {
	for name, script := range map[string]string{
		"matrix":     `go tool govulncheck -tags="${{ matrix.tags }}" ./...`,
		"runner":     `mkdir -p "${{ runner.temp }}/state"`,
		"event name": `test "${{ github.event_name }}" = push`,
		"actor id":   `echo "${{ github.actor_id }}"`,
		"ref name":   `gh release create "${{ github.ref_name }}"`,
		"env var":    `printf '%s' "$COMMIT_MESSAGE"`,
	} {
		t.Run(name, func(t *testing.T) {
			found := scanFixture(t, `on:
  push:
    tags: ['v[0-9]+.[0-9]+.[0-9]+']
jobs:
  release:
    runs-on: ubuntu-latest
    steps:
      - run: `+script+`
`)
			requireMessages(t, found)
		})
	}
}

// An untrusted expression in `env:` is the remediation, not the defect: the
// value arrives as a shell variable and is never expanded into the script text.
// release.yml's "Stage the pinned portal bundle" step uses exactly this shape
// for any repository secret used by a privileged release job.
func TestUntrustedExpressionInEnvIsAllowed(t *testing.T) {
	found := scanFixture(t, `on:
  push:
    tags: ['v[0-9]+.[0-9]+.[0-9]+']
jobs:
  release:
    runs-on: ubuntu-latest
    steps:
      - env:
          MESSAGE: ${{ github.event.head_commit.message }}
        run: printf '%s' "$MESSAGE"
`)
	requireMessages(t, found)
}

// A branch push carries no tag filter, so it is not release-shaped and the new
// rule family stays inert. test.yml and security.yml both look like this.
func TestBranchPushWithoutTagFilterIsNotPrivileged(t *testing.T) {
	found := scanFixture(t, `on:
  push:
    branches: [main]
jobs:
  build:
    runs-on: [self-hosted, linux]
    steps:
      - run: go build ./...
`)
	requireMessages(t, found)
}

// The two remaining ways into the privileged family. `tags-ignore` still means
// "run on tag pushes", and a `release` event runs at a tag ref too, so a
// refactor of hasTagFilter that dropped either arm would take the rule family
// offline for that shape with no other test noticing.
func TestPrivilegedTriggerVariants(t *testing.T) {
	for name, trigger := range map[string]string{
		"tags-ignore": `  push:
    tags-ignore: ['v0.*']`,
		"release": `  release:
    types: [published]`,
	} {
		t.Run(name, func(t *testing.T) {
			found := scanFixture(t, `on:
`+trigger+`
jobs:
  release:
    runs-on: [self-hosted, macOS]
    steps:
      - run: goreleaser release
`)
			requireMessages(t, found, "tag-triggered release workflow reaches self-hosted runner")
		})
	}
}

// repoRoot walks up from the test's working directory to the module root.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod found above the test working directory")
		}
		dir = parent
	}
}

// The one test that cannot be satisfied by a hand-written approximation: it
// scans the workflows this repository actually ships. Without it every fixture
// above stays green while .github/workflows/release.yml is escalated to
// write-all or handed a workflow_dispatch trigger.
func TestRepositoryWorkflowsScanClean(t *testing.T) {
	found, err := scan(repoRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range found {
		t.Errorf("%s:%d: %s", item.path, item.line, item.message)
	}
}
