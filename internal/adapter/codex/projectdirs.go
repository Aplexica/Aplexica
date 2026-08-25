package codex

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/aplexica/aplexica/internal/adapter"
)

// ProjectDirs reports the working directories Codex has been run in,
// read from session_meta.cwd across ~/.codex/sessions/**/*.jsonl.
// Deduped by path, keeping the newest session timestamp per path.
func (a *Adapter) ProjectDirs() ([]adapter.ProjectPresence, error) {
	root := a.sessionsDir()
	worktrees := a.managedWorktrees()
	byPath := map[string]adapter.ProjectPresence{}
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Ext(p) != ".jsonl" {
			return nil
		}
		cwd, ts := firstSessionMetaCwd(p)
		if cwd == "" {
			return nil
		}
		cwd = normalizeManagedWorktreeCwd(cwd, worktrees)
		cur := adapter.ProjectPresence{Path: cwd, LastActive: ts}
		if existing, ok := byPath[cwd]; ok {
			cur = adapter.NewerPresence(existing, cur)
		}
		byPath[cwd] = cur
		return nil
	})
	out := make([]adapter.ProjectPresence, 0, len(byPath))
	for _, v := range byPath {
		out = append(out, v)
	}
	return out, nil
}

// Scanner buffer sizes for reading session lines, which can be large
// (tool outputs, embedded content): 64 KiB initial, 8 MiB max. A line
// exceeding the max ends that file's scan early (best-effort) — far
// beyond any real session line.
const (
	scanBufInitial = 64 * 1024
	scanBufMax     = 8 * 1024 * 1024
)

// firstSessionMetaCwd reads the first session_meta line and returns its
// cwd + timestamp. Empty cwd means "not found / not a session file".
func firstSessionMetaCwd(path string) (string, time.Time) {
	f, err := os.Open(path)
	if err != nil {
		return "", time.Time{}
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, scanBufInitial), scanBufMax)
	for sc.Scan() {
		var line struct {
			Type    string `json:"type"`
			Payload struct {
				Cwd       string    `json:"cwd"`
				Timestamp time.Time `json:"timestamp"`
			} `json:"payload"`
		}
		if json.Unmarshal(sc.Bytes(), &line) == nil && line.Type == "session_meta" && line.Payload.Cwd != "" {
			return line.Payload.Cwd, line.Payload.Timestamp
		}
	}
	return "", time.Time{}
}
