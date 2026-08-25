package securityepoch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/privatefs"
	"github.com/aplexica/aplexica/internal/securityerr"
)

type SecurityEpoch struct {
	CoordinatorGeneration uint64   `json:"coordinatorGeneration"`
	AccessGeneration      uint64   `json:"accessGeneration"`
	AccessSetHash         [32]byte `json:"accessSetHash"`
	BarrierID             [32]byte `json:"barrierId"`
	KeyMode               string   `json:"keyMode"`
	KeyVersion            uint64   `json:"keyVersion"`
}

// TransitionJournalFilename is the per-scope durable roll-forward marker. Any
// object at this name blocks publication and inbound admission before its
// contents are interpreted; an unsafe or corrupt journal must fail closed too.
const TransitionJournalFilename = "security-transition.journal.json"

type SecurityPublishLease interface {
	CheckCurrent() error
	Close() error
}

type scopeCoordinator struct {
	mu      sync.RWMutex
	current SecurityEpoch
	loaded  bool
	path    string
}

type lease struct {
	coordinator *scopeCoordinator
	want        SecurityEpoch
	closed      bool
}

func (l *lease) CheckCurrent() error {
	if l == nil || l.closed || l.coordinator == nil || l.coordinator.current != l.want {
		return securityerr.ErrStaleRoster
	}
	return nil
}
func (l *lease) Close() error {
	if l == nil || l.closed || l.coordinator == nil {
		return nil
	}
	l.closed = true
	l.coordinator.mu.RUnlock()
	return nil
}

type Coordinator struct {
	Root    string
	mu      sync.Mutex
	byScope map[string]*scopeCoordinator
}

func validateEpoch(epoch SecurityEpoch) error {
	modeOK := epoch.KeyMode == "recipient-wrap-v2" && epoch.KeyVersion == 0 || epoch.KeyMode == "namespace-key-v1" && epoch.KeyVersion > 0
	if epoch.CoordinatorGeneration == 0 || epoch.AccessGeneration == 0 || epoch.AccessSetHash == ([32]byte{}) || epoch.BarrierID == ([32]byte{}) || !modeOK {
		return securityerr.ErrMetadataMismatch
	}
	return nil
}

func scopeName(scopeID string) (string, error) {
	if scopeID == "" || scopeID == "account" {
		return "account", nil
	}
	if err := acf.ValidateWireUUIDv7(scopeID); err != nil {
		return "", err
	}
	return "namespace-" + scopeID, nil
}

func (c *Coordinator) scope(scopeID string) (*scopeCoordinator, error) {
	name, err := scopeName(scopeID)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.byScope == nil {
		c.byScope = map[string]*scopeCoordinator{}
	}
	if existing := c.byScope[name]; existing != nil {
		return existing, nil
	}
	path := filepath.Join(c.Root, "account", "security-coordinator.json")
	if name != "account" {
		path = filepath.Join(c.Root, "namespaces", strings.TrimPrefix(name, "namespace-"), "security-coordinator.json")
	}
	s := &scopeCoordinator{path: path}
	c.byScope[name] = s
	return s, nil
}

func loadEpoch(path string) (SecurityEpoch, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return SecurityEpoch{}, err
	}
	root, err := privatefs.OpenRoot(filepath.Dir(abs), privatefs.DirPolicy{Access: privatefs.AccessPrivate, RepairOwned: true, AllowExisting: true})
	if err != nil {
		return SecurityEpoch{}, err
	}
	defer root.Close()
	f, err := root.OpenReadRegular(filepath.Base(abs))
	if err != nil {
		return SecurityEpoch{}, err
	}
	b, err := io.ReadAll(io.LimitReader(f, 64<<10+1))
	closeErr := f.Close()
	if err != nil || closeErr != nil || len(b) > 64<<10 {
		return SecurityEpoch{}, securityerr.ErrLimitExceeded
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var epoch SecurityEpoch
	if err := dec.Decode(&epoch); err != nil {
		return SecurityEpoch{}, err
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return SecurityEpoch{}, securityerr.ErrMetadataMismatch
	}
	return epoch, validateEpoch(epoch)
}

func transitionJournalPending(epochPath string) (bool, error) {
	root, err := privatefs.OpenRoot(filepath.Dir(epochPath), privatefs.DirPolicy{Access: privatefs.AccessPrivate, RepairOwned: true, AllowExisting: true})
	if err != nil {
		return false, err
	}
	defer root.Close()
	entries, err := root.ReadDir(".")
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		if entry.Name() == TransitionJournalFilename {
			return true, nil
		}
	}
	return false, nil
}

