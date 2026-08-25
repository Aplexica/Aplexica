package update

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

type Provenance struct {
	Version      string
	GitCommit    string
	ReleaseTrain string
}

const officialReleaseTrain = "github-actions-aws-kms-v1"

type CommandRunner interface {
	Output(ctx context.Context, name string, args ...string) ([]byte, error)
}

type execRunner struct{}

func (execRunner) Output(ctx context.Context, name string, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, name, args...)
	return command.Output()
}

type Classifier struct {
	Runner CommandRunner
}

const managerProbeTimeout = 4 * time.Second

// homebrewFormula is the formula name in packaging/homebrew/aplexica.rb, which
// is also the Cellar directory Homebrew installs it into.
const homebrewFormula = "aplexica"

// Classify decides which installer owns the running executable. Ownership is
// proved by asking each package manager for a receipt, never by pattern
// matching the path: a directory called Cellar proves nothing, and treating it
// as proof would let anyone who can write a path fake a manager-owned install.
func (classifier Classifier) Classify(ctx context.Context, executable string, provenance Provenance) (Installation, error) {
	if executable == "" {
		var err error
		executable, err = os.Executable()
		if err != nil {
			return Installation{}, fmt.Errorf("locate executable: %w", err)
		}
	}
	absolute, err := filepath.Abs(executable)
	if err != nil {
		return Installation{}, err
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		resolved = absolute
	}

	runner := classifier.Runner
	if runner == nil {
		runner = execRunner{}
	}
	managerCtx, cancel := context.WithTimeout(ctx, managerProbeTimeout)
	defer cancel()
	var owners []Installation
	if ownedByHomebrew(managerCtx, runner, resolved) {
		owners = append(owners, Installation{
			Method: MethodHomebrew, Executable: resolved, Version: trimVersion(provenance.Version),
			Reason:         "Aplexica is managed by Homebrew; the Aplexica Homebrew tap has not been advanced yet.",
			ManagerCommand: "brew upgrade aplexica",
			ChannelEnabled: false,
		})
	}
	if ownedByDPKG(managerCtx, runner, resolved) {
		owners = append(owners, Installation{
			Method: MethodAPT, Executable: resolved, Version: trimVersion(provenance.Version),
			Reason:         "Aplexica is managed by dpkg; no Aplexica APT repository is published yet.",
			ManagerCommand: "sudo apt update && sudo apt install --only-upgrade aplexica",
			ChannelEnabled: false,
		})
	}
	if ownedByWinGet(managerCtx, runner, resolved) {
		owners = append(owners, Installation{
			Method: MethodWinGet, Executable: resolved, Version: trimVersion(provenance.Version),
			Reason:         "Aplexica is managed by WinGet; the Aplexica WinGet channel is paused.",
			ManagerCommand: "winget upgrade --id Aplexica.Aplexica --exact",
			ChannelEnabled: false,
		})
	}
	if len(owners) > 1 {
		// Version is carried here for the same reason as every other branch,
		// and it matters most here: ambiguous ownership is the status a person
		// has to diagnose by hand, and dropping the field would take
		// current_version and current_sequence out of --json for exactly that
		// case.
		return Installation{
			Method: MethodAmbiguous, Executable: resolved, Version: trimVersion(provenance.Version),
			Reason: "multiple package-manager receipts claim this executable",
		}, nil
	}
	if len(owners) == 1 {
		return owners[0], nil
	}
	if provenance.ReleaseTrain != officialReleaseTrain ||
		provenance.GitCommit == "" || provenance.GitCommit == "unknown" ||
		!strings.HasPrefix(provenance.Version, "v") ||
		!versionPattern.MatchString(strings.TrimPrefix(provenance.Version, "v")) {
		return Installation{
			Method: MethodSource, Executable: resolved, Version: trimVersion(provenance.Version),
			Reason: "This is a source or development build; update the checkout and rebuild it.",
		}, nil
	}
	return Installation{
		Method: MethodUnknown, Executable: resolved, Version: trimVersion(provenance.Version),
		Reason: "Aplexica cannot authenticate which installer owns this executable.",
	}, nil
}

// ownedByHomebrew asks Homebrew twice because one question is not enough.
//
// `brew --prefix <formula>` is the precise answer but it exits non-zero when
// the formula is not installed under that name, and Homebrew installs from a
// tap, so a linked keg can be present while the bare formula name resolves to
// nothing. A tapped keg linked from the Homebrew prefix must still classify as
// Homebrew-owned rather than falling through to an unauthenticated install.
//
// So when the formula probe declines, fall back to the Cellar test: resolve
// the Homebrew prefix and ask whether the already-resolved executable lives
// under <prefix>/Cellar/aplexica. That is still a receipt — the prefix comes
// from brew itself, not from the path — but it holds for tapped kegs too.
func ownedByHomebrew(ctx context.Context, runner CommandRunner, executable string) bool {
	if prefix, ok := homebrewPrefix(ctx, runner, homebrewFormula); ok && pathWithin(prefix, executable) {
		return true
	}
	prefix, ok := homebrewPrefix(ctx, runner)
	if !ok {
		return false
	}
	return pathWithin(filepath.Join(prefix, "Cellar", homebrewFormula), executable)
}

// homebrewPrefix runs `brew --prefix [formula]` and resolves the answer.
// `brew --prefix <formula>` prints the opt path, which is a symlink to the
// versioned keg; Classify already resolved the executable, so the prefix has
// to be resolved too or a linked keg never compares as contained.
func homebrewPrefix(ctx context.Context, runner CommandRunner, arguments ...string) (string, bool) {
	output, err := runner.Output(ctx, "brew", append([]string{"--prefix"}, arguments...)...)
	if err != nil {
		return "", false
	}
	prefix := strings.TrimSpace(string(output))
	if prefix == "" || !filepath.IsAbs(prefix) {
		return "", false
	}
	if resolved, resolveErr := filepath.EvalSymlinks(prefix); resolveErr == nil {
		prefix = resolved
	}
	return prefix, true
}

func ownedByDPKG(ctx context.Context, runner CommandRunner, executable string) bool {
	if runtime.GOOS == "windows" {
		return false
	}
	output, err := runner.Output(ctx, "dpkg-query", "-S", executable)
	if err != nil {
		return false
	}
	for _, line := range bytes.Split(output, []byte{'\n'}) {
		parts := bytes.SplitN(line, []byte{':'}, 2)
		if len(parts) == 2 && string(parts[0]) == "aplexica" {
			return true
		}
	}
	return false
}

func ownedByWinGet(ctx context.Context, runner CommandRunner, executable string) bool {
	if runtime.GOOS != "windows" {
		return false
	}
	lower := strings.ToLower(filepath.ToSlash(executable))
	if !strings.Contains(lower, "/winget/packages/") && !strings.Contains(lower, "/microsoft/winget/") {
		return false
	}
	output, err := runner.Output(ctx, "winget", "list", "--id", "Aplexica.Aplexica", "--exact", "--accept-source-agreements")
	return err == nil && bytes.Contains(bytes.ToLower(output), []byte("aplexica.aplexica"))
}

func trimVersion(version string) string {
	return strings.TrimPrefix(version, "v")
}

func pathWithin(parent, candidate string) bool {
	relative, err := filepath.Rel(parent, candidate)
	if err != nil {
		return false
	}
	if runtime.GOOS == "windows" {
		relative = strings.ToLower(relative)
	}
	return relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)))
}
