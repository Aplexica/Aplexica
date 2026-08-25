// Package privatefs provides retained-root filesystem operations for private
// application state. Callers supply absolute paths only when opening a root;
// all subsequent names are validated, root-relative paths.
package privatefs

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/aplexica/aplexica/internal/safepath"
)

type DirAccess uint8

const (
	AccessPrivate DirAccess = iota + 1
	AccessIntegrityOnly
)

type DirPolicy struct {
	Access        DirAccess
	RepairOwned   bool
	AllowExisting bool
}

type FilePolicy struct {
	RequirePrivateParent   bool
	RejectWritableByOthers bool
	PreserveStricter       bool
}

// ErrOpenedFileUnlinked identifies the narrow Unix race where a correctly
// owned regular file was atomically replaced after its handle was opened. A
// caller may retry the complete retained-root open, but must never accept the
// rejected, now-unlinked handle itself.
var ErrOpenedFileUnlinked = errors.New("privatefs: opened file was unlinked")

// ErrUnsafeFileIdentity identifies a file handle that is no longer a safe
// single-link regular file. Callers may retry the complete retained-root open,
// but must never accept the rejected handle itself.
var ErrUnsafeFileIdentity = errors.New("privatefs: unsafe file identity")

// ErrNodeIdentityChanged identifies a pathname replacement detected between
// two independently retained handles. The observed handles remain rejected;
// callers may retry the entire root-relative operation.
var ErrNodeIdentityChanged = errors.New("privatefs: node identity changed during permission repair")

const (
	nodeFile = "file"
	nodeDir  = "dir"
)

// atNode annotates a rejection with the node it was rejected for. Every
// permission check mints its text in exactly one place but is reached from many
// call sites, so one sentence can describe a temporary file, an installed
// secret, or a private root, and an operator reading a log cannot tell which.
// The annotation is purely additive: no check changes, and %w keeps the wrapped
// error matchable by errors.Is and errors.As.
//
// Only rejections from the permission and identity checks are annotated. Raw
// filesystem errors are returned unwrapped so that callers matching fs.ErrExist
// and fs.ErrNotExist keep reading exactly what the OS reported.
func atNode(err error, kind, path string) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w (%s %s)", err, kind, path)
}

// atRelNode annotates a rejection with the absolute path of a root-relative
// node. Callers hold a validated rel; the absolute form is what an operator
// needs in order to inspect the node with icacls or ls.
func (r *Root) atRelNode(err error, kind, rel string) error {
	if err == nil {
		return nil
	}
	if r == nil {
		return atNode(err, kind, rel)
	}
	return atNode(err, kind, filepath.Join(r.path, rel))
}

type Root struct {
	mu          sync.RWMutex
	root        *os.Root
	dir         *os.File
	path        string
	access      DirAccess
	nativeNames bool
	closed      bool

	// syncDirHook is a narrow package-test seam for proving retry semantics
	// around directory durability barriers. Production roots leave it nil.
	syncDirHook func(string) error
}

// ensureDirLocks orders every in-process access to a private node's permission
// state, keyed on the node's absolute path.
//
// Two distinct Windows defects motivate it. First, a directory published by
// MkdirAll carries its parent's inherited DACL until the repair walk reaches it,
// so a peer that observes the path inside that window takes the existing-path
// branch with its own policy and, when that policy declines repair, fails closed
// on a descriptor this process is midway through normalizing. Second — and this
// is the one that survived the first fix — hardening a directory is also a write
// to every child of it: privateACL grants through CONTAINER_INHERIT_ACE and
// OBJECT_INHERIT_ACE, and setting a container's DACL makes Windows propagate
// inheritance to existing children as a client-side, non-atomic, per-child
// read-modify-write. A propagation that reads a freshly created child before
// that child is protected, and writes it afterwards, silently drops
// SE_DACL_PROTECTED and the next validate fails closed.
//
//	Invariant P. Every read or write of node N's security descriptor happens
//	while holding a shared lock on parent(N). A propagating write — a directory
//	harden, which mutates that directory's children — additionally holds the
//	exclusive lock on the directory itself.
//
// sync.RWMutex is neither reentrant nor upgradable, and recursive read-locking
// is explicitly unsafe (a queued writer between the two RLocks deadlocks), so
// realizing P takes six rules. They are what make the call sites implementable;
// breaking one is a deadlock, not a slowdown:
//
//	N1 Non-reentrance. A goroutine never acquires a key it already holds, in
//	   either mode.
//	N2 Order. Acquire shallowest to deepest by path, release in reverse. Never
//	   acquire a key that is an ancestor of one already held.
//	N3 Subsumption. Holding exclusive(D) satisfies any same-goroutine need for
//	   shared(D). Do not also take shared.
//	N4 Windows, not functions. A shared parent lock is scoped to the exact
//	   open-then-harden/validate window and released before returning to a
//	   wrapper. Wrappers that delegate take no lock of their own on that key:
//	   CreateTemp, WriteReader, (*Root).EnsureDir, and the two append fallbacks
//	   rely on the callee's window.
//	N5 r.mu is never held across a path-lock acquisition. Every flat retained
//	   window begins after withRoot has returned. harden.go's recursive tree walk
//	   does re-enter withRoot beneath a held shared lock, which is safe for the
//	   same reason: withRoot releases r.mu before it returns, so no goroutine
//	   ever blocks on a path lock while holding r.mu. Close takes r.mu and no
//	   path locks. The only edge is path lock then r.mu, so there is no cycle.
//	N6 hardenRelative locks only for directories. See hardenRelative.
//
// The lock graph is therefore a strict partial order by path depth, and acyclic.
//
// No validation is relaxed anywhere: validatePrivateDescriptor still runs
// unchanged on every path, so a genuinely foreign inherited DACL — including one
// belonging to another process — still fails closed. The lock is process-local
// by design, mirroring secretAuditMu in internal/secrets; cross-process peers
// remain the caller's concern via DirPolicy.RepairOwned.
//
// Entries are never evicted. The key space is bounded by the number of distinct
// private nodes a process touches, and reference-counted eviction would trade
// that bounded map for a harder lifetime problem.
var (
	ensureDirLocksMu sync.Mutex
	ensureDirLocks   = map[string]*sync.RWMutex{}
)

