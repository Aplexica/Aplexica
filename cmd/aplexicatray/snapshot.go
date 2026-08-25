//go:build tray

package main

import "time"

// StatusSnapshot mirrors cmd/aplexica/cmd_status.go's StatusSnapshot wire
// shape. We deliberately do NOT import from cmd/aplexica — the tray's
// contract is the JSON output of `aplexica status --watch --json`, not
// the producer's source. New fields landing in the daemon's StatusSnapshot
// will appear here on the tray's own cadence as we surface them.
type StatusSnapshot struct {
	Timestamp       time.Time   `json:"timestamp"`
	DaemonAvailable bool        `json:"daemonAvailable"`
	DaemonInfo      *DaemonInfo `json:"daemonInfo,omitempty"`
	Conflicts       []Conflict  `json:"conflicts"`
	ConflictCount   int         `json:"conflictCount"`
}

type DaemonInfo struct {
	PID        int       `json:"pid"`
	StartedAt  time.Time `json:"startedAt"`
	WatchedDir string    `json:"watchedDir"`
	Version    string    `json:"version,omitempty"`
	// LastActivity is the wall-clock time of the daemon orchestrator's
	// last successful primary-import + fan-out cycle (v0.39.0). Older
	// daemons (≤ v0.38.0) omit this field; consumers must treat zero
	// as "unknown — fall back to tick-arrival proxy."
	LastActivity time.Time `json:"lastActivity,omitzero"`
	// PendingImports is the per-path debouncer queue depth at status-
	// request time (v0.44.0; ADR-0159 Candidate A). 0 = steady-state
	// idle. The tray surfaces this as "active (N pending)" in the
	// menu header when > 0. Older daemons (≤ v0.43.0) omit the field;
	// consumers must treat absent as 0.
	PendingImports int `json:"pendingImports,omitempty"`
	// Paused is set by the tray from the shared sync-pause state file.
	// Newer daemons may also include the same field directly in status
	// JSON; either way, true means global sync fan-out is paused.
	Paused bool `json:"paused,omitempty"`

	// AdapterStates (v0.51.0; ADR-0159 Candidate B) maps adapter name
	// to a bucketed state string ("active" / "idle"). Surfaced in the
	// tray "Adapters →" submenu (v0.64.0). Older daemons (≤ v0.50.0)
	// omit the field; consumers must treat absent as empty.
	AdapterStates map[string]string `json:"adapterStates,omitempty"`

	// AdapterLastErrors (v0.51.0; ADR-0159 Candidate D) maps adapter
	// name to its most-recent redacted error string ($HOME → ~/).
	// Cleared per adapter on next successful Import/Export. Surfaced
	// in the tray "⚠ Adapter errors →" submenu (v0.64.0). Older
	// daemons (≤ v0.50.0) omit the field; consumers must treat absent
	// as empty.
	AdapterLastErrors map[string]string `json:"adapterLastErrors,omitempty"`

	// PendingProjects (v0.58.0; BRD-02 §4.13) lists project-scope
	// artifacts whose canonical project ID has no entry in the user's
	// project registry on this device. Each entry has keys "id",
	// "artifactCount", "samplePath". Surfaced in the tray "Pending
	// projects (N) →" submenu. Older daemons (≤ v0.57.0) omit the
	// field; consumers must treat absent as empty.
	PendingProjects []map[string]any `json:"pendingProjects,omitempty"`

	// Store-disk-pressure fields mirror daemon.StatusInfo. High
	// watermark and emergency pressure are action-needed notifications
	// for the tray icon.
	OverHighWatermark bool `json:"overHighWatermark,omitempty"`
	OverEmergency     bool `json:"overEmergency,omitempty"`
}

type Conflict struct {
	ArtifactID string `json:"artifactId"`
	Kind       string `json:"kind"`
	Heads      []Head `json:"heads"`
}

type Head struct {
	SourceAgent    string  `json:"sourceAgent"`
	EventID        string  `json:"eventId"`
	ContentSHA256  string  `json:"contentSha256"`
	AbsTimestamp   float64 `json:"absTimestamp"`
	PayloadPreview string  `json:"payloadPreview,omitempty"`
}
