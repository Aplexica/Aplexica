package adapter

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
)

const (
	nativeMirrorReadMaxBytes = 16 << 20
	nativeMirrorGitTimeout   = 2 * time.Second
)

// SafeNativeMirrorFirstContact is the shared fail-closed baseline check for
// app-managed Git worktrees. An existing destination is safe when Git proves
// it is a tracked, unmodified checkout file, or when its bytes exactly match a
// version Aplexica previously materialized for this artifact. The latter keeps
// ignored/untracked mirrors updateable across daemon restarts without treating
// an app-side edit as disposable.
func SafeNativeMirrorFirstContact(store *acf.Store, artifact acf.Artifact, mirrorPath string, decoder OpaqueDecoder, skillMarker bool) (bool, error) {
	info, err := os.Lstat(mirrorPath)
	if os.IsNotExist(err) {
		return true, nil
	}
	if err != nil {
		return false, nil
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() > nativeMirrorReadMaxBytes {
		return false, nil
	}
	if nativeMirrorTrackedAndUnmodified(mirrorPath) {
		return true, nil
	}

	f, err := os.Open(mirrorPath)
	if err != nil {
		return false, nil
	}
	raw, readErr := io.ReadAll(io.LimitReader(f, nativeMirrorReadMaxBytes+1))
	closeErr := f.Close()
	if readErr != nil || closeErr != nil || len(raw) > nativeMirrorReadMaxBytes {
		return false, nil
	}
	events, err := store.ReadEvents(artifact.Kind, artifact.ArtifactID)
	if err != nil {
		return false, fmt.Errorf("adapter: read native-mirror history: %w", err)
	}
	for _, event := range events {
		content, decodeErr := decoder(event)
		if decodeErr != nil {
			continue
		}
		expected := []byte(content)
		if skillMarker {
			expected = AppendSkillMarker(expected, artifact.ArtifactID)
		}
		if bytes.Equal(raw, expected) {
			return true, nil
		}
	}
	return false, nil
}

// nativeMirrorTrackedAndUnmodified asks Git instead of parsing its private
// index format. It deliberately fails closed for untracked and ignored files.
func nativeMirrorTrackedAndUnmodified(path string) bool {
	root, rel, ok := linkedWorktreePath(path)
	if !ok {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), nativeMirrorGitTimeout)
	defer cancel()

	tracked := exec.CommandContext(ctx, "git", "-C", root, "ls-files", "--error-unmatch", "--", filepath.ToSlash(rel))
	tracked.Stdout = io.Discard
	tracked.Stderr = io.Discard
	if tracked.Run() != nil {
		return false
	}
	unchanged := exec.CommandContext(ctx, "git", "-C", root, "diff", "--quiet", "HEAD", "--", filepath.ToSlash(rel))
	unchanged.Stdout = io.Discard
	unchanged.Stderr = io.Discard
	return unchanged.Run() == nil
}

func linkedWorktreePath(path string) (root, rel string, ok bool) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", "", false
	}
	cur := filepath.Dir(abs)
	for {
		git := filepath.Join(cur, ".git")
		if info, statErr := os.Lstat(git); statErr == nil {
			if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
				return "", "", false
			}
			rel, err = filepath.Rel(cur, abs)
			if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return "", "", false
			}
			return cur, rel, true
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", "", false
		}
		cur = parent
	}
}