// privateDirKey normalizes an absolute path into a lock key. Ambient and
// retained callers share one key space, so an ambient EnsureDir on a root and a
// retained operation on a child of that root contend correctly.
func privateDirKey(path string) string {
	key := filepath.Clean(path)
	if runtime.GOOS == "windows" {
		// NTFS is case-insensitive, so `Identity` and `identity` name one
		// directory and must contend on one lock. Unix paths are case-sensitive
		// and must not be folded, or two genuinely distinct roots would serialize
		// against each other.
		key = strings.ToLower(key)
	}
	return key
}

func privateDirLock(path string) *sync.RWMutex {
	key := privateDirKey(path)

	ensureDirLocksMu.Lock()
	defer ensureDirLocksMu.Unlock()
	lock, ok := ensureDirLocks[key]
	if !ok {
		lock = &sync.RWMutex{}
		ensureDirLocks[key] = lock
	}
	return lock
}

// lockEnsureDirPath acquires the exclusive per-path lock and returns its
// release. Exclusive is required for a propagating write: creating a directory
// chain, repairing a directory in place, or hardening a directory.
func lockEnsureDirPath(path string) func() {
	lock := privateDirLock(path)
	lock.Lock()
	return lock.Unlock
}

// rlockPrivateDir acquires the shared per-path lock and returns its release.
// Shared is what a caller holds on parent(N) while it reads or non-propagatingly
// writes N's descriptor. Concurrent work on siblings does not serialize; only a
// propagating write to the parent excludes it.
func rlockPrivateDir(path string) func() {
	lock := privateDirLock(path)
	lock.RLock()
	return lock.RUnlock
}

// rlockRetainedParent takes the shared lock on the retained parent of rel, which
// is the lock guarding every descriptor access to rel itself. rel must already
// be cleaned; filepath.Dir("x") is ".", which Join folds back to the root.
func (r *Root) rlockRetainedParent(rel string) func() {
	return rlockPrivateDir(filepath.Join(r.path, filepath.Dir(rel)))
}

// hardenRelative narrows a retained node's permissions through its already
// validated handle. It is the one place rule N6 lives.
//
// A directory harden propagates to that directory's children on Windows, so it
// is a write to the whole directory and takes exclusive(node). A file harden
// touches one node and takes nothing: its caller already holds shared(parent),
// which is the lock that orders it against a propagation from that parent.
// Callers of the directory form must hold shared(parent(node)) — that is N6's
// precondition, and it is satisfied by Mkdir, the (*Root).EnsureDir existing
// component helper, and hardenDir's recursion. The rel == "." form writes
// nothing (it validates the retained root in place) and takes nothing.
//
// The lock is taken on every platform rather than only on Windows. POSIX has no
// inheritance analogue and hardenRelativeNode is a plain fchmod there, so the
// exclusion changes no outcome; taking it anyway is what lets the race detector
// on macOS and Linux exercise ordering rules that otherwise only run on Windows.
func (r *Root) hardenRelative(rel string, f *os.File, dir bool) error {
	if !dir || rel == "." {
		return r.hardenRelativeNode(rel, f, dir)
	}
	defer lockEnsureDirPath(filepath.Join(r.path, rel))()
	return r.hardenRelativeNode(rel, f, dir)
}

