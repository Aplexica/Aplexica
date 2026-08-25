package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"

	"github.com/aplexica/aplexica/internal/nativebackup"
)

// NativeBackupsAccessor is the seam between the daemon's on-disk native
// snapshot directory (~/.aplexica/backups) and the API handler. List
// enumerates the snapshots; Restore copies one snapshot's files back
// over the live native roots. Restore is REVERSIBLE — the underlying
// nativebackup.Restore always snapshots the CURRENT native state into a
// sibling pre-restore-* directory before overwriting anything, so the
// handler does not need to enforce that separately; it is structural.
//
// An empty agent string restores every agent recorded in the snapshot's
// manifest; a non-empty agent restores only that agent.
type NativeBackupsAccessor interface {
	List() ([]nativebackup.BackupInfo, error)
	StartCreate(agents []string, destination string) (nativebackup.BackupJob, error)
	CancelJob(jobID string) (nativebackup.BackupJob, error)
	Status() (nativebackup.Status, error)
	Override(agent string) (nativebackup.SafetyStatus, error)
	SetSchedule(cfg nativebackup.ScheduleConfig) (nativebackup.ScheduleConfig, error)
	SetRetention(cfg nativebackup.RetentionConfig) (nativebackup.RetentionConfig, error)
	Restore(ctx context.Context, backupID, agent, location string) (nativebackup.RestoreResult, error)
	Delete(ctx context.Context, backupID, location string) (nativebackup.BackupInfo, error)
}

// NativeBackupsHandler serves the two /api/native-backups endpoints.
type NativeBackupsHandler struct {
	acc NativeBackupsAccessor
}

// NewNativeBackupsHandler returns a NativeBackupsHandler bound to acc.
func NewNativeBackupsHandler(acc NativeBackupsAccessor) *NativeBackupsHandler {
	return &NativeBackupsHandler{acc: acc}
}

// Register attaches the native-backup routes: list + restore.
func (h *NativeBackupsHandler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/native-backups", h.List)
	mux.HandleFunc("POST /api/native-backups", h.Create)
	mux.HandleFunc("GET /api/native-backups/status", h.Status)
	mux.HandleFunc("POST /api/native-backups/jobs/cancel", h.CancelJob)
	mux.HandleFunc("POST /api/native-backups/override", h.Override)
	mux.HandleFunc("PUT /api/native-backups/schedule", h.SetSchedule)
	mux.HandleFunc("PUT /api/native-backups/retention", h.SetRetention)
	mux.HandleFunc("POST /api/native-backups/restore", h.Restore)
	mux.HandleFunc("DELETE /api/native-backups", h.Delete)
}

// List serves GET /api/native-backups — the catalog of first-run
// pre-sync snapshots plus any reversible pre-restore snapshots.
func (h *NativeBackupsHandler) List(w http.ResponseWriter, _ *http.Request) {
	out, err := h.acc.List()
	if err != nil {
		WriteError(w, http.StatusInternalServerError, err.Error(), "internal")
		return
	}
	if out == nil {
		out = []nativebackup.BackupInfo{}
	}
	WriteJSON(w, http.StatusOK, out)
}

type createBackupReq struct {
	Agents      []string `json:"agents,omitempty"`
	Destination string   `json:"destination,omitempty"`
}

// Create serves POST /api/native-backups. Empty agents means every installed
// agent with native backup roots. The handler starts daemon-owned work and
// returns immediately; page navigation or browser disconnects do not cancel the
// job.
func (h *NativeBackupsHandler) Create(w http.ResponseWriter, r *http.Request) {
	var req createBackupReq
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			WriteError(w, http.StatusBadRequest, "invalid JSON: "+err.Error(), "validation")
			return
		}
	}
	res, err := h.acc.StartCreate(req.Agents, req.Destination)
	if err != nil {
		WriteError(w, http.StatusBadRequest, err.Error(), "validation")
		return
	}
	WriteJSON(w, http.StatusAccepted, res)
}

type cancelBackupJobReq struct {
	JobID string `json:"jobId"`
}

// CancelJob serves POST /api/native-backups/jobs/cancel.
func (h *NativeBackupsHandler) CancelJob(w http.ResponseWriter, r *http.Request) {
	var req cancelBackupJobReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		WriteError(w, http.StatusBadRequest, "invalid JSON: "+err.Error(), "validation")
		return
	}
	if req.JobID == "" {
		WriteError(w, http.StatusBadRequest, "jobId is required", "validation")
		return
	}
	res, err := h.acc.CancelJob(req.JobID)
	if err != nil {
		WriteError(w, http.StatusBadRequest, err.Error(), "validation")
		return
	}
	WriteJSON(w, http.StatusOK, res)
}

