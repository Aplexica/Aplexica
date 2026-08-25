package main

import (
	"context"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/aplexica/aplexica/internal/daemon"
	"github.com/aplexica/aplexica/internal/identity"
	"github.com/aplexica/aplexica/internal/keys"
	"github.com/aplexica/aplexica/internal/plugin/secureexec"
	"github.com/aplexica/aplexica/internal/plugin/truststate"
	"github.com/aplexica/aplexica/internal/secrets"
	"github.com/aplexica/aplexica/internal/securityepoch"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

const (
	existingGenesisConfirmationPrefix = "INITIALIZE ACCOUNT "
	existingGenesisConfirmationMax    = 512
)

var existingGenesisIdentityField = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:@+-]{0,255}$`)

type existingGenesisStatus struct {
	ServiceOrigin string
	AccountID     string
	UserID        string
	DeviceID      string
}

type existingGenesisPrompter interface {
	ReadHidden(context.Context, string, int) ([]byte, error)
}

type existingGenesisDependencies struct {
	Status            existingGenesisStatus
	DeviceIdentity    keys.DeviceIdentity
	Build             func(identity.ExistingAccountGenesisInput) (identity.ExistingAccountGenesisResult, error)
	Install           func(context.Context, identity.ExistingAccountGenesisResult) (identity.VerifiedRoster, error)
	RequestActivation func(context.Context) error
}

var remoteInitializeExistingAccountCmd = &cobra.Command{
	Use:   "initialize-existing-account",
	Short: "Create this paired account's explicit local recovery trust root",
	Long: `Initialize an already-paired cloud account's explicit local trust root.