func EnsureDir(path string, policy DirPolicy) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("privatefs: root must be absolute")
	}
	if policy.Access != AccessPrivate && policy.Access != AccessIntegrityOnly {
		return fmt.Errorf("privatefs: invalid directory policy")
	}
	// Both branches below must be atomic with respect to one another: the create
	// branch publishes the chain before it protects it, and the existing-path
	// branch would otherwise validate that intermediate state under a policy that
	// may decline repair. Repairing a component is also a propagating write to
	// its children, so every component this call may repair is held exclusively,
	// and the nearest existing ancestor is held shared because this call reads
	// and writes descriptors directly beneath it.
	//
	// The chain is discovered unlocked, for key ordering only, so that locks can
	// be taken shallowest to deepest (N2). Design A's exclusive hold on the leaf
	// is preserved exactly; only its acquisition point moves, from a
	// top-of-function defer into this ordered acquisition as the deepest key.
	prepass, chainErr := missingDirectoryChain(path)
	if chainErr != nil {
		return chainErr
	}
	defer lockEnsureDirChain(path, prepass)()
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) && policy.AllowExisting {
		// MkdirAll can create more than the requested leaf. On Windows, every
		// such directory initially inherits its parent's DACL. Inventory the
		// missing suffix first so that every component created by this operation
		// is validated and normalized, rather than leaving an inherited
		// intermediate directory around a protected leaf.
		missing, chainErr := missingDirectoryChain(path)
		if chainErr != nil {
			return chainErr
		}
		// The re-derived chain may be a subset of the pre-pass: a peer created
		// some ancestors while locks were being taken, and holding extra locks is
		// harmless. A component absent from the pre-pass means an ancestor was
		// removed underneath this call, so part of the chain about to be created
		// would be repaired unlocked. Fail closed rather than proceed; this is a
		// rejection, not a retry.
		if extra := chainOutsidePrepass(missing, prepass); extra != "" {
			return fmt.Errorf("privatefs: root ancestor changed during creation: %s", extra)
		}
		if err := os.MkdirAll(path, 0o700); err != nil {
			return fmt.Errorf("privatefs: create root: %w", err)
		}
		createdPolicy := policy
		if createdPolicy.Access == AccessPrivate {
			// Every missing component is operation-owned. Normalize it even when
			// the caller correctly declines repair of pre-existing state.
			createdPolicy.RepairOwned = true
		}
		for i := len(missing) - 1; i >= 0; i-- {
			createdInfo, statErr := os.Lstat(missing[i])
			if statErr != nil {
				return fmt.Errorf("privatefs: inspect created root component: %w", statErr)
			}
			if err := validateOrRepairDir(missing[i], createdInfo, createdPolicy); err != nil {
				return fmt.Errorf("privatefs: secure created root component: %w", atNode(err, nodeDir, missing[i]))
			}
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("privatefs: inspect root: %w", err)
	}
	return atNode(validateOrRepairDir(path, info, policy), nodeDir, path)
}

// lockEnsureDirChain acquires every lock an ambient EnsureDir needs, shallowest
// to deepest (N2), and returns their combined release in reverse order.
//
// chain is missingDirectoryChain's output: [leaf … outermost], with chain[0]
// equal to filepath.Clean(path), or empty when path already exists. The nearest
// existing ancestor A is taken shared, because this call reads and writes
// descriptors immediately beneath it; every missing component is taken
// exclusive, because repairing it propagates to its own children.
//
// No key is acquired twice (N1): chain[0] is the leaf and appears once, and A
// exists on disk so it is not in chain. Interior components are the parents of
// the next component down, but they are already held exclusively, which subsumes
// the shared requirement (N3) — do not also take shared. When path is a volume
// root its own parent, the shared acquisition is skipped for the same reason.
func lockEnsureDirChain(path string, chain []string) func() {
	var releases []func()
	release := func() {
		for i := len(releases) - 1; i >= 0; i-- {
			releases[i]()
		}
	}

	ancestor := filepath.Dir(filepath.Clean(path))
	if len(chain) > 0 {
		ancestor = filepath.Dir(chain[len(chain)-1])
	}
	if ancestor != filepath.Clean(path) {
		releases = append(releases, rlockPrivateDir(ancestor))
	}

	for i := len(chain) - 1; i >= 0; i-- {
		releases = append(releases, lockEnsureDirPath(chain[i]))
	}
	if len(chain) == 0 {
		releases = append(releases, lockEnsureDirPath(path))
	}
	return release
}

// chainOutsidePrepass reports the first component of chain that the pre-pass did
// not cover, or "" when chain is a subset of it.
func chainOutsidePrepass(chain, prepass []string) string {
	covered := make(map[string]struct{}, len(prepass))
	for _, p := range prepass {
		covered[privateDirKey(p)] = struct{}{}
	}
	for _, c := range chain {
		if _, ok := covered[privateDirKey(c)]; !ok {
			return c
		}
	}
	return ""
}

// missingDirectoryChain returns missing path components from the requested
// leaf toward the nearest existing ancestor. The ancestor is inspected without
// following links before MkdirAll is permitted to operate beneath it.
func missingDirectoryChain(path string) ([]string, error) {
	var missing []string
	current := filepath.Clean(path)
	for {
		info, err := os.Lstat(current)
		if err == nil {
			if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
				return nil, fmt.Errorf("privatefs: root ancestor is not a real directory")
			}
			return missing, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("privatefs: inspect root ancestor: %w", err)
		}
		missing = append(missing, current)
		parent := filepath.Dir(current)
		if parent == current {
			return nil, fmt.Errorf("privatefs: no existing root ancestor")
		}
		current = parent
	}
}

