package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aplexica/aplexica/internal/nativebackup"
)

type fakeNativeBackupsAccessor struct {
	infos        []nativebackup.BackupInfo
	listErr      error
	startFn      func(agents []string, destination string) (nativebackup.BackupJob, error)
	cancelFn     func(jobID string) (nativebackup.BackupJob, error)
	statusFn     func() (nativebackup.Status, error)
	overrideFn   func(agent string) (nativebackup.SafetyStatus, error)
	scheduleFn   func(cfg nativebackup.ScheduleConfig) (nativebackup.ScheduleConfig, error)
	retentionFn  func(cfg nativebackup.RetentionConfig) (nativebackup.RetentionConfig, error)
	restoreFn    func(backupID, agent, location string) (nativebackup.RestoreResult, error)
	deleteFn     func(backupID, location string) (nativebackup.BackupInfo, error)
	gotID        string
	gotAgent     string
	gotAgents    []string
	gotDest      string
	gotJobID     string
	gotLocation  string
	gotSchedule  nativebackup.ScheduleConfig
	gotRetention nativebackup.RetentionConfig
}

func (f *fakeNativeBackupsAccessor) List() ([]nativebackup.BackupInfo, error) {
	return f.infos, f.listErr
}

func (f *fakeNativeBackupsAccessor) StartCreate(agents []string, destination string) (nativebackup.BackupJob, error) {
	f.gotAgents = agents
	f.gotDest = destination
	if f.startFn != nil {
		return f.startFn(agents, destination)
	}
	return nativebackup.BackupJob{
		ID:          "job-1",
		Kind:        "manual",
		State:       nativebackup.BackupJobStateRunning,
		Destination: destination,
		Agents:      agents,
	}, nil
}

func (f *fakeNativeBackupsAccessor) CancelJob(jobID string) (nativebackup.BackupJob, error) {
	f.gotJobID = jobID
	if f.cancelFn != nil {
		return f.cancelFn(jobID)
	}
	return nativebackup.BackupJob{
		ID:          jobID,
		Kind:        "manual",
		State:       nativebackup.BackupJobStateCanceling,
		Destination: "cloud",
	}, nil
}

func (f *fakeNativeBackupsAccessor) Status() (nativebackup.Status, error) {
	if f.statusFn != nil {
		return f.statusFn()
	}
	return nativebackup.Status{Safety: []nativebackup.SafetyStatus{{Agent: "kilo", State: "protected"}}}, nil
}

func (f *fakeNativeBackupsAccessor) Override(agent string) (nativebackup.SafetyStatus, error) {
	f.gotAgent = agent
	if f.overrideFn != nil {
		return f.overrideFn(agent)
	}
	return nativebackup.SafetyStatus{Agent: agent, State: "overridden", Override: true}, nil
}

func (f *fakeNativeBackupsAccessor) SetSchedule(cfg nativebackup.ScheduleConfig) (nativebackup.ScheduleConfig, error) {
	f.gotSchedule = cfg
	if f.scheduleFn != nil {
		return f.scheduleFn(cfg)
	}
	return cfg, nil
}

func (f *fakeNativeBackupsAccessor) SetRetention(cfg nativebackup.RetentionConfig) (nativebackup.RetentionConfig, error) {
	f.gotRetention = cfg
	if f.retentionFn != nil {
		return f.retentionFn(cfg)
	}
	return cfg, nil
}

func (f *fakeNativeBackupsAccessor) Restore(_ context.Context, backupID, agent, location string) (nativebackup.RestoreResult, error) {
	f.gotID = backupID
	f.gotAgent = agent
	f.gotLocation = location
	if f.restoreFn != nil {
		return f.restoreFn(backupID, agent, location)
	}
	return nativebackup.RestoreResult{PreRestoreDir: "/tmp/pre-restore-x"}, nil
}

func (f *fakeNativeBackupsAccessor) Delete(_ context.Context, backupID, location string) (nativebackup.BackupInfo, error) {
	f.gotID = backupID
	f.gotLocation = location
	if f.deleteFn != nil {
		return f.deleteFn(backupID, location)
	}
	return nativebackup.BackupInfo{ID: backupID, Kind: "manual", Location: location}, nil
}

