package privatefs

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestEnsureDirSecuresNewPrivateRoot(t *testing.T) {
	rootPath := filepath.Join(t.TempDir(), "private")
	require.NoError(t, EnsureDir(rootPath, DirPolicy{Access: AccessPrivate, AllowExisting: true}))
	require.NoError(t, ValidateDir(rootPath))
}

func TestEnsureDirSecuresEveryNewPrivateRootComponent(t *testing.T) {
	base := t.TempDir()
	rootPath := filepath.Join(base, "identity", "account")
	require.NoError(t, EnsureDir(rootPath, DirPolicy{Access: AccessPrivate, AllowExisting: true}))
	require.NoError(t, ValidateDir(filepath.Join(base, "identity")))
	require.NoError(t, ValidateDir(rootPath))
}

// TestEnsureDirConcurrentFirstUseIsProtected drives a concurrent first use of a
// multi-component private root. On Windows MkdirAll publishes every created
// component carrying its parent's inherited DACL, and the chain only becomes
// protected once the repair walk runs over it; a peer that observes the path
// inside that window takes the existing-path branch with its own policy. The
// policy here deliberately leaves RepairOwned unset, which is the shape that
// reproduces the defect: such a peer declines repair and fails closed on a
// descriptor this process is midway through normalizing. The root is
// multi-component on purpose so the outermost-first repair walk is exercised,
// which is where the leaf stays inherited longest.
func TestEnsureDirConcurrentFirstUseIsProtected(t *testing.T) {
	base := t.TempDir()
	leaf := filepath.Join(base, "a", "b", "c")
	const n = 100

	errs := make(chan error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- EnsureDir(leaf, DirPolicy{Access: AccessPrivate, AllowExisting: true})
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err, "a concurrent first use must never observe an unprotected component")
	}

	require.NoError(t, ValidateDir(filepath.Join(base, "a")))
	require.NoError(t, ValidateDir(filepath.Join(base, "a", "b")))
	require.NoError(t, ValidateDir(leaf))
}

// TestRootDirectoryHardenDoesNotUnprotectConcurrentChildren contends a repeated
// harden of one private directory against concurrent creation, read-back, and
// removal of files inside it.
//
// (*Root).EnsureDir hardens its directory unconditionally on every call: the
// fs.ErrExist branch always reaches hardenRelative, which issues SetSecurityInfo
// with DACL_SECURITY_INFORMATION. privateACL grants through CONTAINER_INHERIT_ACE
// and OBJECT_INHERIT_ACE, so on Windows setting a container's DACL propagates
// inheritance to that container's existing children — a client-side, non-atomic,
// per-child read-modify-write. A child created moments earlier carries its
// parent's inherited DACL until CreateExclusive protects it, so a propagation
// that reads the child before that protect and writes it afterwards silently
// drops SE_DACL_PROTECTED. The read-back then fails closed, which is precisely
// what apx-public-v1070 observed on windows-latest, reported against a
// secrets\_device\.secret-* temporary.
//
// This is the privatefs-layer shape of secrets.Store.GetOrCreate — EnsureDir on
// the artifact directory, CreateTemp beneath it, then OpenReadRegular on the
// staged temp — run denser than the real caller so the window is hit sooner.
//
// The test is meaningful only on Windows. POSIX has no SE_DACL_PROTECTED and
// hardenRelative is a plain fchmod on a single inode with no inheritance
// analogue, so every assertion here holds trivially on darwin and linux. It is
// deliberately left unguarded rather than tagged: it must keep compiling and
// running everywhere, and the race detector on POSIX still exercises the
// concurrent access pattern.
func TestRootDirectoryHardenDoesNotUnprotectConcurrentChildren(t *testing.T) {
	base := t.TempDir()
	root, err := OpenRoot(base, DirPolicy{Access: AccessPrivate, RepairOwned: true, AllowExisting: true})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, root.Close()) })
	require.NoError(t, root.EnsureDir("d", DirPolicy{Access: AccessPrivate, AllowExisting: true}))

	const (
		hardeners  = 50
		workers    = 50
		iterations = 20
	)

	// A hardener reports at most one failure per iteration; a worker at most two.
	// Size for that so a send can never block and strand wg.Wait.
	errs := make(chan error, (hardeners+2*workers)*iterations)
	var wg sync.WaitGroup

	for i := 0; i < hardeners; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				if err := root.EnsureDir("d", DirPolicy{Access: AccessPrivate, AllowExisting: true}); err != nil {
					errs <- fmt.Errorf("harden directory: %w", err)
				}
			}
		}()
	}

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				f, rel, err := root.CreateTemp("d", ".x-")
				if err != nil {
					errs <- fmt.Errorf("create staged child: %w", err)
					continue
				}
				if err := f.Close(); err != nil {
					errs <- fmt.Errorf("close staged child: %w", err)
					continue
				}
				// The mirror of secrets/store.go's staged read-back: reopen the
				// completed temp through the retained root and validate it before
				// it is eligible to become identity material. This validate has no
				// repair step, so a lost SE_DACL_PROTECTED is terminal here.
				rf, err := root.OpenReadRegular(rel)
				if err != nil {
					errs <- fmt.Errorf("read back staged child: %w", err)
					_ = root.RemoveRegular(rel)
					continue
				}
				if err := rf.Close(); err != nil {
					errs <- fmt.Errorf("close staged read-back: %w", err)
				}
				if err := root.RemoveRegular(rel); err != nil {
					errs <- fmt.Errorf("remove staged child: %w", err)
				}
			}
		}()
	}

	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err, "hardening a private directory must never unprotect a child of it")
	}
}