func ValidateDir(path string) error {
	// A pure descriptor read of path, so shared on its parent (Invariant P). It
	// takes no other lock and cannot nest.
	defer rlockPrivateDir(filepath.Dir(filepath.Clean(path)))()
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("privatefs: inspect root: %w", err)
	}
	return atNode(validateOrRepairDir(path, info, DirPolicy{Access: AccessPrivate}), nodeDir, path)
}

func OpenRoot(path string, policy DirPolicy) (*Root, error) {
	return openRoot(path, policy, false)
}

// OpenNativeRoot retains a root whose relative paths mirror agent-owned native
// filenames. It differs from OpenRoot only in component validation: names that
// are safe on the current host (notably ':' in historical macOS Codex rollout
// files) are accepted. Containment, no-follow, ownership, hardlink, and object
// type checks remain identical. Canonical store and control-state callers must
// continue to use OpenRoot's platform-independent identifier policy.
func OpenNativeRoot(path string, policy DirPolicy) (*Root, error) {
	return openRoot(path, policy, true)
}

func openRoot(path string, policy DirPolicy, nativeNames bool) (*Root, error) {
	if err := EnsureDir(path, policy); err != nil {
		return nil, err
	}
	// Acquired only after EnsureDir above has returned and released its own
	// locks: sequential, never nested (N1). The retained root's descriptor is
	// read twice below, so shared on its parent for that window.
	defer rlockPrivateDir(filepath.Dir(filepath.Clean(path)))()
	r, err := os.OpenRoot(path)
	if err != nil {
		return nil, fmt.Errorf("privatefs: open root: %w", err)
	}
	// Recheck after retaining the root. os.Root keeps the opened directory
	// identity stable across renames on supported desktop/server platforms.
	if err := ValidateRootHandle(r, policy); err != nil {
		r.Close()
		return nil, atNode(err, nodeDir, path)
	}
	dir, err := openRetainedDirectory(path)
	if err != nil {
		r.Close()
		return nil, err
	}
	if err := validateRegularDirectoryHandle(dir, policy); err != nil {
		dir.Close()
		r.Close()
		return nil, atNode(err, nodeDir, path)
	}
	return &Root{root: r, dir: dir, path: path, access: policy.Access, nativeNames: nativeNames}, nil
}

func (r *Root) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true
	err := r.root.Close()
	if r.dir != nil {
		if e := r.dir.Close(); err == nil {
			err = e
		}
	}
	return err
}

// StatRoot reports the identity of the retained root directory itself. It is
// intentionally descriptor-relative: callers that inspected an ambient path
// before OpenRoot can compare that observation with the directory Aplexica
// actually retained, without reopening the path and reintroducing a race.
func (r *Root) StatRoot() (os.FileInfo, error) {
	or, err := r.withRoot()
	if err != nil {
		return nil, err
	}
	return or.Stat(".")
}