func TestNativeBackupsCreate_HappyPath(t *testing.T) {
	acc := &fakeNativeBackupsAccessor{}
	h := NewNativeBackupsHandler(acc)

	body, _ := json.Marshal(createBackupReq{Agents: []string{"kilo"}})
	req := httptest.NewRequest(http.MethodPost, "/api/native-backups", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	h.Create(rr, req)

	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rr.Code, rr.Body.String())
	}
	if len(acc.gotAgents) != 1 || acc.gotAgents[0] != "kilo" {
		t.Fatalf("agents = %+v", acc.gotAgents)
	}
	var got nativebackup.BackupJob
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Kind != "manual" || got.State != nativebackup.BackupJobStateRunning {
		t.Fatalf("job = %+v, want running manual job", got)
	}
}

func TestNativeBackupsStatus_HappyPath(t *testing.T) {
	acc := &fakeNativeBackupsAccessor{
		statusFn: func() (nativebackup.Status, error) {
			return nativebackup.Status{
				Safety: []nativebackup.SafetyStatus{{Agent: "kilo", State: "protected"}},
				Jobs:   []nativebackup.BackupJob{{ID: "job-1", State: nativebackup.BackupJobStateRunning}},
			}, nil
		},
	}
	h := NewNativeBackupsHandler(acc)

	req := httptest.NewRequest(http.MethodGet, "/api/native-backups/status", nil)
	rr := httptest.NewRecorder()
	h.Status(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var got nativebackup.Status
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Safety) != 1 || got.Safety[0].Agent != "kilo" {
		t.Fatalf("status = %+v", got)
	}
	if len(got.Jobs) != 1 || got.Jobs[0].ID != "job-1" {
		t.Fatalf("jobs = %+v", got.Jobs)
	}
}

