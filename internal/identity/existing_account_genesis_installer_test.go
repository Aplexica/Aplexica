package identity

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/securityepoch"
	"github.com/aplexica/aplexica/internal/securityerr"
	"github.com/stretchr/testify/require"
)

var (
	existingAccountGenesisInstallerFixtureOnce sync.Once
	existingAccountGenesisInstallerResult      ExistingAccountGenesisResult
)

func existingAccountGenesisInstallerFixture(t *testing.T) ExistingAccountGenesisResult {
	t.Helper()
	existingAccountGenesisInstallerFixtureOnce.Do(func() {
		mnemonic := []byte(existingAccountGenesisTestMnemonic)
		result, err := BuildExistingAccountGenesis(existingAccountGenesisInput(t, mnemonic))
		require.NoError(t, err)
		requireCleared(t, mnemonic)
		existingAccountGenesisInstallerResult = result
	})
	return existingAccountGenesisInstallerResult
}

func genesisInstallPaths(root string) (journal, chain, epoch, coordinator string) {
	account := filepath.Join(root, "account")
	return filepath.Join(account, securityepoch.TransitionJournalFilename),
		filepath.Join(account, existingAccountGenesisChainFile),
		filepath.Join(account, existingAccountGenesisEpochFile),
		filepath.Join(account, "security-coordinator.json")
}

func pathExists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false
	}
	require.NoError(t, err)
	return true
}

func requireJournalPreserved(t *testing.T, path string) {
	t.Helper()
	require.True(t, pathExists(t, path), "transition journal must remain for recovery")
}

func TestExistingAccountGenesisInstallerRecoversEveryCrashBoundary(t *testing.T) {
	result := existingAccountGenesisInstallerFixture(t)
	crash := errors.New("simulated process crash")
	tests := []struct {
		name            string
		hooks           existingAccountGenesisInstallHooks
		wantChain       bool
		wantEpoch       bool
		wantCoordinator bool
	}{
		{name: "after journal", hooks: existingAccountGenesisInstallHooks{afterJournalDurable: func() error { return crash }}},
		{name: "after chain", hooks: existingAccountGenesisInstallHooks{afterChainInstalled: func() error { return crash }}, wantChain: true},
		{name: "after security epoch", hooks: existingAccountGenesisInstallHooks{afterSecurityEpochStored: func() error { return crash }}, wantChain: true, wantEpoch: true},
		{name: "after coordinator", hooks: existingAccountGenesisInstallHooks{afterCoordinatorStored: func() error { return crash }}, wantChain: true, wantEpoch: true, wantCoordinator: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "identity")
			coordinator := &securityepoch.Coordinator{Root: root}
			installer := &ExistingAccountGenesisInstaller{IdentityRoot: root, Coordinator: coordinator, hooks: test.hooks}
			_, err := installer.Install(context.Background(), result)
			require.ErrorIs(t, err, crash)
			journalPath, chainPath, epochPath, coordinatorPath := genesisInstallPaths(root)
			requireJournalPreserved(t, journalPath)
			require.Equal(t, test.wantChain, pathExists(t, chainPath))
			require.Equal(t, test.wantEpoch, pathExists(t, epochPath))
			require.Equal(t, test.wantCoordinator, pathExists(t, coordinatorPath))

			// The durable journal gates even legacy publication throughout a
			// partial install, including after the coordinator file exists.
			if lease, leaseErr := coordinator.AcquirePublish(context.Background(), "account", securityepoch.SecurityEpoch{}); leaseErr == nil {
				_ = lease.Close()
				t.Fatal("legacy publication reopened during partial genesis")
			}

			restarted := &ExistingAccountGenesisInstaller{
				IdentityRoot: root, Coordinator: &securityepoch.Coordinator{Root: root},
			}
			recovered, found, err := restarted.Recover(context.Background())
			require.NoError(t, err)
			require.True(t, found)
			require.Equal(t, result.Verified.Hash, recovered.Hash)
			require.False(t, pathExists(t, journalPath))
			require.True(t, pathExists(t, chainPath))
			require.True(t, pathExists(t, epochPath))
			require.True(t, pathExists(t, coordinatorPath))

			chain := &ChainStore{Path: chainPath}
			current, err := chain.Current(existingAccountGenesisTestTime.Add(time.Hour))
			require.NoError(t, err)
			require.Equal(t, result.Verified.Hash, current.Hash)
			require.NoError(t, restarted.Coordinator.VerifyCurrent("account", coordinatorEpochFromGenesis(result.SecurityEpoch)))

			_, found, err = restarted.Recover(context.Background())
			require.NoError(t, err)
			require.False(t, found)
		})
	}
}