func (r *Root) withRoot() (*os.Root, error) {
	if r == nil {
		return nil, fmt.Errorf("privatefs: nil root")
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed || r.root == nil {
		return nil, fmt.Errorf("privatefs: root is closed")
	}
	return r.root, nil
}

func cleanRel(rel string, allowDot bool) (string, error) {
	return cleanRelWithValidator(rel, allowDot, safepath.ValidateStoreComponent)
}

func cleanRelNative(rel string, allowDot bool) (string, error) {
	return cleanRelWithValidator(rel, allowDot, safepath.ValidateNativeComponent)
}

func cleanRelWithValidator(rel string, allowDot bool, validate func(string) error) (string, error) {
	if rel == "." && allowDot {
		return rel, nil
	}
	if rel == "" || filepath.IsAbs(rel) || filepath.Clean(rel) != rel {
		return "", fmt.Errorf("privatefs: invalid relative path")
	}
	// On Unix a backslash is an ordinary filename byte. Splitting it as though
	// it were a separator would silently alias `dir\\file` to `dir/file` before
	// the component validator could reject it. Windows uses backslash as its
	// real separator and is protected by filepath.Clean plus component checks.
	if filepath.Separator != '\\' && strings.ContainsRune(rel, '\\') {
		return "", fmt.Errorf("privatefs: invalid relative path")
	}
	parts := strings.FieldsFunc(rel, func(r rune) bool { return r == '/' || r == '\\' })
	if len(parts) == 0 {
		return "", fmt.Errorf("privatefs: invalid relative path")
	}
	for _, part := range parts {
		if err := validate(part); err != nil {
			return "", fmt.Errorf("privatefs: unsafe path component: %w", err)
		}
	}
	return filepath.Join(parts...), nil
}

func (r *Root) cleanRel(rel string, allowDot bool) (string, error) {
	if r != nil && r.nativeNames {
		return cleanRelNative(rel, allowDot)
	}
	return cleanRel(rel, allowDot)
}

// rejectLinks verifies every existing component without following it. os.Root
// supplies containment if a component is concurrently replaced; this check
// additionally rejects aliases even when their target remains inside the root.
func (r *Root) rejectLinks(rel string, includeFinal bool) error {
	or, err := r.withRoot()
	if err != nil {
		return err
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	limit := len(parts)
	if !includeFinal {
		limit--
	}
	cur := ""
	for i := 0; i < limit; i++ {
		if cur == "" {
			cur = parts[i]
		} else {
			cur = filepath.Join(cur, parts[i])
		}
		info, err := or.Lstat(cur)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || (i < limit-1 && !info.IsDir()) {
			return fmt.Errorf("privatefs: link or non-directory component rejected")
		}
	}
	return nil
}

func (r *Root) OpenReadRegular(rel string) (*os.File, error) {
	return r.openReadRegular(rel, false)
}

func (r *Root) OpenReadRegularIntegrity(rel string) (*os.File, error) {
	return r.openReadRegular(rel, true)
}

// OpenReadRegularRepair opens an owned, single-link regular file and narrows
// its permissions through the retained handle before returning it. This is the
// upgrade path for canonical state created by older releases. Links, special
// files, foreign owners, and hardlinks are rejected rather than repaired.
func (r *Root) OpenReadRegularRepair(rel string) (*os.File, error) {
	rel, err := r.cleanRel(rel, false)
	if err != nil {
		return nil, err
	}
	if err := r.rejectLinks(rel, true); err != nil {
		return nil, err
	}
	or, err := r.withRoot()
	if err != nil {
		return nil, err
	}
	defer r.rlockRetainedParent(rel)()
	f, err := or.OpenFile(rel, regularReadOpenFlags(), 0)
	if err != nil {
		return nil, err
	}
	err = validateRepairHandle(f, false)
	if err == nil {
		err = r.hardenRelative(rel, f, false)
	}
	if err == nil {
		err = validateRegularFile(f, false)
	}
	if err != nil {
		_ = f.Close()
		return nil, r.atRelNode(err, nodeFile, rel)
	}
	return f, nil
}

// OpenReadWriteRegularRepair opens an existing owned, single-link regular file
// for positioned writes and truncation, then narrows its permissions through
// the retained handle before returning it. Unlike OpenAppendRegularRepair, the
// handle deliberately omits O_APPEND: on Windows an append handle does not
// carry the FILE_WRITE_DATA access required by (*os.File).Truncate. Links,
// special files, foreign owners, and hardlinks are rejected rather than
// repaired, and a missing path is never created.
func (r *Root) OpenReadWriteRegularRepair(rel string) (*os.File, error) {
	rel, err := r.cleanRel(rel, false)
	if err != nil {
		return nil, err
	}
	if err := r.rejectLinks(rel, true); err != nil {
		return nil, err
	}
	or, err := r.withRoot()
	if err != nil {
		return nil, err
	}
	defer r.rlockRetainedParent(rel)()
	f, err := or.OpenFile(rel, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	if err = validateRepairHandle(f, false); err == nil {
		err = r.hardenRelative(rel, f, false)
	}
	if err == nil {
		err = validateRegularFile(f, true)
	}
	if err != nil {
		_ = f.Close()
		return nil, r.atRelNode(err, nodeFile, rel)
	}
	return f, nil
}

func (r *Root) openReadRegular(rel string, integrityOnly bool) (*os.File, error) {
	rel, err := r.cleanRel(rel, false)
	if err != nil {
		return nil, err
	}
	if err := r.rejectLinks(rel, true); err != nil {
		return nil, err
	}
	or, err := r.withRoot()
	if err != nil {
		return nil, err
	}
	defer r.rlockRetainedParent(rel)()
	f, err := or.OpenFile(rel, regularReadOpenFlags(), 0)
	if err != nil {
		return nil, err
	}
	var validateErr error
	if integrityOnly {
		validateErr = validateIntegrityFile(f)
	} else {
		validateErr = validateRegularFile(f, false)
	}
	if validateErr != nil {
		f.Close()
		return nil, r.atRelNode(validateErr, nodeFile, rel)
	}
	return f, nil
}

func (r *Root) ReadDir(rel string) ([]fs.DirEntry, error) {
	rel, err := r.cleanRel(rel, true)
	if err != nil {
		return nil, err
	}
	if rel != "." {
		if err := r.rejectLinks(rel, true); err != nil {
			return nil, err
		}
	}
	or, err := r.withRoot()
	if err != nil {
		return nil, err
	}
	// OpenRoot applies the platform's no-follow, directory-only retained open
	// before any read handle is created. In particular, a directory replaced by
	// a FIFO between rejectLinks and this call cannot block an O_RDONLY open.
	dirRoot, err := or.OpenRoot(rel)
	if err != nil {
		return nil, err
	}
	defer dirRoot.Close()
	d, err := dirRoot.Open(".")
	if err != nil {
		return nil, err
	}
	entries, readErr := d.ReadDir(-1)
	closeErr := d.Close()
	if readErr == nil {
		readErr = closeErr
	}
	return entries, readErr
}

func (r *Root) CreateExclusive(rel string, policy FilePolicy) (*os.File, error) {
	rel, err := r.cleanRel(rel, false)
	if err != nil {
		return nil, err
	}
	if err := r.rejectLinks(rel, false); err != nil {
		return nil, err
	}
	or, err := r.withRoot()
	if err != nil {
		return nil, err
	}
	// A file is born carrying its parent's inherited DACL on Windows and is only
	// protected by the harden below. Holding the parent shared across that whole
	// window is what stops a concurrent harden of the parent from propagating
	// over this child between its creation and its validation. This is the lock
	// for every creation path: CreateTemp and WriteReader delegate here and take
	// none of their own (N4).
	defer r.rlockRetainedParent(rel)()
	// Keep newly created private files readable by the creating process. Windows
	// LockFileEx requires a handle with read/write access, so callers such as
	// OpenAppendRegular must not receive a write-only handle on first creation.
	f, err := or.OpenFile(rel, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	if err := validateRepairHandle(f, false); err == nil {
		err = r.hardenRelative(rel, f, false)
	}
	if err == nil {
		err = validateRegularFile(f, true)
	}
	if err != nil {
		f.Close()
		_ = or.Remove(rel)
		return nil, r.atRelNode(err, nodeFile, rel)
	}
	return f, nil
}

func (r *Root) CreateTemp(parentRel, pattern string) (*os.File, string, error) {
	parentRel, err := r.cleanRel(parentRel, true)
	if err != nil {
		return nil, "", err
	}
	if strings.ContainsAny(pattern, `/\\`) {
		return nil, "", fmt.Errorf("privatefs: invalid temp pattern")
	}
	if parentRel != "." {
		if err := r.rejectLinks(parentRel, true); err != nil {
			return nil, "", err
		}
	}
	for i := 0; i < 128; i++ {
		name := pattern + randomSuffix()
		rel := name
		if parentRel != "." {
			rel = filepath.Join(parentRel, name)
		}
		f, err := r.CreateExclusive(rel, FilePolicy{RequirePrivateParent: true, RejectWritableByOthers: true})
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		return f, rel, err
	}
	return nil, "", fmt.Errorf("privatefs: could not allocate temporary file")
}

func (r *Root) WriteFile(rel string, data []byte, policy FilePolicy) error {
	_, _, err := r.WriteReader(rel, bytes.NewReader(data), int64(len(data)), policy)
	return err
}

func (r *Root) WriteReader(rel string, src io.Reader, maxBytes int64, policy FilePolicy) (int64, [32]byte, error) {
	var zero [32]byte
	rel, err := r.cleanRel(rel, false)
	if err != nil {
		return 0, zero, err
	}
	parent := filepath.Dir(rel)
	targetMode := os.FileMode(0o600)
	or, rootErr := r.withRoot()
	if rootErr != nil {
		return 0, zero, rootErr
	}
	if info, e := or.Lstat(rel); e == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return 0, zero, fmt.Errorf("privatefs: unsafe replacement target")
		}
		if e = validateRepairNode(info, false); e != nil {
			return 0, zero, e
		}
		if policy.PreserveStricter {
			targetMode = info.Mode().Perm() & 0o600
			if targetMode == 0 {
				targetMode = 0o400
			}
		}
	} else if !errors.Is(e, fs.ErrNotExist) {
		return 0, zero, e
	}
	f, temp, err := r.CreateTemp(parent, ".aplexica-write-")
	if err != nil {
		return 0, zero, err
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = f.Close()
			if or, e := r.withRoot(); e == nil {
				_ = or.Remove(temp)
			}
		}
	}()
	h := sha256.New()
	limited := io.LimitReader(src, maxBytes+1)
	n, err := io.Copy(io.MultiWriter(f, h), limited)
	if err == nil && n > maxBytes {
		err = fmt.Errorf("privatefs: input exceeds limit")
	}
	if err == nil {
		err = f.Chmod(targetMode)
	}
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return n, zero, err
	}
	var digest [32]byte
	copy(digest[:], h.Sum(nil))
	if err := r.Replace(temp, rel, ""); err != nil {
		return n, zero, err
	}
	cleanup = false
	return n, digest, nil
}

