package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestScanRejectsMutableAndExpressionUses(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".github", "workflows")
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	data := []byte("jobs:\n  x:\n    steps:\n      - uses: actions/checkout@v4\n      - uses: ${{ matrix.action }}\n      - uses: owner/repo/path@0123456789abcdef0123456789abcdef01234567\n")
	if err := os.WriteFile(filepath.Join(path, "x.yml"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	findings, err := scan(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 2 {
		t.Fatalf("findings = %+v", findings)
	}
}
