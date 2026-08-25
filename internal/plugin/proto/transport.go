package proto

import (
	"bufio"
	"fmt"
	"io"
)

const (
	frameReadInitialBuffer = 64 * 1024
	frameReadMaxBuffer     = MaxJSONRPCFrameBytes
)

// FrameReader reads newline-delimited JSON frames from an io.Reader.
// Frames must not contain literal '\n' characters (compact JSON only).
type FrameReader struct {
	scanner *bufio.Scanner
}

// NewFrameReader wraps r. The internal scanner buffer is sized to
// handle near-limit artifact payloads after JSON/base64 framing. This must
// match the cloud plugin's reader cap: a 64 MiB artifact can produce a frame
// over 64 MiB once sealed and encoded.
func NewFrameReader(r io.Reader) *FrameReader {
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, frameReadInitialBuffer), frameReadMaxBuffer)
	return &FrameReader{scanner: s}
}

// Read returns the next frame as a byte slice (without the trailing
// newline), or io.EOF when the stream ends.
func (r *FrameReader) Read() ([]byte, error) {
	if r.scanner.Scan() {
		b := r.scanner.Bytes()
		out := make([]byte, len(b))
		copy(out, b)
		return out, nil
	}
	if err := r.scanner.Err(); err != nil {
		return nil, err
	}
	return nil, io.EOF
}

// FrameWriter writes newline-delimited JSON frames to an io.Writer.
type FrameWriter struct {
	w io.Writer
}

// NewFrameWriter wraps w.
func NewFrameWriter(w io.Writer) *FrameWriter {
	return &FrameWriter{w: w}
}

// Write emits frame followed by '\n'. Caller is responsible for
// ensuring frame is valid compact JSON with no embedded newlines.
func (fw *FrameWriter) Write(frame []byte) error {
	if len(frame) > MaxJSONRPCFrameBytes {
		return fmt.Errorf("proto: JSON-RPC frame exceeds %d bytes", MaxJSONRPCFrameBytes)
	}
	if _, err := fw.w.Write(frame); err != nil {
		return err
	}
	_, err := fw.w.Write([]byte("\n"))
	return err
}