func TestExistingAccountGenesisInstallerIsExactlyIdempotent(t *testing.T) {
	root := filepath.Join(t.TempDir(), "identity")
	result := existingAccountGenesisInstallerFixture(t)
	coordinator := &securityepoch.Coordinator{Root: root}
	installer := &ExistingAccountGenesisInstaller{IdentityRoot: root, Coordinator: coordinator}
	first, err := installer.Install(context.Background(), result)
	require.NoError(t, err)
	second, err := installer.Install(context.Background(), result)
	require.NoError(t, err)
	require.Equal(t, first.Hash, second.Hash)
	journal, _, _, _ := genesisInstallPaths(root)
	require.False(t, pathExists(t, journal))
	require.NoError(t, coordinator.VerifyCurrent("account", coordinatorEpochFromGenesis(result.SecurityEpoch)))
}

func TestExistingAccountGenesisInstallerAppliesOnlyTheDurableJournal(t *testing.T) {
	fixture := existingAccountGenesisInstallerFixture(t)
	record := existingAccountGenesisJournal{
		Version: existingAccountGenesisJournalVersion, Chain: fixture.Chain, SecurityEpoch: fixture.SecurityEpoch,
	}
	raw, err := json.Marshal(record)
	require.NoError(t, err)
	var clone existingAccountGenesisJournal
	require.NoError(t, json.Unmarshal(raw, &clone))
	candidate := ExistingAccountGenesisResult{Chain: clone.Chain, Verified: fixture.Verified, SecurityEpoch: clone.SecurityEpoch}

	root := filepath.Join(t.TempDir(), "identity")
	installer := &ExistingAccountGenesisInstaller{
		IdentityRoot: root,
		hooks: existingAccountGenesisInstallHooks{afterJournalDurable: func() error {
			// The API value is caller-owned. Authority installation must use the
			// exact durable journal even if that value changes after the fsync.
			candidate.Chain.ExpectedRecoveryRoot[0] ^= 1
			return nil
		}},
	}
	verified, err := installer.Install(context.Background(), candidate)
	require.NoError(t, err)
	require.Equal(t, fixture.Verified.Hash, verified.Hash)
	chain := &ChainStore{Path: filepath.Join(root, "account", existingAccountGenesisChainFile)}
	current, err := chain.Current(existingAccountGenesisTestTime.Add(time.Hour))
	require.NoError(t, err)
	require.Equal(t, fixture.Verified.Hash, current.Hash)
}

func TestExistingAccountGenesisJournalContainsOnlySignedPublicState(t *testing.T) {
	root := filepath.Join(t.TempDir(), "identity")
	result := existingAccountGenesisInstallerFixture(t)
	crash := errors.New("stop after journal")
	installer := &ExistingAccountGenesisInstaller{
		IdentityRoot: root, Coordinator: &securityepoch.Coordinator{Root: root},
		hooks: existingAccountGenesisInstallHooks{afterJournalDurable: func() error { return crash }},
	}
	_, err := installer.Install(context.Background(), result)
	require.ErrorIs(t, err, crash)
	journalPath, _, _, _ := genesisInstallPaths(root)
	raw, err := os.ReadFile(journalPath)
	require.NoError(t, err)
	require.NotContains(t, string(raw), existingAccountGenesisTestMnemonic)
	for _, forbidden := range []string{"ConfirmedRecoveryMnemonic", "SigningSeed", "SigningPrivate", "WrapPrivate", "mnemonic", "recoveryPrivate"} {
		require.NotContains(t, string(raw), forbidden)
	}
	record, canonical, found, verified, err := readExistingAccountGenesisJournalFromPath(root)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, raw, canonical)
	require.Equal(t, result.Verified.Hash, verified.Hash)
	require.Equal(t, existingAccountGenesisJournalVersion, record.Version)
	require.NotZero(t, record.Checksum)

	if runtime.GOOS != "windows" {
		info, err := os.Stat(journalPath)
		require.NoError(t, err)
		require.Zero(t, info.Mode().Perm()&0o077)
		accountInfo, err := os.Stat(filepath.Dir(journalPath))
		require.NoError(t, err)
		require.Zero(t, accountInfo.Mode().Perm()&0o077)
	}
}

