package openclaw

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/aplexica/aplexica/internal/adapter"
)

// ProjectDirs reports working directories OpenClaw has run in by reading the
// session header line from ~/.openclaw/agents/<id>/sessions/*.jsonl.
//
// The scan is scoped to exactly the surface Discover advertises: the *.jsonl
// files directly inside each agents/<id>/sessions/ dir, with NO recursion
// below it. A recursive walk of agents/ would reach the swappable backend's
// rollout tree under agents/<id>/agent/codex-home/sessions/ — whose transcripts
// "must NOT import as openclaw conversations" — and would also pay the I/O of
// reading every transcript's first line in that potentially-large tree. Files
// this adapter materialized itself (canonical imports, which carry the
// synthetic ~/.openclaw/workspace cwd) are skipped so they are never reported
// as a user project.
func (a *Adapter) ProjectDirs() ([]adapter.ProjectPresence, error) {
	if a.HomeDir == "" {
		return nil, nil
	}
	sessionDirs, _ := filepath.Glob(filepath.Join(a.HomeDir, ".openclaw", "agents", "*", "sessions"))
	byPath := map[string]adapter.ProjectPresence{}
	for _, dir := range sessionDirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || filepath.Ext(e.Name()) != ".jsonl" {
				continue
			}
			p := filepath.Join(dir, e.Name())
			if sessionFileIsCanonicalImport(p) {
				continue
			}
			cwd, ts := openClawSessionHeader(p)
			if cwd == "" {
				continue
			}
			cur := adapter.ProjectPresence{Path: cwd, LastActive: ts}
			if existing, ok := byPath[cwd]; ok {
				cur = adapter.NewerPresence(existing, cur)
			}
			byPath[cwd] = cur
		}
	}
	out := make([]adapter.ProjectPresence, 0, len(byPath))
	for _, v := range byPath {
		out = append(out, v)
	}
	return out, nil
}

func openClawSessionHeader(path string) (string, time.Time) {
	f, err := os.Open(path)
	if err != nil {
		return "", time.Time{}
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	if !sc.Scan() {
		return "", time.Time{}
	}
	var line struct {
		Type      string    `json:"type"`
		CWD       string    `json:"cwd"`
		Timestamp time.Time `json:"timestamp"`
	}
	if json.Unmarshal(sc.Bytes(), &line) != nil || line.CWD == "" {
		return "", time.Time{}
	}
	if line.Timestamp.IsZero() {
		if st, err := os.Stat(path); err == nil {
			line.Timestamp = st.ModTime()
		}
	}
	return line.CWD, line.Timestamp
}
