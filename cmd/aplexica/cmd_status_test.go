package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/conflicts"
	"github.com/aplexica/aplexica/internal/daemon"
)

func TestQueryDaemonStatus_SyncEvidenceIsOptIn(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("control socket is unavailable on the Windows CI runner")
	}
	dir := t.TempDir()
	if runtime.GOOS == "darwin" {
		var err error
		dir, err = os.MkdirTemp("/tmp", "aplexica-status-test")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(dir) })
	}

	sockPath := filepath.Join(dir, "status.sock")
	srv := daemon.NewControlServer(sockPath, &daemon.StatusInfo{PID: 123}, nil)
	var calls atomic.Int32
	deadlines := make(chan bool, 1)
	srv.SetSyncEvidenceProvider(func(ctx context.Context) daemon.SyncEvidenceStatus {
		calls.Add(1)
		_, bounded := ctx.Deadline()
		deadlines <- bounded
		return daemon.SyncEvidenceStatus{RemoteAvailable: true}
	})
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Stop() })

	ordinary, err := queryDaemonStatus(sockPath, false)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 0 || ordinary.SyncEvidence != nil {
		t.Fatalf("ordinary status must stay local: calls=%d evidence=%+v", calls.Load(), ordinary.SyncEvidence)
	}

	diagnostic, err := queryDaemonStatus(sockPath, true)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || diagnostic.SyncEvidence == nil {
		t.Fatalf("opt-in status must make exactly one provider call: calls=%d evidence=%+v", calls.Load(), diagnostic.SyncEvidence)
	}
	if !<-deadlines {
		t.Fatal("sync-evidence provider call must carry a deadline")
	}
}

func TestAgentAttributed_BySyncedAgents(t *testing.T) {
	art := acf.Artifact{ArtifactID: "x", SyncedAgents: []string{"codex", "claude-code"}}
	if !agentAttributed(art, "codex", nil) {
		t.Errorf("expected attribution via SyncedAgents membership")
	}
	if agentAttributed(art, "hermes", nil) {
		t.Errorf("hermes not in SyncedAgents -> not attributed")
	}
}

func TestAgentAttributed_BySourcePathRoot(t *testing.T) {
	// Build paths with filepath.Join so separators match the OS — agentAttributed
	// compares against filepath.Separator, so hard-coded unix slashes fail on Windows.
	root := filepath.Join(t.TempDir(), ".claude")
	art := acf.Artifact{ArtifactID: "y", SourcePath: filepath.Join(root, "CLAUDE.md")}
	if !agentAttributed(art, "claude-code", []string{root}) {
		t.Errorf("expected attribution when SourcePath is under a global root")
	}
	other := filepath.Join(t.TempDir(), ".codex")
	if agentAttributed(art, "claude-code", []string{other}) {
		t.Errorf("SourcePath not under the given root -> not attributed")
	}
}

func TestAgentAttributed_NoMatch(t *testing.T) {
	art := acf.Artifact{ArtifactID: "z"}
	if agentAttributed(art, "codex", nil) {
		t.Errorf("empty artifact must not be attributed to any agent")
	}
}

// TestCollectAgents_InstalledReflectsHome verifies the daemon-independent
// presence probe: with HOME set to a temp dir containing only ~/.claude, the
// status agents table marks claude-code installed and the rest not — proving
// the panel reflects real on-disk discovery, not the static adapter registry.
func TestCollectAgents_InstalledReflectsHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // Windows: os.UserHomeDir() reads USERPROFILE, not HOME
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	agents := collectAgents(t.TempDir()) // empty store root -> zero counts
	got := map[string]bool{}
	for _, a := range agents {
		got[a.Name] = a.Installed
	}
	if !got["claude-code"] {
		t.Errorf("claude-code should be installed when ~/.claude exists; agents=%+v", agents)
	}
	for _, name := range []string{"codex", "hermes", "openclaw", "kilo"} {
		if got[name] {
			t.Errorf("%s should NOT be installed (no native dir under temp HOME)", name)
		}
	}
}

func TestStatusAgentCache_RefreshesAtSlowCadence(t *testing.T) {
	loads := 0
	cache := statusAgentCache{load: func(string) []AgentStatus {
		loads++
		return []AgentStatus{{Name: "codex", ArtifactCount: loads}}
	}}
	now := time.Now()

	first := cache.snapshot(now, t.TempDir())
	second := cache.snapshot(now.Add(5*time.Second), t.TempDir())
	if loads != 1 || first[0].ArtifactCount != 1 || second[0].ArtifactCount != 1 {
		t.Fatalf("five-second tray tick must reuse agent counts: loads=%d first=%+v second=%+v", loads, first, second)
	}

	third := cache.snapshot(now.Add(statusAgentRefreshInterval), t.TempDir())
	if loads != 2 || third[0].ArtifactCount != 2 {
		t.Fatalf("cache must refresh at the slow cadence: loads=%d third=%+v", loads, third)
	}
}

