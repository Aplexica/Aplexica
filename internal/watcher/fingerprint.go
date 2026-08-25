//go:build darwin || linux

package watcher

import (
	"os"
	"path/filepath"
)

// fileFingerprint is a cheap change-detection token for a single file: its
// size and modification time. Stat-poll backstops (darwin's FSEvents
// supplement, linux's inotify-budget-exhausted fallback) diff fingerprints to
// decide whether a file MIGHT have changed; they deliberately do NOT hash
// content.
//
// Hashing the full content of every file on every poll is O(total bytes on
// disk) per tick — across a recursive watch of a large agent history (hundreds
// of directories, multi-MB conversation files) that re-read-and-hashed the
// entire history every poll and pegged multiple CPU cores at idle. (size,
// mtime) is the standard cheap change detector (make, rsync, git all use it).
// Any false positive it yields (e.g. an mtime bump with identical bytes) is
// harmless: the Debouncer re-hashes content downstream and drops unchanged
// files.
type fileFingerprint struct {
	size    int64
	modTime int64 // Unix nanoseconds
}

// fingerprintDir returns the (size, mtime) fingerprint of every regular file
// directly in dir, keyed by absolute path. It reads only directory-entry
// metadata — never file contents — so its cost scales with the number of
// files, not their size. Unreadable directories and stat failures are skipped
// silently (the next poll retries).
func fingerprintDir(dir string) map[string]fileFingerprint {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return map[string]fileFingerprint{}
	}
	out := make(map[string]fileFingerprint, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out[filepath.Join(dir, e.Name())] = fileFingerprint{
			size:    info.Size(),
			modTime: info.ModTime().UnixNano(),
		}
	}
	return out
}

// fingerprintTree returns the (size, mtime) fingerprint of every regular file
// under root, keyed by absolute path. It is the recursive companion to
// fingerprintDir for platform sources that can watch a whole tree with one
// native subscription. Dependency/VCS cache directories are pruned.
func fingerprintTree(root string) map[string]fileFingerprint {
	out := map[string]fileFingerprint{}
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if SkipWalkDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		out[path] = fileFingerprint{
			size:    info.Size(),
			modTime: info.ModTime().UnixNano(),
		}
		return nil
	})
	return out
}

// diffFingerprints reports the events implied by moving from prev to current:
// an OpChange for every file whose fingerprint is new or differs, and an
// OpRemove for every file present in prev but absent from current. A file with
// an unchanged fingerprint produces no event, so a quiet directory stays
// silent across polls.
func diffFingerprints(prev, current map[string]fileFingerprint) []Event {
	var events []Event
	for path, fp := range current {
		if prev[path] != fp {
			events = append(events, Event{Path: path, Op: OpChange})
		}
	}
	for path := range prev {
		if _, stillThere := current[path]; !stillThere {
			events = append(events, Event{Path: path, Op: OpRemove})
		}
	}
	return events
}