func saveEpoch(path string, epoch SecurityEpoch) error {
	if err := validateEpoch(epoch); err != nil {
		return err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	if err := privatefs.EnsureDir(filepath.Dir(abs), privatefs.DirPolicy{Access: privatefs.AccessPrivate, RepairOwned: true, AllowExisting: true}); err != nil {
		return err
	}
	root, err := privatefs.OpenRoot(filepath.Dir(abs), privatefs.DirPolicy{Access: privatefs.AccessPrivate, RepairOwned: true})
	if err != nil {
		return err
	}
	defer root.Close()
	b, err := json.Marshal(epoch)
	if err != nil {
		return err
	}
	return root.WriteFile(filepath.Base(abs), b, privatefs.FilePolicy{RejectWritableByOthers: true})
}

func (c *Coordinator) AcquirePublish(_ context.Context, scopeID string, want SecurityEpoch) (SecurityPublishLease, error) {
	legacy := want == (SecurityEpoch{})
	if !legacy {
		if err := validateEpoch(want); err != nil {
			return nil, err
		}
	}
	s, err := c.scope(scopeID)
	if err != nil {
		return nil, err
	}
	s.mu.RLock()
	if !s.loaded {
		s.mu.RUnlock()
		s.mu.Lock()
		loadErr := error(nil)
		if !s.loaded {
			s.current, loadErr = loadEpoch(s.path)
			if loadErr == nil {
				s.loaded = true
			}
		}
		s.mu.Unlock()
		if loadErr != nil && !(legacy && errors.Is(loadErr, os.ErrNotExist)) {
			return nil, loadErr
		}
		s.mu.RLock()
	}
	pending, pendingErr := transitionJournalPending(s.path)
	if pendingErr != nil {
		s.mu.RUnlock()
		return nil, pendingErr
	}
	if pending {
		s.mu.RUnlock()
		return nil, securityerr.ErrStaleRoster
	}
	if legacy && s.loaded {
		s.mu.RUnlock()
		return nil, securityerr.ErrStaleRoster
	}
	if s.current != want {
		s.mu.RUnlock()
		return nil, securityerr.ErrStaleRoster
	}
	return &lease{coordinator: s, want: want}, nil
}

// WithAdmission linearizes durable inbound admission with access/key cutover.
// The callback must persist the complete bounded delivery before returning.
// Transition takes the same scope lock exclusively, so it cannot commit while
// an earlier delivery is between header validation and durable admission.
func (c *Coordinator) WithAdmission(scopeID string, persist func(SecurityEpoch) error) error {
	if persist == nil {
		return fmt.Errorf("securityepoch: admission persistence required")
	}
	s, err := c.scope(scopeID)
	if err != nil {
		return err
	}
	s.mu.RLock()
	pending, pendingErr := transitionJournalPending(s.path)
	if pendingErr != nil {
		s.mu.RUnlock()
		return pendingErr
	}
	if pending {
		s.mu.RUnlock()
		return securityerr.ErrStaleRoster
	}
	if !s.loaded {
		s.mu.RUnlock()
		s.mu.Lock()
		if !s.loaded {
			s.current, err = loadEpoch(s.path)
			if err == nil {
				s.loaded = true
			}
		}
		s.mu.Unlock()
		if err != nil {
			return err
		}
		s.mu.RLock()
		pending, pendingErr = transitionJournalPending(s.path)
		if pendingErr != nil {
			s.mu.RUnlock()
			return pendingErr
		}
		if pending {
			s.mu.RUnlock()
			return securityerr.ErrStaleRoster
		}
	}
	defer s.mu.RUnlock()
	return persist(s.current)
}

func (c *Coordinator) Transition(_ context.Context, scopeID string, next SecurityEpoch, commit func() error) error {
	if err := validateEpoch(next); err != nil {
		return err
	}
	s, err := c.scope(scopeID)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.loaded {
		current, loadErr := loadEpoch(s.path)
		if loadErr == nil {
			s.current, s.loaded = current, true
		} else if !errors.Is(loadErr, os.ErrNotExist) {
			return loadErr
		}
	}
	if commit == nil {
		return fmt.Errorf("securityepoch: transition commit required")
	}
	if s.loaded {
		// A process can die after any coordinator generation is durable but
		// before its outer journal is removed. Retrying the exact tuple is safe
		// only while that durable journal still exists; the callback still runs
		// so the transaction owner can exact-check/repair every other participant.
		if next == s.current {
			pending, pendingErr := transitionJournalPending(s.path)
			if pendingErr != nil {
				return pendingErr
			}
			if !pending {
				return fmt.Errorf("securityepoch: exact retry requires transition journal")
			}
			return commit()
		}
		if next.CoordinatorGeneration != s.current.CoordinatorGeneration+1 {
			return fmt.Errorf("securityepoch: generation must increment exactly once")
		}
	} else if next.CoordinatorGeneration != 1 {
		return fmt.Errorf("securityepoch: genesis generation must be one")
	}
	if err := commit(); err != nil {
		return err
	}
	if err := saveEpoch(s.path, next); err != nil {
		return err
	}
	s.current, s.loaded = next, true
	return nil
}

// VerifyCurrent re-reads the durable coordinator authority and requires an
// exact tuple match. It intentionally ignores the transition-journal gate so a
// startup recovery transaction can prove all participants before removing its
// own journal.
func (c *Coordinator) VerifyCurrent(scopeID string, want SecurityEpoch) error {
	if err := validateEpoch(want); err != nil {
		return err
	}
	s, err := c.scope(scopeID)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	persisted, err := loadEpoch(s.path)
	if err != nil {
		return err
	}
	if persisted != want || s.loaded && s.current != want {
		return securityerr.ErrStaleRoster
	}
	s.current, s.loaded = persisted, true
	return nil
}

// CurrentForRecovery reads the durable tuple even while an outer transition
// journal is present. It is for transaction recovery/renewal planning only;
// publication and admission must use the gated APIs above.
func (c *Coordinator) CurrentForRecovery(scopeID string) (SecurityEpoch, error) {
	s, err := c.scope(scopeID)
	if err != nil {
		return SecurityEpoch{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	persisted, err := loadEpoch(s.path)
	if err != nil {
		return SecurityEpoch{}, err
	}
	if s.loaded && s.current != persisted {
		return SecurityEpoch{}, securityerr.ErrStaleRoster
	}
	s.current, s.loaded = persisted, true
	return persisted, nil
}