func TestStatusConflictSummaries_RedactsFullPayload(t *testing.T) {
	full := json.RawMessage(`{"body":"large secret conflict body"}`)
	in := []conflicts.Conflict{{
		ArtifactID: "artifact-1",
		Kind:       acf.KindConversation,
		Heads: []conflicts.Head{{
			SourceAgent:    "remote",
			EventID:        "evt-1",
			ContentSHA256:  "sha",
			PayloadPreview: "preview survives",
			FullPayload:    full,
		}},
	}}

	out := statusConflictSummaries(in)
	if len(out) != 1 || len(out[0].Heads) != 1 {
		t.Fatalf("unexpected summary shape: %+v", out)
	}
	if len(out[0].Heads[0].FullPayload) != 0 {
		t.Fatalf("status summary must not include full payload: %s", string(out[0].Heads[0].FullPayload))
	}
	if out[0].Heads[0].PayloadPreview != "preview survives" {
		t.Fatalf("status summary should preserve the compact preview")
	}
	if len(in[0].Heads[0].FullPayload) == 0 {
		t.Fatalf("status summary must not mutate the stored conflict value")
	}

	raw, err := json.Marshal(StatusSnapshot{Conflicts: out, ConflictCount: len(out)})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "fullPayload") || strings.Contains(string(raw), "large secret conflict body") {
		t.Fatalf("status JSON leaked full conflict payload: %s", string(raw))
	}
}

func TestRenderStorePressure_DisabledIsQuiet(t *testing.T) {
	var buf bytes.Buffer
	// StoreMaxBytes==0 → cap disabled (store_max_gb=0): render nothing.
	renderStorePressure(&buf, &daemon.StatusInfo{StoreBytes: 5 << 30, StoreMaxBytes: 0})
	if buf.Len() != 0 {
		t.Errorf("disabled cap must render nothing, got %q", buf.String())
	}
}

func TestRenderStorePressure_UnderWatermarkIsQuietOK(t *testing.T) {
	var buf bytes.Buffer
	renderStorePressure(&buf, &daemon.StatusInfo{
		StoreBytes:    5 << 30,
		StoreMaxBytes: 10 << 30,
	})
	out := buf.String()
	if strings.Contains(out, "WARNING") {
		t.Errorf("under-watermark must not warn, got %q", out)
	}
	if !strings.Contains(out, "Store:") {
		t.Errorf("expected a quiet Store usage line, got %q", out)
	}
}

func TestRenderStorePressure_OverHighWatermarkWarns(t *testing.T) {
	var buf bytes.Buffer
	renderStorePressure(&buf, &daemon.StatusInfo{
		StoreBytes:        85 << 29, // 8.5 GB
		StoreMaxBytes:     10 << 30,
		OverHighWatermark: true,
	})
	out := buf.String()
	if !strings.Contains(out, "WARNING") {
		t.Errorf("over high watermark must print a WARNING, got %q", out)
	}
	if !strings.Contains(out, "high watermark") {
		t.Errorf("warning should name the high watermark, got %q", out)
	}
}

func TestRenderStorePressure_HonestSplit(t *testing.T) {
	var buf bytes.Buffer
	renderStorePressure(&buf, &daemon.StatusInfo{
		StoreBytes:            5 << 30,
		StoreMaxBytes:         10 << 30,
		StoreReclaimableBytes: 1 << 30,
		StorePinnedBytes:      4 << 30,
		StoreEventLogBytes:    3 << 30,
	})
	out := buf.String()
	if !strings.Contains(out, "Pinned: 4.0 GB") {
		t.Errorf("expected pinned bytes in the split line, got %q", out)
	}
	if !strings.Contains(out, "reclaimable by retention: 1.0 GB") {
		t.Errorf("expected reclaimable bytes in the split line, got %q", out)
	}
	if !strings.Contains(out, "event logs 3.0 GB") {
		t.Errorf("expected the event-log share of pinned bytes, got %q", out)
	}
	if strings.Contains(out, "unreachable") {
		t.Errorf("watermark reachable — must not claim otherwise, got %q", out)
	}
}

func TestRenderStorePressure_WatermarkUnreachable(t *testing.T) {
	// Model a store whose pinned bytes alone exceed its watermark. The report
	// must say the watermark is unreachable, not imply retention is merely behind.
	gb := func(f float64) int64 { return int64(f * (1 << 30)) }
	var buf bytes.Buffer
	renderStorePressure(&buf, &daemon.StatusInfo{
		StoreBytes:                gb(9.7),
		StoreMaxBytes:             gb(10),
		StoreHighWatermarkBytes:   gb(8.6),
		StoreReclaimableBytes:     gb(0.5),
		StorePinnedBytes:          gb(9.2),
		StoreEventLogBytes:        gb(9.0),
		OverHighWatermark:         true,
		StoreWatermarkUnreachable: true,
	})
	out := buf.String()
	if !strings.Contains(out, "WARNING") {
		t.Errorf("still over the watermark — the WARNING line must remain, got %q", out)
	}
	if !strings.Contains(out, "Watermark unreachable: 9.2 GB pinned") {
		t.Errorf("expected the unreachable line naming the pinned bytes, got %q", out)
	}
}

