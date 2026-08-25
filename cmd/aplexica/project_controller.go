package main

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/audit"
	"github.com/aplexica/aplexica/internal/daemon"
	"github.com/aplexica/aplexica/internal/project"
	syncd "github.com/aplexica/aplexica/internal/sync"
)

type projectControllerLogger interface {
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
}

// registerImplicitDirProject registers --dir as an implicit LOCAL project.
// This keeps --dir driving the orchestrator's primary watcher while ALSO
// marking its files project-scoped (so the registry scope-override applies)
// and surfacing it in the projects list. project.Detect derives the canonical
// ID + VCS; on the rare path-resolution error we fall back to a stable
// path-derived ID so registration is never blocked.
//
// Best-effort BY CONTRACT: any registry refusal — including the AddOrUpdate
// displacement guard when --dir is a second live clone of an already
// registered repository — logs a warning and returns. Daemon startup and the
// orchestrator's --dir watcher must never be blocked by this registration.
func registerImplicitDirProject(projectReg *project.Registry, lg projectControllerLogger, daemonDir string) {
	if daemonDir == "" {
		return
	}
	abs, aerr := filepath.Abs(daemonDir)
	if aerr != nil {
		lg.Warn("could not resolve --dir to an absolute path; skipping implicit project registration",
			"dir", daemonDir, "err", aerr)
		return
	}
	id, vcs := abs, "none"
	if info, derr := project.Detect(abs); derr == nil {
		id, vcs = info.ID, info.VCS
	}
	if uerr := projectReg.AddOrUpdate(project.Entry{
		ID:          id,
		Path:        abs,
		VCS:         vcs,
		Scope:       "local",
		DisplayName: filepath.Base(abs),
	}); uerr != nil {
		lg.Warn("registering --dir as a local project failed (orchestrator still watches it)",
			"dir", abs, "err", uerr)
		return
	}
	lg.Info("registered --dir as an implicit local project",
		"id", id, "path", abs, "vcs", vcs)
}

func cleanupRevokedProject(entry project.Entry, orch *syncd.Orchestrator, publisher *daemon.RemotePublishAdapter) error {
	if publisher != nil {
		if _, err := publisher.PurgeProject(entry.ID); err != nil {
			return fmt.Errorf("project revocation: quarantine outbox: %w", err)
		}
	}
	if orch != nil {
		if err := orch.UnwatchFolder(entry.Path); err != nil {
			return fmt.Errorf("project revocation: stop watcher: %w", err)
		}
	}
	return nil
}

func revokeProject(reg *project.Registry, orch *syncd.Orchestrator, publisher *daemon.RemotePublishAdapter, recorder audit.Recorder, id string) error {
	entry, exists := reg.Get(id)
	if !exists {
		return reg.Remove(id)
	}
	if recorder == nil {
		return fmt.Errorf("project revocation: audit recorder unavailable")
	}
	field, err := audit.SafeID("project_id", id)
	if err != nil {
		return err
	}
	txnID := acf.NewID()
	if err := recorder.BeginTransaction(context.Background(), txnID, audit.Event{Code: "project.revoked", Fields: []audit.Field{field}}); err != nil {
		return fmt.Errorf("project revocation: begin audit: %w", err)
	}
	if err := reg.Remove(id); err != nil {
		return err
	}
	if !exists {
		return nil
	}
	if err := cleanupRevokedProject(entry, orch, publisher); err != nil {
		return err
	}
	if err := recorder.CompleteTransaction(context.Background(), txnID, "success"); err != nil {
		return fmt.Errorf("project revocation: complete audit: %w", err)
	}
	return nil
}

// runProjectRegistryController catches authorized external mutations and
// atomic replacements. Control-socket mutations are synchronous; this poll is
// the missed-event fallback and fail-closed corrupt/rollback detector.
func runProjectRegistryController(ctx context.Context, reg *project.Registry, orch *syncd.Orchestrator, publisher *daemon.RemotePublishAdapter, logger projectControllerLogger) {
	known := map[string]project.Entry{}
	for _, e := range reg.List() {
		known[e.ID] = e
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if err := reg.Reload(); err != nil {
			for _, e := range known {
				_ = cleanupRevokedProject(e, orch, publisher)
			}
			if logger != nil {
				logger.Warn("project registry authorization paused", "err", err)
			}
			continue
		}
		current := map[string]project.Entry{}
		for _, e := range reg.List() {
			current[e.ID] = e
		}
		for id, prior := range known {
			next, exists := current[id]
			if !exists || next.AuthorizationGeneration != prior.AuthorizationGeneration {
				if err := cleanupRevokedProject(prior, orch, publisher); err != nil && logger != nil {
					logger.Warn("project revocation cleanup incomplete", "project_id", id, "err", err)
				}
			}
		}
		known = current
	}
}
