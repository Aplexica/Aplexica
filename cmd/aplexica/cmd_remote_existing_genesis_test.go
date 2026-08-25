package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/aplexica/aplexica/internal/identity"
	"github.com/aplexica/aplexica/internal/keys"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/stretchr/testify/require"
)

const existingGenesisTestPhrase = "abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon art"

type existingGenesisPromptAnswer struct {
	value []byte
	err   error
}

type scriptedExistingGenesisPrompter struct {
	answers []existingGenesisPromptAnswer
	maxima  []int
}

func (p *scriptedExistingGenesisPrompter) ReadHidden(_ context.Context, _ string, maximum int) ([]byte, error) {
	p.maxima = append(p.maxima, maximum)
	if len(p.answers) == 0 {
		return nil, errors.New("no scripted input")
	}
	answer := p.answers[0]
	p.answers = p.answers[1:]
	if len(answer.value) > maximum {
		return answer.value, errors.New("bounded input rejected")
	}
	return answer.value, answer.err
}

func existingGenesisTestDevice(t *testing.T) keys.DeviceIdentity {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	wrapPrivate, wrapPublic, err := keys.NewDeviceKey()
	require.NoError(t, err)
	return keys.DeviceIdentity{
		SigningPrivate: private, SigningPublic: public, SigningKeyID: sha256.Sum256(public),
		WrapPrivate: wrapPrivate, WrapPublic: wrapPublic, WrapKeyID: sha256.Sum256(wrapPublic[:]),
	}
}

func existingGenesisTestStatus() existingGenesisStatus {
	return existingGenesisStatus{
		ServiceOrigin: "https://api.aplexica.com",
		AccountID:     "account-7", UserID: "user-7", DeviceID: "device-7",
	}
}

func requireClearedExistingGenesisBytes(t *testing.T, value []byte) {
	t.Helper()
	for i, b := range value {
		require.Zero(t, b, "byte %d was not cleared", i)
	}
}

func TestExistingAccountGenesisCommandUsesOnlyAuthenticatedIdentityAndClearsInputs(t *testing.T) {
	status := existingGenesisTestStatus()
	confirmation := []byte(existingGenesisConfirmationPrefix + status.AccountID)
	phrase := []byte(existingGenesisTestPhrase)
	prompter := &scriptedExistingGenesisPrompter{answers: []existingGenesisPromptAnswer{{value: confirmation}, {value: phrase}}}
	device := existingGenesisTestDevice(t)
	var installed, requested int
	var built identity.ExistingAccountGenesisInput
	var out bytes.Buffer
	err := runExistingAccountGenesis(context.Background(), &out, prompter, existingGenesisDependencies{
		Status: status, DeviceIdentity: device,
		Build: func(input identity.ExistingAccountGenesisInput) (identity.ExistingAccountGenesisResult, error) {
			built = input
			return identity.ExistingAccountGenesisResult{}, nil
		},
		Install: func(context.Context, identity.ExistingAccountGenesisResult) (identity.VerifiedRoster, error) {
			installed++
			return identity.VerifiedRoster{}, nil
		},
		RequestActivation: func(context.Context) error { requested++; return nil },
	})
	require.NoError(t, err)
	require.Equal(t, status.ServiceOrigin, built.ServiceOrigin)
	require.Equal(t, status.AccountID, built.AccountID)
	require.Equal(t, status.UserID, built.UserID)
	require.Equal(t, status.DeviceID, built.DeviceID)
	require.True(t, built.Confirmed)
	require.Equal(t, 1, installed)
	require.Equal(t, 1, requested)
	require.Equal(t, []int{existingGenesisConfirmationMax, identity.MaxRecoveryMnemonicBytes}, prompter.maxima)
	requireClearedExistingGenesisBytes(t, confirmation)
	requireClearedExistingGenesisBytes(t, phrase)
	require.NotContains(t, out.String(), existingGenesisTestPhrase)
	require.Contains(t, out.String(), status.AccountID)
	require.Contains(t, out.String(), status.DeviceID)
	require.Contains(t, out.String(), fmt.Sprintf("%x", device.SigningKeyID))
	require.Contains(t, out.String(), fmt.Sprintf("%x", device.WrapKeyID))
}

