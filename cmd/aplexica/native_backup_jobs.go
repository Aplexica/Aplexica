package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/aplexica/aplexica/internal/nativebackup"
)

const nativeBackupJobHistoryLimit = 20

type nativeBackupJobManager struct {
	mu    sync.Mutex
	jobs  map[string]*nativeBackupJobRecord
	order []string
}

type nativeBackupJobRecord struct {
	job    nativebackup.BackupJob
	cancel context.CancelFunc
}

func newNativeBackupJobManager() *nativeBackupJobManager {
	return &nativeBackupJobManager{jobs: map[string]*nativeBackupJobRecord{}}
}

func (m *nativeBackupJobManager) Start(parent context.Context, agents []string, destination string, run func(context.Context) (nativebackup.BackupInfo, error)) (nativebackup.BackupJob, error) {
	if run == nil {
		return nativebackup.BackupJob{}, errors.New("backup job runner is required")
	}
	if parent == nil {
		parent = context.Background()
	}
	if destination == "" {
		destination = "local"
	}
	now := time.Now().UTC()
	ctx, cancel := context.WithCancel(parent)
	job := nativebackup.BackupJob{
		ID:          fmt.Sprintf("manual-backup-%s", now.Format("2006-01-02T15-04-05.000000000Z")),
		Kind:        "manual",
		State:       nativebackup.BackupJobStateRunning,
		Destination: destination,
		Agents:      cloneStrings(agents),
		CreatedAt:   now,
		StartedAt:   now,
	}

	m.mu.Lock()
	if active := m.activeLocked(); active != nil {
		cancel()
		job := cloneBackupJob(active.job)
		m.mu.Unlock()
		return job, nil
	}
	for {
		if _, exists := m.jobs[job.ID]; !exists {
			break
		}
		job.ID = fmt.Sprintf("manual-backup-%s-%d", now.Format("2006-01-02T15-04-05.000000000Z"), len(m.jobs)+1)
	}
	m.jobs[job.ID] = &nativeBackupJobRecord{job: job, cancel: cancel}
	m.order = append(m.order, job.ID)
	m.pruneLocked()
	m.mu.Unlock()

	go m.run(job.ID, ctx, run)
	return cloneBackupJob(job), nil
}

func (m *nativeBackupJobManager) Cancel(id string) (nativebackup.BackupJob, error) {
	if id == "" {
		return nativebackup.BackupJob{}, errors.New("jobId is required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, ok := m.jobs[id]
	if !ok {
		return nativebackup.BackupJob{}, fmt.Errorf("backup job %q not found", id)
	}
	if !isActiveBackupJob(rec.job.State) {
		return cloneBackupJob(rec.job), nil
	}
	rec.job.State = nativebackup.BackupJobStateCanceling
	if rec.cancel != nil {
		rec.cancel()
	}
	return cloneBackupJob(rec.job), nil
}

func (m *nativeBackupJobManager) List() []nativebackup.BackupJob {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.order) == 0 {
		return []nativebackup.BackupJob{}
	}
	out := make([]nativebackup.BackupJob, 0, len(m.order))
	for i := len(m.order) - 1; i >= 0; i-- {
		rec := m.jobs[m.order[i]]
		if rec == nil {
			continue
		}
		out = append(out, cloneBackupJob(rec.job))
	}
	return out
}

func (m *nativeBackupJobManager) run(id string, ctx context.Context, run func(context.Context) (nativebackup.BackupInfo, error)) {
	info, err := run(ctx)
	now := time.Now().UTC()

	m.mu.Lock()
	defer m.mu.Unlock()
	rec, ok := m.jobs[id]
	if !ok {
		return
	}
	rec.cancel = nil
	rec.job.CompletedAt = now
	switch {
	case err == nil:
		rec.job.State = nativebackup.BackupJobStateSucceeded
		infoCopy := info
		rec.job.Backup = &infoCopy
	case errors.Is(ctx.Err(), context.Canceled) || errors.Is(err, context.Canceled):
		rec.job.State = nativebackup.BackupJobStateCanceled
		rec.job.Error = ""
	default:
		rec.job.State = nativebackup.BackupJobStateFailed
		rec.job.Error = err.Error()
	}
	m.pruneLocked()
}

func (m *nativeBackupJobManager) activeLocked() *nativeBackupJobRecord {
	for _, rec := range m.jobs {
		if rec != nil && isActiveBackupJob(rec.job.State) {
			return rec
		}
	}
	return nil
}

func (m *nativeBackupJobManager) pruneLocked() {
	if len(m.order) <= nativeBackupJobHistoryLimit {
		return
	}
	keep := make(map[string]struct{}, nativeBackupJobHistoryLimit)
	for i := len(m.order) - 1; i >= 0 && len(keep) < nativeBackupJobHistoryLimit; i-- {
		id := m.order[i]
		if rec := m.jobs[id]; rec != nil {
			keep[id] = struct{}{}
		}
	}
	for id, rec := range m.jobs {
		if rec != nil && isActiveBackupJob(rec.job.State) {
			keep[id] = struct{}{}
		}
	}
	nextOrder := m.order[:0]
	for _, id := range m.order {
		if _, ok := keep[id]; ok {
			nextOrder = append(nextOrder, id)
		} else {
			delete(m.jobs, id)
		}
	}
	m.order = nextOrder
}

func isActiveBackupJob(state string) bool {
	return state == nativebackup.BackupJobStateRunning || state == nativebackup.BackupJobStateCanceling
}

func cloneBackupJob(job nativebackup.BackupJob) nativebackup.BackupJob {
	job.Agents = cloneStrings(job.Agents)
	if job.Backup != nil {
		info := *job.Backup
		info.Agents = cloneStrings(info.Agents)
		job.Backup = &info
	}
	return job
}

func cloneStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := append([]string{}, in...)
	sort.Strings(out)
	return out
}
