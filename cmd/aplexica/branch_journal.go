package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// shortHashLen is the git-style 8-character prefix used throughout the
// branch CLI for human-readable hash references. BRD-04 §5.1 shows
// auto-derived names like "<short-event-id>-<target-agent>".
const shortHashLen = 8

// journalBranchOp appends one structured JSONL line to
// ~/.aplexica/logs/branch-ops.jsonl per FR-04.10. Best-effort: returns
// the underlying error but callers downgrade write failures to warnings
// since the in-store mutation has already completed.
//
// storeRoot is used solely to derive the logs directory (sibling of the
// store under ~/.aplexica/). When the inferred logs dir cannot be
// determined (e.g. tests passing a tempdir), the journal entry is
// written under <storeRoot>/../logs/ or, failing that, skipped.
func journalBranchOp(storeRoot, op string, fields map[string]any) error {
	logsDir, err := branchOpLogsDir(storeRoot)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(logsDir, 0o755); err != nil {
		return fmt.Errorf("branch-ops mkdir: %w", err)
	}
	entry := map[string]any{
		"at": time.Now().UTC().Format(time.RFC3339Nano),
		"op": op,
	}
	for k, v := range fields {
		entry[k] = v
	}
	line, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("branch-ops marshal: %w", err)
	}
	path := filepath.Join(logsDir, "branch-ops.jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("branch-ops open: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("branch-ops write: %w", err)
	}
	return nil
}

// branchOpLogsDir derives the logs/ directory from a store path. If
// storeRoot ends in ".aplexica/store" we return ".aplexica/logs"
// adjacent to it; otherwise we use <storeRoot>/../logs.
func branchOpLogsDir(storeRoot string) (string, error) {
	if storeRoot == "" {
		return "", errors.New("branch-ops: empty store root")
	}
	parent := filepath.Dir(storeRoot)
	return filepath.Join(parent, "logs"), nil
}