func readExistingAccountGenesisJournalFromPath(root string) (existingAccountGenesisJournal, []byte, bool, VerifiedRoster, error) {
	installer := &ExistingAccountGenesisInstaller{IdentityRoot: root}
	account, _, err := installer.open(context.Background())
	if err != nil {
		return existingAccountGenesisJournal{}, nil, false, VerifiedRoster{}, err
	}
	defer account.Close()
	return readExistingAccountGenesisJournal(account)
}

func TestExistingAccountGenesisInstallerFailsClosedOnEveryAuthorityMismatch(t *testing.T) {
	result := existingAccountGenesisInstallerFixture(t)
	tests := []struct {
		name   string
		setup  func(*testing.T, string, ExistingAccountGenesisResult)
		mutate func(*ExistingAccountGenesisResult)
	}{
		{
			name: "existing journal differs",
			setup: func(t *testing.T, root string, result ExistingAccountGenesisResult) {
				stop := errors.New("journal only")
				installer := &ExistingAccountGenesisInstaller{IdentityRoot: root, hooks: existingAccountGenesisInstallHooks{afterJournalDurable: func() error { return stop }}}
				_, err := installer.Install(context.Background(), result)
				require.ErrorIs(t, err, stop)
			},
			mutate: func(result *ExistingAccountGenesisResult) { result.SecurityEpoch.BarrierID[0] ^= 1 },
		},
		{
			name: "chain bytes differ",
			setup: func(t *testing.T, root string, result ExistingAccountGenesisResult) {
				stop := errors.New("journal only")
				installer := &ExistingAccountGenesisInstaller{IdentityRoot: root, hooks: existingAccountGenesisInstallHooks{afterJournalDurable: func() error { return stop }}}
				_, err := installer.Install(context.Background(), result)
				require.ErrorIs(t, err, stop)
				_, chain, _, _ := genesisInstallPaths(root)
				require.NoError(t, os.WriteFile(chain, []byte("foreign chain"), 0o600))
			},
		},
		{
			name: "security epoch bytes differ",
			setup: func(t *testing.T, root string, result ExistingAccountGenesisResult) {
				stop := errors.New("after chain")
				installer := &ExistingAccountGenesisInstaller{IdentityRoot: root, hooks: existingAccountGenesisInstallHooks{afterChainInstalled: func() error { return stop }}}
				_, err := installer.Install(context.Background(), result)
				require.ErrorIs(t, err, stop)
				_, _, epoch, _ := genesisInstallPaths(root)
				require.NoError(t, os.WriteFile(epoch, []byte("{}"), 0o600))
			},
		},
		{
			name: "coordinator tuple differs",
			setup: func(t *testing.T, root string, result ExistingAccountGenesisResult) {
				stop := errors.New("after epoch")
				installer := &ExistingAccountGenesisInstaller{IdentityRoot: root, hooks: existingAccountGenesisInstallHooks{afterSecurityEpochStored: func() error { return stop }}}
				_, err := installer.Install(context.Background(), result)
				require.ErrorIs(t, err, stop)
				conflict := coordinatorEpochFromGenesis(result.SecurityEpoch)
				conflict.BarrierID[0] ^= 1
				require.NoError(t, (&securityepoch.Coordinator{Root: root}).Transition(context.Background(), "account", conflict, func() error { return nil }))
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "identity")
			test.setup(t, root, result)
			journalPath, chainPath, epochPath, _ := genesisInstallPaths(root)
			beforeJournal, err := os.ReadFile(journalPath)
			require.NoError(t, err)
			candidate := result
			if test.mutate != nil {
				test.mutate(&candidate)
			}
			installer := &ExistingAccountGenesisInstaller{IdentityRoot: root, Coordinator: &securityepoch.Coordinator{Root: root}}
			if test.mutate != nil {
				_, err = installer.Install(context.Background(), candidate)
			} else {
				_, _, err = installer.Recover(context.Background())
			}
			require.Error(t, err)
			requireJournalPreserved(t, journalPath)
			afterJournal, readErr := os.ReadFile(journalPath)
			require.NoError(t, readErr)
			require.Equal(t, beforeJournal, afterJournal)
			if test.name == "chain bytes differ" {
				require.Equal(t, []byte("foreign chain"), mustReadFile(t, chainPath))
			}
			if test.name == "security epoch bytes differ" {
				require.Equal(t, []byte("{}"), mustReadFile(t, epochPath))
			}
		})
	}
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	value, err := os.ReadFile(path)
	require.NoError(t, err)
	return value
}

