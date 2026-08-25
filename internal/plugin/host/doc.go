// Package host is the Go SDK plugin authors use to implement an
// Aplexica adapter as an out-of-process plugin. Authors implement
// Handler and call Serve(handler, os.Stdin, os.Stdout) from their
// main(). See docs/plugin-author-guide.md.
package host
