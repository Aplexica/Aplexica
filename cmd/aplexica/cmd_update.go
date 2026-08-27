package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"

	aplexicaupdate "github.com/aplexica/aplexica/internal/update"
	"github.com/aplexica/aplexica/internal/version"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

var (
	updateFlagCheck          bool
	updateFlagYes            bool
	updateFlagJSON           bool
	updateFlagAllowDowngrade bool

	newUpdateEngine  = func() aplexicaupdate.Engine { return aplexicaupdate.Engine{} }
	updateExecutable = os.Executable
	updateIsTerminal = func(file *os.File) bool { return term.IsTerminal(int(file.Fd())) }
)

var updateCmd = &cobra.Command{
	Use:   "update",
	Short: "Report which installer owns this build and how to upgrade it",
	// This text has to match both update.Engine.Check paths. Package-manager and
	// source builds stop at local ownership. An unclaimed build reads one GitHub
	// metadata document and checks only that the manual verification assets are
	// listed; it never downloads or authenticates them.
	Long: "Report which installer owns this build and print the command that upgrades it.\n\n" +
		"If a package manager owns the executable (Homebrew, apt/dpkg, WinGet) or it is\n" +
		"a source build, this command answers from local ownership or build provenance and\n" +
		"does not contact GitHub. For an installation no package manager claims, it reads\n" +
		"the newest stable release's GitHub metadata and requires that metadata to list\n" +
		"SHA256SUMS, SHA256SUMS.sigstore.json, and\n" +
		"aplexica.provenance.sigstore.json. It does not download them or\n" +
		"verify a signature; authenticate downloaded bytes with the documented verify\n" +
		"command before replacing anything.\n\n" +
		"This command is advisory. It never downloads a platform archive and never\n" +
		"replaces a file on disk; you run the steps it prints.",
	Args: cobra.NoArgs,
	RunE: runUpdate,
}

func init() {
	updateCmd.Flags().BoolVar(&updateFlagCheck, "check", false, "report without prompting")
	updateCmd.Flags().BoolVar(&updateFlagYes, "yes", false, "do not prompt for confirmation")
	updateCmd.Flags().BoolVar(&updateFlagJSON, "json", false, "emit one versioned JSON result")
	updateCmd.Flags().BoolVar(&updateFlagAllowDowngrade, "allow-downgrade", false,
		"accept a release older than the highest version this machine has run")
	rootCmd.AddCommand(updateCmd)
}

func runUpdate(cmd *cobra.Command, _ []string) error {
	executable, err := updateExecutable()
	if err != nil {
		return err
	}
	provenance := aplexicaupdate.Provenance{
		Version:      version.Version,
		GitCommit:    version.GitCommit,
		ReleaseTrain: version.ReleaseTrain,
	}
	engine := newUpdateEngine()
	result, _, _, checkErr := engine.Check(
		cmd.Context(), executable, provenance,
		aplexicaupdate.CheckOptions{AllowDowngrade: updateFlagAllowDowngrade},
	)
	if checkErr != nil {
		renderUpdateResult(cmd, result)
		return checkErr
	}
	if result.Status != aplexicaupdate.StatusUpdateAvailable {
		renderUpdateResult(cmd, result)
		return nil
	}
	if updateFlagCheck {
		renderUpdateResult(cmd, result)
		return &aplexicaupdate.Error{
			Class: aplexicaupdate.ClassOperational, Code: aplexicaupdate.ExitUpdateAvailable,
			Err: fmt.Errorf("update is available"), Quiet: true,
		}
	}
	// The confirmation gate survives the move to advisory-only for one
	// reason: `aplexica update` without --check is what a person types when
	// they mean "do it", and the honest answer is a list of steps they have to
	// carry out themselves. Asking first — and refusing to block on a terminal
	// that is not there — keeps `aplexica update` safe to leave in a cron
	// entry and keeps exit code 2 meaning what it has always meant.
	if !updateFlagYes {
		inputFile, isFile := cmd.InOrStdin().(*os.File)
		if !isFile || !updateIsTerminal(inputFile) {
			result.Status = aplexicaupdate.StatusDeclined
			result.Message = "Confirmation is required; re-run with --yes in a non-interactive shell."
			renderUpdateResult(cmd, result)
			return &aplexicaupdate.Error{
				Class: aplexicaupdate.ClassConfirmation, Code: aplexicaupdate.ExitConfirmation,
				Err: fmt.Errorf("confirmation required"), Quiet: true,
			}
		}
		fmt.Fprintf(cmd.ErrOrStderr(), "Aplexica %s is available. Print the upgrade steps? [y/N] ",
			result.TargetVersion)
		answer, readErr := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
		if readErr != nil && readErr != io.EOF {
			return readErr
		}
		answer = strings.ToLower(strings.TrimSpace(answer))
		if answer != "y" && answer != "yes" {
			result.Status = aplexicaupdate.StatusDeclined
			result.Message = "Update declined."
			renderUpdateResult(cmd, result)
			return &aplexicaupdate.Error{
				Class: aplexicaupdate.ClassConfirmation, Code: aplexicaupdate.ExitConfirmation,
				Err: fmt.Errorf("update declined"), Quiet: true,
			}
		}
	}
	renderUpdateResult(cmd, result)
	return nil
}

