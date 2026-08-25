package claudecode

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/aplexica/aplexica/internal/adapter"
)

// ProjectDirs reports the working directories Claude Code has run in. CLI
// transcripts provide cwd under ~/.claude/projects; Claude Code Desktop's
// session catalog additionally maps its automatic worktree cwd back to the
// canonical originCwd. The directory name itself is a lossy encoding, so file
// content is authoritative. Deduped by path (newest timestamp per path).
func (a *Adapter) ProjectDirs() ([]adapter.ProjectPresence, error) {
	root := filepath.Join(a.HomeDir, ".claude", "projects")
	byPath := map[string]adapter.ProjectPresence{}
	desktopByCLI := map[string]desktopSessionRecord{}
	for _, record := range a.desktopSessions() {
		projectPath := record.projectPath()
		if projectPath != "" {
			addClaudeProjectPresence(byPath, projectPath, record.lastActive())
		}
		if record.CLISessionID != "" {
			desktopByCLI[record.CLISessionID] = record
		}
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return claudeProjectPresenceSlice(byPath), nil // Desktop-only is valid.
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		cwd, sessionID, ts := firstSessionMetadata(filepath.Join(root, e.Name()))
		if cwd == "" {
			continue
		}
		if record, ok := desktopByCLI[sessionID]; ok {
			if projectPath := record.projectPath(); projectPath != "" {
				cwd = projectPath
			}
			if record.lastActive().After(ts) {
				ts = record.lastActive()
			}
		}
		addClaudeProjectPresence(byPath, cwd, ts)
	}
	return claudeProjectPresenceSlice(byPath), nil
}

func addClaudeProjectPresence(byPath map[string]adapter.ProjectPresence, path string, ts time.Time) {
	cur := adapter.ProjectPresence{Path: path, LastActive: ts}
	if ex, ok := byPath[path]; ok {
		cur = adapter.NewerPresence(ex, cur)
	}
	byPath[path] = cur
}

func claudeProjectPresenceSlice(byPath map[string]adapter.ProjectPresence) []adapter.ProjectPresence {
	out := make([]adapter.ProjectPresence, 0, len(byPath))
	for _, v := range byPath {
		out = append(out, v)
	}
	return out
}

// Scanner buffer sizes for reading session lines, which can be large
// (tool outputs, embedded content): 64 KiB initial, 8 MiB max. A line
// exceeding the max ends that file's scan early (best-effort) — far
// beyond any real session line.
const (
	scanBufInitial = 64 * 1024
	scanBufMax     = 8 * 1024 * 1024
)

func firstSessionMetadata(projDir string) (string, string, time.Time) {
	files, err := os.ReadDir(projDir)
	if err != nil {
		return "", "", time.Time{}
	}
	for _, f := range files {
		if f.IsDir() || filepath.Ext(f.Name()) != ".jsonl" {
			continue
		}
		fh, err := os.Open(filepath.Join(projDir, f.Name()))
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(fh)
		sc.Buffer(make([]byte, 0, scanBufInitial), scanBufMax)
		var sessionID string
		for sc.Scan() {
			var line struct {
				Cwd       string    `json:"cwd"`
				SessionID string    `json:"sessionId"`
				Timestamp time.Time `json:"timestamp"`
			}
			if json.Unmarshal(sc.Bytes(), &line) != nil {
				continue
			}
			if sessionID == "" {
				sessionID = line.SessionID
			}
			if line.Cwd != "" {
				fh.Close()
				return line.Cwd, sessionID, line.Timestamp
			}
		}
		fh.Close()
	}
	return "", "", time.Time{}
}
