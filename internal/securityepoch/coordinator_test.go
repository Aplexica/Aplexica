package securityepoch

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/aplexica/aplexica/internal/securityerr"
)

func epoch(generation uint64, label string) SecurityEpoch {
	return SecurityEpoch{
		CoordinatorGeneration: generation,
		AccessGeneration:      generation,
		AccessSetHash:         sha256.Sum256([]byte("access-" + label)),
		BarrierID:             sha256.Sum256([]byte("barrier-" + label)),
		KeyMode:               "recipient-wrap-v2",
	}
}

func TestTransitionBlocksPublishAndPersistsExactEpoch(t *testing.T) {
	c := &Coordinator{Root: filepath.Join(t.TempDir(), "security")}
	one := epoch(1, "one")
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- c.Transition(context.Background(), "account", one, func() error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered
	acquired := make(chan error, 1)
	go func() {
		lease, err := c.AcquirePublish(context.Background(), "account", one)
		if err == nil {
			err = lease.Close()
		}
		acquired <- err
	}()
	select {
	case <-acquired:
		t.Fatal("publish lease passed an in-progress transition")
	default:
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := <-acquired; err != nil {
		t.Fatal(err)
	}

	reopened := &Coordinator{Root: c.Root}
	lease, err := reopened.AcquirePublish(context.Background(), "account", one)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	if _, err := reopened.AcquirePublish(context.Background(), "account", epoch(2, "two")); !errors.Is(err, securityerr.ErrStaleRoster) {
		t.Fatalf("stale/future publish error = %v", err)
	}
}

func TestFailedTransitionDoesNotAdvanceEpoch(t *testing.T) {
	c := &Coordinator{Root: filepath.Join(t.TempDir(), "security")}
	one := epoch(1, "one")
	if err := c.Transition(context.Background(), "account", one, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	want := errors.New("plugin prepare failed")
	if err := c.Transition(context.Background(), "account", epoch(2, "two"), func() error { return want }); !errors.Is(err, want) {
		t.Fatalf("transition error = %v", err)
	}
	lease, err := c.AcquirePublish(context.Background(), "account", one)
	if err != nil {
		t.Fatal(err)
	}
	_ = lease.Close()
}

func TestLegacyPublishLeaseExistsOnlyBeforeFirstSecurityEpoch(t *testing.T) {
	c := &Coordinator{Root: filepath.Join(t.TempDir(), "security")}
	legacy := SecurityEpoch{}
	lease, err := c.AcquirePublish(context.Background(), "account", legacy)
	if err != nil {
		t.Fatalf("pre-cutover legacy lease: %v", err)
	}
	if err := lease.CheckCurrent(); err != nil {
		t.Fatalf("pre-cutover legacy lease check: %v", err)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}

	if err := c.Transition(context.Background(), "account", epoch(1, "one"), func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := c.AcquirePublish(context.Background(), "account", legacy); !errors.Is(err, securityerr.ErrStaleRoster) {
		t.Fatalf("legacy publish after cutover error = %v", err)
	}

	reopened := &Coordinator{Root: c.Root}
	if _, err := reopened.AcquirePublish(context.Background(), "account", legacy); !errors.Is(err, securityerr.ErrStaleRoster) {
		t.Fatalf("legacy publish after restart error = %v", err)
	}
}

func TestGenesisTransitionRetryIsIdempotentOnlyForExactGenerationOne(t *testing.T) {
	root := filepath.Join(t.TempDir(), "security")
	c := &Coordinator{Root: root}
	one := epoch(1, "one")
	commits := 0
	commit := func() error {
		commits++
		return nil
	}
	if err := c.Transition(context.Background(), "account", one, commit); err != nil {
		t.Fatal(err)
	}
	journal := filepath.Join(root, "account", TransitionJournalFilename)
	if err := os.WriteFile(journal, []byte("recovery pending"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := c.Transition(context.Background(), "account", one, commit); err != nil {
		t.Fatalf("exact genesis retry: %v", err)
	}
	if commits != 2 {
		t.Fatalf("exact retry commits=%d, want 2", commits)
	}
	conflict := one
	conflict.BarrierID = sha256.Sum256([]byte("conflicting barrier"))
	if err := c.Transition(context.Background(), "account", conflict, commit); err == nil {
		t.Fatal("conflicting generation-one retry succeeded")
	}
	if commits != 2 {
		t.Fatalf("conflicting retry invoked commit: %d", commits)
	}
	if err := os.Remove(journal); err != nil {
		t.Fatal(err)
	}
	if err := c.Transition(context.Background(), "account", one, commit); err == nil {
		t.Fatal("generation-one retry without a transition journal succeeded")
	}
	if commits != 2 {
		t.Fatalf("journal-free retry invoked commit: %d", commits)
	}

	two := epoch(2, "two")
	if err := c.Transition(context.Background(), "account", two, commit); err != nil {
		t.Fatal(err)
	}
	if err := c.Transition(context.Background(), "account", two, commit); err == nil {
		t.Fatal("non-genesis same-generation retry succeeded")
	}
	if commits != 3 {
		t.Fatalf("same generation two invoked commit: %d", commits)
	}

	reopened := &Coordinator{Root: c.Root}
	if err := reopened.VerifyCurrent("account", two); err != nil {
		t.Fatalf("verify durable current: %v", err)
	}
	if err := reopened.VerifyCurrent("account", one); !errors.Is(err, securityerr.ErrStaleRoster) {
		t.Fatalf("verify stale current error = %v", err)
	}
}

func TestTransitionJournalBlocksLegacyCurrentPublishAndInboundAdmission(t *testing.T) {
	root := filepath.Join(t.TempDir(), "security")
	account := filepath.Join(root, "account")
	if err := os.MkdirAll(account, 0o700); err != nil {
		t.Fatal(err)
	}
	journal := filepath.Join(account, TransitionJournalFilename)
	if err := os.WriteFile(journal, []byte("incomplete but blocking"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := &Coordinator{Root: root}
	if _, err := c.AcquirePublish(context.Background(), "account", SecurityEpoch{}); !errors.Is(err, securityerr.ErrStaleRoster) {
		t.Fatalf("legacy publish with journal error = %v", err)
	}
	called := false
	if err := c.WithAdmission("account", func(SecurityEpoch) error { called = true; return nil }); !errors.Is(err, securityerr.ErrStaleRoster) {
		t.Fatalf("inbound admission with genesis journal error = %v", err)
	}
	if called {
		t.Fatal("inbound callback ran with genesis journal")
	}
	if err := os.Remove(journal); err != nil {
		t.Fatal(err)
	}
	legacy, err := c.AcquirePublish(context.Background(), "account", SecurityEpoch{})
	if err != nil {
		t.Fatalf("legacy publish after journal removal: %v", err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	one := epoch(1, "one")
	if err := c.Transition(context.Background(), "account", one, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(journal, []byte("next transition"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := c.AcquirePublish(context.Background(), "account", one); !errors.Is(err, securityerr.ErrStaleRoster) {
		t.Fatalf("current publish with journal error = %v", err)
	}
	called = false
	if err := c.WithAdmission("account", func(SecurityEpoch) error { called = true; return nil }); !errors.Is(err, securityerr.ErrStaleRoster) {
		t.Fatalf("current admission with journal error = %v", err)
	}
	if called {
		t.Fatal("current inbound callback ran with transition journal")
	}
}