func TestExistingAccountGenesisConfirmationFailureMakesNoWrites(t *testing.T) {
	confirmation := []byte("NO")
	called := false
	prompter := &scriptedExistingGenesisPrompter{answers: []existingGenesisPromptAnswer{{value: confirmation}}}
	err := runExistingAccountGenesis(context.Background(), &bytes.Buffer{}, prompter, existingGenesisDependencies{
		Status: existingGenesisTestStatus(), DeviceIdentity: existingGenesisTestDevice(t),
		Build: func(identity.ExistingAccountGenesisInput) (identity.ExistingAccountGenesisResult, error) {
			called = true
			return identity.ExistingAccountGenesisResult{}, nil
		},
		Install: func(context.Context, identity.ExistingAccountGenesisResult) (identity.VerifiedRoster, error) {
			called = true
			return identity.VerifiedRoster{}, nil
		},
	})
	require.ErrorContains(t, err, "no identity state was written")
	require.False(t, called)
	requireClearedExistingGenesisBytes(t, confirmation)
}

func TestExistingAccountGenesisCancellationClearsReturnedSecretAndMakesNoWrites(t *testing.T) {
	status := existingGenesisTestStatus()
	confirmation := []byte(existingGenesisConfirmationPrefix + status.AccountID)
	partialSecret := []byte("abandon abandon partial")
	prompter := &scriptedExistingGenesisPrompter{answers: []existingGenesisPromptAnswer{
		{value: confirmation}, {value: partialSecret, err: context.Canceled},
	}}
	installed := false
	err := runExistingAccountGenesis(context.Background(), &bytes.Buffer{}, prompter, existingGenesisDependencies{
		Status: status, DeviceIdentity: existingGenesisTestDevice(t),
		Build: func(identity.ExistingAccountGenesisInput) (identity.ExistingAccountGenesisResult, error) {
			t.Fatal("build called after cancellation")
			return identity.ExistingAccountGenesisResult{}, nil
		},
		Install: func(context.Context, identity.ExistingAccountGenesisResult) (identity.VerifiedRoster, error) {
			installed = true
			return identity.VerifiedRoster{}, nil
		},
	})
	require.Error(t, err)
	require.False(t, installed)
	requireClearedExistingGenesisBytes(t, confirmation)
	requireClearedExistingGenesisBytes(t, partialSecret)
}

func TestExistingAccountGenesisBuildErrorDoesNotEchoPhraseOrRequestActivation(t *testing.T) {
	status := existingGenesisTestStatus()
	confirmation := []byte(existingGenesisConfirmationPrefix + status.AccountID)
	secret := []byte("this invalid recovery phrase must never appear")
	prompter := &scriptedExistingGenesisPrompter{answers: []existingGenesisPromptAnswer{{value: confirmation}, {value: secret}}}
	requested := false
	var out bytes.Buffer
	err := runExistingAccountGenesis(context.Background(), &out, prompter, existingGenesisDependencies{
		Status: status, DeviceIdentity: existingGenesisTestDevice(t),
		Build: identity.BuildExistingAccountGenesis,
		Install: func(context.Context, identity.ExistingAccountGenesisResult) (identity.VerifiedRoster, error) {
			t.Fatal("install called after invalid phrase")
			return identity.VerifiedRoster{}, nil
		},
		RequestActivation: func(context.Context) error { requested = true; return nil },
	})
	require.Error(t, err)
	require.False(t, requested)
	require.NotContains(t, err.Error(), "this invalid recovery phrase")
	require.NotContains(t, out.String(), "this invalid recovery phrase")
	requireClearedExistingGenesisBytes(t, secret)
}