func (r *Root) OpenAppendRegular(rel string) (*os.File, error) {
	rel, err := r.cleanRel(rel, false)
	if err != nil {
		return nil, err
	}
	if err := r.rejectLinks(rel, true); errors.Is(err, fs.ErrNotExist) {
		f, createErr := r.CreateExclusive(rel, FilePolicy{RequirePrivateParent: true, RejectWritableByOthers: true})
		if createErr == nil || !errors.Is(createErr, fs.ErrExist) {
			return f, createErr
		}
		// Another process won first creation. Revalidate and open its exact
		// object instead of turning an ordinary lock-creation race into a
		// deployment failure.
	} else if err != nil {
		return nil, err
	}
	if err := r.rejectLinks(rel, true); err != nil {
		return nil, err
	}
	or, err := r.withRoot()
	if err != nil {
		return nil, err
	}
	// Scoped to this window only. The ErrNotExist fallback above delegated to
	// CreateExclusive, which took and released its own lock on this same key
	// before control reached here, so there is no nesting (N4).
	defer r.rlockRetainedParent(rel)()
	// Windows LockFileEx rejects append-only handles. O_RDWR preserves append
	// semantics while allowing the caller to take an exclusive file lock.
	f, err := or.OpenFile(rel, os.O_RDWR|os.O_APPEND, 0)
	if err != nil {
		return nil, err
	}
	if err := validateRegularFile(f, true); err != nil {
		f.Close()
		return nil, r.atRelNode(err, nodeFile, rel)
	}
	return f, nil
}