func TestEnsureDirRejectsLinkAsNearestExistingAncestor(t *testing.T) {
	base := t.TempDir()
	realRoot := filepath.Join(base, "real")
	require.NoError(t, os.Mkdir(realRoot, 0o700))
	linkRoot := filepath.Join(base, "link")
	if err := os.Symlink(realRoot, linkRoot); err != nil {
		t.Skipf("symlink fixture unavailable: %v", err)
	}
	require.Error(t, EnsureDir(filepath.Join(linkRoot, "nested"), DirPolicy{Access: AccessPrivate, AllowExisting: true}))
	_, err := os.Stat(filepath.Join(realRoot, "nested"))
	require.ErrorIs(t, err, fs.ErrNotExist)
}

func TestRootEnsureDirRetryRepeatsParentDurabilityBarrier(t *testing.T) {
	rootPath := filepath.Join(t.TempDir(), "private")
	root, err := OpenRoot(rootPath, DirPolicy{Access: AccessPrivate, AllowExisting: true})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, root.Close()) })

	injected := errors.New("injected first directory sync failure")
	rootSyncAttempts := 0
	root.syncDirHook = func(rel string) error {
		if rel == "." {
			rootSyncAttempts++
			if rootSyncAttempts == 1 {
				return injected
			}
		}
		return nil
	}
	err = root.EnsureDir(filepath.Join("events", "conversations"), DirPolicy{Access: AccessPrivate, AllowExisting: true})
	require.ErrorIs(t, err, injected)
	require.DirExists(t, filepath.Join(rootPath, "events"), "the component was created before its parent sync failed")

	require.NoError(t, root.EnsureDir(filepath.Join("events", "conversations"), DirPolicy{Access: AccessPrivate, AllowExisting: true}))
	require.GreaterOrEqual(t, rootSyncAttempts, 2, "retry must repeat the missed parent durability barrier")
	require.DirExists(t, filepath.Join(rootPath, "events", "conversations"))
}

func TestRootSecuresNewFilesAndDirectories(t *testing.T) {
	rootPath := filepath.Join(t.TempDir(), "private")
	root, err := OpenRoot(rootPath, DirPolicy{Access: AccessPrivate, AllowExisting: true})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, root.Close()) })

	require.NoError(t, root.Mkdir("nested", DirPolicy{Access: AccessPrivate}))
	nested, err := OpenRoot(filepath.Join(rootPath, "nested"), DirPolicy{Access: AccessPrivate})
	require.NoError(t, err)
	require.NoError(t, nested.Close())

	file, err := root.CreateExclusive(filepath.Join("nested", "state"), FilePolicy{RequirePrivateParent: true, RejectWritableByOthers: true})
	require.NoError(t, err)
	require.NoError(t, file.Close())
	opened, err := root.OpenReadRegular(filepath.Join("nested", "state"))
	require.NoError(t, err)
	require.NoError(t, opened.Close())
}

func TestOpenNativeRootPreservesHostNativeColonName(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("colon is not a legal Windows filename component")
	}
	rootPath := filepath.Join(t.TempDir(), "native")
	name := "rollout-2026-06-30T18:16:48.3NZ.jsonl"
	require.NoError(t, os.Mkdir(rootPath, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(rootPath, name), []byte("turn"), 0o600))

	strict, err := OpenRoot(rootPath, DirPolicy{Access: AccessIntegrityOnly})
	require.NoError(t, err)
	_, err = strict.OpenReadRegularIntegrity(name)
	require.Error(t, err)
	require.NoError(t, strict.Close())

	native, err := OpenNativeRoot(rootPath, DirPolicy{Access: AccessIntegrityOnly})
	require.NoError(t, err)
	opened, err := native.OpenReadRegularIntegrity(name)
	require.NoError(t, err)
	data, err := os.ReadFile(filepath.Join(rootPath, name))
	require.NoError(t, err)
	require.Equal(t, "turn", string(data))
	require.NoError(t, opened.Close())
	require.NoError(t, native.Close())
}

func TestNativeRootRejectsUnixBackslashAlias(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("backslash is the native Windows separator")
	}
	rootPath := filepath.Join(t.TempDir(), "native")
	native, err := OpenNativeRoot(rootPath, DirPolicy{Access: AccessPrivate, AllowExisting: true})
	require.NoError(t, err)
	_, err = native.CreateExclusive(`nested\file`, FilePolicy{RejectWritableByOthers: true})
	require.Error(t, err)
	require.NoError(t, native.Close())
	_, err = os.Stat(filepath.Join(rootPath, "nested", "file"))
	require.ErrorIs(t, err, fs.ErrNotExist)
}