// Status serves GET /api/native-backups/status.
func (h *NativeBackupsHandler) Status(w http.ResponseWriter, _ *http.Request) {
	res, err := h.acc.Status()
	if err != nil {
		WriteError(w, http.StatusInternalServerError, err.Error(), "internal")
		return
	}
	if res.Safety == nil {
		res.Safety = []nativebackup.SafetyStatus{}
	}
	WriteJSON(w, http.StatusOK, res)
}

type overrideBackupReq struct {
	Agent string `json:"agent"`
}

// Override serves POST /api/native-backups/override.
func (h *NativeBackupsHandler) Override(w http.ResponseWriter, r *http.Request) {
	var req overrideBackupReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		WriteError(w, http.StatusBadRequest, "invalid JSON: "+err.Error(), "validation")
		return
	}
	if req.Agent == "" {
		WriteError(w, http.StatusBadRequest, "agent is required", "validation")
		return
	}
	res, err := h.acc.Override(req.Agent)
	if err != nil {
		WriteError(w, http.StatusBadRequest, err.Error(), "validation")
		return
	}
	WriteJSON(w, http.StatusOK, res)
}

// SetSchedule serves PUT /api/native-backups/schedule.
func (h *NativeBackupsHandler) SetSchedule(w http.ResponseWriter, r *http.Request) {
	req, err := decodeScheduleRequest(r.Body)
	if err != nil {
		WriteError(w, http.StatusBadRequest, "invalid JSON: "+err.Error(), "validation")
		return
	}
	res, err := h.acc.SetSchedule(req)
	if err != nil {
		WriteError(w, http.StatusBadRequest, err.Error(), "validation")
		return
	}
	WriteJSON(w, http.StatusOK, res)
}

func decodeScheduleRequest(r io.Reader) (nativebackup.ScheduleConfig, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nativebackup.ScheduleConfig{}, err
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nativebackup.ScheduleConfig{}, err
	}
	delete(raw, "lastRunAt")
	delete(raw, "nextRunAt")
	data, err = json.Marshal(raw)
	if err != nil {
		return nativebackup.ScheduleConfig{}, err
	}
	var out nativebackup.ScheduleConfig
	if err := json.Unmarshal(data, &out); err != nil {
		return nativebackup.ScheduleConfig{}, err
	}
	return out, nil
}

// SetRetention serves PUT /api/native-backups/retention.
func (h *NativeBackupsHandler) SetRetention(w http.ResponseWriter, r *http.Request) {
	var req nativebackup.RetentionConfig
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		WriteError(w, http.StatusBadRequest, "invalid JSON: "+err.Error(), "validation")
		return
	}
	res, err := h.acc.SetRetention(req)
	if err != nil {
		WriteError(w, http.StatusBadRequest, err.Error(), "validation")
		return
	}
	WriteJSON(w, http.StatusOK, res)
}

// restoreReq is the POST /api/native-backups/restore body shape.
// BackupID is the snapshot directory's base name (the BackupInfo.ID
// from List). Agent is optional — empty means restore every agent in
// the snapshot.
type restoreReq struct {
	BackupID string `json:"backupId"`
	Agent    string `json:"agent,omitempty"`
	Location string `json:"location,omitempty"`
}

// Restore serves POST /api/native-backups/restore. It is DESTRUCTIVE:
// the named snapshot's files overwrite the live native roots. The
// underlying nativebackup.Restore first snapshots the current native
// state into a sibling pre-restore-* directory so the operation can
// itself be undone (the PreRestoreDir is echoed back in the response).
func (h *NativeBackupsHandler) Restore(w http.ResponseWriter, r *http.Request) {
	var req restoreReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		WriteError(w, http.StatusBadRequest, "invalid JSON: "+err.Error(), "validation")
		return
	}
	if req.BackupID == "" {
		WriteError(w, http.StatusBadRequest, "backupId is required", "validation")
		return
	}
	res, err := h.acc.Restore(r.Context(), req.BackupID, req.Agent, req.Location)
	if err != nil {
		WriteError(w, http.StatusBadRequest, err.Error(), "validation")
		return
	}
	WriteJSON(w, http.StatusOK, res)
}

type deleteBackupReq struct {
	BackupID string `json:"backupId"`
	Location string `json:"location,omitempty"`
}

// Delete serves DELETE /api/native-backups. It removes either a local native
// snapshot directory or a client-encrypted cloud backup object, depending on
// Location.
func (h *NativeBackupsHandler) Delete(w http.ResponseWriter, r *http.Request) {
	var req deleteBackupReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		WriteError(w, http.StatusBadRequest, "invalid JSON: "+err.Error(), "validation")
		return
	}
	if req.BackupID == "" {
		WriteError(w, http.StatusBadRequest, "backupId is required", "validation")
		return
	}
	res, err := h.acc.Delete(r.Context(), req.BackupID, req.Location)
	if err != nil {
		WriteError(w, http.StatusBadRequest, err.Error(), "validation")
		return
	}
	WriteJSON(w, http.StatusOK, res)
}