// OpenAppendRegularRepair is the upgrade path for an owned legacy lock file
// whose Unix permissions or Windows DACL predate the privatefs invariants. It
// rejects links, hardlinks, special files, and foreign owners, then narrows the
// descriptor through the retained root before returning a read/write handle.
func (r *Root) OpenAppendRegularRepair(rel string) (*os.File, error) {
	rel, err := r.cleanRel(rel, false)
	if err != nil {
		return nil, err
	}
	if err := r.rejectLinks(rel, true); errors.Is(err, fs.ErrNotExist) {
		f, createErr := r.CreateExclusive(rel, FilePolicy{RequirePrivateParent: true, RejectWritableByOthers: true})
		if createErr == nil || !errors.Is(createErr, fs.ErrExist) {
			return f, createErr
		}
		// A concurrent creator won. Fall through to the retained-root repair
		// path so both contenders coordinate on the same lock file.
	} else if err != nil {
		return nil, err
	}
	if err := r.rejectLinks(rel, true); err != nil {
		return nil, err
	}
	or, err := r.withRoot()
	if err != nil {
		return nil, err
	}
	// Scoped to this window only; the fallback's CreateExclusive already released
	// its own lock on this key (N4).
	defer r.rlockRetainedParent(rel)()
	f, err := or.OpenFile(rel, os.O_RDWR|os.O_APPEND, 0)
	if err != nil {
		return nil, err
	}
	if err = validateRepairHandle(f, false); err == nil {
		err = r.hardenRelative(rel, f, false)
	}
	if err == nil {
		err = validateRegularFile(f, true)
	}
	if err != nil {
		_ = f.Close()
		return nil, r.atRelNode(err, nodeFile, rel)
	}
	return f, nil
}

func (r *Root) Mkdir(rel string, policy DirPolicy) error {
	rel, err := r.cleanRel(rel, false)
	if err != nil {
		return err
	}
	if err := r.rejectLinks(rel, false); err != nil {
		return err
	}
	or, err := r.withRoot()
	if err != nil {
		return err
	}
	// A directory is born inheriting its parent's DACL on Windows exactly as a
	// file is, and hardenRelative below additionally takes exclusive(rel) for the
	// propagating write to rel's own children.
	defer r.rlockRetainedParent(rel)()
	if err := or.Mkdir(rel, 0o700); err != nil {
		return err
	}
	d, err := or.Open(rel)
	if err == nil {
		err = validateRepairHandle(d, true)
	}
	if err == nil {
		err = r.hardenRelative(rel, d, true)
	}
	if err == nil {
		err = validateRegularDirectoryHandle(d, policy)
	}
	if d != nil {
		if closeErr := d.Close(); err == nil {
			err = closeErr
		}
	}
	if err != nil {
		_ = or.Remove(rel)
	}
	return r.atRelNode(err, nodeDir, rel)
}

// ensureExistingComponent narrows and validates one already-existing component
// of a retained directory chain.
//
// Existing private children are already beneath a retained, validated private
// root. Narrow an owned child unconditionally: this closes the Windows creation
// race where another goroutine can observe an inherited DACL between Mkdir and
// hardening, and safely upgrades owned legacy children without following links.
//
// This is deliberately a function rather than a block inside (*Root).EnsureDir's
// loop. Its locks must be released at the end of each iteration, and a defer
// inside a for body would instead hold every iteration's locks until the whole
// walk returned — acquiring a descendant's keys while still holding an
// ancestor's, which is the ordering N2 forbids.
func (r *Root) ensureExistingComponent(cur string, policy DirPolicy) error {
	or, err := r.withRoot()
	if err != nil {
		return err
	}
	defer r.rlockRetainedParent(cur)()
	d, err := or.Open(cur)
	if err != nil {
		return err
	}
	var e error
	if policy.Access == AccessPrivate {
		e = validateRepairHandle(d, true)
		if e == nil {
			e = r.hardenRelative(cur, d, true)
		}
	}
	if e == nil {
		e = validateRegularDirectoryHandle(d, policy)
	}
	if closeErr := d.Close(); e == nil {
		e = closeErr
	}
	return r.atRelNode(e, nodeDir, cur)
}

