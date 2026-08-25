// Package update answers one question and changes nothing: which release is
// current, and what command upgrades the installation the user is running.
//
// It is advisory by design, not by omission. The updater that used to live
// here downloaded an archive, verified it against a signed inventory, ran an
// installer helper, quiesced the daemon and the tray, swapped the runtime
// selection, and rolled the whole thing back on a failed health check. That
// machinery only ever engaged for installations it could authenticate as
// self-managed, and the classifier had no way to prove that ownership for a
// hand-placed binary. Package-manager and source builds delegated elsewhere;
// unclaimed binaries could be advised but not safely mutated. It was therefore
// a transactional installer for a population of zero, and every one of its
// failure modes landed on a live daemon.
//
// So the whole apply path is gone. What remains is the part that was always
// useful: report the newest complete release, name the installer that owns
// this executable, and print the command that upgrades it. The user downloads
// and authenticates any release files before installing them.
package update

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aplexica/aplexica/internal/atomicfile"
	"github.com/aplexica/aplexica/internal/filelock"
)

type DiscoveryFactory func(stateDir string) (Discovery, error)

// defaultDiscoveryFactory resolves release metadata from GitHub. The
// updater is advisory and deliberately does not authenticate or install an
// artifact; the manual installation path authenticates downloaded bytes with
// the documented cosign command.
func defaultDiscoveryFactory(stateDir string) (Discovery, error) {
	return NewReleaseDiscovery(stateDir)
}

// updateFloorFile holds the rollback watermark. See raiseUpdateFloor.
const updateFloorFile = "update-floor"

const floorFileMode = os.FileMode(0o600)

const updateFloorLockTimeout = 2 * time.Second

type Engine struct {
	Classifier       Classifier
	DiscoveryFactory DiscoveryFactory

	// StateDir overrides where the rollback floor is kept. Empty means the
	// daemon's state directory, resolved the same way the daemon resolves it.
	StateDir string
}

// Check reports the newest complete release and what to do about it. It never
// downloads a release asset, writes outside the rollback floor, or touches the
// installation.
func (engine Engine) Check(
	ctx context.Context,
	executable string,
	provenance Provenance,
	options CheckOptions,
) (Result, Installation, *Target, error) {
	result := NewResult()
	channel := options.Channel
	if channel == "" {
		channel = "stable"
	}
	result.Channel = channel
	if channel != "stable" {
		result.Status = StatusFailed
		return result, Installation{}, nil, &Error{
			Stage: "discover", Class: ClassOperational, Code: ExitOperational,
			Err: fmt.Errorf("unsupported update channel %q", channel),
		}
	}

	installation, err := engine.Classifier.Classify(ctx, executable, provenance)
	if err != nil {
		result.Status = StatusFailed
		result.Message = err.Error()
		return result, Installation{}, nil, Wrap("classify", ClassOperational, ExitOperational, err)
	}
	installation.StateDir = engine.StateDir
	if installation.StateDir == "" {
		if resolved, resolveErr := resolveStateDir(); resolveErr == nil {
			installation.StateDir = resolved
		}
	}
	result.InstallMethod = installation.Method
	result.CurrentVersion = installation.Version
	currentSequence, currentErr := VersionSequence(installation.Version)
	if currentErr == nil {
		result.CurrentSequence = currentSequence
	}

	switch installation.Method {
	case MethodHomebrew, MethodAPT, MethodWinGet:
		// Delegated installs short-circuit before discovery on purpose. The
		// ownership answer does not depend on which GitHub release is newest.
		// Failing to explain the owning manager because an unrelated API call
		// was refused would be worse than the local answer we already have.
		result.Status = StatusManagerDelegated
		result.Message = installation.Reason
		if installation.ChannelEnabled && installation.ManagerCommand != "" {
			command := installation.ManagerCommand
			result.ManagerCommand = &command
		}
		return result, installation, nil, &Error{
			Class: ClassDelegated, Code: ExitDelegated,
			Err: fmt.Errorf("package manager owns this installation"), Quiet: true,
		}
	case MethodSource:
		result.Status = StatusManagerDelegated
		result.Message = installation.Reason
		return result, installation, nil, &Error{
			Class: ClassDelegated, Code: ExitDelegated,
			Err: fmt.Errorf("source owner must update this build"), Quiet: true,
		}
	case MethodAmbiguous:
		// Two package managers claim the same file. There is no single
		// upgrade command to print and guessing one could have the loser
		// overwrite the winner's files, so this stays an error the operator
		// resolves.
		result.Status = StatusFailed
		result.Message = installation.Reason
		return result, installation, nil, &Error{
			Stage: "classify", Class: ClassOwnership, Code: ExitOwnership,
			Err: fmt.Errorf("%s", installation.Reason), Quiet: true,
		}
	case MethodUnknown:
	default:
		result.Status = StatusFailed
		err := fmt.Errorf("unsupported installation method %q", installation.Method)
		result.Message = err.Error()
		return result, installation, nil, err
	}

	// Only an unclaimed release build reaches discovery. A source build can
	// carry an arbitrary development version and must never raise the floor;
	// package managers already own ordering for their installations and return
	// above without touching updater state.
	if currentErr == nil {
		raiseUpdateFloor(installation.StateDir, installation.Version)
	}

	factory := engine.DiscoveryFactory
	if factory == nil {
		factory = defaultDiscoveryFactory
	}
	discovery, err := factory(installation.StateDir)
	if err != nil {
		result.Status = StatusFailed
		result.Message = err.Error()
		return result, installation, nil, Wrap("discover", ClassOperational, ExitOperational, err)
	}
	target, err := discovery.Refresh(ctx, channel)
	if err != nil {
		result.Status = StatusFailed
		var typed *Error
		if AsError(err, &typed) {
			// Preserve the existing bootstrap status for discovery
			// implementations supplied by callers, even though the built-in
			// GitHub metadata discovery has no separate trust-root bootstrap.
			if typed.Class == ClassBootstrap {
				result.Status = StatusBootstrapRequired
			}
			// Typed errors are copied into the result so JSON callers do not
			// lose the reason when stderr is suppressed.
			result.Message = typed.Error()
			return result, installation, nil, err
		}
		wrapped := Wrap("discover", ClassOperational, ExitOperational, err)
		result.Message = wrapped.Error()
		return result, installation, nil, wrapped
	}
	// Discovery is an extension point, so enforce the comparison invariants in
	// the engine as well as in the built-in implementation. A caller-provided
	// discovery must not smuggle an arbitrary sequence or terminal URL past the
	// same checks production metadata receives.
	if err := target.Validate(); err != nil {
		result.Status = StatusFailed
		wrapped := Wrap("discover", ClassOperational, ExitOperational, err)
		result.Message = wrapped.Error()
		return result, installation, nil, wrapped
	}
	result.TargetVersion = target.Version
	result.TargetSequence = target.Sequence
	result.ReleaseNotesURL = target.ReleaseNotesURL

	floorVersion, floorSequence := readUpdateFloor(installation.StateDir)
	if target.Sequence < floorSequence && !options.AllowDowngrade {
		result.Status = StatusFailed
		floor := &Error{
			Stage: "rollback", Class: ClassSecurity, Code: ExitSecurity,
			Err: fmt.Errorf(
				"stable version %s is below accepted floor %s; pass --allow-downgrade to accept it",
				target.Version, floorVersion,
			),
		}
		result.Message = floor.Error()
		return result, installation, &target, floor
	}
	if target.Sequence <= currentSequence {
		result.Status = StatusUpToDate
		return result, installation, &target, nil
	}
	result.Status = StatusUpdateAvailable
	// Nothing here restarts anything. The flag says the upgrade the user is
	// about to perform by hand will not take effect until the daemon is
	// restarted, which is what docs/install/update.md tells them to do.
	result.RestartRequired = true
	return result, installation, &target, nil
}