The recovery phrase and typed confirmation are accepted only from a local,
interactive terminal with echo disabled. Account, user, and device identity
come only from the configured signed plugin's private authenticated status;
this command has no identity or recovery flags and reads none from the
environment.`,
	Args: cobra.NoArgs,
	RunE: runRemoteInitializeExistingAccount,
}

func init() {
	remoteCmd.AddCommand(remoteInitializeExistingAccountCmd)
}

func runRemoteInitializeExistingAccount(cmd *cobra.Command, _ []string) error {
	prompter, out, err := newExistingGenesisTTY(cmd)
	if err != nil {
		return err
	}
	stateRoot, secretsRoot, cfg, err := existingGenesisRuntimeConfig()
	if err != nil {
		return err
	}
	status, err := authenticatedExistingGenesisStatus(cmd.Context(), stateRoot, cfg)
	if err != nil {
		return err
	}
	secretStore := &secrets.Store{Root: secretsRoot}
	deviceIdentity, err := (&keys.DeviceIdentityStore{Secrets: secretStore}).LoadExisting()
	if err != nil {
		return fmt.Errorf("load existing local device identity: %w", err)
	}
	defer clearExistingGenesisDeviceIdentity(&deviceIdentity)

	identityRoot := filepath.Join(stateRoot, "identity")
	coordinator := &securityepoch.Coordinator{Root: identityRoot}
	installer := &identity.ExistingAccountGenesisInstaller{IdentityRoot: identityRoot, Coordinator: coordinator}
	deps := existingGenesisDependencies{
		Status:         status,
		DeviceIdentity: deviceIdentity,
		Build:          identity.BuildExistingAccountGenesis,
		Install:        installer.Install,
		RequestActivation: func(context.Context) error {
			response, requestErr := daemon.SendCommand(
				filepath.Join(stateRoot, "aplexicad.sock"),
				daemon.Request{Command: "generation-activation-request"},
			)
			if requestErr != nil {
				return requestErr
			}
			if !response.OK {
				return errors.New("generation activation request rejected")
			}
			return nil
		},
	}
	return runExistingAccountGenesis(cmd.Context(), out, prompter, deps)
}

func runExistingAccountGenesis(ctx context.Context, out io.Writer, prompter existingGenesisPrompter, deps existingGenesisDependencies) error {
	if ctx == nil || out == nil || prompter == nil || deps.Build == nil || deps.Install == nil ||
		!validExistingGenesisStatus(deps.Status) {
		return errors.New("existing-account initialization is unavailable")
	}
	defer clearExistingGenesisDeviceIdentity(&deps.DeviceIdentity)

	confirmationText := existingGenesisConfirmationPrefix + deps.Status.AccountID
	fmt.Fprintln(out, "Existing paired cloud identity:")
	fmt.Fprintf(out, "  Account: %s\n", deps.Status.AccountID)
	fmt.Fprintf(out, "  Device:  %s\n", deps.Status.DeviceID)
	fmt.Fprintf(out, "  Local signing fingerprint (SHA-256): %s\n", hex.EncodeToString(deps.DeviceIdentity.SigningKeyID[:]))
	fmt.Fprintf(out, "  Local wrap fingerprint (SHA-256):    %s\n", hex.EncodeToString(deps.DeviceIdentity.WrapKeyID[:]))
	fmt.Fprintln(out, "WARNING: This recovery phrase will become the account trust root for the exact paired identity above.")
	fmt.Fprintf(out, "Type exactly %q to continue. Input is hidden.\n", confirmationText)

	confirmation, err := prompter.ReadHidden(ctx, "Confirmation: ", existingGenesisConfirmationMax)
	if err != nil {
		clearExistingGenesisBytes(confirmation)
		return errors.New("read typed confirmation: interactive input unavailable")
	}
	defer clearExistingGenesisBytes(confirmation)
	if len(confirmation) != len(confirmationText) || subtle.ConstantTimeCompare(confirmation, []byte(confirmationText)) != 1 {
		return errors.New("typed confirmation did not match; no identity state was written")
	}
	clearExistingGenesisBytes(confirmation)

	phrase, err := prompter.ReadHidden(ctx, "24-word recovery phrase: ", identity.MaxRecoveryMnemonicBytes)
	if err != nil {
		clearExistingGenesisBytes(phrase)
		return errors.New("read recovery phrase: interactive input unavailable")
	}
	defer clearExistingGenesisBytes(phrase)
	result, err := deps.Build(identity.ExistingAccountGenesisInput{
		ServiceOrigin:             deps.Status.ServiceOrigin,
		AccountID:                 deps.Status.AccountID,
		UserID:                    deps.Status.UserID,
		DeviceID:                  deps.Status.DeviceID,
		Confirmed:                 true,
		ConfirmedRecoveryMnemonic: phrase,
		DeviceIdentity:            deps.DeviceIdentity,
	})
	clearExistingGenesisBytes(phrase)
	if err != nil {
		return fmt.Errorf("construct existing-account trust root: %w", err)
	}
	if _, err := deps.Install(ctx, result); err != nil {
		return fmt.Errorf("install existing-account trust root: %w", err)
	}

	fmt.Fprintln(out, "Existing-account trust root installed successfully.")
	if deps.RequestActivation == nil {
		fmt.Fprintln(out, "Generation activation will begin on the next daemon start.")
		return nil
	}
	if err := deps.RequestActivation(ctx); err != nil {
		fmt.Fprintln(out, "Generation activation is pending; start or restart the daemon to continue it.")
		return nil
	}
	fmt.Fprintln(out, "Generation activation requested from the running daemon.")
	return nil
}

type existingGenesisTTY struct {
	in  *os.File
	out *os.File
}

func newExistingGenesisTTY(cmd *cobra.Command) (existingGenesisPrompter, io.Writer, error) {
	in, inOK := cmd.InOrStdin().(*os.File)
	out, outOK := cmd.OutOrStdout().(*os.File)
	if !inOK || !outOK || !term.IsTerminal(int(in.Fd())) || !term.IsTerminal(int(out.Fd())) {
		return nil, nil, errors.New("existing-account initialization requires an interactive local terminal")
	}
	return &existingGenesisTTY{in: in, out: out}, out, nil
}

func (t *existingGenesisTTY) ReadHidden(ctx context.Context, prompt string, maximum int) ([]byte, error) {
	if t == nil || t.in == nil || t.out == nil || maximum <= 0 {
		return nil, errors.New("terminal unavailable")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, err := io.WriteString(t.out, prompt); err != nil {
		return nil, err
	}
	previous, err := term.MakeRaw(int(t.in.Fd()))
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = term.Restore(int(t.in.Fd()), previous)
		_, _ = io.WriteString(t.out, "\n")
	}()

	value := make([]byte, 0, min(maximum, 64))
	var one [1]byte
	defer clearExistingGenesisBytes(one[:])
	for {
		if err := ctx.Err(); err != nil {
			clearExistingGenesisBytes(value)
			return nil, err
		}
		n, readErr := t.in.Read(one[:])
		if readErr != nil {
			clearExistingGenesisBytes(value)
			return nil, readErr
		}
		if n != 1 {
			continue
		}
		switch one[0] {
		case '\r', '\n':
			return value, nil
		case 3, 4:
			clearExistingGenesisBytes(value)
			return nil, context.Canceled
		case 8, 127:
			if len(value) > 0 {
				value[len(value)-1] = 0
				value = value[:len(value)-1]
			}
		default:
			if len(value) == maximum {
				clearExistingGenesisBytes(value)
				return nil, errors.New("input exceeds maximum length")
			}
			value = append(value, one[0])
		}
	}
}

func existingGenesisRuntimeConfig() (stateRoot, secretsRoot string, cfg *daemon.Config, err error) {
	stateRoot, err = defaultStateDir()
	if err != nil {
		return "", "", nil, err
	}
	stateRoot, err = filepath.Abs(stateRoot)
	if err != nil {
		return "", "", nil, err
	}
	cfg, err = daemon.LoadConfig(filepath.Join(stateRoot, "config.json"))
	if err != nil {
		return "", "", nil, fmt.Errorf("load daemon configuration: %w", err)
	}
	if cfg.StateDir != "" {
		stateRoot, err = filepath.Abs(cfg.StateDir)
		if err != nil {
			return "", "", nil, err
		}
	}
	secretsRoot = cfg.SecretsRoot
	if secretsRoot == "" {
		home, homeErr := os.UserHomeDir()
		if homeErr != nil {
			return "", "", nil, homeErr
		}
		secretsRoot = filepath.Join(home, ".aplexica", "secrets")
	}
	secretsRoot, err = filepath.Abs(secretsRoot)
	if err != nil {
		return "", "", nil, err
	}
	return filepath.Clean(stateRoot), filepath.Clean(secretsRoot), cfg, nil
}

func authenticatedExistingGenesisStatus(ctx context.Context, stateRoot string, cfg *daemon.Config) (existingGenesisStatus, error) {
	if cfg == nil || cfg.Remote.Executable == "" {
		return existingGenesisStatus{}, errors.New("no signed remote plugin is configured")
	}
	execPath := cfg.Remote.Executable
	verified, err := verifyRemotePluginWithCompiledTrust(execPath)
	if err != nil {
		return existingGenesisStatus{}, fmt.Errorf("verify configured remote plugin: %w", err)
	}
	trustStore := truststate.Store{Root: filepath.Join(stateRoot, "remote-plugin-trust")}
	if _, err := trustStore.VerifyCurrent(execPath, verified, remotePluginTrustPolicy()); err != nil {
		return existingGenesisStatus{}, fmt.Errorf("authorize configured remote plugin: %w", err)
	}
	return queryAuthenticatedExistingGenesisStatus(ctx, execPath, func(callCtx context.Context, candidate string, args ...string) (preparedRemotePluginCommand, error) {
		if candidate != execPath {
			return nil, errors.New("configured plugin path changed before status")
		}
		return secureexec.Prepare(callCtx, candidate, verified.Manifest.BinarySHA256, args...)
	})
}

func queryAuthenticatedExistingGenesisStatus(ctx context.Context, execPath string, prepare remotePluginCommandPreparerForPath) (existingGenesisStatus, error) {
	if ctx == nil || execPath == "" || prepare == nil {
		return existingGenesisStatus{}, errors.New("authenticated plugin status unavailable")
	}
	statusCtx, cancel := context.WithTimeout(ctx, remoteExecTimeout)
	defer cancel()
	prepared, err := prepare(statusCtx, execPath, "--status")
	if err != nil {
		return existingGenesisStatus{}, errors.New("prepare authenticated plugin status failed")
	}
	defer func() { _ = prepared.Close() }()
	out, err := runRemoteCommandBounded(prepared.Cmd())
	if err != nil {
		return existingGenesisStatus{}, errors.New("authenticated plugin status failed")
	}
	defer clearExistingGenesisBytes(out)
	status, err := parseExistingGenesisStatus(out)
	if err != nil {
		return existingGenesisStatus{}, err
	}
	return status, nil
}

func parseExistingGenesisStatus(raw []byte) (existingGenesisStatus, error) {
	fields := map[string]string{}
	for _, line := range strings.Split(string(raw), "\n") {
		match := statusFieldRe.FindStringSubmatch(strings.TrimSpace(line))
		if match == nil {
			continue
		}
		key := match[1]
		if key != "paired" && key != "device_id" && key != "account_id" && key != "user_id" && key != "api_base_url" {
			continue
		}
		if _, duplicate := fields[key]; duplicate {
			return existingGenesisStatus{}, errors.New("authenticated plugin status contains duplicate identity fields")
		}
		fields[key] = strings.TrimSpace(match[2])
	}
	if !strings.EqualFold(fields["paired"], "yes") && !strings.EqualFold(fields["paired"], "true") {
		return existingGenesisStatus{}, errors.New("configured remote plugin is not paired")
	}
	status := existingGenesisStatus{
		ServiceOrigin: fields["api_base_url"], AccountID: fields["account_id"],
		UserID: fields["user_id"], DeviceID: fields["device_id"],
	}
	if !validExistingGenesisStatus(status) {
		if status.UserID == "" || status.UserID == "(unknown)" {
			return existingGenesisStatus{}, errors.New("paired credentials lack an authenticated user identity; refresh or re-pair the device")
		}
		return existingGenesisStatus{}, errors.New("authenticated plugin status contains invalid identity fields")
	}
	return status, nil
}

func validExistingGenesisStatus(status existingGenesisStatus) bool {
	if !existingGenesisIdentityField.MatchString(status.AccountID) || !existingGenesisIdentityField.MatchString(status.UserID) ||
		!existingGenesisIdentityField.MatchString(status.DeviceID) {
		return false
	}
	parsed, err := url.Parse(status.ServiceOrigin)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" ||
		parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") || parsed.Opaque != "" {
		return false
	}
	canonical := "https://" + parsed.Host
	return status.ServiceOrigin == canonical || status.ServiceOrigin == canonical+"/"
}

func clearExistingGenesisDeviceIdentity(device *keys.DeviceIdentity) {
	if device == nil {
		return
	}
	clearExistingGenesisBytes(device.WrapPrivate[:])
	clearExistingGenesisBytes(device.SigningPrivate)
	runtime.KeepAlive(device)
}

func clearExistingGenesisBytes(value []byte) {
	for i := range value {
		value[i] = 0
	}
	runtime.KeepAlive(value)
}