func TestOpenReadRegularRejectsHardLinks(t *testing.T) {
	rootPath := filepath.Join(t.TempDir(), "private")
	root, err := OpenRoot(rootPath, DirPolicy{Access: AccessPrivate, AllowExisting: true})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, root.Close()) })

	file, err := root.CreateExclusive("state", FilePolicy{RequirePrivateParent: true, RejectWritableByOthers: true})
	require.NoError(t, err)
	require.NoError(t, file.Close())
	require.NoError(t, os.Link(filepath.Join(rootPath, "state"), filepath.Join(rootPath, "alias")))
	_, err = root.OpenReadRegular("state")
	require.Error(t, err)
}

func TestInstallNoReplaceCreatesDestinationWithoutOverwritingWinner(t *testing.T) {
	rootPath := filepath.Join(t.TempDir(), "private")
	root, err := OpenRoot(rootPath, DirPolicy{Access: AccessPrivate, AllowExisting: true})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, root.Close()) })

	first, err := root.CreateExclusive("first.tmp", FilePolicy{RequirePrivateParent: true, RejectWritableByOthers: true})
	require.NoError(t, err)
	_, err = first.WriteString("winner")
	require.NoError(t, err)
	require.NoError(t, first.Close())
	require.NoError(t, root.InstallNoReplace("first.tmp", "identity"))

	second, err := root.CreateExclusive("second.tmp", FilePolicy{RequirePrivateParent: true, RejectWritableByOthers: true})
	require.NoError(t, err)
	_, err = second.WriteString("loser")
	require.NoError(t, err)
	require.NoError(t, second.Close())
	err = root.InstallNoReplace("second.tmp", "identity")
	require.ErrorIs(t, err, fs.ErrExist)

	got, err := os.ReadFile(filepath.Join(rootPath, "identity"))
	require.NoError(t, err)
	require.Equal(t, "winner", string(got))
	_, err = os.Stat(filepath.Join(rootPath, "second.tmp"))
	require.NoError(t, err, "failed no-replace install must leave its source available to the caller")
}

func TestOpenAppendRegularRepairNarrowsOwnedLegacyFile(t *testing.T) {
	rootPath := filepath.Join(t.TempDir(), "private")
	root, err := OpenRoot(rootPath, DirPolicy{Access: AccessPrivate, AllowExisting: true})
	require.NoError(t, err)

	legacyPath := filepath.Join(rootPath, "legacy.lock")
	require.NoError(t, os.WriteFile(legacyPath, []byte("legacy"), 0o644))
	_, strictErr := root.OpenAppendRegular("legacy.lock")
	require.Error(t, strictErr, "strict open must reject a legacy descriptor")

	f, err := root.OpenAppendRegularRepair("legacy.lock")
	require.NoError(t, err)
	require.NoError(t, f.Close())
	verified, err := root.OpenReadRegular("legacy.lock")
	require.NoError(t, err)
	require.NoError(t, verified.Close())
	require.NoError(t, root.Close())

	if errors.Is(strictErr, fs.ErrNotExist) {
		t.Fatal("legacy lock unexpectedly disappeared")
	}
}

func TestOpenReadWriteRegularRepairSupportsVerifiedTruncate(t *testing.T) {
	rootPath := filepath.Join(t.TempDir(), "private")
	root, err := OpenRoot(rootPath, DirPolicy{Access: AccessPrivate, AllowExisting: true})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, root.Close()) })

	legacyPath := filepath.Join(rootPath, "legacy-audit.jsonl")
	require.NoError(t, os.WriteFile(legacyPath, []byte("oversized audit log"), 0o644))

	f, err := root.OpenReadWriteRegularRepair("legacy-audit.jsonl")
	require.NoError(t, err)
	require.NoError(t, f.Truncate(0),
		"the non-append repair handle must carry Windows FILE_WRITE_DATA access")
	require.NoError(t, f.Sync())
	require.NoError(t, f.Close())

	verified, err := root.OpenReadRegular("legacy-audit.jsonl")
	require.NoError(t, err)
	info, err := verified.Stat()
	require.NoError(t, err)
	require.Zero(t, info.Size())
	require.NoError(t, verified.Close())

	_, err = root.OpenReadWriteRegularRepair("missing")
	require.ErrorIs(t, err, fs.ErrNotExist, "the repair path must never create its target")
	_, err = os.Lstat(filepath.Join(rootPath, "missing"))
	require.ErrorIs(t, err, fs.ErrNotExist)
}

func TestOpenReadWriteRegularRepairRejectsHardlink(t *testing.T) {
	rootPath := filepath.Join(t.TempDir(), "private")
	root, err := OpenRoot(rootPath, DirPolicy{Access: AccessPrivate, AllowExisting: true})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, root.Close()) })

	target := filepath.Join(rootPath, "audit.jsonl")
	require.NoError(t, os.WriteFile(target, []byte("audit"), 0o600))
	require.NoError(t, os.Link(target, filepath.Join(rootPath, "alias.jsonl")))

	f, err := root.OpenReadWriteRegularRepair("audit.jsonl")
	require.Error(t, err)
	if f != nil {
		require.NoError(t, f.Close())
	}
}
