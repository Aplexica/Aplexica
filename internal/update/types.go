package update

import (
	"errors"
	"fmt"
	"regexp"

	"github.com/aplexica/aplexica/internal/releasetrust"
)

const (
	ExitOperational       = 1
	ExitConfirmation      = 2
	ExitSecurity          = 3
	ExitUpdateAvailable   = 10
	ExitDelegated         = 20
	ExitBootstrapRequired = 21
	ExitOwnership         = 22

	decimalRadix        = 10
	unsignedIntegerBits = 64
)

// versionPattern is the one accepted release version shape. The release train
// publishes vMAJOR.MINOR.PATCH and nothing else — no pre-releases, no build
// metadata — so anything that does not match is not a release this updater may
// reason about.
var versionPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

type InstallMethod string

const (
	MethodHomebrew  InstallMethod = "homebrew"
	MethodAPT       InstallMethod = "apt"
	MethodWinGet    InstallMethod = "winget"
	MethodSource    InstallMethod = "source"
	MethodUnknown   InstallMethod = "unknown"
	MethodAmbiguous InstallMethod = "ambiguous"
)

type Status string

const (
	StatusUpToDate          Status = "up_to_date"
	StatusUpdateAvailable   Status = "update_available"
	StatusDeclined          Status = "declined"
	StatusManagerDelegated  Status = "manager_delegated"
	StatusBootstrapRequired Status = "bootstrap_required"
	StatusFailed            Status = "failed"
)

// Result is the one JSON document `aplexica update --json` prints. The schema
// string is part of the CLI contract: wrapper scripts branch on `status` and
// `install_method`, so fields may be added but never repurposed.
type Result struct {
	Schema          string        `json:"schema"`
	Status          Status        `json:"status"`
	InstallMethod   InstallMethod `json:"install_method"`
	Channel         string        `json:"channel"`
	CurrentVersion  string        `json:"current_version,omitempty"`
	CurrentSequence uint64        `json:"current_sequence,omitempty"`
	TargetVersion   string        `json:"target_version,omitempty"`
	TargetSequence  uint64        `json:"target_sequence,omitempty"`
	RestartRequired bool          `json:"restart_required"`
	ManagerCommand  *string       `json:"manager_command"`
	ReleaseNotesURL string        `json:"release_notes_url,omitempty"`
	Message         string        `json:"message,omitempty"`
}

func NewResult() Result {
	return Result{Schema: "aplexica.update-result/v1", Channel: "stable"}
}

type ErrorClass string

const (
	ClassOperational  ErrorClass = "operational"
	ClassConfirmation ErrorClass = "confirmation"
	ClassSecurity     ErrorClass = "security"
	ClassDelegated    ErrorClass = "delegated"
	ClassBootstrap    ErrorClass = "bootstrap"
	ClassOwnership    ErrorClass = "ownership"
)

type Error struct {
	Stage string
	Class ErrorClass
	Code  int
	Err   error
	Quiet bool
}

func (err *Error) Error() string {
	if err.Stage == "" {
		return err.Err.Error()
	}
	return fmt.Sprintf("%s: %v", err.Stage, err.Err)
}

func (err *Error) Unwrap() error { return err.Err }
func (err *Error) ExitCode() int { return err.Code }
func (err *Error) Silent() bool  { return err.Quiet }

func Wrap(stage string, class ErrorClass, code int, err error) error {
	if err == nil {
		return nil
	}
	var typed *Error
	if errors.As(err, &typed) {
		return err
	}
	return &Error{Stage: stage, Class: class, Code: code, Err: err}
}

func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	var coder interface{ ExitCode() int }
	if errors.As(err, &coder) {
		return coder.ExitCode()
	}
	return ExitOperational
}

// Target is the release metadata the updater discovered. It is a report, not
// an authenticated install plan: nothing downstream downloads, verifies,
// unpacks, or executes an artifact. Users authenticate downloaded bytes with
// the documented cosign command before installing them.
type Target struct {
	Schema          string `json:"schema"`
	Channel         string `json:"channel"`
	Repository      string `json:"repository"`
	Version         string `json:"version"`
	Sequence        uint64 `json:"sequence"`
	ReleaseNotesURL string `json:"release_notes_url"`
}

// releaseNotesURL is the release page for one tag, built from compiled-in
// constants and a tag this package has already validated.
//
// It is deliberately not copied out of the release document's html_url. That
// document is untrusted display metadata — `aplexica update` does not
// authenticate it — and the command prints this string straight to a terminal.
// A free-text field there can carry any scheme, an ANSI escape, or an embedded
// newline that appends an entire extra line of apparently-Aplexica advice to
// the output.
func releaseNotesURL(tag string) string {
	return "https://github.com/" + releasetrust.Repository + "/releases/tag/" + tag
}

// Validate is the last check between discovery and the comparison logic. It
// guards the invariants the engine then assumes: the target names this
// repository, carries a real release version, has a non-zero sequence to
// compare against the rollback floor, and carries a release-notes URL derived
// locally rather than copied from server-controlled metadata.
func (target Target) Validate() error {
	if target.Schema != "aplexica.channel-target/v1" || target.Channel != "stable" ||
		target.Repository != releasetrust.Repository ||
		!versionPattern.MatchString(target.Version) || target.Sequence == 0 {
		return fmt.Errorf("stable target has invalid fixed identity")
	}
	sequence, err := VersionSequence(target.Version)
	if err != nil || target.Sequence != sequence {
		return fmt.Errorf("stable target sequence does not match its version")
	}
	// versionPattern has already passed, so "v"+Version is the exact tag.
	if target.ReleaseNotesURL != releaseNotesURL("v"+target.Version) {
		return fmt.Errorf("stable target carries a release-notes URL it did not derive")
	}
	return nil
}

// Installation is what the classifier could prove about the executable that is
// running. StateDir is the daemon's state directory — the only thing the
// advisory updater persists there is the rollback floor.
type Installation struct {
	Method         InstallMethod
	Executable     string
	Version        string
	StateDir       string
	Reason         string
	ManagerCommand string
	ChannelEnabled bool
}

type CheckOptions struct {
	Channel string
	// AllowDowngrade waives the local rollback floor. It exists because the
	// floor is a local watermark rather than a signed predecessor chain, so a
	// deliberate downgrade is a legitimate operator decision, not an attack.
	AllowDowngrade bool
}