func TestRenderStorePressure_NoSplitFromOldDaemon(t *testing.T) {
	// A daemon predating the split reports zero for all split fields — the
	// renderer must fall back to exactly the original lines.
	var buf bytes.Buffer
	renderStorePressure(&buf, &daemon.StatusInfo{
		StoreBytes:    5 << 30,
		StoreMaxBytes: 10 << 30,
	})
	out := buf.String()
	if strings.Contains(out, "Pinned") || strings.Contains(out, "unreachable") {
		t.Errorf("zero split fields must render no split lines, got %q", out)
	}
}

func TestRenderStorePressure_OverEmergencyWarnsRefusal(t *testing.T) {
	var buf bytes.Buffer
	renderStorePressure(&buf, &daemon.StatusInfo{
		StoreBytes:        97 << 29, // 9.7 GB
		StoreMaxBytes:     10 << 30,
		OverHighWatermark: true,
		OverEmergency:     true,
	})
	out := buf.String()
	if !strings.Contains(out, "WARNING") {
		t.Errorf("over emergency must print a WARNING, got %q", out)
	}
	if !strings.Contains(strings.ToUpper(out), "REFUS") {
		t.Errorf("over emergency must state ingestion is being refused, got %q", out)
	}
}

func TestRenderDeferredMaterializations_EmptyIsQuiet(t *testing.T) {
	var buf bytes.Buffer
	renderDeferredMaterializations(&buf, &daemon.StatusInfo{})
	if buf.Len() != 0 {
		t.Errorf("an empty retry queue must render nothing, got %q", buf.String())
	}
}

func TestRenderDeferredMaterializations_PendingIsNotAWarning(t *testing.T) {
	var buf bytes.Buffer
	renderDeferredMaterializations(&buf, &daemon.StatusInfo{
		DeferredMaterializations: []map[string]any{
			{"agent": "claude-code", "artifactId": "a1", "state": "pending", "attempts": 2},
		},
	})
	out := buf.String()
	if strings.Contains(out, "WARNING") {
		t.Errorf("a normal in-flight retry must not warn, got %q", out)
	}
	if !strings.Contains(out, "1 pending retry") {
		t.Errorf("expected a pending retry count, got %q", out)
	}
}

func TestRenderDeferredMaterializations_AbandonedWarnsAndNamesEntries(t *testing.T) {
	var buf bytes.Buffer
	renderDeferredMaterializations(&buf, &daemon.StatusInfo{
		DeferredMaterializations: []map[string]any{
			{"agent": "claude-code", "artifactId": "a1", "state": "pending", "attempts": 2},
			{"agent": "codex", "artifactId": "a2", "state": "abandoned", "attempts": 64},
		},
	})
	out := buf.String()
	if !strings.Contains(out, "WARNING") {
		t.Errorf("an abandoned materialization must warn, got %q", out)
	}
	for _, want := range []string{"codex", "a2", "aplexica repair materialization"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in status output, got %q", want, out)
		}
	}
}

// A row that qualified to be raised but hit the device's daily escalation cap
// must be counted, not silently dropped: the cap can carry real backlog, and
// hiding it would recreate the
// silence the whole surface exists to end.
func TestRenderDeferredMaterializations_ReportsEscalationsHeldByTheCap(t *testing.T) {
	var buf bytes.Buffer
	renderDeferredMaterializations(&buf, &daemon.StatusInfo{
		DeferredMaterializations: []map[string]any{
			{"agent": "claude-code", "artifactId": "a1", "state": "pending", "attempts": 0,
				"escalationDeferred": true},
			{"agent": "claude-code", "artifactId": "a2", "state": "pending", "attempts": 0,
				"escalationDeferred": true},
			{"agent": "claude-code", "artifactId": "a3", "state": "pending", "attempts": 1},
		},
	})
	out := buf.String()
	if !strings.Contains(out, "2 more writes waiting to be raised") {
		t.Errorf("the held backlog must be stated, got %q", out)
	}
}

// The remedy printed beside a stuck write must be the one for THAT class, and
// a class no shipped command resolves must print no command at all.
func TestRenderDeferredMaterializations_PrintsThePerClassRemedy(t *testing.T) {
	var buf bytes.Buffer
	renderDeferredMaterializations(&buf, &daemon.StatusInfo{
		DeferredMaterializations: []map[string]any{
			{"agent": "claude-code", "artifactId": "a1", "state": "needs_attention", "attempts": 64,
				"remedy": "aplexica repair conversation a1"},
			{"agent": "codex", "artifactId": "a2", "state": "needs_attention", "attempts": 64},
		},
	})
	out := buf.String()
	if !strings.Contains(out, "Fix with: aplexica repair conversation a1") {
		t.Errorf("expected the class remedy, got %q", out)
	}
	if strings.Count(out, "Fix with:") != 1 {
		t.Errorf("a class with no shipped remedy must offer none, got %q", out)
	}
}