func TestExistingAccountGenesisRecoveryRejectsCorruptUnsafeOrLinkedJournal(t *testing.T) {
	result := existingAccountGenesisInstallerFixture(t)
	tests := []struct {
		name   string
		mutate func(*testing.T, string)
	}{
		{name: "corrupt checksum", mutate: func(t *testing.T, journal string) {
			raw := mustReadFile(t, journal)
			raw[len(raw)/2] ^= 1
			require.NoError(t, os.WriteFile(journal, raw, 0o600))
		}},
		{name: "noncanonical bytes", mutate: func(t *testing.T, journal string) {
			raw := append(mustReadFile(t, journal), '\n')
			require.NoError(t, os.WriteFile(journal, raw, 0o600))
		}},
		{name: "group readable", mutate: func(t *testing.T, journal string) {
			if runtime.GOOS == "windows" {
				t.Skip("Unix permission mode test")
			}
			require.NoError(t, os.Chmod(journal, 0o640))
		}},
		{name: "hard linked", mutate: func(t *testing.T, journal string) {
			require.NoError(t, os.Link(journal, filepath.Join(t.TempDir(), "journal-link")))
		}},
		{name: "symlink", mutate: func(t *testing.T, journal string) {
			target := filepath.Join(t.TempDir(), "journal-target")
			require.NoError(t, os.WriteFile(target, mustReadFile(t, journal), 0o600))
			require.NoError(t, os.Remove(journal))
			if err := os.Symlink(target, journal); err != nil {
				if runtime.GOOS == "windows" {
					t.Skipf("Windows token cannot create symlinks: %v", err)
				}
				t.Fatal(err)
			}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "identity")
			stop := errors.New("journal only")
			installer := &ExistingAccountGenesisInstaller{IdentityRoot: root, hooks: existingAccountGenesisInstallHooks{afterJournalDurable: func() error { return stop }}}
			_, err := installer.Install(context.Background(), result)
			require.ErrorIs(t, err, stop)
			journal, _, _, _ := genesisInstallPaths(root)
			test.mutate(t, journal)
			_, _, err = (&ExistingAccountGenesisInstaller{IdentityRoot: root}).Recover(context.Background())
			require.Error(t, err)
			requireJournalPreserved(t, journal)
		})
	}
}