// resolveStateDir mirrors the daemon's own resolution (APLEXICA_STATE_DIR,
// else ~/.aplexica/state) so the updater's floor lands beside the state the
// daemon already keeps, instead of inventing a second location.
func resolveStateDir() (string, error) {
	if env := strings.TrimSpace(os.Getenv("APLEXICA_STATE_DIR")); env != "" {
		return env, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	return filepath.Join(home, ".aplexica", "state"), nil
}

// readUpdateFloor returns the highest version this machine is known to have
// run, and its sequence. An absent, unreadable, or malformed floor reads as no
// floor: the file is a local convenience, and refusing to check for updates
// because a scratch file got truncated would be the wrong trade.
func readUpdateFloor(stateDir string) (string, uint64) {
	if stateDir == "" {
		return "", 0
	}
	data, err := os.ReadFile(filepath.Join(stateDir, updateFloorFile))
	if err != nil {
		return "", 0
	}
	version := strings.TrimSpace(string(data))
	sequence, err := VersionSequence(version)
	if err != nil {
		return "", 0
	}
	return version, sequence
}

// raiseUpdateFloor records version as the highest release ever seen running on
// this machine, and is the whole of the anti-rollback story.
//
// It is honestly weaker than what it replaces. The retired updater refused any
// release whose signed inventory did not chain to the one already accepted, so
// a mirror could not serve an old release even to a machine that had never
// seen a new one. This is local state: it protects a machine that has already
// run a newer release, and it protects nothing at all on a fresh install,
// where there is no watermark to compare against. A machine whose state
// directory is wiped starts over with no floor.
//
// Failures are deliberately silent. The floor is a local downgrade warning,
// and a read-only or absent state directory must not turn "check for updates"
// into an error. It is not a substitute for authenticating downloaded bytes.
func raiseUpdateFloor(stateDir, version string) {
	if stateDir == "" {
		return
	}
	sequence, err := VersionSequence(version)
	if err != nil {
		return
	}
	if _, recorded := readUpdateFloor(stateDir); sequence <= recorded {
		return
	}
	// Only record into a state directory that already exists. `aplexica
	// update` reads; creating the daemon's private state tree as a side
	// effect of a read is a surprise the command has no business springing.
	if info, statErr := os.Stat(stateDir); statErr != nil || !info.IsDir() {
		return
	}
	lockPath, err := filepath.Abs(filepath.Join(stateDir, updateFloorFile+".lock"))
	if err != nil {
		return
	}
	lock, err := filelock.Acquire(lockPath, updateFloorLockTimeout)
	if err != nil {
		return
	}
	defer lock.Close()
	// The comparison belongs inside the same critical section as the write;
	// otherwise two checks can both observe the old floor and the older one can
	// win the final rename.
	if _, recorded := readUpdateFloor(stateDir); sequence <= recorded {
		return
	}
	_ = atomicfile.WriteFile(filepath.Join(stateDir, updateFloorFile), []byte(version+"\n"), floorFileMode)
}

// AsError finds the first *Error in err's chain. It is errors.As with the
// target type fixed, so callers cannot accidentally pass something that makes
// errors.As panic; the hand-rolled single-Unwrap walk it replaces silently
// gave up on an errors.Join chain.
func AsError(err error, target **Error) bool {
	return errors.As(err, target)
}