func (r *Root) EnsureDir(rel string, policy DirPolicy) error {
	rel, err := r.cleanRel(rel, true)
	if err != nil {
		return err
	}
	if rel == "." {
		return nil
	}
	cur := ""
	for _, p := range strings.Split(filepath.ToSlash(rel), "/") {
		if cur == "" {
			cur = p
		} else {
			cur = filepath.Join(cur, p)
		}
		err := r.Mkdir(cur, policy)
		if errors.Is(err, fs.ErrExist) {
			if e := r.ensureExistingComponent(cur, policy); e != nil {
				return e
			}
		} else if err != nil {
			return err
		}
		// Mkdir made cur reachable through its parent. The new directory's own
		// handle validation/hardening does not make that parent entry durable on
		// Unix, so sync the retained parent before creating a deeper component or
		// reporting success. Repeat the barrier for an existing component too: a
		// previous attempt may have created it and then failed this exact sync.
		// syncDirectoryHandle performs the strongest portable retained-handle
		// validation available on Windows.
		if err := r.SyncDir(filepath.Dir(cur)); err != nil {
			return err
		}
	}
	return nil
}

func (r *Root) RemoveEmptyDir(rel string) error { return r.removeTyped(rel, true) }
func (r *Root) RemoveRegular(rel string) error  { return r.removeTyped(rel, false) }

func (r *Root) removeTyped(rel string, dir bool) error {
	rel, err := r.cleanRel(rel, false)
	if err != nil {
		return err
	}
	if err := r.rejectLinks(rel, true); err != nil {
		return err
	}
	or, err := r.withRoot()
	if err != nil {
		return err
	}
	info, err := or.Lstat(rel)
	if err != nil {
		return err
	}
	if dir != info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("privatefs: unexpected object type")
	}
	if !dir && !info.Mode().IsRegular() {
		return fmt.Errorf("privatefs: non-regular file")
	}
	if err := or.Remove(rel); err != nil {
		return err
	}
	return r.SyncDir(filepath.Dir(rel))
}

func (r *Root) Rename(oldRel, newRel string) error {
	oldRel, err := r.cleanRel(oldRel, false)
	if err != nil {
		return err
	}
	newRel, err = r.cleanRel(newRel, false)
	if err != nil {
		return err
	}
	if err := r.rejectLinks(oldRel, true); err != nil {
		return err
	}
	if err := r.rejectLinks(newRel, false); err != nil {
		return err
	}
	or, err := r.withRoot()
	if err != nil {
		return err
	}
	if err := or.Rename(oldRel, newRel); err != nil {
		return err
	}
	return r.SyncDir(filepath.Dir(newRel))
}

func (r *Root) Replace(tempRel, finalRel, backupRel string) error {
	tempRel, err := r.cleanRel(tempRel, false)
	if err != nil {
		return err
	}
	finalRel, err = r.cleanRel(finalRel, false)
	if err != nil {
		return err
	}
	if backupRel != "" {
		backupRel, err = r.cleanRel(backupRel, false)
		if err != nil {
			return err
		}
		or, e := r.withRoot()
		if e != nil {
			return e
		}
		if _, e = or.Lstat(finalRel); e == nil {
			if e = r.InstallNoReplace(finalRel, backupRel); e != nil {
				return e
			}
		} else if !errors.Is(e, fs.ErrNotExist) {
			return e
		}
	}
	if err := r.Rename(tempRel, finalRel); err != nil {
		if backupRel != "" {
			_ = r.Rename(backupRel, finalRel)
		}
		return err
	}
	return nil
}

func (r *Root) InstallNoReplace(tempRel, finalRel string) error {
	tempRel, err := r.cleanRel(tempRel, false)
	if err != nil {
		return err
	}
	finalRel, err = r.cleanRel(finalRel, false)
	if err != nil {
		return err
	}
	if err := r.rejectLinks(tempRel, true); err != nil {
		return err
	}
	if err := r.rejectLinks(finalRel, false); err != nil {
		return err
	}
	if err := r.installNoReplace(tempRel, finalRel); err != nil {
		return err
	}
	return r.SyncDir(filepath.Dir(finalRel))
}

func (r *Root) SyncDir(rel string) error {
	rel, err := r.cleanRel(rel, true)
	if err != nil {
		return err
	}
	if r.syncDirHook != nil {
		if err := r.syncDirHook(rel); err != nil {
			return err
		}
	}
	if rel != "." {
		if err := r.rejectLinks(rel, true); err != nil {
			return err
		}
	}
	or, err := r.withRoot()
	if err != nil {
		return err
	}
	f, err := or.Open(rel)
	if err != nil {
		return err
	}
	defer f.Close()
	return syncDirectoryHandle(f)
}