func TestExistingAccountGenesisRecoveryRejectsLinkedOrSymlinkedAuthority(t *testing.T) {
	result := existingAccountGenesisInstallerFixture(t)
	tests := []struct {
		name  string
		plant func(*testing.T, string, []byte)
	}{
		{name: "hard link", plant: func(t *testing.T, path string, exact []byte) {
			source := filepath.Join(t.TempDir(), "chain-source")
			require.NoError(t, os.WriteFile(source, exact, 0o600))
			require.NoError(t, os.Link(source, path))
		}},
		{name: "symlink", plant: func(t *testing.T, path string, exact []byte) {
			target := filepath.Join(t.TempDir(), "chain-target")
			require.NoError(t, os.WriteFile(target, exact, 0o600))
			if err := os.Symlink(target, path); err != nil {
				if runtime.GOOS == "windows" {
					t.Skipf("Windows token cannot create symlinks: %v", err)
				}
				t.Fatal(err)
			}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "identity")
			stop := errors.New("journal only")
			installer := &ExistingAccountGenesisInstaller{IdentityRoot: root, hooks: existingAccountGenesisInstallHooks{afterJournalDurable: func() error { return stop }}}
			_, err := installer.Install(context.Background(), result)
			require.ErrorIs(t, err, stop)
			journal, chain, _, _ := genesisInstallPaths(root)
			chainBytes, _, err := canonicalGenesisChain(result.Chain)
			require.NoError(t, err)
			test.plant(t, chain, chainBytes)
			_, _, err = (&ExistingAccountGenesisInstaller{IdentityRoot: root}).Recover(context.Background())
			require.Error(t, err)
			requireJournalPreserved(t, journal)
		})
	}
}

func TestExistingAccountGenesisInstallerRejectsUnsafeRootAndCancellation(t *testing.T) {
	result := existingAccountGenesisInstallerFixture(t)
	relative := &ExistingAccountGenesisInstaller{IdentityRoot: "relative"}
	_, err := relative.Install(context.Background(), result)
	require.Error(t, err)

	base := t.TempDir()
	target := filepath.Join(base, "target")
	require.NoError(t, os.Mkdir(target, 0o700))
	linked := filepath.Join(base, "identity")
	if err := os.Symlink(target, linked); err != nil {
		if runtime.GOOS != "windows" {
			t.Fatal(err)
		}
	} else {
		_, err = (&ExistingAccountGenesisInstaller{IdentityRoot: linked}).Install(context.Background(), result)
		require.Error(t, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	root := filepath.Join(t.TempDir(), "identity")
	_, err = (&ExistingAccountGenesisInstaller{IdentityRoot: root}).Install(ctx, result)
	require.ErrorIs(t, err, context.Canceled)
	require.False(t, pathExists(t, filepath.Join(root, "account")))
}

func TestExistingAccountGenesisJournalGateReturnsStaleRoster(t *testing.T) {
	root := filepath.Join(t.TempDir(), "identity")
	result := existingAccountGenesisInstallerFixture(t)
	stop := errors.New("journal only")
	coordinator := &securityepoch.Coordinator{Root: root}
	installer := &ExistingAccountGenesisInstaller{IdentityRoot: root, Coordinator: coordinator, hooks: existingAccountGenesisInstallHooks{afterJournalDurable: func() error { return stop }}}
	_, err := installer.Install(context.Background(), result)
	require.ErrorIs(t, err, stop)
	_, err = coordinator.AcquirePublish(context.Background(), "account", securityepoch.SecurityEpoch{})
	require.ErrorIs(t, err, securityerr.ErrStaleRoster)
	called := false
	err = coordinator.WithAdmission("account", func(securityepoch.SecurityEpoch) error { called = true; return nil })
	require.Error(t, err)
	require.False(t, called)
}

func TestExistingAccountGenesisInstallerRejectsCoordinatorRootMismatch(t *testing.T) {
	result := existingAccountGenesisInstallerFixture(t)
	root := filepath.Join(t.TempDir(), "identity")
	other := filepath.Join(t.TempDir(), "other")
	installer := &ExistingAccountGenesisInstaller{IdentityRoot: root, Coordinator: &securityepoch.Coordinator{Root: other}}
	_, err := installer.Install(context.Background(), result)
	require.Error(t, err)
}

func TestExistingAccountGenesisInstalledFilesArePrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows ACLs are validated by privatefs")
	}
	root := filepath.Join(t.TempDir(), "identity")
	result := existingAccountGenesisInstallerFixture(t)
	_, err := (&ExistingAccountGenesisInstaller{IdentityRoot: root}).Install(context.Background(), result)
	require.NoError(t, err)
	_, chain, epoch, coordinator := genesisInstallPaths(root)
	for _, path := range []string{chain, epoch, coordinator} {
		info, err := os.Stat(path)
		require.NoError(t, err)
		require.Zero(t, info.Mode().Perm()&0o077, path)
	}
}
