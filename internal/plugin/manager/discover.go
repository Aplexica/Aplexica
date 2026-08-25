// SPDX-License-Identifier: AGPL-3.0-or-later
package manager

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/aplexica/aplexica/internal/plugin/proto"
)

// manifestFilename is the fixed name of the per-plugin manifest the
// daemon looks for inside each plugin subdirectory.
const manifestFilename = "plugin.json"

// Discovered describes one well-formed adapter plugin manifest found on
// disk, BEFORE any subprocess is spawned.
type Discovered struct {
	// Dir is the absolute plugin subdirectory holding the manifest and
	// (by default) the executable.
	Dir string
	// Manifest is the parsed and Validate()'d manifest.
	Manifest proto.Manifest
	// Executable is the absolute path to the plugin executable. Relative
	// manifest.Executable values are resolved against Dir.
	Executable string
}

// Discover scans the manager's plugins root for <dir>/*/plugin.json,
// parses and Validate()s each, and returns only the well-formed ADAPTER
// manifests. Remote-kind manifests are skipped with a log line; invalid
// or unreadable manifests are likewise logged and skipped — Discover never
// fails for a single bad plugin. A missing root directory is not an error:
// absence returns (nil, nil), matching the "no plugins dir" degrade-to-
// today's-behavior contract. No subprocess is spawned.
func (m *Manager) Discover() ([]Discovered, error) {
	entries, err := os.ReadDir(m.dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// Absent plugins dir is the common case (no plugins
			// installed). Not an error.
			return nil, nil
		}
		return nil, fmt.Errorf("plugin/manager: read plugins dir %q: %w", m.dir, err)
	}

	var found []Discovered
	for _, entry := range entries {
		if !entry.IsDir() {
			// Only per-plugin subdirectories are considered; stray
			// files at the root are ignored.
			continue
		}
		sub := filepath.Join(m.dir, entry.Name())
		manifestPath := filepath.Join(sub, manifestFilename)

		raw, err := os.ReadFile(manifestPath)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				// Subdir without a manifest — not a plugin dir. Skip
				// silently to avoid noise from unrelated directories.
				continue
			}
			m.logger.Warn("plugin/manager: cannot read manifest, skipping",
				"path", manifestPath, "error", err)
			continue
		}

		var man proto.Manifest
		if err := json.Unmarshal(raw, &man); err != nil {
			m.logger.Warn("plugin/manager: malformed manifest JSON, skipping",
				"path", manifestPath, "error", err)
			continue
		}
		if err := man.Validate(); err != nil {
			m.logger.Warn("plugin/manager: invalid manifest, skipping",
				"path", manifestPath, "name", man.Name, "error", err)
			continue
		}
		if !man.IsAdapter() {
			// Remote-transport plugins are loaded by a different code
			// path; this manager only spawns adapter plugins.
			m.logger.Info("plugin/manager: skipping non-adapter plugin",
				"path", manifestPath, "name", man.Name, "kind", man.PluginKind)
			continue
		}

		exe := man.Executable
		if !filepath.IsAbs(exe) {
			exe = filepath.Join(sub, exe)
		}
		found = append(found, Discovered{
			Dir:        sub,
			Manifest:   man,
			Executable: exe,
		})
	}
	return found, nil
}