func renderUpdateResult(cmd *cobra.Command, result aplexicaupdate.Result) {
	if updateFlagJSON {
		encoder := json.NewEncoder(cmd.OutOrStdout())
		encoder.SetEscapeHTML(false)
		_ = encoder.Encode(result)
		return
	}
	out := cmd.OutOrStdout()
	switch result.Status {
	case aplexicaupdate.StatusUpToDate:
		fmt.Fprintf(out, "Aplexica %s matches the newest complete GitHub release (%s, %s).\n",
			result.CurrentVersion, result.InstallMethod, result.Channel)
		fmt.Fprintln(out, "No release asset was downloaded or verified by this check.")
	case aplexicaupdate.StatusUpdateAvailable:
		fmt.Fprintf(out, "Current: %s (%s, %s)\nNewest complete GitHub release: %s\n",
			result.CurrentVersion, result.InstallMethod, result.Channel,
			result.TargetVersion)
		if result.ReleaseNotesURL != "" {
			fmt.Fprintln(out, "Release notes:", result.ReleaseNotesURL)
		}
		fmt.Fprintln(out, "No release asset was downloaded or verified by this check.")
		if updateFlagCheck {
			fmt.Fprintln(out, "Run `aplexica update` for the upgrade steps.")
			return
		}
		renderManualUpgrade(out, result.TargetVersion)
	case aplexicaupdate.StatusManagerDelegated:
		fmt.Fprintln(out, result.Message)
		fmt.Fprintln(out, "`aplexica update` never replaces files; run the upgrade yourself.")
		if result.ManagerCommand != nil {
			fmt.Fprintln(out, "Run:", *result.ManagerCommand)
		}
		switch result.InstallMethod {
		case aplexicaupdate.MethodHomebrew:
			fmt.Fprintln(out, "See: https://github.com/Aplexica/Aplexica/blob/main/docs/install/brew.md")
		case aplexicaupdate.MethodAPT:
			fmt.Fprintln(out, "See: https://github.com/Aplexica/Aplexica/blob/main/docs/install/apt.md")
		case aplexicaupdate.MethodWinGet:
			fmt.Fprintln(out, "See: https://github.com/Aplexica/Aplexica/blob/main/docs/install/_index.md")
		case aplexicaupdate.MethodSource:
			fmt.Fprintln(out, "See: https://github.com/Aplexica/Aplexica/blob/main/docs/install/build.md")
		}
	case aplexicaupdate.StatusBootstrapRequired:
		fmt.Fprintln(out, "Release discovery requires additional setup.")
		if result.Message != "" {
			fmt.Fprintln(out, result.Message)
		}
		fmt.Fprintln(out, "No release assets were downloaded and nothing was changed.")
	case aplexicaupdate.StatusDeclined, aplexicaupdate.StatusFailed:
		if result.Message != "" {
			fmt.Fprintln(out, result.Message)
		}
	}
}

// renderManualUpgrade prints the download-verify-replace recipe for an
// installation no package manager claims. The version is substituted into
// both the URL and the archive name so the steps are copy-pasteable, and the
// verification steps themselves live in one place — docs/install/verify.md —
// rather than being restated here where they would drift.
func renderManualUpgrade(out io.Writer, version string) {
	fmt.Fprintln(out)
	fmt.Fprintln(out, "No package manager owns this executable, so Aplexica will not replace it.")
	fmt.Fprintln(out, "Download the release, verify it, then replace the binaries yourself:")
	fmt.Fprintln(out)
	fmt.Fprintf(out, "  %s/%s\n", releaseDownloadBase(version), platformArchiveName(version))
	fmt.Fprintf(out, "  %s/SHA256SUMS\n", releaseDownloadBase(version))
	fmt.Fprintf(out, "  %s/SHA256SUMS.sigstore.json\n", releaseDownloadBase(version))
	fmt.Fprintf(out, "  %s/aplexica.provenance.sigstore.json\n", releaseDownloadBase(version))
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Verify: https://github.com/Aplexica/Aplexica/blob/main/docs/install/verify.md")
	fmt.Fprintln(out, "Then restart the daemon: aplexica daemon restart")
}

func releaseDownloadBase(version string) string {
	return "https://github.com/Aplexica/Aplexica/releases/download/v" + version
}

// platformArchiveName is the one archive-name template the release publishes:
// aplexica-<VERSION>-<GOOS>-<GOARCH>.<EXT>, bare version, hyphen separated,
// zip on Windows and tar.gz everywhere else.
func platformArchiveName(version string) string {
	extension := "tar.gz"
	if runtime.GOOS == "windows" {
		extension = "zip"
	}
	return fmt.Sprintf("aplexica-%s-%s-%s.%s", version, runtime.GOOS, runtime.GOARCH, extension)
}