func TestNativeBackupsCancelJob_HappyPath(t *testing.T) {
	acc := &fakeNativeBackupsAccessor{}
	h := NewNativeBackupsHandler(acc)

	body, _ := json.Marshal(cancelBackupJobReq{JobID: "job-1"})
	req := httptest.NewRequest(http.MethodPost, "/api/native-backups/jobs/cancel", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	h.CancelJob(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if acc.gotJobID != "job-1" {
		t.Fatalf("jobID = %q, want job-1", acc.gotJobID)
	}
	var got nativebackup.BackupJob
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.State != nativebackup.BackupJobStateCanceling {
		t.Fatalf("state = %q, want canceling", got.State)
	}
}

func TestNativeBackupsOverride_RequiresAgent(t *testing.T) {
	acc := &fakeNativeBackupsAccessor{}
	h := NewNativeBackupsHandler(acc)

	req := httptest.NewRequest(http.MethodPost, "/api/native-backups/override", bytes.NewReader([]byte(`{}`)))
	rr := httptest.NewRecorder()
	h.Override(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}

func TestNativeBackupsSchedule_HappyPath(t *testing.T) {
	acc := &fakeNativeBackupsAccessor{}
	h := NewNativeBackupsHandler(acc)

	body, _ := json.Marshal(nativebackup.ScheduleConfig{Enabled: true, IntervalMinutes: 60, Agents: []string{"kilo"}})
	req := httptest.NewRequest(http.MethodPut, "/api/native-backups/schedule", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	h.SetSchedule(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
}

func TestNativeBackupsSchedule_IgnoresEmptyRunTimestamps(t *testing.T) {
	acc := &fakeNativeBackupsAccessor{}
	h := NewNativeBackupsHandler(acc)

	body := []byte(`{"enabled":true,"intervalMinutes":1440,"agents":["kilo"],"destination":"cloud","lastRunAt":"","nextRunAt":""}`)
	req := httptest.NewRequest(http.MethodPut, "/api/native-backups/schedule", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	h.SetSchedule(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if !acc.gotSchedule.LastRunAt.IsZero() || !acc.gotSchedule.NextRunAt.IsZero() {
		t.Fatalf("run timestamps should be ignored, got last=%s next=%s", acc.gotSchedule.LastRunAt, acc.gotSchedule.NextRunAt)
	}
	if acc.gotSchedule.Destination != "cloud" {
		t.Fatalf("destination = %q, want cloud", acc.gotSchedule.Destination)
	}
}

func TestNativeBackupsRetention_HappyPath(t *testing.T) {
	acc := &fakeNativeBackupsAccessor{}
	h := NewNativeBackupsHandler(acc)

	body, _ := json.Marshal(nativebackup.RetentionConfig{PerAgent: map[string]int{"claude-code": 2, "codex": 5}})
	req := httptest.NewRequest(http.MethodPut, "/api/native-backups/retention", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	h.SetRetention(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if acc.gotRetention.PerAgent["claude-code"] != 2 || acc.gotRetention.PerAgent["codex"] != 5 {
		t.Fatalf("retention = %+v", acc.gotRetention)
	}
}

func TestNativeBackupsList_HappyPath(t *testing.T) {
	acc := &fakeNativeBackupsAccessor{
		infos: []nativebackup.BackupInfo{
			{ID: "pre-sync-2026", Kind: "pre-sync", FileCount: 3, Agents: []string{"hermes"}},
		},
	}
	h := NewNativeBackupsHandler(acc)

	req := httptest.NewRequest(http.MethodGet, "/api/native-backups", nil)
	rr := httptest.NewRecorder()
	h.List(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rr.Code, rr.Body.String())
	}
	var got []nativebackup.BackupInfo
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got) != 1 || got[0].ID != "pre-sync-2026" {
		t.Errorf("backups = %+v", got)
	}
}

func TestNativeBackupsList_NilBecomesEmptyArray(t *testing.T) {
	acc := &fakeNativeBackupsAccessor{infos: nil}
	h := NewNativeBackupsHandler(acc)

	req := httptest.NewRequest(http.MethodGet, "/api/native-backups", nil)
	rr := httptest.NewRecorder()
	h.List(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	// Must serialize as [] not null so the SPA can iterate unconditionally.
	if got := bytes.TrimSpace(rr.Body.Bytes()); string(got) != "[]" {
		t.Errorf("body = %s, want []", got)
	}
}

func TestNativeBackupsList_Error(t *testing.T) {
	acc := &fakeNativeBackupsAccessor{listErr: errors.New("disk gone")}
	h := NewNativeBackupsHandler(acc)

	req := httptest.NewRequest(http.MethodGet, "/api/native-backups", nil)
	rr := httptest.NewRecorder()
	h.List(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rr.Code)
	}
}

func TestNativeBackupsRestore_HappyPath(t *testing.T) {
	acc := &fakeNativeBackupsAccessor{
		restoreFn: func(backupID, agent, location string) (nativebackup.RestoreResult, error) {
			return nativebackup.RestoreResult{
				PreRestoreDir: "/tmp/pre-restore-1",
				Files:         []nativebackup.FileResult{{Path: "/x", OK: true}},
			}, nil
		},
	}
	h := NewNativeBackupsHandler(acc)

	body, _ := json.Marshal(restoreReq{BackupID: "pre-sync-2026", Agent: "hermes"})
	req := httptest.NewRequest(http.MethodPost, "/api/native-backups/restore", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	h.Restore(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if acc.gotID != "pre-sync-2026" || acc.gotAgent != "hermes" || acc.gotLocation != "" {
		t.Errorf("passed id=%q agent=%q location=%q", acc.gotID, acc.gotAgent, acc.gotLocation)
	}
	var res nativebackup.RestoreResult
	if err := json.Unmarshal(rr.Body.Bytes(), &res); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if res.PreRestoreDir != "/tmp/pre-restore-1" {
		t.Errorf("preRestoreDir = %q", res.PreRestoreDir)
	}
}

func TestNativeBackupsRestore_MissingBackupID(t *testing.T) {
	acc := &fakeNativeBackupsAccessor{}
	h := NewNativeBackupsHandler(acc)

	body, _ := json.Marshal(restoreReq{Agent: "hermes"})
	req := httptest.NewRequest(http.MethodPost, "/api/native-backups/restore", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	h.Restore(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
	var be ErrorBody
	if err := json.Unmarshal(rr.Body.Bytes(), &be); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if be.Code != "validation" {
		t.Errorf("code = %q, want validation", be.Code)
	}
}

func TestNativeBackupsRestore_BadJSON(t *testing.T) {
	acc := &fakeNativeBackupsAccessor{}
	h := NewNativeBackupsHandler(acc)

	req := httptest.NewRequest(http.MethodPost, "/api/native-backups/restore", bytes.NewReader([]byte("{notjson")))
	rr := httptest.NewRecorder()
	h.Restore(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}

func TestNativeBackupsRestore_AccessorError(t *testing.T) {
	acc := &fakeNativeBackupsAccessor{
		restoreFn: func(backupID, agent, location string) (nativebackup.RestoreResult, error) {
			return nativebackup.RestoreResult{}, errors.New("no such backup")
		},
	}
	h := NewNativeBackupsHandler(acc)

	body, _ := json.Marshal(restoreReq{BackupID: "missing"})
	req := httptest.NewRequest(http.MethodPost, "/api/native-backups/restore", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	h.Restore(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}

func TestNativeBackupsDelete_HappyPath(t *testing.T) {
	acc := &fakeNativeBackupsAccessor{
		deleteFn: func(backupID, location string) (nativebackup.BackupInfo, error) {
			return nativebackup.BackupInfo{ID: backupID, Kind: "manual", Location: location, Agents: []string{"kilo"}}, nil
		},
	}
	h := NewNativeBackupsHandler(acc)

	body, _ := json.Marshal(deleteBackupReq{BackupID: "manual-kilo-2026", Location: "cloud"})
	req := httptest.NewRequest(http.MethodDelete, "/api/native-backups", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	h.Delete(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if acc.gotID != "manual-kilo-2026" || acc.gotLocation != "cloud" {
		t.Errorf("passed id=%q location=%q", acc.gotID, acc.gotLocation)
	}
	var got nativebackup.BackupInfo
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.ID != "manual-kilo-2026" || got.Location != "cloud" {
		t.Fatalf("backup = %+v", got)
	}
}

func TestNativeBackupsDelete_MissingBackupID(t *testing.T) {
	acc := &fakeNativeBackupsAccessor{}
	h := NewNativeBackupsHandler(acc)

	body, _ := json.Marshal(deleteBackupReq{Location: "local"})
	req := httptest.NewRequest(http.MethodDelete, "/api/native-backups", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	h.Delete(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
	var be ErrorBody
	if err := json.Unmarshal(rr.Body.Bytes(), &be); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if be.Code != "validation" {
		t.Errorf("code = %q, want validation", be.Code)
	}
}

// Register wires both routes onto a mux without panicking and they
// dispatch to the right handlers.
func TestNativeBackupsRegister(t *testing.T) {
	acc := &fakeNativeBackupsAccessor{
		infos: []nativebackup.BackupInfo{{ID: "pre-sync-x", Kind: "pre-sync"}},
	}
	h := NewNativeBackupsHandler(acc)
	mux := http.NewServeMux()
	h.Register(mux)

	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/native-backups", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET status = %d", rr.Code)
	}

	body, _ := json.Marshal(restoreReq{BackupID: "pre-sync-x"})
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/native-backups/restore", bytes.NewReader(body)))
	if rr.Code != http.StatusOK {
		t.Fatalf("POST status = %d; body=%s", rr.Code, rr.Body.String())
	}

	body, _ = json.Marshal(deleteBackupReq{BackupID: "pre-sync-x"})
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodDelete, "/api/native-backups", bytes.NewReader(body)))
	if rr.Code != http.StatusOK {
		t.Fatalf("DELETE status = %d; body=%s", rr.Code, rr.Body.String())
	}
}