func TestExistingAccountGenesisRequestsActivationOnlyAfterInstallSuccess(t *testing.T) {
	status := existingGenesisTestStatus()
	confirmation := []byte(existingGenesisConfirmationPrefix + status.AccountID)
	phrase := []byte(existingGenesisTestPhrase)
	prompter := &scriptedExistingGenesisPrompter{answers: []existingGenesisPromptAnswer{{value: confirmation}, {value: phrase}}}
	requested := false
	err := runExistingAccountGenesis(context.Background(), &bytes.Buffer{}, prompter, existingGenesisDependencies{
		Status: status, DeviceIdentity: existingGenesisTestDevice(t),
		Build: func(identity.ExistingAccountGenesisInput) (identity.ExistingAccountGenesisResult, error) {
			return identity.ExistingAccountGenesisResult{}, nil
		},
		Install: func(context.Context, identity.ExistingAccountGenesisResult) (identity.VerifiedRoster, error) {
			return identity.VerifiedRoster{}, errors.New("durable write failed")
		},
		RequestActivation: func(context.Context) error { requested = true; return nil },
	})
	require.Error(t, err)
	require.False(t, requested)
}

func TestExistingAccountGenesisProductionCommandRejectsNonInteractiveAndArgvInputs(t *testing.T) {
	cmd := &cobra.Command{}
	cmd.SetIn(bytes.NewBufferString(existingGenesisTestPhrase))
	cmd.SetOut(&bytes.Buffer{})
	_, _, err := newExistingGenesisTTY(cmd)
	require.ErrorContains(t, err, "interactive local terminal")

	require.Error(t, remoteInitializeExistingAccountCmd.Args(remoteInitializeExistingAccountCmd, []string{existingGenesisTestPhrase}))
	flagNames := []string{}
	remoteInitializeExistingAccountCmd.LocalNonPersistentFlags().VisitAll(func(flag *pflag.Flag) { flagNames = append(flagNames, flag.Name) })
	require.Empty(t, flagNames, "recovery and identity inputs must have no command flags")
}

func TestParseExistingGenesisStatusRequiresAuthenticatedUserBinding(t *testing.T) {
	valid := []byte("api_base_url: https://api.aplexica.com\npaired: yes\ndevice_id: device-7\naccount_id: account-7\nuser_id: user-7\n")
	status, err := parseExistingGenesisStatus(valid)
	require.NoError(t, err)
	require.Equal(t, existingGenesisTestStatus(), status)

	legacy := []byte("api_base_url: https://api.aplexica.com\npaired: yes\ndevice_id: device-7\naccount_id: account-7\n")
	_, err = parseExistingGenesisStatus(legacy)
	require.ErrorContains(t, err, "refresh or re-pair")

	duplicate := append(append([]byte(nil), valid...), []byte("user_id: attacker\n")...)
	_, err = parseExistingGenesisStatus(duplicate)
	require.ErrorContains(t, err, "duplicate")
}

type existingGenesisPreparedCommand struct{ command *exec.Cmd }

func (p *existingGenesisPreparedCommand) Cmd() *exec.Cmd { return p.command }
func (p *existingGenesisPreparedCommand) Close() error   { return nil }

func TestQueryAuthenticatedExistingGenesisStatusUsesStatusOnlyArgvAndNoIdentityEnvironment(t *testing.T) {
	var command *exec.Cmd
	status, err := queryAuthenticatedExistingGenesisStatus(context.Background(), "/signed/plugin", func(ctx context.Context, path string, args ...string) (preparedRemotePluginCommand, error) {
		require.Equal(t, "/signed/plugin", path)
		require.Equal(t, []string{"--status"}, args)
		command = exec.CommandContext(ctx, os.Args[0], "-test.run=^TestExistingGenesisStatusHelperProcess$")
		command.Env = append(os.Environ(), "GO_WANT_EXISTING_GENESIS_STATUS_HELPER=1")
		return &existingGenesisPreparedCommand{command: command}, nil
	})
	require.NoError(t, err)
	require.Equal(t, existingGenesisTestStatus(), status)
	combined := strings.Join(append(append([]string(nil), command.Args...), command.Env...), "\n")
	for _, forbidden := range []string{existingGenesisTestPhrase, status.AccountID, status.UserID, status.DeviceID} {
		require.NotContains(t, combined, forbidden)
	}
}

func TestExistingGenesisStatusHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_EXISTING_GENESIS_STATUS_HELPER") != "1" {
		return
	}
	fmt.Print("api_base_url: https://api.aplexica.com\npaired: yes\ndevice_id: device-7\naccount_id: account-7\nuser_id: user-7\n")
	os.Exit(0)
}
