//go:build !tray

package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr,
		"aplexicatray: this binary was built without -tags tray.\n"+
			"Rebuild with: go build -tags tray -o aplexicatray ./cmd/aplexicatray\n"+
			"Or via Makefile: make tray")
	os.Exit(1)
}
