package adapter

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/aplexica/aplexica/internal/privatefs"
)

const SecretConfigMode fs.FileMode = 0o600

func WriteSecretConfig(dest string, data []byte) error {
	if dest == "" || !filepath.IsAbs(dest) {
		return fmt.Errorf("adapter: secret config destination must be absolute")
	}
	if filepath.Base(dest) == ".mcp.json" {
		tracked, err := projectMCPTracked(dest)
		if err != nil {
			return err
		}
		if tracked {
			return fmt.Errorf("adapter: refusing secret expansion into tracked project .mcp.json; use an untracked local config or environment indirection")
		}
	}
	parent := filepath.Dir(dest)
	if _, err := os.Lstat(parent); os.IsNotExist(err) {
		if err := os.MkdirAll(parent, 0o700); err != nil {
			return err
		}
	}
	root, err := privatefs.OpenRoot(parent, privatefs.DirPolicy{Access: privatefs.AccessIntegrityOnly, AllowExisting: true})
	if err != nil {
		return fmt.Errorf("adapter: unsafe secret config parent: %w", err)
	}
	defer root.Close()
	if err := root.WriteFile(filepath.Base(dest), data, privatefs.FilePolicy{RejectWritableByOthers: true, PreserveStricter: true}); err != nil {
		return fmt.Errorf("adapter: write private config: %w", err)
	}
	return nil
}
func HardenSecretConfig(dest string) error {
	info, err := os.Lstat(dest)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("adapter: unsafe secret config object")
	}
	data, err := os.ReadFile(dest)
	if err != nil {
		return err
	}
	return WriteSecretConfig(dest, data)
}

func projectMCPTracked(dest string) (bool, error) {
	dir := filepath.Dir(dest)
	for {
		git := filepath.Join(dir, ".git")
		if info, err := os.Lstat(git); err == nil {
			if info.Mode()&os.ModeSymlink != 0 {
				return false, fmt.Errorf("adapter: unsafe repository metadata link")
			}
			if info.IsDir() {
				index, err := os.ReadFile(filepath.Join(git, "index"))
				if os.IsNotExist(err) {
					return false, nil
				}
				if err != nil {
					return false, fmt.Errorf("adapter: cannot establish whether .mcp.json is tracked")
				}
				rel, err := filepath.Rel(dir, dest)
				if err != nil {
					return false, err
				}
				needle := []byte(filepath.ToSlash(rel))
				return bytes.Contains(index, needle), nil
			}
			return false, fmt.Errorf("adapter: linked-worktree .mcp.json tracking requires environment indirection")
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return false, nil
		}
		dir = parent
	}
}

func IsSecretConfigPath(path string) bool {
	base := strings.ToLower(filepath.Base(path))
	return base == ".mcp.json" || strings.Contains(base, "mcp") || strings.HasSuffix(base, "config.json")
}
