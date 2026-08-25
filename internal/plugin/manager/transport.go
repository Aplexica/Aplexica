// SPDX-License-Identifier: AGPL-3.0-or-later
package manager

import "io"

// stdioTransport adapts a plugin subprocess's stdout (read side) and
// stdin (write side) into a single io.ReadWriter that is ALSO an
// io.Closer. proxy.Open adopts the transport as its closer only when it
// implements io.Closer, so Close MUST close the write end (the plugin's
// stdin) — that is what makes the plugin's host.Serve loop see EOF on its
// stdin and return cleanly.
//
// We deliberately do NOT close the read end (the plugin's stdout) here:
// the daemon-side exec.Cmd owns those pipes and closes them on Wait/Kill.
// Double-closing an os.File is harmless but the manager relies on Close
// closing only stdin so a still-draining response frame is not truncated.
type stdioTransport struct {
	r io.Reader      // plugin stdout — we read response frames from here
	w io.WriteCloser // plugin stdin — we write request frames here
}

// Read implements io.Reader, reading from the plugin's stdout.
func (t *stdioTransport) Read(p []byte) (int, error) { return t.r.Read(p) }

// Write implements io.Writer, writing to the plugin's stdin.
func (t *stdioTransport) Write(p []byte) (int, error) { return t.w.Write(p) }

// Close closes the write half (plugin stdin) so the plugin's host.Serve
// reads io.EOF and shuts down. Idempotent at the os.File level.
func (t *stdioTransport) Close() error { return t.w.Close() }
