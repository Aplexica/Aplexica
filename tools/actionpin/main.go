package main

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

var (
	actionSHA = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+(?:/[A-Za-z0-9_./-]+)?@[0-9a-f]{40}$`)
	dockerSHA = regexp.MustCompile(`^docker://[^[:space:]@]+@sha256:[0-9a-f]{64}$`)
)

type finding struct {
	path  string
	line  int
	value string
}

func scanNode(path string, node *yaml.Node, findings *[]finding) {
	if node.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(node.Content); i += 2 {
			key, value := node.Content[i], node.Content[i+1]
			if key.Value == "uses" && value.Kind == yaml.ScalarNode {
				uses := strings.TrimSpace(value.Value)
				valid := strings.HasPrefix(uses, "./") && !strings.Contains(uses, "..") || actionSHA.MatchString(uses) || dockerSHA.MatchString(uses)
				if !valid {
					*findings = append(*findings, finding{path, value.Line, uses})
				}
			}
			scanNode(path, value, findings)
		}
		return
	}
	for _, child := range node.Content {
		scanNode(path, child, findings)
	}
}

func scan(root string) ([]finding, error) {
	var findings []finding
	for _, sub := range []string{filepath.Join(root, ".github", "workflows"), filepath.Join(root, ".github", "actions")} {
		err := filepath.WalkDir(sub, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				if os.IsNotExist(err) {
					return nil
				}
				return err
			}
			if entry.IsDir() || filepath.Ext(path) != ".yml" && filepath.Ext(path) != ".yaml" {
				return nil
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			var document yaml.Node
			if err := yaml.Unmarshal(b, &document); err != nil {
				return fmt.Errorf("%s: invalid workflow YAML: %w", path, err)
			}
			scanNode(path, &document, &findings)
			return nil
		})
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
	}
	return findings, nil
}

func main() {
	findings, err := scan(".")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	for _, item := range findings {
		fmt.Fprintf(os.Stderr, "%s:%d: mutable action reference %q\n", item.path, item.line, item.value)
	}
	if len(findings) != 0 {
		os.Exit(1)
	}
}
